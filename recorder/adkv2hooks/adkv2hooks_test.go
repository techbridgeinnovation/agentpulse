package adkv2hooks

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
	"github.com/techbridgeinnovation/agentpulse/recorder"
)

// fakeContext stands in for what the framework hands a callback.
//
// Built on the framework's own strict mock, which panics on everything, so a callback that starts reading a field these tests do not supply fails loudly rather than reading a zero value that looks plausible.
type fakeContext struct {
	agent.StrictContextMock
	invocation string
	user       string
	sessionID  string
	agentName  string
}

func (f *fakeContext) InvocationID() string { return f.invocation }
func (f *fakeContext) AgentName() string    { return f.agentName }
func (f *fakeContext) UserID() string       { return f.user }
func (f *fakeContext) SessionID() string    { return f.sessionID }

func newContext() *fakeContext {
	return &fakeContext{
		StrictContextMock: agent.NewStrictContextMock(context.Background()),
		invocation:        "inv-1",
		user:              "users/abc123",
		sessionID:         "sess-1",
		agentName:         "pulseagent-v1",
	}
}

// collector keeps what the recorder delivers.
type collector struct{ seen []*pb.Activity }

func (c *collector) Send(_ context.Context, activities []*pb.Activity) error {
	c.seen = append(c.seen, activities...)
	return nil
}
func (c *collector) Name() string { return "collector" }

func record(t *testing.T, run func(*recorder.Recorder)) []*pb.Activity {
	t.Helper()
	sink := &collector{}
	r := recorder.New(recorder.Config{Sinks: []recorder.Sink{sink}, FlushEvery: time.Hour})
	run(r)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r.Close(ctx)
	return sink.seen
}

func options() Options {
	return Options{
		Agent:   "organisations/techbridge/agents/pulse",
		Service: "pulseagent-v1",
		Model:   "gemini-3.5-flash-lite",
	}
}

// The second major hands all three callbacks one context type where the first handed two. This is the port's whole risk: that the fields are read off the wrong thing, or off nothing.
func TestEverythingIsDerivedFromTheOneContextType(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		AfterModel(r, options())(newContext(), &adkmodel.LLMResponse{ModelVersion: "m"}, nil)
	})

	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
	a := got[0]
	if a.GetRequest() != "inv-1" {
		t.Errorf("request = %q, want the invocation id", a.GetRequest())
	}
	if a.GetSession() != "sess-1" {
		t.Errorf("session = %q, want sess-1", a.GetSession())
	}
	if a.GetUser() != "users/abc123" {
		t.Errorf("user = %q, want users/abc123", a.GetUser())
	}
	if a.GetCallerComponent() != "pulseagent-v1" {
		t.Errorf("component = %q, want the agent name the framework reports", a.GetCallerComponent())
	}
	if a.GetProvider() != pb.Activity_VERTEX_AI {
		t.Errorf("provider = %v, want VERTEX_AI by default rather than unspecified", a.GetProvider())
	}
}

func TestTokensAreRecordedBrokenOutByKind(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		AfterModel(r, options())(newContext(), &adkmodel.LLMResponse{
			ModelVersion: "gemini-3.5-flash-lite",
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:        1000,
				CandidatesTokenCount:    500,
				CachedContentTokenCount: 200,
				ThoughtsTokenCount:      300,
				TotalTokenCount:         2000,
			},
		}, nil)
	})

	a := got[0]
	if a.GetPromptTokens() != 1000 || a.GetCandidateTokens() != 500 || a.GetCachedTokens() != 200 {
		t.Fatalf("token counts wrong: %+v", a)
	}
	if a.GetReasoningTokens() != 300 {
		t.Fatalf("reasoning tokens = %d, want 300 — a reasoning model is undercounted without them", a.GetReasoningTokens())
	}
}

// A streamed answer reaches this callback as one summary the framework assembled from the pieces, and that summary names no model. Both majors drop it, so both need the fallback.
func TestAStreamedCallStillNamesAModel(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		AfterModel(r, options())(newContext(), &adkmodel.LLMResponse{
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     1000,
				CandidatesTokenCount: 500,
				TotalTokenCount:      1500,
			},
		}, nil)
	})

	if model := got[0].GetModel(); model != "gemini-3.5-flash-lite" {
		t.Fatalf("model = %q, want the configured one — a streamed call recorded counts with no model, which prices at nothing", model)
	}
}

func TestTheReportedModelWinsOverTheConfiguredOne(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		AfterModel(r, options())(newContext(), &adkmodel.LLMResponse{ModelVersion: "gemini-3.5-pro"}, nil)
	})

	if model := got[0].GetModel(); model != "gemini-3.5-pro" {
		t.Fatalf("model = %q, want the model that answered rather than the one configured", model)
	}
}

func TestPartialResponsesAreNotRecorded(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		hook := AfterModel(r, options())
		hook(newContext(), &adkmodel.LLMResponse{ModelVersion: "m", Partial: true}, nil)
		hook(newContext(), &adkmodel.LLMResponse{ModelVersion: "m", Partial: true}, nil)
		hook(newContext(), &adkmodel.LLMResponse{ModelVersion: "m"}, nil)
	})

	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1 — counting chunks multiplies a turn's usage", len(got))
	}
}

func TestAFailedCallKeepsItsCodeAndNoneOfItsMessage(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		AfterModel(r, options())(newContext(), &adkmodel.LLMResponse{
			ModelVersion: "m",
			ErrorCode:    "RESOURCE_EXHAUSTED",
			ErrorMessage: "quota exceeded while processing: <the user's prompt>",
		}, errors.New("quota exceeded while processing: <the user's prompt>"))
	})

	a := got[0]
	if a.GetStatus() != pb.Activity_FAILED {
		t.Errorf("status = %v, want FAILED", a.GetStatus())
	}
	if a.GetErrorCode() != "RESOURCE_EXHAUSTED" {
		t.Errorf("error code = %q, want RESOURCE_EXHAUSTED", a.GetErrorCode())
	}
	// A provider message routinely quotes the prompt back, so no field on the record may carry it.
	for _, field := range []string{a.GetErrorCode(), a.GetModel(), a.GetCallerComponent(), a.GetUser(), a.GetSkill()} {
		if field == "quota exceeded while processing: <the user's prompt>" {
			t.Fatalf("the provider's message reached the record")
		}
	}
}

func TestHittingTheTokenCeilingReadsAsTruncatedNotFailed(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		AfterModel(r, options())(newContext(), &adkmodel.LLMResponse{
			ModelVersion: "m",
			FinishReason: genai.FinishReasonMaxTokens,
		}, nil)
	})

	if got[0].GetStatus() != pb.Activity_TRUNCATED {
		t.Fatalf("status = %v, want TRUNCATED — a truncated call was charged for the work it did", got[0].GetStatus())
	}
}

func TestBeforeModelLabelsTheRequestWithTheSanitisedIdentity(t *testing.T) {
	request := &adkmodel.LLMRequest{}
	r := recorder.New(recorder.Config{FlushEvery: time.Hour})
	BeforeModel(r, options())(newContext(), request)

	labels := request.Config.Labels
	if labels["ap_user"] != "users_abc123" {
		t.Errorf("ap_user = %q, want users_abc123 — the billing export accepts no slashes", labels["ap_user"])
	}
	if labels["ap_component"] != "pulseagent-v1" {
		t.Errorf("ap_component = %q, want pulseagent-v1", labels["ap_component"])
	}
}

func TestBeforeModelNeverReplacesALabelSetCloserToTheCall(t *testing.T) {
	request := &adkmodel.LLMRequest{
		Config: &genai.GenerateContentConfig{Labels: map[string]string{"ap_user": "set_by_the_caller"}},
	}
	r := recorder.New(recorder.Config{FlushEvery: time.Hour})
	BeforeModel(r, options())(newContext(), request)

	if got := request.Config.Labels["ap_user"]; got != "set_by_the_caller" {
		t.Fatalf("ap_user = %q, want the caller's own value left alone", got)
	}
}

// The turn ending is what releases the running total, so an adopter who registers the callbacks gets the cleanup without writing anything.
func TestAfterAgentReleasesTheRunningTotalForTheTurn(t *testing.T) {
	r := recorder.New(recorder.Config{FlushEvery: time.Hour})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		r.Close(ctx)
	})

	r.Record(&pb.Activity{Request: "inv-1", EstimatedCostMicros: 4500})
	if got := r.SpentOn("inv-1"); got != 4500 {
		t.Fatalf("SpentOn before the turn ended = %d, want 4500", got)
	}

	content, err := AfterAgent(r, options())(newContext())
	if content != nil || err != nil {
		t.Fatalf("AfterAgent() = (%v, %v), want (nil, nil)", content, err)
	}

	if got := r.SpentOn("inv-1"); got != 0 {
		t.Fatalf("SpentOn after the turn ended = %d, want 0", got)
	}
}

// A request set on the context wins over the framework's invocation id everywhere else, so it has to win here too or the entry released is not the one that was filled.
func TestAfterAgentReleasesTheRequestTheContextCarries(t *testing.T) {
	r := recorder.New(recorder.Config{FlushEvery: time.Hour})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		r.Close(ctx)
	})

	r.Record(&pb.Activity{Request: "requests/7f3a", EstimatedCostMicros: 4500})

	ctx := newContext()
	ctx.StrictContextMock = agent.NewStrictContextMock(recorder.WithRequest(context.Background(), "requests/7f3a"))
	if _, err := AfterAgent(r, options())(ctx); err != nil {
		t.Fatalf("AfterAgent: %v", err)
	}

	if got := r.SpentOn("requests/7f3a"); got != 0 {
		t.Fatalf("SpentOn after the turn ended = %d, want 0", got)
	}
}
