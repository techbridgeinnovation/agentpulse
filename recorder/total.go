package recorder

import (
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// maxTrackedRequests is the most requests a running total is kept for at once.
	//
	// Fixed rather than configurable, and deliberately so: this is the limit that makes the map incapable of growing without bound whatever a caller does, and a limit a caller can raise is not that. It is far above the requests any one agent process has in flight, and an entry is a request id and two machine words, so the whole map at its cap is a fraction of a megabyte.
	maxTrackedRequests = 4096

	// requestTotalTTL is how long a request's total survives without being added to.
	//
	// Longer than any agent turn, which runs in seconds, so a live request is never forgotten under it. Short enough that a request abandoned midway is reclaimed while the process is still the same size.
	requestTotalTTL = 15 * time.Minute

	// totalsSweepEvery bounds how often expired entries are swept out.
	//
	// Sweeping is one pass over a map of at most maxTrackedRequests entries, so it is measured in microseconds, but it happens in the host's own goroutine on the way out of a model call and there is no reason to do it more often than this.
	totalsSweepEvery = time.Minute

	// evictionFraction is the share of the map dropped when it is full, as a divisor.
	//
	// A batch rather than one entry at a time. Dropping one entry would mean scanning for the oldest on every record once the cap is reached; dropping a share of them pays that scan once and leaves room for many more records before the next.
	evictionFraction = 8
)

// runningTotals tracks what each in-flight request has spent so far.
//
// Exact and local: no network call, so the spend decision before a model call costs nothing. It only ever knows about this process, which is why it is half of the enforcement story and not the whole of it. The other half is the budget envelope governance hands out.
//
// It cannot grow without bound. An entry expires on its own after requestTotalTTL and the map never holds more than maxTrackedRequests of them, so a caller who never calls FinishRequest costs this process a bounded amount of memory rather than a leak. A library running inside someone else's agent does not get to depend on its adopter remembering something.
type runningTotals struct {
	max int
	ttl time.Duration
	now func() time.Time

	mu        sync.Mutex
	totals    map[string]requestTotal
	lastSweep time.Time

	// evicted counts totals dropped because more requests were in flight than the cap allows. Expiry is not counted: an entry passing its TTL is a finished request being reclaimed, while an eviction is a live request whose total is now an undercount, and an undercount is wrong in the one direction that matters for a spend limit.
	evicted atomic.Int64
}

// requestTotal is one request's spend and when it was last added to.
type requestTotal struct {
	micros    int64
	touchedAt time.Time
}

func newRunningTotals() *runningTotals {
	return newRunningTotalsWith(maxTrackedRequests, requestTotalTTL, time.Now)
}

// newRunningTotalsWith is newRunningTotals with the limits and the clock injected, so tests can reach the cap and the expiry without four thousand records or a fifteen minute wait.
func newRunningTotalsWith(max int, ttl time.Duration, now func() time.Time) *runningTotals {
	return &runningTotals{
		max:       max,
		ttl:       ttl,
		now:       now,
		totals:    make(map[string]requestTotal),
		lastSweep: now(),
	}
}

// add accumulates spend against a request and returns the new total.
//
// An entry that has already expired starts again from this record rather than accumulating onto a figure that was due to be forgotten, so the answer never depends on whether a sweep happened to have run.
func (t *runningTotals) add(request string, micros int64) int64 {
	if request == "" {
		return 0
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	t.sweep(now)

	existing, held := t.totals[request]
	total := micros
	switch {
	case held && !t.expired(existing, now):
		total = existing.micros + micros
	case !held && len(t.totals) >= t.max:
		// Room is only needed for a request the map is not already holding a slot for.
		t.makeRoom(now)
	}

	t.totals[request] = requestTotal{micros: total, touchedAt: now}
	return total
}

// get reads a request's total so far.
func (t *runningTotals) get(request string) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()

	entry, ok := t.totals[request]
	if !ok || t.expired(entry, t.now()) {
		return 0
	}
	return entry.micros
}

// forget releases a finished request.
//
// Called when a request completes, which is what keeps the map to the requests actually in flight rather than to whatever the cap and the TTL allow.
func (t *runningTotals) forget(request string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.totals, request)
}

// evictedCount is how many totals were dropped for want of room. See the field.
func (t *runningTotals) evictedCount() int64 {
	return t.evicted.Load()
}

func (t *runningTotals) expired(entry requestTotal, now time.Time) bool {
	return now.Sub(entry.touchedAt) >= t.ttl
}

// sweep drops expired entries, at most once every totalsSweepEvery.
//
// Called with the lock held.
func (t *runningTotals) sweep(now time.Time) {
	if now.Sub(t.lastSweep) < totalsSweepEvery {
		return
	}
	t.lastSweep = now

	for request, entry := range t.totals {
		if t.expired(entry, now) {
			delete(t.totals, request)
		}
	}
}

// makeRoom drops the oldest share of the map so a new request has somewhere to go.
//
// Reached only when more requests are genuinely in flight than the cap allows, since anything expired has already been swept. The oldest go first because the longest a request has gone without spending anything is the best available guess at which one is over.
//
// Called with the lock held.
func (t *runningTotals) makeRoom(now time.Time) {
	// Expired entries the sweep interval has not come round to yet are free to take.
	for request, entry := range t.totals {
		if t.expired(entry, now) {
			delete(t.totals, request)
		}
	}
	if len(t.totals) < t.max {
		return
	}

	drop := t.max / evictionFraction
	if drop < 1 {
		drop = 1
	}

	// A batch rather than one entry, so the cost of finding the oldest is paid once and then leaves room for many more records, instead of once per record for as long as the map stays full.
	cutoff := nthOldest(t.totals, drop)
	for request, entry := range t.totals {
		if !entry.touchedAt.After(cutoff) {
			delete(t.totals, request)
			t.evicted.Add(1)
		}
	}
}

// nthOldest returns the timestamp of the nth oldest entry, which is the cutoff at or below which an entry is dropped.
func nthOldest(totals map[string]requestTotal, n int) time.Time {
	touched := make([]time.Time, 0, len(totals))
	for _, entry := range totals {
		touched = append(touched, entry.touchedAt)
	}
	slices.SortFunc(touched, func(a, b time.Time) int { return a.Compare(b) })
	if n > len(touched) {
		n = len(touched)
	}
	return touched[n-1]
}

// SpentOn returns what the given request has cost so far in this process, in millionths of a dollar.
//
// Exact for work this process did, and blind to anything another replica spent on the same request. Use it for a per-request ceiling, not an organisation's budget.
//
// Zero when no rate card has reached this process, because a record priced against no rates costs nothing. See Config.Rates.
func (r *Recorder) SpentOn(request string) int64 {
	if r == nil {
		return 0
	}
	return r.totals.get(request)
}

// FinishRequest releases the running total for a request.
//
// Call it when a request is done. The framework hooks call it for an agent on ADK, so an agent gets this for free; code reporting for itself calls it where the request ends.
//
// Not calling it does not leak: an entry expires on its own and the map has a hard cap. It does mean an entry stays around for longer than the request did, which is memory held for nothing and an eviction that may come sooner for some other request.
func (r *Recorder) FinishRequest(request string) {
	if r == nil {
		return
	}
	r.totals.forget(request)
}
