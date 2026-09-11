package recorder

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

// Recorder accepts records from an agent and delivers them in the background.
type Recorder struct {
	config Config
	queue  chan *pb.Activity

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
}

// New starts a recorder. It never returns an error: a recorder that cannot be
// configured is not a reason to stop an agent from running.
func New(config Config) *Recorder {
	config = config.withDefaults()
	r := &Recorder{
		config:  config,
		queue:   make(chan *pb.Activity, config.QueueSize),
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
func (r *Recorder) Record(activity *pb.Activity) {
	if r == nil || activity == nil {
		return
	}
	// The running total is kept whether or not the record survives the queue.
	// A dropped record still cost money, and a spend decision that ignored it
	// would be wrong in the one direction that matters.
	r.totals.add(activity.GetRequest(), r.estimate(activity))

	select {
	case r.queue <- activity:
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

	batch := make([]*pb.Activity, 0, r.config.BatchSize)

	for {
		select {
		case activity := <-r.queue:
			batch = append(batch, activity)
			if len(batch) >= r.config.BatchSize {
				r.deliver(batch)
				batch = batch[:0]
			}

		case <-ticker.C:
			if len(batch) > 0 {
				r.deliver(batch)
				batch = batch[:0]
			}

		case <-r.done:
			// Drain what is already queued, then deliver and stop.
			for {
				select {
				case activity := <-r.queue:
					batch = append(batch, activity)
					if len(batch) >= r.config.BatchSize {
						r.deliver(batch)
						batch = batch[:0]
					}
					continue
				default:
				}
				break
			}
			if len(batch) > 0 {
				r.deliver(batch)
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

// deliver sends one batch to every sink, each isolated from the others.
//
// Sinks run concurrently so a slow one does not delay the rest, and each is
// wrapped so a panic inside a sink is counted rather than taking down the
// process it is embedded in.
func (r *Recorder) deliver(batch []*pb.Activity) {
	sent := make([]*pb.Activity, len(batch))
	copy(sent, batch)

	var wg sync.WaitGroup
	for _, sink := range r.config.Sinks {
		wg.Add(1)
		go func(s Sink) {
			defer wg.Done()
			r.sendTo(s, sent)
		}(sink)
	}
	wg.Wait()
}

func (r *Recorder) sendTo(sink Sink, batch []*pb.Activity) {
	defer func() {
		if recovered := recover(); recovered != nil {
			r.counters.Panicked.Add(1)
			r.counters.Failed.Add(int64(len(batch)))
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), r.config.SendTimeout)
	defer cancel()

	if err := sink.Send(ctx, batch); err != nil {
		r.counters.Failed.Add(int64(len(batch)))
		return
	}
	r.counters.Delivered.Add(int64(len(batch)))
}
