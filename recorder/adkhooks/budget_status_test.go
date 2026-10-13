package adkhooks

import (
	"context"
	"sync"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"

	"github.com/techbridgeinnovation/agentpulse/recorder"
	governancepb "github.com/techbridgeinnovation/agentpulse/recorder/pb/governance"
)

// BeforeModel hands the product the budget statuses governance sent beside its verdict, and the verdict is what decides the call.
func TestBeforeModelHandsTheProductItsBudgetStatuses(t *testing.T) {
	statuses := []*governancepb.BudgetStatus{
		{Budget: "organisations/techbridge/budgets/team", Scope: governancepb.BudgetStatus_SHARED_POOL, SpentMicros: 800_000, LimitMicros: 1_000_000, Level: governancepb.BudgetStatus_PASSED_75_PERCENT},
	}
	for decision, denied := range map[governancepb.DecideResponse_Decision]bool{
		governancepb.DecideResponse_ALLOW: false,
		governancepb.DecideResponse_DENY:  true,
	} {
		var mu sync.Mutex
		var handed []*governancepb.BudgetStatus
		opts := options()
		opts.Decider = &fakeDecider{resp: &governancepb.DecideResponse{Decision: decision, BudgetStatuses: statuses}}
		opts.OnBudgetStatus = func(_ context.Context, got []*governancepb.BudgetStatus, _ time.Time) {
			mu.Lock()
			defer mu.Unlock()
			handed = got
		}
		r := recorder.New(recorder.Config{FlushEvery: time.Hour})

		resp, err := BeforeModel(r, opts)(newContext(), &adkmodel.LLMRequest{})
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			mu.Lock()
			done := handed != nil
			mu.Unlock()
			if done {
				break
			}
		}
		closing, cancel := context.WithTimeout(context.Background(), time.Second)
		r.Close(closing)
		cancel()

		if err != nil || (resp != nil) != denied {
			t.Errorf("%v: BeforeModel() = (%v, %v), want the call skipped only on DENY", decision, resp, err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(handed) != 1 || handed[0].GetLevel() != governancepb.BudgetStatus_PASSED_75_PERCENT {
			t.Errorf("%v: handed %v, want the one status governance sent", decision, handed)
		}
	}
}

// A product's callback that panics leaves the call to go ahead, and is counted.
func TestBeforeModelSurvivesAPanickingOnBudgetStatus(t *testing.T) {
	opts := options()
	opts.Decider = &fakeDecider{resp: &governancepb.DecideResponse{
		Decision:       governancepb.DecideResponse_ALLOW,
		BudgetStatuses: []*governancepb.BudgetStatus{{Level: governancepb.BudgetStatus_EXHAUSTED, WarnOnly: true}},
	}}
	opts.OnBudgetStatus = func(context.Context, []*governancepb.BudgetStatus, time.Time) { panic("product bug") }
	r := recorder.New(recorder.Config{FlushEvery: time.Hour})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		r.Close(ctx)
	})

	resp, err := BeforeModel(r, opts)(newContext(), &adkmodel.LLMRequest{})
	if resp != nil || err != nil {
		t.Fatalf("BeforeModel() = (%v, %v), want (nil, nil)", resp, err)
	}
	for deadline := time.Now().Add(5 * time.Second); r.Stats().BudgetStatusFailed == 0 && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
	}
	if s := r.Stats(); s.BudgetStatusFailed != 1 || s.Panicked != 1 {
		t.Fatalf("BudgetStatusFailed = %d, Panicked = %d, want 1 and 1", s.BudgetStatusFailed, s.Panicked)
	}
}

// With no statuses, as an older governance answers, nothing is called.
func TestBeforeModelCallsNothingWithoutBudgetStatuses(t *testing.T) {
	called := false
	opts := options()
	opts.Decider = &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_NOTIFY}}
	opts.OnBudgetStatus = func(context.Context, []*governancepb.BudgetStatus, time.Time) { called = true }
	r := recorder.New(recorder.Config{FlushEvery: time.Hour})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		r.Close(ctx)
	})

	if _, err := BeforeModel(r, opts)(newContext(), &adkmodel.LLMRequest{}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("OnBudgetStatus called for a decision carrying no statuses")
	}
}
