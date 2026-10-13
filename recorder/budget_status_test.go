package recorder

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	governancepb "github.com/techbridgeinnovation/agentpulse/recorder/pb/governance"
	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

// statusDecider answers every decision with one verdict and the budget statuses governance would send beside it.
type statusDecider struct {
	decision governancepb.DecideResponse_Decision
	statuses []*governancepb.BudgetStatus
	evaluate *timestamppb.Timestamp
	err      error
	calls    int
}

func (d *statusDecider) Decide(context.Context, *governancepb.DecideRequest) (*governancepb.DecideResponse, error) {
	d.calls++
	if d.err != nil {
		return nil, d.err
	}
	return &governancepb.DecideResponse{Decision: d.decision, BudgetStatuses: d.statuses, EvaluateTime: d.evaluate}, nil
}

func pressingStatuses() []*governancepb.BudgetStatus {
	return []*governancepb.BudgetStatus{
		{Budget: "organisations/techbridge/budgets/team", Scope: governancepb.BudgetStatus_SHARED_POOL, SpentMicros: 960_000, LimitMicros: 1_000_000, Level: governancepb.BudgetStatus_PASSED_95_PERCENT},
		{Budget: "organisations/techbridge/budgets/reserve", Scope: governancepb.BudgetStatus_RESERVE, SpentMicros: 0, LimitMicros: 0, Level: governancepb.BudgetStatus_WITHIN},
	}
}

// seenStatuses keeps what a product's OnBudgetStatus was handed.
type seenStatuses struct {
	mu        sync.Mutex
	calls     int
	statuses  []*governancepb.BudgetStatus
	evaluated time.Time
	ctx       context.Context
}

// waitFor returns once want deliveries have arrived, since delivery is asynchronous, and fails the test if they do not.
func (s *seenStatuses) waitFor(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s.count() < want {
		if time.Now().After(deadline) {
			t.Fatalf("%d deliveries, want %d", s.count(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func (s *seenStatuses) on(ctx context.Context, statuses []*governancepb.BudgetStatus, evaluated time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.ctx = ctx
	s.statuses = statuses
	s.evaluated = evaluated
}

func (s *seenStatuses) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// A product that asks for budget statuses is handed every one governance sent, most pressing first, with when they were read, and the call goes ahead on its verdict.
func TestAGovernedClientHandsTheProductItsBudgetStatuses(t *testing.T) {
	server := provider(t, 200, "application/json", anthropicReply)
	evaluated := time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC)
	d := &statusDecider{decision: governancepb.DecideResponse_NOTIFY, statuses: pressingStatuses(), evaluate: timestamppb.New(evaluated)}
	seen := &seenStatuses{}

	got, rec := directWithRecorder(t, service, func(rp *Reporter) {
		through(t, context.Background(), rp.Governed(Governance{Decider: d, OnBudgetStatus: seen.on}).AnthropicMiddleware(), server.URL+"/v1/messages")
		seen.waitFor(t, 1)
	})

	if a := only(t, got); a.GetStatus() != pb.Activity_OK {
		t.Errorf("status = %v, want the call made", a.GetStatus())
	}
	seen.mu.Lock()
	defer seen.mu.Unlock()
	if seen.calls != 1 || len(seen.statuses) != 2 {
		t.Fatalf("handed %d statuses over %d calls, want 2 over 1", len(seen.statuses), seen.calls)
	}
	if seen.statuses[0].GetLevel() != governancepb.BudgetStatus_PASSED_95_PERCENT || seen.statuses[1].GetScope() != governancepb.BudgetStatus_RESERVE {
		t.Errorf("statuses = %v, want governance's order kept", seen.statuses)
	}
	if !seen.evaluated.Equal(evaluated) {
		t.Errorf("evaluated = %v, want %v", seen.evaluated, evaluated)
	}
	if s := rec.Stats(); s.Notified != 1 || s.BudgetStatusFailed != 0 || s.BudgetStatusDropped != 0 {
		t.Errorf("stats = %+v, want one NOTIFY and nothing failed or dropped", s)
	}
}

// A refusal still hands the product its statuses, which is how it can say why, and the refusal stands.
func TestARefusedCallStillHandsTheProductItsBudgetStatuses(t *testing.T) {
	server := provider(t, 200, "application/json", anthropicReply)
	d := &statusDecider{decision: governancepb.DecideResponse_DENY, statuses: pressingStatuses()}
	seen := &seenStatuses{}

	a := only(t, direct(t, service, func(rp *Reporter) {
		through(t, context.Background(), rp.Governed(Governance{Decider: d, OnBudgetStatus: seen.on}).AnthropicMiddleware(), server.URL+"/v1/messages")
		seen.waitFor(t, 1)
	}))

	if a.GetStatus() != pb.Activity_DENIED {
		t.Errorf("status = %v, want DENIED", a.GetStatus())
	}
	if !seen.evaluated.IsZero() {
		t.Errorf("evaluated = %v, want the zero time when governance did not say", seen.evaluated)
	}
}

// Nothing is queued when there is nothing to hand over: no answer, an answer with no statuses as an older governance gives, or a refusal repeated from memory.
func TestNoBudgetStatusesMeansNoCall(t *testing.T) {
	server := provider(t, 200, "application/json", anthropicReply)
	for name, d := range map[string]*statusDecider{
		"unreachable":      {err: errors.New("governance down")},
		"older governance": {decision: governancepb.DecideResponse_NOTIFY},
	} {
		seen := &seenStatuses{}
		got, rec := directWithRecorder(t, service, func(rp *Reporter) {
			through(t, context.Background(), rp.Governed(Governance{Decider: d, OnBudgetStatus: seen.on}).AnthropicMiddleware(), server.URL+"/v1/messages")
		})
		if a := only(t, got); a.GetStatus() != pb.Activity_OK {
			t.Errorf("%s: status = %v, want the call made", name, a.GetStatus())
		}
		if seen.count() != 0 || rec.budgetQueue != nil {
			t.Errorf("%s: OnBudgetStatus called %d times or a worker started, want neither", name, seen.count())
		}
	}

	d := &statusDecider{decision: governancepb.DecideResponse_DENY, statuses: pressingStatuses()}
	remembering := RememberRefusals(d)
	req := &governancepb.DecideRequest{Parent: "organisations/techbridge", Agent: service.Agent}
	if _, err := remembering.Decide(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	d.err = errors.New("governance down")
	again, err := remembering.Decide(context.Background(), req)
	if err != nil || again.GetDecision() != governancepb.DecideResponse_DENY {
		t.Fatalf("remembered = (%v, %v), want the refusal repeated", again, err)
	}
	if len(again.GetBudgetStatuses()) != 0 {
		t.Errorf("a refusal repeated from memory carries %d statuses, want none", len(again.GetBudgetStatuses()))
	}
}

// With no function, nil resp, or a nil recorder, reporting is a no-op.
func TestReportingBudgetStatusesNeedsAFunctionAResponseAndARecorder(t *testing.T) {
	resp := &governancepb.DecideResponse{BudgetStatuses: pressingStatuses()}
	var none *Recorder
	none.ReportBudgetStatus(context.Background(), func(context.Context, []*governancepb.BudgetStatus, time.Time) {}, resp)

	rec := New(Config{FlushEvery: time.Hour})
	rec.ReportBudgetStatus(context.Background(), nil, resp)
	rec.ReportBudgetStatus(context.Background(), (&seenStatuses{}).on, nil)
	rec.ReportBudgetStatus(context.Background(), (&seenStatuses{}).on, &governancepb.DecideResponse{BudgetStatuses: []*governancepb.BudgetStatus{nil}})
	if rec.budgetQueue != nil {
		t.Error("a worker was started with nothing to deliver")
	}
	rec.Close(context.Background())
}

// Left unset, a decision carrying statuses is handled exactly as one without.
func TestAnUnsetOnBudgetStatusChangesNothing(t *testing.T) {
	server := provider(t, 200, "application/json", anthropicReply)
	d := &statusDecider{decision: governancepb.DecideResponse_ALLOW, statuses: pressingStatuses()}
	got, rec := directWithRecorder(t, service, func(rp *Reporter) {
		through(t, context.Background(), rp.Governed(Governance{Decider: d}).AnthropicMiddleware(), server.URL+"/v1/messages")
	})
	if a := only(t, got); a.GetStatus() != pb.Activity_OK {
		t.Errorf("status = %v, want the call made", a.GetStatus())
	}
	if s := rec.Stats(); s.BudgetStatusFailed != 0 || s.BudgetStatusDropped != 0 || s.Panicked != 0 {
		t.Errorf("stats = %+v, want nothing counted", s)
	}
}

// A product's callback that panics costs the call nothing: the verdict stands and the panic is counted.
func TestAPanickingOnBudgetStatusIsCountedAndChangesNoVerdict(t *testing.T) {
	server := provider(t, 200, "application/json", anthropicReply)
	for decision, want := range map[governancepb.DecideResponse_Decision]pb.Activity_Status{
		governancepb.DecideResponse_ALLOW: pb.Activity_OK,
		governancepb.DecideResponse_DENY:  pb.Activity_DENIED,
	} {
		d := &statusDecider{decision: decision, statuses: pressingStatuses()}
		var rec *Recorder
		got, rec := directWithRecorder(t, service, func(rp *Reporter) {
			panics := func(context.Context, []*governancepb.BudgetStatus, time.Time) { panic("product bug") }
			through(t, context.Background(), rp.Governed(Governance{Decider: d, OnBudgetStatus: panics}).AnthropicMiddleware(), server.URL+"/v1/messages")
			rec = rp.recorder
			waitUntil(t, func() bool { return rec.Stats().BudgetStatusFailed == 1 })
		})
		if a := only(t, got); a.GetStatus() != want {
			t.Errorf("%v: status = %v, want %v", decision, a.GetStatus(), want)
		}
		if s := rec.Stats(); s.BudgetStatusFailed != 1 || s.Panicked != 1 {
			t.Errorf("%v: BudgetStatusFailed = %d, Panicked = %d, want 1 and 1", decision, s.BudgetStatusFailed, s.Panicked)
		}
	}
}

func decisionWithStatuses() *governancepb.DecideResponse {
	return &governancepb.DecideResponse{Decision: governancepb.DecideResponse_ALLOW, BudgetStatuses: pressingStatuses()}
}

// The call never waits for the callback: with the callback held on a channel, the decision path returns at once, and the callback still runs when released.
func TestTheCallNeverWaitsForTheCallback(t *testing.T) {
	server := provider(t, 200, "application/json", anthropicReply)
	d := &statusDecider{decision: governancepb.DecideResponse_ALLOW, statuses: pressingStatuses()}
	entered := make(chan struct{})
	release := make(chan struct{})
	var released sync.Once
	releaseNow := func() { released.Do(func() { close(release) }) }
	defer releaseNow()
	hold := func(context.Context, []*governancepb.BudgetStatus, time.Time) {
		close(entered)
		<-release
	}

	got, rec := directWithRecorder(t, service, func(rp *Reporter) {
		governed := rp.Governed(Governance{Decider: d, OnBudgetStatus: hold}).AnthropicMiddleware()
		returned := make(chan struct{})
		go func() {
			through(t, context.Background(), governed, server.URL+"/v1/messages")
			close(returned)
		}()
		select {
		case <-returned:
		case <-time.After(5 * time.Second):
			t.Error("the call did not return while the callback was held")
		}
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Error("the callback never started")
		}
		releaseNow()
	})
	if a := only(t, got); a.GetStatus() != pb.Activity_OK {
		t.Errorf("status = %v, want the call made", a.GetStatus())
	}
	if s := rec.Stats(); s.BudgetStatusDropped != 0 {
		t.Errorf("BudgetStatusDropped = %d, want 0", s.BudgetStatusDropped)
	}
}

// Deliveries arrive one at a time, in the order the decisions were obtained.
func TestBudgetStatusesAreDeliveredInOrder(t *testing.T) {
	rec := New(Config{FlushEvery: time.Hour})
	defer rec.Close(context.Background())
	var mu sync.Mutex
	var order []int64
	active := 0
	overlapped := false
	fn := func(_ context.Context, statuses []*governancepb.BudgetStatus, _ time.Time) {
		mu.Lock()
		active++
		overlapped = overlapped || active > 1
		mu.Unlock()
		time.Sleep(time.Millisecond)
		mu.Lock()
		order = append(order, statuses[0].GetSpentMicros())
		active--
		mu.Unlock()
	}
	const n = 40
	for i := range n {
		rec.ReportBudgetStatus(context.Background(), fn, &governancepb.DecideResponse{BudgetStatuses: []*governancepb.BudgetStatus{{SpentMicros: int64(i)}}})
	}
	waitUntil(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(order) == n })
	for i, got := range order {
		if got != int64(i) {
			t.Fatalf("delivery %d carried %d, want order kept: %v", i, got, order)
		}
	}
	if overlapped {
		t.Error("two deliveries ran at once, want one at a time")
	}
}

// Behind a blocked callback the queue fills and further events are dropped and counted, and nothing ever blocks the caller.
func TestASaturatedQueueDropsAndCounts(t *testing.T) {
	rec := New(Config{FlushEvery: time.Hour})
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	fn := func(context.Context, []*governancepb.BudgetStatus, time.Time) {
		once.Do(func() { close(entered) })
		<-release
	}
	resp := decisionWithStatuses()
	rec.ReportBudgetStatus(context.Background(), fn, resp)
	<-entered // the worker holds one; the queue itself is empty

	const pushed = budgetStatusQueueSize + 10
	started := time.Now()
	for range pushed {
		rec.ReportBudgetStatus(context.Background(), fn, resp)
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Errorf("reporting took %v behind a blocked callback, want it never to wait", took)
	}
	if got := rec.Stats().BudgetStatusDropped; got != 10 {
		t.Errorf("BudgetStatusDropped = %d, want 10", got)
	}
	close(release)
	rec.Close(context.Background())
}

// A cached decision hands a warm call the same statuses the cold one had, and a product changing what it was handed changes nothing the cache keeps.
func TestACachedDecisionCarriesItsBudgetStatuses(t *testing.T) {
	server := provider(t, 200, "application/json", anthropicReply)
	d := &statusDecider{decision: governancepb.DecideResponse_ALLOW, statuses: pressingStatuses()}
	var mu sync.Mutex
	var spent []int64
	mutates := func(_ context.Context, statuses []*governancepb.BudgetStatus, _ time.Time) {
		mu.Lock()
		defer mu.Unlock()
		spent = append(spent, statuses[0].GetSpentMicros())
		statuses[0].SpentMicros = 0
		statuses[0].Level = governancepb.BudgetStatus_WITHIN
	}
	delivered := func() int { mu.Lock(); defer mu.Unlock(); return len(spent) }

	direct(t, service, func(rp *Reporter) {
		governed := rp.Governed(Governance{Decider: d, CacheTTL: time.Minute, OnBudgetStatus: mutates}).AnthropicMiddleware()
		through(t, context.Background(), governed, server.URL+"/v1/messages")
		waitUntil(t, func() bool { return delivered() == 1 })
		through(t, context.Background(), governed, server.URL+"/v1/messages")
		waitUntil(t, func() bool { return delivered() == 2 })
	})

	if d.calls != 1 {
		t.Errorf("governance asked %d times, want once with the second call answered from the cache", d.calls)
	}
	if spent[0] != 960_000 || spent[1] != 960_000 {
		t.Errorf("spent handed over = %v, want 960000 both times", spent)
	}
}

type ctxKey struct{}

// The context the callback gets keeps the values of the call's and outlives the call's cancellation.
func TestTheCallbackContextOutlivesTheCall(t *testing.T) {
	rec := New(Config{FlushEvery: time.Hour})
	defer rec.Close(context.Background())
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "kept"))
	cancel()
	got := make(chan context.Context, 1)
	rec.ReportBudgetStatus(ctx, func(c context.Context, _ []*governancepb.BudgetStatus, _ time.Time) { got <- c }, decisionWithStatuses())
	select {
	case c := <-got:
		if c.Value(ctxKey{}) != "kept" || c.Err() != nil {
			t.Errorf("ctx value = %v, err = %v, want the value kept and no cancellation", c.Value(ctxKey{}), c.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery")
	}
}

// Close does not wait for a callback that never returns, the events still queued are counted as dropped, and nothing is delivered afterwards.
func TestCloseDoesNotWaitForABlockedCallback(t *testing.T) {
	rec := New(Config{FlushEvery: time.Hour})
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	fn := func(context.Context, []*governancepb.BudgetStatus, time.Time) {
		once.Do(func() { close(entered) })
		<-release
	}
	resp := decisionWithStatuses()
	rec.ReportBudgetStatus(context.Background(), fn, resp)
	<-entered
	rec.ReportBudgetStatus(context.Background(), fn, resp)
	rec.ReportBudgetStatus(context.Background(), fn, resp)

	closed := make(chan struct{})
	go func() {
		rec.Close(context.Background())
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited for a blocked callback")
	}
	if got := rec.Stats().BudgetStatusDropped; got != 2 {
		t.Errorf("BudgetStatusDropped = %d after Close, want the 2 still queued", got)
	}
	rec.ReportBudgetStatus(context.Background(), fn, resp)
	if got := rec.Stats().BudgetStatusDropped; got != 3 {
		t.Errorf("BudgetStatusDropped = %d, want a report after Close counted as dropped", got)
	}
	close(release)
}

// Reporting a great many times starts one worker, not one goroutine per call, and Close ends it.
func TestReportingStartsOneWorkerAndCloseEndsIt(t *testing.T) {
	before := runtime.NumGoroutine()
	rec := New(Config{FlushEvery: time.Hour})
	seen := &seenStatuses{}
	for range 200 {
		rec.ReportBudgetStatus(context.Background(), seen.on, decisionWithStatuses())
	}
	if grew := runtime.NumGoroutine() - before; grew > 6 {
		t.Errorf("%d goroutines added by 200 reports, want a handful at most", grew)
	}
	rec.Close(context.Background())
	waitUntil(t, func() bool { return runtime.NumGoroutine() <= before })
}
