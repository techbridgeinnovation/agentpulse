package adkhooks

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/techbridgeinnovation/agentpulse/recorder"
	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
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

// promptQuote is what a provider's error message looks like: it quotes the prompt back, which is why no field on a record may carry it.
const promptQuote = "request rejected while processing: <the user's prompt>"

// The framework skips AfterModel for a call that returned an error, so a rejected call was never recorded at all.
func TestARejectedModelCallIsRecordedAsFailed(t *testing.T) {
	ctx := contextFor(t.Name(), "atlas")
	var resp *adkmodel.LLMResponse
	var err error
	got := record(t, func(r *recorder.Recorder) {
		before, _ := hooks(r)
		before(ctx, "gemini-3.1-pro-preview")
		time.Sleep(2 * time.Millisecond)
		resp, err = OnModelError(r, options())(ctx, &adkmodel.LLMRequest{Model: "gemini-3.1-pro-preview"},
			genai.APIError{Code: 400, Status: "INVALID_ARGUMENT", Message: promptQuote})
	})

	if resp != nil || err != nil {
		t.Fatalf("OnModelError = (%v, %v), want (nil, nil) so the error reaches the agent unchanged", resp, err)
	}
	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
	a := got[0]
	if a.GetStatus() != pb.Activity_FAILED {
		t.Errorf("status = %v, want FAILED", a.GetStatus())
	}
	if a.GetErrorCode() != "INVALID_ARGUMENT" {
		t.Errorf("error code = %q, want INVALID_ARGUMENT", a.GetErrorCode())
	}
	if a.GetErrorFormat() == "" || len(a.GetReportedError()) == 0 {
		t.Errorf("reported error = %q %v, want the provider's own fields", a.GetErrorFormat(), a.GetReportedError())
	}
	for _, field := range a.GetReportedError() {
		if field.GetValue() == promptQuote {
			t.Fatal("the provider's message reached the record")
		}
	}
	if a.GetModel() != "gemini-3.1-pro-preview" {
		t.Errorf("model = %q, want the model requested", a.GetModel())
	}
	if a.GetDurationMs() < 1 {
		t.Errorf("duration = %dms, want the time since BeforeModel", a.GetDurationMs())
	}
	if a.GetFramework() != framework {
		t.Errorf("framework = %q, want %q", a.GetFramework(), framework)
	}
}

// A stream that breaks partway has already been billed for the chunks it delivered, and those chunks are the only place the usage was reported.
func TestAStreamThatBreaksKeepsTheUsageItsChunksReported(t *testing.T) {
	ctx := contextFor(t.Name(), "atlas")
	got := record(t, func(r *recorder.Recorder) {
		before, after := hooks(r)
		before(ctx, "gemini-3.1-pro-preview")
		after(ctx, &adkmodel.LLMResponse{ModelVersion: "gemini-3.1-pro-preview-001", Partial: true,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 800, CandidatesTokenCount: 10, TotalTokenCount: 810}})
		after(ctx, &adkmodel.LLMResponse{Partial: true,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 800, CandidatesTokenCount: 40, TotalTokenCount: 840}})
		OnModelError(r, options())(ctx, &adkmodel.LLMRequest{Model: "gemini-3.1-pro-preview"}, context.DeadlineExceeded)
	})

	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
	a := got[0]
	if a.GetModel() != "gemini-3.1-pro-preview-001" {
		t.Errorf("model = %q, want the model the stream reported", a.GetModel())
	}
	if a.GetPromptTokens() != 800 || a.GetCandidateTokens() != 40 || a.GetTotalTokens() != 840 {
		t.Errorf("tokens = %d/%d/%d, want the latest chunk's 800/40/840", a.GetPromptTokens(), a.GetCandidateTokens(), a.GetTotalTokens())
	}
	if a.GetErrorCode() != "DeadlineExceeded" {
		t.Errorf("error code = %q, want DeadlineExceeded", a.GetErrorCode())
	}
}

// An error callback may hand the framework a response in place of the error, which then reaches AfterModel as an ordinary success. That is the same call, already recorded.
func TestACallRescuedAfterItFailedIsRecordedOnce(t *testing.T) {
	ctx := contextFor(t.Name(), "atlas")
	got := record(t, func(r *recorder.Recorder) {
		before, after := hooks(r)
		before(ctx, "gemini-3.1-pro-preview")
		OnModelError(r, options())(ctx, &adkmodel.LLMRequest{Model: "gemini-3.1-pro-preview"}, genai.APIError{Code: 429, Status: "RESOURCE_EXHAUSTED"})
		after(ctx, &adkmodel.LLMResponse{Content: genai.NewContentFromText("try again later", genai.RoleModel)})
	})

	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
	if got[0].GetStatus() != pb.Activity_FAILED {
		t.Errorf("status = %v, want the failure, not the substitute's success", got[0].GetStatus())
	}
}

func TestTheCallAfterAFailedOneIsRecorded(t *testing.T) {
	ctx := contextFor(t.Name(), "atlas")
	got := record(t, func(r *recorder.Recorder) {
		before, after := hooks(r)
		before(ctx, "gemini-3.1-pro-preview")
		OnModelError(r, options())(ctx, &adkmodel.LLMRequest{Model: "gemini-3.1-pro-preview"}, context.Canceled)
		before(ctx, "gemini-2.5-flash")
		after(ctx, &adkmodel.LLMResponse{UsageMetadata: usage()})
	})

	if len(got) != 2 {
		t.Fatalf("recorded %d activities, want 2", len(got))
	}
	if got[1].GetStatus() != pb.Activity_OK || got[1].GetModel() != "gemini-2.5-flash" {
		t.Errorf("second call = %v %q, want OK against its own model", got[1].GetStatus(), got[1].GetModel())
	}
}

// An agent that registers the error callback and not BeforeModel still records the model the request named.
func TestAFailedCallWithoutBeforeModelNamesTheRequestedModel(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		OnModelError(r, options())(contextFor(t.Name(), "atlas"), &adkmodel.LLMRequest{Model: "gemini-2.5-flash"}, errors.New(promptQuote))
	})

	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
	if got[0].GetModel() != "gemini-2.5-flash" {
		t.Errorf("model = %q, want gemini-2.5-flash", got[0].GetModel())
	}
	if got[0].GetErrorCode() == promptQuote {
		t.Fatal("the error's message reached the record")
	}
}

// A chunk arriving for a call past its life must not revive what was held for it, or a failure would be timed from a call long gone.
func TestAnExpiredCallIsNotRevivedByALaterChunk(t *testing.T) {
	clock := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	store := newInFlightModels(8, time.Minute, func() time.Time { return clock })

	store.requested("call", "gemini-3.1-pro-preview")
	clock = clock.Add(2 * time.Minute)
	store.chunk("call", "gemini-3.1-pro-preview-001", nil)

	if got := store.fail("call"); got.requested != "" || !got.startedAt.IsZero() {
		t.Fatalf("fail returned %+v for a call past its ttl, want only what the late chunk reported", got)
	}
}
