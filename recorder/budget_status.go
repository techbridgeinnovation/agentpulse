package recorder

import (
	"context"
	"time"

	"google.golang.org/protobuf/proto"

	governancepb "github.com/techbridgeinnovation/agentpulse/recorder/pb/governance"
)

// budgetStatusQueueSize bounds how many deliveries wait for the product's callback. Not configurable, for the reason the record queue's bound is not: a slow callback must cost a counted drop, never memory.
const budgetStatusQueueSize = 64

// BudgetStatusFunc is handed where every budget a call is held to stands, most pressing first, after an instrumented call obtains a decision that carries any.
//
// It runs on the recorder's own goroutine, one delivery at a time and in the order the decisions were obtained, never on the call's. Delivery is asynchronous, so by the time it runs the model call may already be under way or finished. ctx carries the values the call's context did, but is not cancelled when the call ends.
//
// evaluated is when governance read the figures, and the zero time when it did not say. The figures are a delayed snapshot, never a live balance and never a hard cap: a cached decision is up to the cache's lifetime old, and what a call spends is recorded after it finishes, so the call that crosses a threshold is not in the figures that let it through. A budget that is a reserve may have a limit of zero, so a percentage is never computed from it.
//
// Agent Pulse shows nothing to the product's own users. A warning they see is one the product's code shows from what it is handed here.
type BudgetStatusFunc func(ctx context.Context, statuses []*governancepb.BudgetStatus, evaluated time.Time)

// budgetStatusEvent is one decision's statuses waiting for the product's callback.
type budgetStatusEvent struct {
	ctx       context.Context
	fn        BudgetStatusFunc
	statuses  []*governancepb.BudgetStatus
	evaluated time.Time
}

// ReportBudgetStatus queues the budget statuses on resp for fn and returns at once; the call never waits for fn.
//
// It is the one place every instrumented path — the adk callbacks and the direct clients — hands statuses to a product, so they behave the same. Nothing is queued when fn is nil, when resp is nil because no decision was obtained, or when governance sent no statuses, which an older governance never does.
//
// One worker per Recorder, started on first use, calls fn once per event, in order. Delivery is at most once: a full queue drops the event and counts it in BudgetStatusDropped, and events still queued when Close is called or the process exits are discarded, counted in BudgetStatusDropped on Close. A callback that panics is recovered and counted in Panicked and BudgetStatusFailed. The recorder never interrupts a callback that is running, so a slow one only delays later deliveries, which are dropped once the queue is full. Close does not wait for it.
//
// fn is handed copies, so it cannot change the verdict or a cached decision a later call reuses.
//
// Exported for the same reason NoteNotified is: the adk and adkv2 adapters are separate packages.
func (r *Recorder) ReportBudgetStatus(ctx context.Context, fn BudgetStatusFunc, resp *governancepb.DecideResponse) {
	if r == nil || fn == nil || len(resp.GetBudgetStatuses()) == 0 {
		return
	}
	statuses := make([]*governancepb.BudgetStatus, 0, len(resp.GetBudgetStatuses()))
	for _, status := range resp.GetBudgetStatuses() {
		if status != nil {
			statuses = append(statuses, proto.Clone(status).(*governancepb.BudgetStatus))
		}
	}
	if len(statuses) == 0 {
		return
	}
	var evaluated time.Time
	if at := resp.GetEvaluateTime(); at != nil {
		evaluated = at.AsTime()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if r.closed.Load() {
		r.counters.BudgetStatusDropped.Add(1)
		return
	}
	r.budgetOnce.Do(func() {
		r.budgetQueue = make(chan budgetStatusEvent, budgetStatusQueueSize)
		go r.deliverBudgetStatuses()
	})
	select {
	case r.budgetQueue <- budgetStatusEvent{ctx: context.WithoutCancel(ctx), fn: fn, statuses: statuses, evaluated: evaluated}:
	default:
		r.counters.BudgetStatusDropped.Add(1)
	}
}

// deliverBudgetStatuses is the one worker that calls the product's callback, until Close.
func (r *Recorder) deliverBudgetStatuses() {
	for {
		select {
		case <-r.done:
			return
		case event := <-r.budgetQueue:
			// Close drained or is draining the queue; an event taken after it is discarded and counted rather than delivered into a shutdown.
			if r.closed.Load() {
				r.counters.BudgetStatusDropped.Add(1)
				continue
			}
			r.callBudgetStatus(event)
		}
	}
}

// callBudgetStatus runs one delivery and recovers whatever the product's callback does.
func (r *Recorder) callBudgetStatus(event budgetStatusEvent) {
	defer func() {
		if recover() != nil {
			r.NotePanicked()
			r.counters.BudgetStatusFailed.Add(1)
		}
	}()
	event.fn(event.ctx, event.statuses, event.evaluated)
}

// discardBudgetStatuses counts what is still queued when the recorder closes, taken from the same queue the worker reads so each event is delivered or counted, never both.
func (r *Recorder) discardBudgetStatuses() {
	// Once.Do returns only after a running first call finishes, so the queue is either fully made or never will be.
	r.budgetOnce.Do(func() {})
	if r.budgetQueue == nil {
		return
	}
	for {
		select {
		case <-r.budgetQueue:
			r.counters.BudgetStatusDropped.Add(1)
		default:
			return
		}
	}
}
