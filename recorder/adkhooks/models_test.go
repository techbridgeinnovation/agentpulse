package adkhooks

import (
	"fmt"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
	"github.com/techbridgeinnovation/agentpulse/recorder"
)

// contextFor is newContext in its own invocation, so a test's calls never meet
// another test's in the shared in-flight store.
func contextFor(invocation, agentName string) fakeContext {
	ctx := newContext()
	ctx.invocation = invocation
	ctx.agentName = agentName
	return ctx
}

// usage is what ADK's final streamed response keeps: the counts, not the model.
func usage() *genai.GenerateContentResponseUsageMetadata {
	return &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 12374, CandidatesTokenCount: 9, TotalTokenCount: 12457}
}

// streamed replays a streamed call the way ADK delivers it: chunks carrying
// servedModel, then a final response assembled without one.
func streamed(after func(ctx fakeContext, response *adkmodel.LLMResponse), ctx fakeContext, servedModel string) {
	for range 3 {
		after(ctx, &adkmodel.LLMResponse{ModelVersion: servedModel, Partial: true})
	}
	after(ctx, &adkmodel.LLMResponse{UsageMetadata: usage()})
}

func hooks(r *recorder.Recorder) (before func(fakeContext, string), after func(fakeContext, *adkmodel.LLMResponse)) {
	beforeModel := BeforeModel(r, options())
	afterModel := AfterModel(r, options())
	before = func(ctx fakeContext, model string) {
		beforeModel(ctx, &adkmodel.LLMRequest{Model: model})
	}
	after = func(ctx fakeContext, response *adkmodel.LLMResponse) {
		afterModel(ctx, response, nil)
	}
	return before, after
}

func onlyModel(t *testing.T, got []*pb.Activity) string {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
	return got[0].GetModel()
}

// Metering prices a call by its model, so a streamed call recorded without one
// is recorded as having cost nothing.
func TestAStreamedCallIsRecordedAgainstTheModelItsChunksReported(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		_, after := hooks(r)
		streamed(after, contextFor(t.Name(), "atlas"), "gemini-3.1-pro-preview")
	})

	if model := onlyModel(t, got); model != "gemini-3.1-pro-preview" {
		t.Fatalf("model = %q, want the model the stream reported", model)
	}
	if got[0].GetTotalTokens() != 12457 {
		t.Fatalf("total tokens = %d, want the final response's usage", got[0].GetTotalTokens())
	}
}

func TestACallWhoseProviderReportsNoModelIsRecordedAgainstTheModelRequested(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		before, after := hooks(r)
		ctx := contextFor(t.Name(), "atlas")
		before(ctx, "gemini-3.1-pro-preview")
		streamed(after, ctx, "")
	})

	if model := onlyModel(t, got); model != "gemini-3.1-pro-preview" {
		t.Fatalf("model = %q, want the model requested", model)
	}
}

func TestAModelTheFinalResponseReportsIsNeverReplaced(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		before, after := hooks(r)
		ctx := contextFor(t.Name(), "atlas")
		before(ctx, "gemini-2.5-flash")
		after(ctx, &adkmodel.LLMResponse{ModelVersion: "gemini-2.5-flash-lite", Partial: true})
		after(ctx, &adkmodel.LLMResponse{ModelVersion: "gemini-2.5-flash-001", UsageMetadata: usage()})
	})

	if model := onlyModel(t, got); model != "gemini-2.5-flash-001" {
		t.Fatalf("model = %q, want the model the final response reported", model)
	}
}

// What was served is what was billed, so it wins over what was asked for.
func TestTheModelServedWinsOverTheModelRequested(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		before, after := hooks(r)
		ctx := contextFor(t.Name(), "atlas")
		before(ctx, "gemini-3.1-pro-preview")
		streamed(after, ctx, "gemini-3.1-pro-preview-customtools")
	})

	if model := onlyModel(t, got); model != "gemini-3.1-pro-preview-customtools" {
		t.Fatalf("model = %q, want the model served", model)
	}
}

func TestAModelDoesNotCarryIntoTheNextCallOfTheSameTurn(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		before, after := hooks(r)
		ctx := contextFor(t.Name(), "atlas")

		before(ctx, "gemini-3.1-pro-preview")
		streamed(after, ctx, "gemini-3.1-pro-preview")

		before(ctx, "gemini-2.5-flash")
		streamed(after, ctx, "")
	})

	if len(got) != 2 {
		t.Fatalf("recorded %d activities, want 2", len(got))
	}
	if got[1].GetModel() != "gemini-2.5-flash" {
		t.Fatalf("second call model = %q, want its own request, not the first call's model", got[1].GetModel())
	}
}

// A sub-agent runs its calls inside its parent's turn, under the same
// invocation, so its model must not be mistaken for the parent's or the reverse.
func TestASubAgentsCallIsKeptApartFromItsParents(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		_, after := hooks(r)
		parent := contextFor(t.Name(), "orchestrator")
		research := contextFor(t.Name(), "company_research")

		after(parent, &adkmodel.LLMResponse{ModelVersion: "gemini-3.1-pro-preview", Partial: true})
		after(research, &adkmodel.LLMResponse{UsageMetadata: usage()})
		after(parent, &adkmodel.LLMResponse{UsageMetadata: usage()})
	})

	if len(got) != 2 {
		t.Fatalf("recorded %d activities, want 2", len(got))
	}
	if got[0].GetModel() != "" {
		t.Fatalf("sub-agent call model = %q, want none: it reported none of its own", got[0].GetModel())
	}
	if got[1].GetModel() != "gemini-3.1-pro-preview" {
		t.Fatalf("parent call model = %q, want its own stream's model", got[1].GetModel())
	}
}

func TestAFinishedCallLeavesNothingHeld(t *testing.T) {
	held := inFlight.size()
	record(t, func(r *recorder.Recorder) {
		before, after := hooks(r)
		ctx := contextFor(t.Name(), "atlas")
		before(ctx, "gemini-3.1-pro-preview")
		streamed(after, ctx, "gemini-3.1-pro-preview")
	})

	if now := inFlight.size(); now != held {
		t.Fatalf("store holds %d calls after a finished call, want %d as before it", now, held)
	}
}

// A call that never reaches its final response — cut short by another
// before-model callback — must not make the store grow for as long as the
// process runs.
func TestTheInFlightStoreStaysWithinItsCap(t *testing.T) {
	clock := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	store := newInFlightModels(8, time.Hour, func() time.Time { return clock })

	for i := range 100 {
		store.requested(fmt.Sprintf("call-%d", i), "gemini-3.1-pro-preview")
		clock = clock.Add(time.Second)
	}

	if size := store.size(); size > 8 {
		t.Fatalf("store holds %d calls, want at most 8", size)
	}
	if got := store.take("call-99"); got.requested == "" {
		t.Fatal("the most recent call was evicted ahead of older ones")
	}
}

func TestAnExpiredCallIsNotUsed(t *testing.T) {
	clock := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	store := newInFlightModels(8, time.Minute, func() time.Time { return clock })

	store.requested("call", "gemini-3.1-pro-preview")
	clock = clock.Add(2 * time.Minute)

	if got := store.take("call"); got.requested != "" || got.served != "" {
		t.Fatalf("take returned %+v for a call past its ttl, want nothing", got)
	}
}
