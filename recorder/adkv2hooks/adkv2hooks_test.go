package adkv2hooks

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	governancepb "github.com/techbridgeinnovation/agentpulse/recorder/pb/governance"
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

// Branch is stubbed to empty because callKey now reads it, for the same
// reason the fields above already are — a downgrade's in-flight tracking
// needs to tell a sub-agent's or a parallel branch's calls apart from the
// rest of the turn, exactly as adkhooks' own callKey already does.
func (f *fakeContext) Branch() string { return "" }

func newContext() *fakeContext {
	return &fakeContext{
		StrictContextMock: agent.NewStrictContextMock(context.Background()),
		invocation:        "inv-1",
		user:              "users/abc123",
		sessionID:         "sess-1",
		agentName:         "pulseagent-v1",
	}
}

// collector keeps what the recorder delivers, and the workspace each batch was
// delivered under.
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
	if a.GetBilledBy() != recorder.ProviderVertexAI {
		t.Errorf("billed_by = %q, want %q by default rather than empty", a.GetBilledBy(), recorder.ProviderVertexAI)
	}
	if a.GetObservedAs() != pb.Agent_AGENT {
		t.Errorf("observed_as = %v, want AGENT: the call came through an agent's callbacks", a.GetObservedAs())
	}
	if a.GetSubAgent() != "pulseagent-v1" {
		t.Errorf("sub_agent = %q, want the agent that made the call", a.GetSubAgent())
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

// Neither is asked for at a call site: both are read off the turn's own
// context, where the product set them when it checked the sign-in. The same
// two values the first major reads, off the one context type this one hands
// every callback.
func TestTheCallbacksTakeTheWorkspaceAndProjectFromTheContext(t *testing.T) {
	ctx := newContext()
	ctx.StrictContextMock = agent.NewStrictContextMock(
		recorder.WithProject(recorder.WithWorkspace(context.Background(), "acme"), "matter-1183"),
	)

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
	if s := r.Stats(); s.Notified != 0 || s.Denied != 0 || s.DecisionErrors != 0 {
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
	if s := r.Stats(); s.Notified != 0 || s.Denied != 0 || s.DecisionErrors != 0 {
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
		t.Fatalf("BeforeModel() = (%v, %v), want (nil, nil) — this integration proceeds on any Decide error", resp, err)
	}
	if s := r.Stats(); s.DecisionErrors != 1 {
		t.Fatalf("DecisionErrors = %d, want 1", s.DecisionErrors)
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
		t.Fatalf("BeforeModel() = (%v, %v), want (nil, nil) — a malformed agent name must still fail open", resp, err)
	}
	if s := r.Stats(); s.DecisionErrors != 1 {
		t.Fatalf("DecisionErrors = %d, want 1", s.DecisionErrors)
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

	request := &adkmodel.LLMRequest{Model: "gemini-3.5-pro"}
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
	if request.Model != "gemini-3.5-pro" {
		t.Fatalf("request.Model = %q, want the original gemini-3.5-pro unchanged", request.Model)
	}
}

// TestBeforeModelAppliesADowngradeToTheOutgoingRequest proves a DOWNGRADE
// whose replacement provider matches this agent's own opts.billedBy() is
// applied by rewriting request.Model in place, before the framework ever
// sends the call — and that AfterModel records the applied replacement,
// not the agent's static configured model, for a call whose final response
// carries no model of its own.
func TestBeforeModelAppliesADowngradeToTheOutgoingRequest(t *testing.T) {
	opts := options() // billedBy() defaults to VERTEX_AI; opts.Model (the static fallback) is "gemini-3.5-flash-lite"
	opts.Decider = &fakeDecider{resp: &governancepb.DecideResponse{
		// Deliberately a third, distinct model name from both opts.Model and
		// the originally requested one, so a test bug that fell back to
		// either of those instead of the applied replacement would be
		// caught rather than accidentally matching.
		Decision: governancepb.DecideResponse_DOWNGRADE, ReplacementProvider: "VERTEX_AI", ReplacementModel: "gemini-3.5-nano",
	}}

	ctx := newContext()
	request := &adkmodel.LLMRequest{Model: "gemini-3.5-pro"}
	var stats recorder.Stats
	sink := delivered(t, func(r *recorder.Recorder) {
		if _, err := BeforeModel(r, opts)(ctx, request); err != nil {
			t.Fatalf("BeforeModel: %v", err)
		}
		AfterModel(r, opts)(ctx, &adkmodel.LLMResponse{}, nil)
		stats = r.Stats()
	})

	if request.Model != "gemini-3.5-nano" {
		t.Fatalf("request.Model = %q, want the replacement gemini-3.5-nano applied in place", request.Model)
	}
	if stats.DowngradeApplied != 1 || stats.DowngradeNotApplied != 0 {
		t.Fatalf("stats = %+v, want DowngradeApplied=1, DowngradeNotApplied=0", stats)
	}
	if got := sink.seen[0].GetModel(); got != "gemini-3.5-nano" {
		t.Fatalf("recorded model = %q, want the applied replacement gemini-3.5-nano, never opts.Model's static gemini-3.5-flash-lite or the originally requested gemini-3.5-pro", got)
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

	request := &adkmodel.LLMRequest{Model: "gemini-3.5-pro"}
	if _, err := BeforeModel(r, opts)(newContext(), request); err != nil {
		t.Fatalf("BeforeModel: %v", err)
	}
	if request.Model != "gemini-3.5-pro" {
		t.Fatalf("request.Model = %q, want the original gemini-3.5-pro — a provider-mismatched replacement must never be attempted", request.Model)
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

	if _, err := BeforeModel(nil, opts)(newContext(), &adkmodel.LLMRequest{Model: "gemini-3.5-pro"}); err != nil {
		t.Fatalf("BeforeModel: %v", err)
	}

	if decider.got.GetRequestedProvider() != "VERTEX_AI" || decider.got.GetRequestedModel() != "gemini-3.5-pro" {
		t.Fatalf("requested = %q/%q, want VERTEX_AI/gemini-3.5-pro", decider.got.GetRequestedProvider(), decider.got.GetRequestedModel())
	}
}
