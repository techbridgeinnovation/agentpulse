package adkhooks

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/adk/agent"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/techbridgeinnovation/agentpulse/recorder"
	governancepb "github.com/techbridgeinnovation/agentpulse/recorder/pb/governance"
	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
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

func TestBeforeModelLabelsTheRequestWithTheSanitisedIdentity(t *testing.T) {
	request := &adkmodel.LLMRequest{}
	BeforeModel(nil, options())(newContext(), request)

	labels := request.Config.Labels
	// The whole value is sanitised, so the label is the sanitised form of what
	// gets recorded and the two join.
	if labels["ap_user"] != "users_abc123" {
		t.Fatalf("user label = %q, want users_abc123", labels["ap_user"])
	}
	if labels["ap_component"] != "atlas" {
		t.Fatalf("component label = %q", labels["ap_component"])
	}
}

func TestBeforeModelNeverReplacesALabelSetCloserToTheCall(t *testing.T) {
	request := &adkmodel.LLMRequest{
		Config: &genai.GenerateContentConfig{Labels: map[string]string{"ap_user": "set_by_hand"}},
	}
	BeforeModel(nil, options())(newContext(), request)

	if got := request.Config.Labels["ap_user"]; got != "set_by_hand" {
		t.Fatalf("user label = %q, want the value already there", got)
	}
}

func TestProviderDefaultsToVertexRatherThanUnspecified(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		AfterModel(r, Options{Agent: "organisations/x/agents/y"})(newContext(), &adkmodel.LLMResponse{ModelVersion: "m"}, nil)
	})

	if got[0].GetProvider() != pb.Activity_VERTEX_AI {
		t.Fatalf("provider = %v, want VERTEX_AI", got[0].GetProvider())
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
	if decider.got.GetProduct() != "rezco" {
		t.Fatalf("product = %q, want %q", decider.got.GetProduct(), "rezco")
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
