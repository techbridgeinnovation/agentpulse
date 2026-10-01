package adkv2hooks

import (
	"fmt"
	"testing"
	"time"
)

// A call that never reaches its final response — cut short by another
// before-model callback — must not make the store grow for as long as the
// process runs.
func TestTheInFlightStoreStaysWithinItsCap(t *testing.T) {
	clock := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	store := newInFlightModels(8, time.Hour, func() time.Time { return clock })

	for i := range 100 {
		store.requested(fmt.Sprintf("call-%d", i), "gemini-3.5-nano")
		clock = clock.Add(time.Second)
	}

	if size := store.size(); size > 8 {
		t.Fatalf("store holds %d calls, want at most 8", size)
	}
	if got := store.take("call-99"); got.requested == "" {
		t.Fatal("the most recent call was evicted ahead of older ones")
	}
}

// take forgets what it returns, since the callback that pairs with it has now run.
func TestTakeForgetsTheCall(t *testing.T) {
	store := newInFlightModels(8, time.Hour, time.Now)
	store.requested("call-1", "gemini-3.5-nano")

	if got := store.take("call-1"); got.requested != "gemini-3.5-nano" {
		t.Fatalf("first take = %+v, want the model requested", got)
	}
	if got := store.take("call-1"); got.requested != "" {
		t.Fatalf("second take = %+v, want the call already forgotten", got)
	}
}

func TestAnExpiredCallIsNotUsed(t *testing.T) {
	clock := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	store := newInFlightModels(8, time.Minute, func() time.Time { return clock })

	store.requested("call", "gemini-3.5-nano")
	clock = clock.Add(2 * time.Minute)

	if got := store.take("call"); got.requested != "" || got.served != "" {
		t.Fatalf("take returned %+v for a call past its ttl, want nothing", got)
	}
}

// A chunk arriving for a call past its life must not revive what was held for it, or a failure would be timed from a call long gone.
func TestAnExpiredCallIsNotRevivedByALaterChunk(t *testing.T) {
	clock := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	store := newInFlightModels(8, time.Minute, func() time.Time { return clock })

	store.requested("call", "gemini-3.5-nano")
	clock = clock.Add(2 * time.Minute)
	store.chunk("call", "gemini-3.5-nano-001", nil)

	if got := store.fail("call"); got.requested != "" || !got.startedAt.IsZero() {
		t.Fatalf("fail returned %+v for a call past its ttl, want only what the late chunk reported", got)
	}
}

func TestCallKeyKeepsSubAgentsAndBranchesApart(t *testing.T) {
	a := callKey(&fakeContext{StrictContextMock: newContext().StrictContextMock, invocation: "inv-1", agentName: "atlas"})
	b := callKey(&fakeContext{StrictContextMock: newContext().StrictContextMock, invocation: "inv-1", agentName: "russell"})
	if a == b {
		t.Fatal("two different agents in the same invocation produced the same key")
	}
}
