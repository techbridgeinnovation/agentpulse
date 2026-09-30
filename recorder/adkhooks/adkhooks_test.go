package adkhooks

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"google.golang.org/adk/agent"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	governancepb "github.com/techbridgeinnovation/agentpulse/recorder/pb/governance"
	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
	"github.com/techbridgeinnovation/agentpulse/recorder"
)

// fakeContext stands in for what the framework hands a callback.
type fakeContext struct {
	context.Context
	invocation string
	user       string
	sessionID  string
	agentName  string
}

func (f fakeContext) UserContent() *genai.Content          { return nil }
func (f fakeContext) InvocationID() string                 { return f.invocation }
func (f fakeContext) AgentName() string                    { return f.agentName }
func (f fakeContext) ReadonlyState() session.ReadonlyState { return nil }
func (f fakeContext) UserID() string                       { return f.user }
func (f fakeContext) AppName() string                      { return "test" }
func (f fakeContext) SessionID() string                    { return f.sessionID }
func (f fakeContext) Branch() string                       { return "" }
func (f fakeContext) Artifacts() agent.Artifacts           { return nil }
func (f fakeContext) State() session.State                 { return nil }

func newContext() fakeContext {
	return fakeContext{
		Context:    context.Background(),
		invocation: "inv-1",
		user:       "users/abc123",
		sessionID:  "sess-1",
		agentName:  "atlas",
	}
}

// collect returns a sink that keeps what the recorder delivers, and the
// workspace each batch was delivered under.
type collector struct {
	seen       []*pb.Activity
	workspaces []string
}

func (c *collector) Send(ctx context.Context, activities []*pb.Activity) error {
	c.seen = append(c.seen, activities...)
	c.workspaces = append(c.workspaces, recorder.WorkspaceFrom(ctx))
	return nil
}
func (c *collector) Name() string { return "collector" }

func record(t *testing.T, run func(*recorder.Recorder)) []*pb.Activity {
	t.Helper()
	return delivered(t, run).seen
}

func delivered(t *testing.T, run func(*recorder.Recorder)) *collector {
	t.Helper()
	sink := &collector{}
	r := recorder.New(recorder.Config{Sinks: []recorder.Sink{sink}, FlushEvery: time.Hour})
	run(r)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r.Close(ctx)
	return sink
}

func options() Options {
	return Options{Agent: "organisations/techbridge/agents/atlas", Service: "atlas-agent", Skill: "canvas"}
}

func TestAfterModelRecordsTokensBrokenOutByKind(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		AfterModel(r, options())(newContext(), &adkmodel.LLMResponse{
			ModelVersion: "gemini-2.5-pro",
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:        1000,
				CandidatesTokenCount:    500,
				CachedContentTokenCount: 200,
				ThoughtsTokenCount:      300,
				TotalTokenCount:         2000,
			},
		}, nil)
	})

	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
	a := got[0]
	if a.GetPromptTokens() != 1000 || a.GetCandidateTokens() != 500 || a.GetCachedTokens() != 200 {
		t.Fatalf("token counts wrong: %+v", a)
	}
	// The existing implementations drop this one, which silently undercounts a
	// reasoning model.
	if a.GetReasoningTokens() != 300 {
		t.Fatalf("reasoning tokens = %d, want 300", a.GetReasoningTokens())
	}
	if a.GetModel() != "gemini-2.5-pro" {
		t.Fatalf("model = %q", a.GetModel())
	}
}

func TestEverythingIsDerivedWithNoCallSiteInput(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		AfterModel(r, options())(newContext(), &adkmodel.LLMResponse{ModelVersion: "m"}, nil)
	})

	a := got[0]
	if a.GetRequest() != "inv-1" {
		t.Fatalf("request = %q, want the framework's invocation id", a.GetRequest())
	}
	if a.GetUser() != "users/abc123" || a.GetSession() != "sess-1" {
		t.Fatalf("identity not derived: %+v", a)
	}
	if a.GetCallerComponent() != "atlas" {
		t.Fatalf("component = %q, want the agent name", a.GetCallerComponent())
	}
	if a.GetSkill() != "canvas" {
		t.Fatalf("skill = %q", a.GetSkill())
	}
}

// A delegating agent must keep the caller's request, or the rows for one
// question end up in two groups.
func TestARequestOnTheContextWinsOverTheInvocationId(t *testing.T) {
	ctx := newContext()
	ctx.Context = recorder.WithRequest(context.Background(), "req-from-caller")

	got := record(t, func(r *recorder.Recorder) {
		AfterModel(r, options())(ctx, &adkmodel.LLMResponse{ModelVersion: "m"}, nil)
	})

	if got[0].GetRequest() != "req-from-caller" {
		t.Fatalf("request = %q, want the caller's", got[0].GetRequest())
	}
}

// In streaming mode every chunk reaches the callback. Counting them would
// multiply a turn's usage by however many pieces it was split into.
func TestPartialResponsesAreNotRecorded(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		after := AfterModel(r, options())
		for range 20 {
			after(newContext(), &adkmodel.LLMResponse{ModelVersion: "m", Partial: true}, nil)
		}
		after(newContext(), &adkmodel.LLMResponse{ModelVersion: "m"}, nil)
	})

	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
}

func TestAFailedCallIsStillRecordedBecauseItStillCost(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		AfterModel(r, options())(newContext(), &adkmodel.LLMResponse{
			ModelVersion:  "m",
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 900},
		}, errors.New("provider refused"))
	})

	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
	if got[0].GetStatus() != pb.Activity_FAILED {
		t.Fatalf("status = %v, want FAILED", got[0].GetStatus())
	}
	if got[0].GetPromptTokens() != 900 {
		t.Fatal("tokens spent before the failure were not recorded")
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
		t.Fatalf("status = %v, want TRUNCATED — partial work was done and charged for", got[0].GetStatus())
	}
}

func TestAnErrorCodeIsKeptAndNoMessageIs(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		AfterModel(r, options())(newContext(), &adkmodel.LLMResponse{
			ModelVersion: "m",
			ErrorCode:    "RESOURCE_EXHAUSTED",
			ErrorMessage: "quota exceeded while processing: <the user's prompt>",
		}, nil)
	})

	a := got[0]
	if a.GetErrorCode() != "RESOURCE_EXHAUSTED" {
		t.Fatalf("error code = %q", a.GetErrorCode())
	}
	// Provider messages quote prompt fragments, so no field carries one.
	for _, field := range []string{a.GetModel(), a.GetSkill(), a.GetCallerComponent(), a.GetErrorCode()} {
		if field == "quota exceeded while processing: <the user's prompt>" {
			t.Fatal("an error message reached the record")
		}
	}
}

// The framework reports a code on the response it built, and nothing at all on
// a call that failed before there was one. Without the fallback, a call that
// died on the way to the provider is recorded as failed with no reason.
func TestAModelFailureWithNoResponseCodeIsStillNamed(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		AfterModel(r, options())(newContext(), &adkmodel.LLMResponse{ModelVersion: "m"},
			genai.APIError{Code: 429, Status: "RESOURCE_EXHAUSTED"})
	})

	if got[0].GetStatus() != pb.Activity_FAILED {
		t.Errorf("status = %v, want FAILED", got[0].GetStatus())
	}
	if got[0].GetErrorCode() != "RESOURCE_EXHAUSTED" {
		t.Errorf("error code = %q, want RESOURCE_EXHAUSTED", got[0].GetErrorCode())
	}
}

// A duration exists only between the two callbacks; the framework hands neither
// of them one, and a report without it cannot tell a slow model from a broken
// one.
func TestAModelCallReportsHowLongItTook(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		ctx := newContext()
		BeforeModel(r, options())(ctx, &adkmodel.LLMRequest{Model: "gemini-2.5-pro"})
		time.Sleep(2 * time.Millisecond)
		AfterModel(r, options())(ctx, &adkmodel.LLMResponse{ModelVersion: "gemini-2.5-pro"}, nil)
	})

	if got[0].GetDurationMs() < 1 {
		t.Errorf("duration = %dms, want the time between the two callbacks", got[0].GetDurationMs())
	}
}

// An agent that registers only the callback that records still records
// everything else about its calls.
func TestAModelCallWithoutTheCallbackThatTimesItIsStillRecorded(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		AfterModel(r, options())(newContext(), &adkmodel.LLMResponse{ModelVersion: "gemini-2.5-pro"}, nil)
	})

	if got[0].GetDurationMs() != 0 {
		t.Errorf("duration = %dms, want 0 for a call nothing timed", got[0].GetDurationMs())
	}
	if got[0].GetModel() != "gemini-2.5-pro" {
		t.Errorf("model = %q, want it recorded all the same", got[0].GetModel())
	}
}

// The same blocked call through the framework path: 200, no error, no content,
// a finish reason and a bill for the prompt.
func TestAResponseBlockedBySafetyIsRecordedAsAFailure(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		AfterModel(r, options())(newContext(), &adkmodel.LLMResponse{
			ModelVersion: "gemini-2.5-pro",
			FinishReason: genai.FinishReasonSafety,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount: 1200,
				TotalTokenCount:  1200,
			},
		}, nil)
	})

	if got[0].GetStatus() != pb.Activity_FAILED {
		t.Errorf("status = %v, want FAILED", got[0].GetStatus())
	}
	if got[0].GetErrorCode() != "SAFETY" {
		t.Errorf("error code = %q, want SAFETY", got[0].GetErrorCode())
	}
	// The prompt was still charged for, so the tokens stay on the record.
	if got[0].GetPromptTokens() != 1200 {
		t.Errorf("prompt tokens = %d, want the tokens the provider billed", got[0].GetPromptTokens())
	}
}

// A malformed function call is a failure for the same reason: the turn got
// nothing it could use, and the model was paid for producing it.
func TestAMalformedFunctionCallIsRecordedAsAFailure(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		AfterModel(r, options())(newContext(), &adkmodel.LLMResponse{
			ModelVersion: "m",
			FinishReason: genai.FinishReasonMalformedFunctionCall,
		}, nil)
	})
	if got[0].GetStatus() != pb.Activity_FAILED || got[0].GetErrorCode() != "MALFORMED_FUNCTION_CALL" {
		t.Errorf("status = %v, code = %q", got[0].GetStatus(), got[0].GetErrorCode())
	}
}

// The token ceiling is checked before the blocked reasons, so a truncated call
// does not become a failure.
func TestTheTokenCeilingIsStillTruncationNotFailure(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		AfterModel(r, options())(newContext(), &adkmodel.LLMResponse{
			ModelVersion: "m",
			FinishReason: genai.FinishReasonMaxTokens,
		}, nil)
	})
	if got[0].GetStatus() != pb.Activity_TRUNCATED {
		t.Errorf("status = %v, want TRUNCATED", got[0].GetStatus())
	}
	if got[0].GetErrorCode() != "" {
		t.Errorf("error code = %q, want none: nothing failed", got[0].GetErrorCode())
	}
}

// A normal answer must not be dragged into the failure count.
func TestAnOrdinaryStopIsStillASuccess(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		AfterModel(r, options())(newContext(), &adkmodel.LLMResponse{
			ModelVersion: "m",
			FinishReason: genai.FinishReasonStop,
		}, nil)
	})
	if got[0].GetStatus() != pb.Activity_OK || got[0].GetErrorCode() != "" {
		t.Errorf("status = %v, code = %q", got[0].GetStatus(), got[0].GetErrorCode())
	}
}

func TestBeforeModelLabelsTheRequestWithItsAgentAndComponent(t *testing.T) {
	request := &adkmodel.LLMRequest{}
	opts := options()
	opts.Backend = genai.BackendVertexAI
	BeforeModel(nil, opts)(newContext(), request)

	labels := request.Config.Labels
	if labels["ap_agent"] != "atlas" {
		t.Fatalf("agent label = %q, want atlas, the agent's name without its organisation", labels["ap_agent"])
	}
	// Google keeps 1,000 values of a label key per billing account, which a label per person would pass.
	if _, ok := labels["ap_user"]; ok {
		t.Fatalf("user label = %q, want none", labels["ap_user"])
	}
	if labels["ap_component"] != "atlas" {
		t.Fatalf("component label = %q", labels["ap_component"])
	}
}

// The Gemini API refuses a request that carries labels, so labelling one would fail every call the agent makes.
func TestBeforeModelLabelsNothingOnTheGeminiAPI(t *testing.T) {
	request := &adkmodel.LLMRequest{}
	opts := options()
	opts.Backend = genai.BackendGeminiAPI
	BeforeModel(nil, opts)(newContext(), request)

	if request.Config != nil && len(request.Config.Labels) > 0 {
		t.Fatalf("labels = %v, want none on the Gemini API", request.Config.Labels)
	}
}

func TestBeforeModelReadsTheBackendFromTheGenaiSettings(t *testing.T) {
	for _, tc := range []struct {
		enterprise, vertex string
		want               bool
	}{
		{"", "true", true},
		{"", "1", true},
		{"", "false", false},
		{"true", "false", true},
		{"false", "true", false},
		{"", "", false},
	} {
		unsetenv(t, "GOOGLE_GENAI_USE_ENTERPRISE", tc.enterprise)
		unsetenv(t, "GOOGLE_GENAI_USE_VERTEXAI", tc.vertex)
		request := &adkmodel.LLMRequest{}
		opts := options()
		BeforeModel(nil, opts)(newContext(), request)

		got := request.Config != nil && len(request.Config.Labels) > 0
		if got != tc.want {
			t.Errorf("enterprise=%q vertex=%q: labelled = %v, want %v", tc.enterprise, tc.vertex, got, tc.want)
		}
	}
}

// unsetenv sets name to value for the test, or leaves it unset when value is empty, since an empty setting still chooses the Gemini API.
func unsetenv(t *testing.T, name, value string) {
	t.Helper()
	t.Setenv(name, value)
	if value == "" {
		os.Unsetenv(name)
	}
}

func TestBeforeModelNeverReplacesALabelSetCloserToTheCall(t *testing.T) {
	request := &adkmodel.LLMRequest{
		Config: &genai.GenerateContentConfig{Labels: map[string]string{"ap_agent": "set_by_hand"}},
	}
	opts := options()
	opts.Backend = genai.BackendVertexAI
	BeforeModel(nil, opts)(newContext(), request)

	if got := request.Config.Labels["ap_agent"]; got != "set_by_hand" {
		t.Fatalf("agent label = %q, want the value already there", got)
	}
}

func TestProviderDefaultsToVertexRatherThanUnspecified(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		AfterModel(r, Options{Agent: "organisations/x/agents/y"})(newContext(), &adkmodel.LLMResponse{ModelVersion: "m"}, nil)
	})

	if got[0].GetBilledBy() != recorder.ProviderVertexAI {
		t.Fatalf("billed_by = %q, want %q", got[0].GetBilledBy(), recorder.ProviderVertexAI)
	}
}

// fakeDecider stands in for a governance client, recording the request it
// was given and returning whatever a test configures.
//
// calls counts every invocation, so a test can assert how many times the
// wrapped Decider was actually reached — the thing a local cache in front of
// it is supposed to reduce.
type fakeDecider struct {
	resp  *governancepb.DecideResponse
	err   error
	got   *governancepb.DecideRequest
	calls int
}

func (f *fakeDecider) Decide(ctx context.Context, req *governancepb.DecideRequest) (*governancepb.DecideResponse, error) {
	f.calls++
	f.got = req
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func TestBeforeModelSkipsGovernanceWhenNoDeciderIsConfigured(t *testing.T) {
	r := recorder.New(recorder.Config{FlushEvery: time.Hour})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		r.Close(ctx)
	})

	resp, err := BeforeModel(r, options())(newContext(), &adkmodel.LLMRequest{})
	if resp != nil || err != nil {
		t.Fatalf("BeforeModel() = (%v, %v), want (nil, nil)", resp, err)
	}
	if s := r.Stats(); s.Notified != 0 || s.DecisionErrors != 0 {
		t.Fatalf("stats = %+v, want no decision activity when no Decider is configured", s)
	}
}

func TestBeforeModelSendsTheExpectedDecideRequest(t *testing.T) {
	decider := &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_ALLOW}}
	opts := options()
	// Product is deprecated: setting it must have no effect on the request sent.
	opts.Product = "rezco"
	opts.Decider = decider

	ctx := newContext()
	ctx.user = "users/jane"
	if _, err := BeforeModel(nil, opts)(ctx, &adkmodel.LLMRequest{}); err != nil {
		t.Fatalf("BeforeModel: %v", err)
	}

	if decider.got == nil {
		t.Fatal("Decide was never called")
	}
	if decider.got.GetParent() != "organisations/techbridge" {
		t.Fatalf("parent = %q, want the organisation derived from Options.Agent", decider.got.GetParent())
	}
	if decider.got.GetAgent() != opts.Agent {
		t.Fatalf("agent = %q, want %q", decider.got.GetAgent(), opts.Agent)
	}
	if decider.got.GetProduct() != "" {
		t.Fatalf("product = %q, want empty — Options.Product is deprecated and never sent", decider.got.GetProduct())
	}
	if decider.got.GetUser() != "users/jane" {
		t.Fatalf("user = %q, want %q", decider.got.GetUser(), "users/jane")
	}
	// A turn that names no workspace asks about none, which governance reads as
	// the organisation's default.
	if decider.got.GetWorkspace() != "" || decider.got.GetProject() != "" {
		t.Fatalf("workspace = %q and project = %q, want neither named", decider.got.GetWorkspace(), decider.got.GetProject())
	}
}

func TestBeforeModelAsksAboutTheWorkspaceAndProjectTheTurnIsFor(t *testing.T) {
	decider := &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_ALLOW}}
	opts := options()
	opts.Product = "rezco"
	opts.Decider = decider

	ctx := newContext()
	ctx.Context = recorder.WithProject(recorder.WithWorkspace(context.Background(), "acme"), "matter-1183")
	if _, err := BeforeModel(nil, opts)(ctx, &adkmodel.LLMRequest{}); err != nil {
		t.Fatalf("BeforeModel: %v", err)
	}

	if decider.got == nil {
		t.Fatal("Decide was never called")
	}
	// The contract names a workspace in full; the context carries the bare
	// identifier and the organisation comes from Options.Agent.
	want := "organisations/techbridge/workspaces/acme"
	if decider.got.GetWorkspace() != want {
		t.Fatalf("workspace = %q, want %q", decider.got.GetWorkspace(), want)
	}
	if decider.got.GetProject() != "matter-1183" {
		t.Fatalf("project = %q, want %q", decider.got.GetProject(), "matter-1183")
	}
}

// Neither is asked for at a call site: both are read off the turn's own
// context, where the product set them when it checked the sign-in.
func TestTheCallbacksTakeTheWorkspaceAndProjectFromTheContext(t *testing.T) {
	ctx := newContext()
	ctx.Context = recorder.WithProject(recorder.WithWorkspace(context.Background(), "acme"), "matter-1183")

	sink := delivered(t, func(r *recorder.Recorder) {
		AfterModel(r, options())(ctx, &adkmodel.LLMResponse{ModelVersion: "m"}, nil)
	})

	if len(sink.seen) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(sink.seen))
	}
	if got := sink.seen[0].GetProject(); got != "matter-1183" {
		t.Fatalf("project = %q, want %q", got, "matter-1183")
	}
	// The workspace is not a field on the record: it is the batch the record
	// was delivered in.
	if got := sink.workspaces; len(got) != 1 || got[0] != "acme" {
		t.Fatalf("delivered under %v, want one batch under acme", got)
	}
}

func TestATurnThatNamesNoWorkspaceOrProjectRecordsNeither(t *testing.T) {
	sink := delivered(t, func(r *recorder.Recorder) {
		AfterModel(r, options())(newContext(), &adkmodel.LLMResponse{ModelVersion: "m"}, nil)
	})

	if got := sink.seen[0].GetProject(); got != "" {
		t.Fatalf("project = %q, want none", got)
	}
	if got := sink.workspaces; len(got) != 1 || got[0] != "" {
		t.Fatalf("delivered under %v, want one batch under no workspace", got)
	}
}

func TestBeforeModelProceedsOnAllow(t *testing.T) {
	opts := options()
	opts.Decider = &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_ALLOW}}
	r := recorder.New(recorder.Config{FlushEvery: time.Hour})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		r.Close(ctx)
	})

	resp, err := BeforeModel(r, opts)(newContext(), &adkmodel.LLMRequest{})
	if resp != nil || err != nil {
		t.Fatalf("BeforeModel() = (%v, %v), want (nil, nil) — ALLOW must never preempt the model call", resp, err)
	}
	if s := r.Stats(); s.Notified != 0 || s.DecisionErrors != 0 {
		t.Fatalf("stats = %+v, want no decision activity for ALLOW", s)
	}
}

func TestBeforeModelProceedsAndCountsOnNotify(t *testing.T) {
	opts := options()
	opts.Decider = &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_NOTIFY}}
	r := recorder.New(recorder.Config{FlushEvery: time.Hour})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		r.Close(ctx)
	})

	resp, err := BeforeModel(r, opts)(newContext(), &adkmodel.LLMRequest{})
	if resp != nil || err != nil {
		t.Fatalf("BeforeModel() = (%v, %v), want (nil, nil) — NOTIFY must still let the call proceed", resp, err)
	}
	if s := r.Stats(); s.Notified != 1 {
		t.Fatalf("Notified = %d, want 1", s.Notified)
	}
}

func TestBeforeModelProceedsAndCountsOnDecideError(t *testing.T) {
	opts := options()
	opts.Decider = &fakeDecider{err: errors.New("governance unreachable")}
	r := recorder.New(recorder.Config{FlushEvery: time.Hour})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		r.Close(ctx)
	})

	resp, err := BeforeModel(r, opts)(newContext(), &adkmodel.LLMRequest{})
	if resp != nil || err != nil {
		t.Fatalf("BeforeModel() = (%v, %v), want (nil, nil) — this version proceeds on any Decide error", resp, err)
	}
	if s := r.Stats(); s.DecisionErrors != 1 {
		t.Fatalf("DecisionErrors = %d, want 1", s.DecisionErrors)
	}
}

func TestBeforeModelSkipsTheProviderAndReturnsTheConfiguredMessageOnDeny(t *testing.T) {
	opts := options()
	opts.DeniedMessage = "You are out of credits."
	opts.Decider = &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_DENY}}
	r := recorder.New(recorder.Config{FlushEvery: time.Hour})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		r.Close(ctx)
	})

	resp, err := BeforeModel(r, opts)(newContext(), &adkmodel.LLMRequest{})
	if err != nil {
		t.Fatalf("BeforeModel: %v", err)
	}
	// A non-nil response from a BeforeModelCallback is what makes ADK skip
	// the actual model call and use this response instead — see llmagent's
	// own doc on BeforeModelCallbacks.
	if resp == nil {
		t.Fatal("BeforeModel() returned (nil, nil) on DENY, want a synthetic response that skips the provider call")
	}
	if got := len(resp.Content.Parts); got != 1 || resp.Content.Parts[0].Text != "You are out of credits." {
		t.Fatalf("response content = %+v, want the configured denied message", resp.Content)
	}
	if s := r.Stats(); s.Denied != 1 {
		t.Fatalf("Denied = %d, want 1", s.Denied)
	}
}

func TestBeforeModelUsesTheDefaultDeniedMessageWhenNoneIsConfigured(t *testing.T) {
	opts := options()
	opts.Decider = &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_DENY}}

	resp, err := BeforeModel(nil, opts)(newContext(), &adkmodel.LLMRequest{})
	if err != nil {
		t.Fatalf("BeforeModel: %v", err)
	}
	if resp == nil || len(resp.Content.Parts) != 1 || resp.Content.Parts[0].Text != DefaultDeniedMessage {
		t.Fatalf("response = %+v, want the default denied message %q", resp, DefaultDeniedMessage)
	}
}

func TestBeforeModelRecordsTheDenialAsAZeroCostDeniedActivity(t *testing.T) {
	opts := options()
	opts.Decider = &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_DENY}}

	sink := delivered(t, func(r *recorder.Recorder) {
		if _, err := BeforeModel(r, opts)(newContext(), &adkmodel.LLMRequest{}); err != nil {
			t.Fatalf("BeforeModel: %v", err)
		}
	})

	if len(sink.seen) != 1 {
		t.Fatalf("recorded %d activities, want 1 — a denial must be recorded even though the model was never called", len(sink.seen))
	}
	got := sink.seen[0]
	if got.GetStatus() != pb.Activity_DENIED {
		t.Fatalf("status = %v, want DENIED", got.GetStatus())
	}
	if got.GetEstimatedCostMicros() != 0 || got.GetTotalTokens() != 0 {
		t.Fatalf("activity = %+v, want zero cost and zero tokens — nothing was spent", got)
	}
}

// TestBeforeModelDoesNotTreatAnUnrecognizedDecisionAsDeny proves a decision
// value this build does not recognize at all — never DOWNGRADE itself,
// which is handled explicitly, but a hypothetical future value — proceeds
// exactly like ALLOW, never as DENY.
func TestBeforeModelDoesNotTreatAnUnrecognizedDecisionAsDeny(t *testing.T) {
	opts := options()
	opts.Decider = &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_Decision(99)}}
	r := recorder.New(recorder.Config{FlushEvery: time.Hour})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		r.Close(ctx)
	})

	resp, err := BeforeModel(r, opts)(newContext(), &adkmodel.LLMRequest{})
	if resp != nil || err != nil {
		t.Fatalf("BeforeModel() = (%v, %v), want (nil, nil) — an unrecognized verdict must never preempt the call", resp, err)
	}
	if s := r.Stats(); s.Denied != 0 {
		t.Fatalf("Denied = %d, want 0 — an unrecognized value is not DENY", s.Denied)
	}
}

// TestBeforeModelDoesNotTreatDowngradeAsDeny proves DOWNGRADE never preempts
// the call, even where governance names no replacement at all — there is
// nothing to apply, but the call still proceeds exactly like ALLOW.
func TestBeforeModelDoesNotTreatDowngradeAsDeny(t *testing.T) {
	opts := options()
	opts.Decider = &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_DOWNGRADE}}
	r := recorder.New(recorder.Config{FlushEvery: time.Hour})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		r.Close(ctx)
	})

	request := &adkmodel.LLMRequest{Model: "gemini-2.5-pro"}
	resp, err := BeforeModel(r, opts)(newContext(), request)
	if resp != nil || err != nil {
		t.Fatalf("BeforeModel() = (%v, %v), want (nil, nil) — DOWNGRADE must never preempt the call", resp, err)
	}
	if s := r.Stats(); s.Denied != 0 {
		t.Fatalf("Denied = %d, want 0 — DOWNGRADE is not DENY", s.Denied)
	}
	if s := r.Stats(); s.Downgraded != 1 || s.DowngradeApplied != 0 || s.DowngradeNotApplied != 0 {
		t.Fatalf("stats = %+v, want Downgraded=1, DowngradeApplied=0, DowngradeNotApplied=0 — nothing was ever attempted with no replacement named", s)
	}
	if request.Model != "gemini-2.5-pro" {
		t.Fatalf("request.Model = %q, want the original gemini-2.5-pro unchanged", request.Model)
	}
}

// TestBeforeModelAppliesADowngradeToTheOutgoingRequest proves a DOWNGRADE
// whose replacement provider matches this agent's own opts.billedBy() is
// applied by rewriting request.Model in place, before the framework ever
// sends the call — and that the replacement, not the original request, is
// what AfterModel falls back to recording when the provider's own reply
// carries no model of its own.
func TestBeforeModelAppliesADowngradeToTheOutgoingRequest(t *testing.T) {
	opts := options() // billedBy() defaults to VERTEX_AI
	opts.Decider = &fakeDecider{resp: &governancepb.DecideResponse{
		Decision: governancepb.DecideResponse_DOWNGRADE, ReplacementProvider: "VERTEX_AI", ReplacementModel: "gemini-2.5-flash-lite",
	}}

	ctx := newContext()
	request := &adkmodel.LLMRequest{Model: "gemini-2.5-pro"}
	var stats recorder.Stats
	sink := delivered(t, func(r *recorder.Recorder) {
		if _, err := BeforeModel(r, opts)(ctx, request); err != nil {
			t.Fatalf("BeforeModel: %v", err)
		}
		// A streamed call's final response carries no model at all —
		// modelOf then falls back to what AfterModel considers
		// "requested", which BeforeModel must have already set to the
		// applied replacement.
		AfterModel(r, opts)(ctx, &adkmodel.LLMResponse{}, nil)
		stats = r.Stats()
	})

	if request.Model != "gemini-2.5-flash-lite" {
		t.Fatalf("request.Model = %q, want the replacement gemini-2.5-flash-lite applied in place", request.Model)
	}
	if stats.DowngradeApplied != 1 || stats.DowngradeNotApplied != 0 {
		t.Fatalf("stats = %+v, want DowngradeApplied=1, DowngradeNotApplied=0", stats)
	}
	if got := sink.seen[0].GetModel(); got != "gemini-2.5-flash-lite" {
		t.Fatalf("recorded model = %q, want the applied replacement gemini-2.5-flash-lite, never the original gemini-2.5-pro", got)
	}
}

// TestBeforeModelFailsOpenWhenDowngradeReplacementProviderDoesNotMatch
// proves a DOWNGRADE naming a provider this agent's model client does not
// talk to is never applied — rewriting only the model name would ask the
// wrong provider for it. The original request proceeds unchanged.
func TestBeforeModelFailsOpenWhenDowngradeReplacementProviderDoesNotMatch(t *testing.T) {
	opts := options() // billedBy() defaults to VERTEX_AI
	opts.Decider = &fakeDecider{resp: &governancepb.DecideResponse{
		Decision: governancepb.DecideResponse_DOWNGRADE, ReplacementProvider: "OPENAI", ReplacementModel: "gpt-5-mini",
	}}
	r := recorder.New(recorder.Config{FlushEvery: time.Hour})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		r.Close(ctx)
	})

	request := &adkmodel.LLMRequest{Model: "gemini-2.5-pro"}
	if _, err := BeforeModel(r, opts)(newContext(), request); err != nil {
		t.Fatalf("BeforeModel: %v", err)
	}
	if request.Model != "gemini-2.5-pro" {
		t.Fatalf("request.Model = %q, want the original gemini-2.5-pro — a provider-mismatched replacement must never be attempted", request.Model)
	}
	if s := r.Stats(); s.DowngradeApplied != 0 || s.DowngradeNotApplied != 1 {
		t.Fatalf("stats = %+v, want DowngradeApplied=0, DowngradeNotApplied=1", s)
	}
}

// TestBeforeModelSendsTheRequestedModelAndProviderToGovernance proves the
// DecideRequest carries what the call was about to ask for, so governance
// can decide a downgrade against it.
func TestBeforeModelSendsTheRequestedModelAndProviderToGovernance(t *testing.T) {
	decider := &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_ALLOW}}
	opts := options()
	opts.Decider = decider

	if _, err := BeforeModel(nil, opts)(newContext(), &adkmodel.LLMRequest{Model: "gemini-2.5-pro"}); err != nil {
		t.Fatalf("BeforeModel: %v", err)
	}

	if decider.got.GetRequestedProvider() != "VERTEX_AI" || decider.got.GetRequestedModel() != "gemini-2.5-pro" {
		t.Fatalf("requested = %q/%q, want VERTEX_AI/gemini-2.5-pro", decider.got.GetRequestedProvider(), decider.got.GetRequestedModel())
	}
}

func TestBeforeModelBoundsDecideWithADeadline(t *testing.T) {
	decider := &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_ALLOW}}
	wrapped := &deadlineCapturingDecider{fakeDecider: decider}
	opts := options()
	opts.Decider = wrapped
	opts.DecideTimeout = 10 * time.Millisecond

	if _, err := BeforeModel(nil, opts)(newContext(), &adkmodel.LLMRequest{}); err != nil {
		t.Fatalf("BeforeModel: %v", err)
	}
	if !wrapped.hadDeadline {
		t.Fatal("Decide was called without a bounded context")
	}
}

// deadlineCapturingDecider records whether the context Decide was called
// with carried a deadline.
type deadlineCapturingDecider struct {
	*fakeDecider
	hadDeadline bool
}

func (d *deadlineCapturingDecider) Decide(ctx context.Context, req *governancepb.DecideRequest) (*governancepb.DecideResponse, error) {
	_, d.hadDeadline = ctx.Deadline()
	return d.fakeDecider.Decide(ctx, req)
}

func TestBeforeModelCachesRepeatedDecideCallsForTheSameContext(t *testing.T) {
	decider := &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_ALLOW}}
	opts := options()
	opts.Decider = decider
	opts.DecideCacheTTL = time.Minute

	before := BeforeModel(nil, opts)
	ctx := newContext()

	if _, err := before(ctx, &adkmodel.LLMRequest{}); err != nil {
		t.Fatalf("first BeforeModel: %v", err)
	}
	if _, err := before(ctx, &adkmodel.LLMRequest{}); err != nil {
		t.Fatalf("second BeforeModel: %v", err)
	}

	if decider.calls != 1 {
		t.Fatalf("wrapped Decider called %d times, want 1 — a repeated call for the same decision context should answer from the cache", decider.calls)
	}
}

func TestBeforeModelWithoutCacheTTLCallsDeciderEveryTime(t *testing.T) {
	decider := &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_ALLOW}}
	opts := options()
	opts.Decider = decider
	// DecideCacheTTL left at zero.

	before := BeforeModel(nil, opts)
	ctx := newContext()

	if _, err := before(ctx, &adkmodel.LLMRequest{}); err != nil {
		t.Fatalf("first BeforeModel: %v", err)
	}
	if _, err := before(ctx, &adkmodel.LLMRequest{}); err != nil {
		t.Fatalf("second BeforeModel: %v", err)
	}

	if decider.calls != 2 {
		t.Fatalf("wrapped Decider called %d times, want 2 — a zero DecideCacheTTL must not cache", decider.calls)
	}
}

// TestBeforeModelDecideTimeoutAppliesOnMissNotOnCachedHit proves DecideTimeout
// still bounds the real call to the wrapped Decider on a cache miss, and that
// a cached hit never reaches the wrapped Decider — and so never needs the
// deadline — at all.
func TestBeforeModelDecideTimeoutAppliesOnMissNotOnCachedHit(t *testing.T) {
	inner := &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_ALLOW}}
	wrapped := &deadlineCapturingDecider{fakeDecider: inner}
	opts := options()
	opts.Decider = wrapped
	opts.DecideCacheTTL = time.Minute
	opts.DecideTimeout = 10 * time.Millisecond

	before := BeforeModel(nil, opts)
	ctx := newContext()

	if _, err := before(ctx, &adkmodel.LLMRequest{}); err != nil {
		t.Fatalf("first BeforeModel: %v", err)
	}
	if !wrapped.hadDeadline {
		t.Fatal("the first (miss) call reached the wrapped Decider without a bounded context")
	}
	if inner.calls != 1 {
		t.Fatalf("wrapped Decider called %d times after the first call, want 1", inner.calls)
	}

	wrapped.hadDeadline = false // reset, so the next assertion cannot pass by accident
	if _, err := before(ctx, &adkmodel.LLMRequest{}); err != nil {
		t.Fatalf("second BeforeModel: %v", err)
	}
	if wrapped.hadDeadline {
		t.Fatal("the second (cached) call reached the wrapped Decider again")
	}
	if inner.calls != 1 {
		t.Fatalf("wrapped Decider called %d times after the cached call, want still 1", inner.calls)
	}
}

// TestBeforeModelCachesNotifyDecisionsToo proves a NOTIFY verdict is cached
// and reused exactly like ALLOW, and that Stats().Notified still reports it
// on the cached call — the cache changes whether governance is asked again,
// never whether the host is told about the verdict it already gave.
func TestBeforeModelCachesNotifyDecisionsToo(t *testing.T) {
	decider := &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_NOTIFY}}
	opts := options()
	opts.Decider = decider
	opts.DecideCacheTTL = time.Minute
	r := recorder.New(recorder.Config{FlushEvery: time.Hour})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		r.Close(ctx)
	})

	before := BeforeModel(r, opts)
	ctx := newContext()

	if _, err := before(ctx, &adkmodel.LLMRequest{}); err != nil {
		t.Fatalf("first BeforeModel: %v", err)
	}
	if _, err := before(ctx, &adkmodel.LLMRequest{}); err != nil {
		t.Fatalf("second BeforeModel: %v", err)
	}

	if decider.calls != 1 {
		t.Fatalf("wrapped Decider called %d times, want 1 — a NOTIFY verdict should be cached like ALLOW", decider.calls)
	}
	if s := r.Stats(); s.Notified != 2 {
		t.Fatalf("Notified = %d, want 2 — a cached NOTIFY must still be reported each time it's returned", s.Notified)
	}
}

func TestOrganisationOfDerivesTheParentFromAnAgentName(t *testing.T) {
	got, err := organisationOf("organisations/acme/agents/atlas")
	if err != nil {
		t.Fatalf("organisationOf: %v", err)
	}
	if got != "organisations/acme" {
		t.Fatalf("organisationOf() = %q, want %q", got, "organisations/acme")
	}
}

func TestOrganisationOfRejectsAMalformedAgentName(t *testing.T) {
	if _, err := organisationOf("not-an-agent-name"); err == nil {
		t.Fatal("organisationOf() error = nil, want an error for a malformed agent name")
	}
}

func TestBeforeModelCountsADecisionErrorForAMalformedAgentName(t *testing.T) {
	opts := options()
	opts.Agent = "not-an-agent-name"
	opts.Decider = &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_ALLOW}}
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
	if s := r.Stats(); s.DecisionErrors != 1 {
		t.Fatalf("DecisionErrors = %d, want 1 for a malformed agent name", s.DecisionErrors)
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
	ctx.Context = recorder.WithRequest(context.Background(), "requests/7f3a")
	if _, err := AfterAgent(r, options())(ctx); err != nil {
		t.Fatalf("AfterAgent: %v", err)
	}

	if got := r.SpentOn("requests/7f3a"); got != 0 {
		t.Fatalf("SpentOn after the turn ended = %d, want 0", got)
	}
}
