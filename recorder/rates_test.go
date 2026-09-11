package recorder

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

// fakeRates stands in for the metering rate card.
type fakeRates struct {
	mu    sync.Mutex
	units []*pb.PriceableUnit
	err   error
	panic bool
}

func (f *fakeRates) ListRates(context.Context) ([]*pb.PriceableUnit, error) {
	if f.panic {
		panic("a rate source that misbehaves")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.units, f.err
}

// geminiRates is one real pair of prices: input at 1.25 millionths of a dollar per token, output at 10.
func geminiRates() *fakeRates {
	from := time.Now().Add(-24 * time.Hour)
	return &fakeRates{units: []*pb.PriceableUnit{
		rate("priceableUnits/gemini-in", "VERTEX_AI", "gemini-2.5-pro", pb.PriceableUnit_PROMPT_TOKENS, 1250, from, time.Time{}),
		rate("priceableUnits/gemini-out", "VERTEX_AI", "gemini-2.5-pro", pb.PriceableUnit_CANDIDATE_TOKENS, 10000, from, time.Time{}),
	}}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func reporterFor(r *Recorder) *Reporter {
	return r.For(Attribution{
		Agent:    "organisations/techbridge/agents/atlas",
		Service:  "atlas-agent",
		Provider: pb.Activity_VERTEX_AI,
	})
}

// A model call recorded the way an adopting service records one leaves a running total that is the real cost of it.
//
// 1200 prompt tokens at 1.25 millionths is 1500, and 300 candidate tokens at 10 is 3000. Metering's own pricing produces 4500 for the same counts against the same rates, which is the figure that matters: the local total exists so a spend decision needs no network call, and one that disagreed with the stored cost would be worse than none.
func TestARecordedModelCallProducesTheSameFigureTheServerWouldPrice(t *testing.T) {
	r := New(Config{Rates: geminiRates(), FlushEvery: time.Hour})
	t.Cleanup(func() { r.Close(context.Background()) })

	waitFor(t, "the rate card", func() bool { return r.rates.current() != nil })

	ctx := WithRequest(context.Background(), "requests/7f3a")
	reporterFor(r).ModelCall(ctx, ModelCall{
		Model:     "gemini-2.5-pro",
		Component: "asset_summary",
		Tokens:    Tokens{Prompt: 1200, Candidate: 300},
	})

	if got := r.SpentOn("requests/7f3a"); got != 4500 {
		t.Fatalf("SpentOn = %d, want 4500 millionths", got)
	}
}

// Every model call in one request adds to the same figure, which is what a per-request ceiling reads.
func TestTheRunningTotalAccumulatesAcrossTheCallsInOneRequest(t *testing.T) {
	r := New(Config{Rates: geminiRates(), FlushEvery: time.Hour})
	t.Cleanup(func() { r.Close(context.Background()) })

	waitFor(t, "the rate card", func() bool { return r.rates.current() != nil })

	ctx := WithRequest(context.Background(), "requests/7f3a")
	reporter := reporterFor(r)
	for range 3 {
		reporter.ModelCall(ctx, ModelCall{Model: "gemini-2.5-pro", Tokens: Tokens{Prompt: 1200, Candidate: 300}})
	}

	if got := r.SpentOn("requests/7f3a"); got != 13500 {
		t.Fatalf("SpentOn = %d, want 13500 millionths", got)
	}
}

// With no rate source the recorder behaves as it did before there was one.
func TestWithNoRateSourceTheTotalIsZeroAndNothingFails(t *testing.T) {
	r := New(Config{FlushEvery: time.Hour})
	t.Cleanup(func() { r.Close(context.Background()) })

	ctx := WithRequest(context.Background(), "requests/7f3a")
	reporterFor(r).ModelCall(ctx, ModelCall{Model: "gemini-2.5-pro", Tokens: Tokens{Prompt: 1200, Candidate: 300}})

	if got := r.SpentOn("requests/7f3a"); got != 0 {
		t.Fatalf("SpentOn = %d with no rate source, want 0", got)
	}
	if s := r.Stats(); s.Recorded != 1 {
		t.Fatalf("recorded %d, want 1: the record itself is unaffected", s.Recorded)
	}
}

// A rate source that cannot be read leaves the total where it would have been with no rate source at all, and says so in the counters rather than going quiet.
func TestARateSourceThatFailsLeavesTheTotalAtZeroAndIsCounted(t *testing.T) {
	rates := &fakeRates{err: errors.New("unreachable")}
	r := New(Config{Rates: rates, FlushEvery: time.Hour})
	t.Cleanup(func() { r.Close(context.Background()) })

	waitFor(t, "the failed fetch", func() bool { return r.Stats().RateFetchErrors > 0 })

	ctx := WithRequest(context.Background(), "requests/7f3a")
	reporterFor(r).ModelCall(ctx, ModelCall{Model: "gemini-2.5-pro", Tokens: Tokens{Prompt: 1200, Candidate: 300}})

	if got := r.SpentOn("requests/7f3a"); got != 0 {
		t.Fatalf("SpentOn = %d with an unreadable rate card, want 0", got)
	}
	if s := r.Stats(); s.Recorded != 1 {
		t.Fatalf("recorded %d, want 1: an unreadable rate card stops nothing", s.Recorded)
	}
}

// A rate source that panics must not take its host with it.
func TestARateSourceThatPanicsIsContained(t *testing.T) {
	r := New(Config{Rates: &fakeRates{panic: true}, FlushEvery: time.Hour})
	t.Cleanup(func() { r.Close(context.Background()) })

	waitFor(t, "the panic to be counted", func() bool { return r.Stats().Panicked > 0 })

	ctx := WithRequest(context.Background(), "requests/7f3a")
	reporterFor(r).ModelCall(ctx, ModelCall{Model: "gemini-2.5-pro", Tokens: Tokens{Prompt: 1200, Candidate: 300}})

	if got := r.SpentOn("requests/7f3a"); got != 0 {
		t.Fatalf("SpentOn = %d, want 0", got)
	}
	if s := r.Stats(); s.Recorded != 1 || s.RateFetchErrors == 0 {
		t.Fatalf("stats after a panicking rate source: %+v", s)
	}
}

// A record made before any card has arrived prices at nothing, and the next one after it arrives prices properly. Fetching is never on the path of a model call, so this is the ordinary first moments of a process.
func TestRecordsMadeBeforeTheCardArrivesPriceAtNothing(t *testing.T) {
	rates := &fakeRates{}
	r := New(Config{Rates: rates, FlushEvery: time.Hour, RateRefresh: 5 * time.Millisecond})
	t.Cleanup(func() { r.Close(context.Background()) })

	ctx := WithRequest(context.Background(), "requests/7f3a")
	reporter := reporterFor(r)
	reporter.ModelCall(ctx, ModelCall{Model: "gemini-2.5-pro", Tokens: Tokens{Prompt: 1200, Candidate: 300}})

	if got := r.SpentOn("requests/7f3a"); got != 0 {
		t.Fatalf("SpentOn = %d before any card arrived, want 0", got)
	}

	rates.mu.Lock()
	rates.units = geminiRates().units
	rates.mu.Unlock()

	waitFor(t, "the rate card", func() bool { return r.rates.current() != nil })

	reporter.ModelCall(ctx, ModelCall{Model: "gemini-2.5-pro", Tokens: Tokens{Prompt: 1200, Candidate: 300}})

	if got := r.SpentOn("requests/7f3a"); got != 4500 {
		t.Fatalf("SpentOn = %d, want 4500: only the call made after the card arrived is priced", got)
	}
}

// An empty card is not a card. Replacing prices that work with nothing would read as an agent that suddenly spends nothing.
func TestAnEmptyRateCardLeavesThePreviousOneInPlace(t *testing.T) {
	cache := newRateCache(geminiRates())
	if err := cache.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	cache.source = &fakeRates{}
	if err := cache.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	got := cache.current().priceOf(&pb.Activity{
		Provider:     pb.Activity_VERTEX_AI,
		Model:        "gemini-2.5-pro",
		PromptTokens: 1200,
		OccurredAt:   timestamppb.Now(),
	})
	if got != 1500 {
		t.Fatalf("priced at %d after an empty refresh, want 1500", got)
	}
}

// Recording, reading and releasing all happen in the host's own goroutines while the card is being replaced underneath them. Run with -race.
func TestConcurrentRecordingIsSafe(t *testing.T) {
	rates := geminiRates()
	r := New(Config{Rates: rates, FlushEvery: time.Millisecond, RateRefresh: time.Millisecond})
	t.Cleanup(func() { r.Close(context.Background()) })

	reporter := reporterFor(r)

	var wg sync.WaitGroup
	for worker := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request := "requests/" + string(rune('a'+worker))
			ctx := WithRequest(context.Background(), request)
			for range 200 {
				reporter.ModelCall(ctx, ModelCall{Model: "gemini-2.5-pro", Tokens: Tokens{Prompt: 12, Candidate: 3}})
				r.SpentOn(request)
			}
			r.FinishRequest(request)
		}()
	}
	wg.Wait()

	if s := r.Stats(); s.Recorded+s.Dropped != 16*200 {
		t.Fatalf("recorded %d and dropped %d, want %d between them", s.Recorded, s.Dropped, 16*200)
	}
}
