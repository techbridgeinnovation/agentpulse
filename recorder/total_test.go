package recorder

import (
	"context"
	"fmt"
	"testing"
	"time"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

func costing(request string, micros int64) *pb.Activity {
	return &pb.Activity{Name: "x", Request: request, EstimatedCostMicros: micros}
}

func TestSpentOnAccumulatesAcrossTheCallsInOneRequest(t *testing.T) {
	r := New(Config{FlushEvery: time.Hour})
	t.Cleanup(func() { r.Close(context.Background()) })

	r.Record(costing("req-1", 140000))
	r.Record(costing("req-1", 190000))
	r.Record(costing("req-1", 90000))

	if got := r.SpentOn("req-1"); got != 420000 {
		t.Fatalf("SpentOn = %d, want 420000", got)
	}
}

func TestSpentOnKeepsRequestsApart(t *testing.T) {
	r := New(Config{FlushEvery: time.Hour})
	t.Cleanup(func() { r.Close(context.Background()) })

	r.Record(costing("req-1", 100))
	r.Record(costing("req-2", 900))

	if got := r.SpentOn("req-1"); got != 100 {
		t.Fatalf("req-1 = %d, want 100", got)
	}
	if got := r.SpentOn("req-2"); got != 900 {
		t.Fatalf("req-2 = %d, want 900", got)
	}
}

// A dropped record still cost money. Leaving it out of the running total would
// under-report spend, which is wrong in the one direction that matters for a cap.
func TestADroppedRecordStillCountsTowardsTheRunningTotal(t *testing.T) {
	r := New(Config{
		Sinks:      []Sink{&captureSink{block: time.Hour}},
		QueueSize:  1,
		FlushEvery: time.Hour,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		r.Close(ctx)
	})

	for range 100 {
		r.Record(costing("req-1", 1000))
	}

	if s := r.Stats(); s.Dropped == 0 {
		t.Fatal("expected drops with a queue of one and a blocked sink")
	}
	if got := r.SpentOn("req-1"); got != 100000 {
		t.Fatalf("SpentOn = %d, want 100000 — dropped records still cost money", got)
	}
}

func TestFinishRequestReleasesTheTotal(t *testing.T) {
	r := New(Config{FlushEvery: time.Hour})
	t.Cleanup(func() { r.Close(context.Background()) })

	r.Record(costing("req-1", 500))
	r.FinishRequest("req-1")

	if got := r.SpentOn("req-1"); got != 0 {
		t.Fatalf("SpentOn after FinishRequest = %d, want 0", got)
	}
}

func TestRecordsWithNoRequestAreNotAccumulatedAnywhere(t *testing.T) {
	r := New(Config{FlushEvery: time.Hour})
	t.Cleanup(func() { r.Close(context.Background()) })

	r.Record(costing("", 5000))

	if got := r.SpentOn(""); got != 0 {
		t.Fatalf("SpentOn with no request = %d, want 0", got)
	}
}

// totalsClock is the shared fakeClock started somewhere fixed, so a cap and a fifteen minute expiry are reached without four thousand records or a fifteen minute wait.
func totalsClock() *fakeClock {
	return newFakeClock(time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC))
}

// The map is bounded whatever a caller does. A caller that never releases a request is the ordinary case this has to survive, not the exceptional one.
func TestTheMapCannotGrowPastItsCap(t *testing.T) {
	clock := totalsClock()
	totals := newRunningTotalsWith(64, requestTotalTTL, clock.Now)

	for i := range 5000 {
		totals.add(fmt.Sprintf("requests/%d", i), 100)
		clock.Advance(time.Millisecond)
	}

	if len(totals.totals) > 64 {
		t.Fatalf("holding %d totals, want at most 64", len(totals.totals))
	}
	if totals.evictedCount() == 0 {
		t.Fatal("evictions were not counted, so the loss would be silent")
	}
}

// The most recent requests are the ones still running, so they are the ones kept.
func TestEvictionDropsTheOldestFirst(t *testing.T) {
	clock := totalsClock()
	totals := newRunningTotalsWith(64, requestTotalTTL, clock.Now)

	for i := range 200 {
		totals.add(fmt.Sprintf("requests/%d", i), 100)
		clock.Advance(time.Millisecond)
	}

	if got := totals.get("requests/199"); got != 100 {
		t.Fatalf("the newest request reads %d, want 100", got)
	}
	if got := totals.get("requests/0"); got != 0 {
		t.Fatalf("the oldest request reads %d, want 0", got)
	}
}

// An entry goes on its own, whether or not anything ever calls FinishRequest.
func TestAnOldTotalIsForgottenWithoutFinishRequest(t *testing.T) {
	clock := totalsClock()
	totals := newRunningTotalsWith(64, requestTotalTTL, clock.Now)

	totals.add("requests/abandoned", 5000)
	clock.Advance(requestTotalTTL + time.Minute)

	if got := totals.get("requests/abandoned"); got != 0 {
		t.Fatalf("an expired total reads %d, want 0", got)
	}

	// The next record is what sweeps it out of the map, so the memory goes with it.
	totals.add("requests/live", 100)
	if _, held := totals.totals["requests/abandoned"]; held {
		t.Fatal("the expired entry is still held, which is the leak this prevents")
	}
}

// A request that expires and then records again starts from that record, rather than accumulating onto a figure that was already due to be forgotten.
func TestAnExpiredTotalStartsAgainRatherThanAccumulating(t *testing.T) {
	clock := totalsClock()
	totals := newRunningTotalsWith(64, requestTotalTTL, clock.Now)

	totals.add("requests/7f3a", 5000)
	clock.Advance(requestTotalTTL + time.Minute)

	if got := totals.add("requests/7f3a", 300); got != 300 {
		t.Fatalf("total after expiry = %d, want 300", got)
	}
}

// A live request keeps its total for as long as it keeps spending, however long the turn runs.
func TestATotalThatKeepsBeingAddedToDoesNotExpire(t *testing.T) {
	clock := totalsClock()
	totals := newRunningTotalsWith(64, requestTotalTTL, clock.Now)

	for range 10 {
		totals.add("requests/7f3a", 100)
		clock.Advance(requestTotalTTL - time.Minute)
	}

	if got := totals.get("requests/7f3a"); got != 1000 {
		t.Fatalf("SpentOn on a long running request = %d, want 1000", got)
	}
}

func TestTheRequestTravelsOnTheContext(t *testing.T) {
	ctx := WithRequest(context.Background(), "req-1")

	if got := RequestFrom(ctx); got != "req-1" {
		t.Fatalf("RequestFrom = %q, want req-1", got)
	}
	if got := RequestFrom(context.Background()); got != "" {
		t.Fatalf("RequestFrom on a bare context = %q, want empty", got)
	}
}
