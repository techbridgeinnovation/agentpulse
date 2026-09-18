package recorder

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

// record is one activity and the workspace whose spend it is.
//
// The workspace travels beside the activity rather than on it because it is not a field of one: an activity's tenant is the parent its batch was written under, so that a record can never state a tenant that disagrees with where it was filed. Carrying it here is what lets the worker keep a flush's records apart by tenant on the way out.
type record struct {
	activity  *pb.Activity
	workspace string
}

// Recorder accepts records from an agent and delivers them in the background.
type Recorder struct {
	config Config
	queue  chan record

	// names carries people to name, apart from the records so a burst of
	// either cannot crowd out the other, and seen is what was last sent.
	names chan person
	seen  *seenNames

	counters Counters
	totals   *runningTotals
	rates    *rateCache

	closeOnce sync.Once
	done      chan struct{}
	stopped   chan struct{}
}

// Counters report what the recorder has done. Read them with Stats.
type Counters struct {
	Recorded  atomic.Int64
	Dropped   atomic.Int64
	Delivered atomic.Int64
	Failed    atomic.Int64
	Panicked  atomic.Int64

	// Notified counts every Decide call that came back NOTIFY. The model
	// call still proceeds in this version — this is a visible signal for a
	// host to watch, the same way Dropped is, not a block.
	Notified atomic.Int64

	// DecisionErrors counts every Decide call that could not be completed —
	// unreachable, timed out, or refused for a reason such as a
	// misconfigured budget, among others. The model call still proceeds:
	// this integration only ever produces ALLOW or NOTIFY, so an error here
	// has nowhere else to go yet. That is this version's behavior, not a
	// settled fail-open policy — DecisionErrors exists so a degraded
	// governance dependency stays visible rather than silent while that
	// policy is still undecided.
	DecisionErrors atomic.Int64

	// RateFetchErrors counts every rate card refresh that failed.
	//
	// Above zero and still climbing means the running total is being priced against a card that is going stale, or against no card at all if none ever arrived. Both read as an agent spending nothing, which is exactly the shape of silence this counter exists to break.
	RateFetchErrors atomic.Int64

	// Named counts every person queued for the directory: once per person
	// per process, and again when their name changes.
	Named atomic.Int64

	// NamesDropped counts people not queued because the queue was full.
	// They are tried again the next time they act.
	NamesDropped atomic.Int64

	// NamesFailed counts people a sink could not name, and identifiers that
	// cannot be named at all because they hold a slash. Above zero means a
	// report is showing an identifier where a name was given.
	NamesFailed atomic.Int64
}

// Stats is a snapshot of the counters.
type Stats struct {
	// Recorded is how many records were accepted.
	Recorded int64
	// Dropped is how many were discarded because the queue was full. Anything
	// above zero means cost data is incomplete and the queue or the sink needs
	// attention.
	Dropped int64
	// Delivered is how many reached a sink successfully.
	Delivered int64
	// Failed is how many a sink rejected.
	Failed int64
	// Panicked is how many times a sink panicked. Above zero means a sink is
	// broken; the agent was unaffected.
	Panicked int64
	// Notified is how many Decide calls came back NOTIFY.
	Notified int64
	// DecisionErrors is how many Decide calls could not be completed. See
	// Counters.DecisionErrors.
	DecisionErrors int64
	// RateFetchErrors is how many rate card refreshes failed. Above zero and still climbing means the running total is priced against a stale card, or against none. See Counters.RateFetchErrors.
	RateFetchErrors int64
	// TotalsEvicted is how many running totals were dropped because more requests were in flight at once than the recorder keeps totals for. Above zero means SpentOn is an undercount for some request that is still running.
	TotalsEvicted int64
	// Named is how many people were queued for the directory. See Counters.Named.
	Named int64
	// NamesDropped is how many people were not queued because the queue was full.
	NamesDropped int64
	// NamesFailed is how many people could not be named. See Counters.NamesFailed.
	NamesFailed int64
}

// New starts a recorder. It never returns an error: a recorder that cannot be
// configured is not a reason to stop an agent from running.
func New(config Config) *Recorder {
	config = config.withDefaults()
	r := &Recorder{
		config:  config,
		queue:   make(chan record, config.QueueSize),
		names:   make(chan person, config.QueueSize),
		seen:    newSeenNames(),
		totals:  newRunningTotals(),
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	go r.run()

	if config.Rates != nil {
		r.rates = newRateCache(config.Rates)
		go r.keepRatesCurrent()
	}
	return r
}

// Record hands over one record and returns immediately.
//
// It never blocks, never returns an error and never panics. If the queue is
// full the record is dropped and counted — losing a record is always preferable
// to delaying the work the agent was actually asked to do.
//
// The record names no workspace and is filed under the organisation. A product
// with one tenant has nothing more to say; a product serving several says which
// on the context and hands the record over with RecordIn.
func (r *Recorder) Record(activity *pb.Activity) {
	r.enqueue(record{activity: activity})
}

// RecordIn hands over one record produced under ctx, and returns immediately.
//
// The same contract as Record in every respect. What ctx adds is the workspace the work was for, read from it here rather than asked for at the call site, so that the tenant a record is filed under is the one the request was authenticated for and not one a call site chose.
func (r *Recorder) RecordIn(ctx context.Context, activity *pb.Activity) {
	r.enqueue(record{activity: activity, workspace: WorkspaceFrom(ctx)})
}

func (r *Recorder) enqueue(entry record) {
	if r == nil || entry.activity == nil {
		return
	}
	// The running total is kept whether or not the record survives the queue.
	// A dropped record still cost money, and a spend decision that ignored it
	// would be wrong in the one direction that matters.
	r.totals.add(entry.activity.GetRequest(), r.estimate(entry.activity))

	select {
	case r.queue <- entry:
		r.counters.Recorded.Add(1)
	default:
		r.counters.Dropped.Add(1)
	}
}

// Stats returns a snapshot of what the recorder has done.
func (r *Recorder) Stats() Stats {
	if r == nil {
		return Stats{}
	}
	return Stats{
		Recorded:        r.counters.Recorded.Load(),
		Dropped:         r.counters.Dropped.Load(),
		Delivered:       r.counters.Delivered.Load(),
		Failed:          r.counters.Failed.Load(),
		Panicked:        r.counters.Panicked.Load(),
		Notified:        r.counters.Notified.Load(),
		DecisionErrors:  r.counters.DecisionErrors.Load(),
		RateFetchErrors: r.counters.RateFetchErrors.Load(),
		TotalsEvicted:   r.totals.evictedCount(),
		Named:           r.counters.Named.Load(),
		NamesDropped:    r.counters.NamesDropped.Load(),
		NamesFailed:     r.counters.NamesFailed.Load(),
	}
}

// estimate is what a record is expected to cost, for the running total and for nothing else.
//
// Never written onto the record itself. Metering prices what it stores against the card it holds, and discards whatever a caller claimed, which is what makes the stored figures trustworthy: a recorder does not get to assert what its own work was worth. This figure answers a different question, in this process only, which is what the request in hand has spent so far without a network call to ask.
//
// With no card in hand the record's own figure stands, which is nothing, because neither of the two ways in fills it in. A recorder configured with no rate source therefore behaves exactly as it did before there was one.
func (r *Recorder) estimate(activity *pb.Activity) int64 {
	if card := r.rates.current(); card != nil {
		return card.priceOf(activity)
	}
	return activity.GetEstimatedCostMicros()
}

// NoteNotified records that a Decide call returned NOTIFY.
//
// Exported so the adk adapter — a separate package — can report into the
// same Counters/Stats surface everything else here uses, rather than
// keeping a second, disconnected set of counters.
func (r *Recorder) NoteNotified() {
	if r == nil {
		return
	}
	r.counters.Notified.Add(1)
}

// NoteDecisionError records that a Decide call could not be completed. See
// Counters.DecisionErrors.
func (r *Recorder) NoteDecisionError() {
	if r == nil {
		return
	}
	r.counters.DecisionErrors.Add(1)
}

// Close stops the recorder and makes a final attempt to deliver what is queued.
//
// It returns when the queue is drained or ctx is done, whichever comes first.
// Shutting down is not a reason to hang: anything still queued when ctx expires
// is counted as dropped.
func (r *Recorder) Close(ctx context.Context) {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() { close(r.done) })
	select {
	case <-r.stopped:
	case <-ctx.Done():
	}
}

func (r *Recorder) run() {
	defer close(r.stopped)

	ticker := time.NewTicker(r.config.FlushEvery)
	defer ticker.Stop()

	batch := make([]record, 0, r.config.BatchSize)
	people := make([]person, 0, r.config.BatchSize)

	for {
		select {
		case entry := <-r.queue:
			batch = append(batch, entry)
			if len(batch) >= r.config.BatchSize {
				r.deliver(batch)
				batch = batch[:0]
			}

		case user := <-r.names:
			people = append(people, user)
			if len(people) >= r.config.BatchSize {
				r.deliverNames(people)
				people = people[:0]
			}

		case <-ticker.C:
			if len(batch) > 0 {
				r.deliver(batch)
				batch = batch[:0]
			}
			if len(people) > 0 {
				r.deliverNames(people)
				people = people[:0]
			}

		case <-r.done:
			// Drain what is already queued, then deliver and stop.
			for {
				select {
				case entry := <-r.queue:
					batch = append(batch, entry)
					if len(batch) >= r.config.BatchSize {
						r.deliver(batch)
						batch = batch[:0]
					}
					continue
				case user := <-r.names:
					people = append(people, user)
					if len(people) >= r.config.BatchSize {
						r.deliverNames(people)
						people = people[:0]
					}
					continue
				default:
				}
				break
			}
			if len(batch) > 0 {
				r.deliver(batch)
			}
			if len(people) > 0 {
				r.deliverNames(people)
			}
			return
		}
	}
}

// keepRatesCurrent fetches the rate card on its own schedule until the recorder is closed.
//
// Its own goroutine, apart from the delivery loop and far apart from anything a model call waits on. A rate source that is slow, unreachable or broken delays nothing an agent is doing: the price of that is a card that is stale or absent, and a card that is absent prices at nothing, which is where this started.
//
// The first fetch happens straight away rather than after one interval, so a process has prices as soon as it can have them.
func (r *Recorder) keepRatesCurrent() {
	ticker := time.NewTicker(r.config.RateRefresh)
	defer ticker.Stop()

	for {
		r.fetchRates()

		select {
		case <-r.done:
			return
		case <-ticker.C:
		}
	}
}

// fetchRates makes one attempt, isolated so a rate source that fails or panics costs its host nothing.
//
// Bounded by SendTimeout rather than a timeout of its own: both are the same question, how long one call to a platform service may take before it is abandoned, and a second knob for it would be a second answer to drift from the first.
func (r *Recorder) fetchRates() {
	defer func() {
		if recovered := recover(); recovered != nil {
			r.counters.Panicked.Add(1)
			r.counters.RateFetchErrors.Add(1)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), r.config.SendTimeout)
	defer cancel()

	if err := r.rates.refresh(ctx); err != nil {
		r.counters.RateFetchErrors.Add(1)
	}
}

// workspaceBatch is the part of one flush that belongs to one workspace.
type workspaceBatch struct {
	workspace  string
	activities []*pb.Activity
}

// groupByWorkspace splits a flush into one batch per workspace, in the order the workspaces were first seen.
//
// A destination that files records under a parent can only file one batch under one parent, so a flush that covers several tenants is several batches however it is delivered. Splitting it here, rather than leaving each sink to do it, is what keeps a failure for one tenant from touching another's records and keeps Delivered and Failed counting what they always counted: records that reached a destination and records that did not.
//
// Each group holds its own slice, so the worker is free to reuse the flush it passed in.
func groupByWorkspace(batch []record) []workspaceBatch {
	groups := make([]workspaceBatch, 0, 1)
	at := make(map[string]int, 1)

	for _, entry := range batch {
		position, held := at[entry.workspace]
		if !held {
			position = len(groups)
			at[entry.workspace] = position
			groups = append(groups, workspaceBatch{workspace: entry.workspace})
		}
		groups[position].activities = append(groups[position].activities, entry.activity)
	}
	return groups
}

// deliver sends one batch per workspace to every sink, each isolated from the others.
//
// Sinks run concurrently so a slow one does not delay the rest, and each is
// wrapped so a panic inside a sink is counted rather than taking down the
// process it is embedded in.
func (r *Recorder) deliver(batch []record) {
	var wg sync.WaitGroup
	for _, group := range groupByWorkspace(batch) {
		for _, sink := range r.config.Sinks {
			wg.Add(1)
			go func(s Sink, g workspaceBatch) {
				defer wg.Done()
				r.sendTo(s, g)
			}(sink, group)
		}
	}
	wg.Wait()
}

func (r *Recorder) sendTo(sink Sink, group workspaceBatch) {
	defer func() {
		if recovered := recover(); recovered != nil {
			r.counters.Panicked.Add(1)
			r.counters.Failed.Add(int64(len(group.activities)))
		}
	}()

	ctx, cancel := context.WithTimeout(WithWorkspace(context.Background(), group.workspace), r.config.SendTimeout)
	defer cancel()

	if err := sink.Send(ctx, group.activities); err != nil {
		r.counters.Failed.Add(int64(len(group.activities)))
		return
	}
	r.counters.Delivered.Add(int64(len(group.activities)))
}
