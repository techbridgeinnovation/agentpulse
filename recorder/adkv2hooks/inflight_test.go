package adkv2hooks

import (
	"fmt"
	"testing"
	"time"
)

func TestInFlightDowngradeStorePutThenTake(t *testing.T) {
	store := newInFlightDowngradeStore(8, time.Hour, time.Now)
	store.put("call-1", "gemini-3.5-nano")

	got, ok := store.take("call-1")
	if !ok || got != "gemini-3.5-nano" {
		t.Fatalf("take = (%q, %v), want (gemini-3.5-nano, true)", got, ok)
	}
}

// take forgets what it returns, since the callback that pairs with it has
// now run — a second take for the same call must find nothing, exactly like
// adkhooks' own inFlightStore.
func TestInFlightDowngradeStoreTakeForgetsTheEntry(t *testing.T) {
	store := newInFlightDowngradeStore(8, time.Hour, time.Now)
	store.put("call-1", "gemini-3.5-nano")

	if _, ok := store.take("call-1"); !ok {
		t.Fatal("first take found nothing")
	}
	if _, ok := store.take("call-1"); ok {
		t.Fatal("second take for the same call found an entry, want it already forgotten")
	}
}

func TestInFlightDowngradeStoreTakeOnAnUnknownKeyIsAMiss(t *testing.T) {
	store := newInFlightDowngradeStore(8, time.Hour, time.Now)
	if _, ok := store.take("never-put"); ok {
		t.Fatal("take found an entry for a key nothing ever put, want a miss")
	}
}

// A call that never reaches AfterModel — cut short elsewhere — must not
// make the store grow without bound for as long as the process runs.
func TestInFlightDowngradeStoreStaysWithinItsCap(t *testing.T) {
	clock := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	store := newInFlightDowngradeStore(8, time.Hour, func() time.Time { return clock })

	for i := range 100 {
		store.put(fmt.Sprintf("call-%d", i), "gemini-3.5-nano")
		clock = clock.Add(time.Second)
	}

	if size := store.size(); size > 8 {
		t.Fatalf("store holds %d calls, want at most 8", size)
	}
	if _, ok := store.take("call-99"); !ok {
		t.Fatal("the most recent call was evicted ahead of older ones")
	}
}

func TestInFlightDowngradeStoreExpiredEntryIsNotUsed(t *testing.T) {
	clock := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	store := newInFlightDowngradeStore(8, time.Minute, func() time.Time { return clock })

	store.put("call", "gemini-3.5-nano")
	clock = clock.Add(2 * time.Minute)

	if _, ok := store.take("call"); ok {
		t.Fatal("take returned an entry past its ttl, want a miss")
	}
}

func TestCallKeyKeepsSubAgentsAndBranchesApart(t *testing.T) {
	a := callKey(&fakeContext{StrictContextMock: newContext().StrictContextMock, invocation: "inv-1", agentName: "atlas"})
	b := callKey(&fakeContext{StrictContextMock: newContext().StrictContextMock, invocation: "inv-1", agentName: "russell"})
	if a == b {
		t.Fatal("two different agents in the same invocation produced the same key")
	}
}
