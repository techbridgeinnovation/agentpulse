package recorder

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	governancepb "github.com/techbridgeinnovation/agentpulse/recorder/pb/governance"
)

// fakeClock is a manually advanced clock, so cache-expiry tests never sleep.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeDecider stands in for the wrapped Decider, counting calls and
// optionally blocking until the test releases it — used to exercise
// in-flight coalescing deterministically.
type fakeDecider struct {
	mu    sync.Mutex
	calls int

	resp *governancepb.DecideResponse
	err  error

	// block, when non-nil, is waited on before returning — letting a test
	// hold a call open while other goroutines pile up behind it.
	block chan struct{}
}

func (f *fakeDecider) Decide(ctx context.Context, _ *governancepb.DecideRequest) (*governancepb.DecideResponse, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.resp, f.err
}

func (f *fakeDecider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func testRequest() *governancepb.DecideRequest {
	return &governancepb.DecideRequest{
		Parent:  "organisations/techbridge",
		Product: "rezco",
		User:    "users/jane",
		Agent:   "organisations/techbridge/agents/atlas",
	}
}

func TestCachingDeciderCacheHitAvoidsWrappedCall(t *testing.T) {
	inner := &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_ALLOW}}
	clock := newFakeClock(time.Now())
	cache := newCachingDecider(inner, time.Minute, clock.Now)
	req := testRequest()

	first, err := cache.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("first Decide: %v", err)
	}
	second, err := cache.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("second Decide: %v", err)
	}

	if inner.callCount() != 1 {
		t.Fatalf("wrapped Decider called %d times, want 1", inner.callCount())
	}
	if first != second {
		t.Fatalf("cache hit returned a different response pointer than the original call")
	}
}

func TestCachingDeciderExpiryCallsWrappedAgain(t *testing.T) {
	inner := &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_ALLOW}}
	clock := newFakeClock(time.Now())
	cache := newCachingDecider(inner, time.Minute, clock.Now)
	req := testRequest()

	if _, err := cache.Decide(context.Background(), req); err != nil {
		t.Fatalf("first Decide: %v", err)
	}
	clock.Advance(time.Minute + time.Second)
	if _, err := cache.Decide(context.Background(), req); err != nil {
		t.Fatalf("second Decide: %v", err)
	}

	if inner.callCount() != 2 {
		t.Fatalf("wrapped Decider called %d times after expiry, want 2", inner.callCount())
	}
}

func TestCachingDeciderExpiresExactlyAtTTL(t *testing.T) {
	inner := &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_ALLOW}}
	clock := newFakeClock(time.Now())
	cache := newCachingDecider(inner, time.Minute, clock.Now)
	req := testRequest()

	if _, err := cache.Decide(context.Background(), req); err != nil {
		t.Fatalf("first Decide: %v", err)
	}
	// Advance by exactly the TTL, not past it: now == expiresAt must count
	// as expired, not as one instant still inside the window.
	clock.Advance(time.Minute)
	if _, err := cache.Decide(context.Background(), req); err != nil {
		t.Fatalf("second Decide: %v", err)
	}

	if inner.callCount() != 2 {
		t.Fatalf("wrapped Decider called %d times when clock advanced exactly one ttl, want 2", inner.callCount())
	}
}

func TestCachingDeciderDoesNotCacheNilResponse(t *testing.T) {
	inner := &fakeDecider{resp: nil}
	clock := newFakeClock(time.Now())
	cache := newCachingDecider(inner, time.Minute, clock.Now)
	req := testRequest()

	first, err := cache.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("first Decide: %v", err)
	}
	if first != nil {
		t.Fatalf("first Decide returned %v, want nil", first)
	}
	second, err := cache.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("second Decide: %v", err)
	}
	if second != nil {
		t.Fatalf("second Decide returned %v, want nil", second)
	}

	if inner.callCount() != 2 {
		t.Fatalf("wrapped Decider called %d times across two nil responses, want 2 (nothing cached)", inner.callCount())
	}
}

func TestCachingDeciderDifferentKeysDoNotShareCache(t *testing.T) {
	inner := &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_ALLOW}}
	clock := newFakeClock(time.Now())
	cache := newCachingDecider(inner, time.Minute, clock.Now)

	req1 := testRequest()
	req2 := testRequest()
	req2.User = "users/moses"

	if _, err := cache.Decide(context.Background(), req1); err != nil {
		t.Fatalf("Decide req1: %v", err)
	}
	if _, err := cache.Decide(context.Background(), req2); err != nil {
		t.Fatalf("Decide req2: %v", err)
	}

	if inner.callCount() != 2 {
		t.Fatalf("wrapped Decider called %d times for two distinct keys, want 2", inner.callCount())
	}
}

func TestCachingDeciderDoesNotCacheErrors(t *testing.T) {
	inner := &fakeDecider{err: status.Error(codes.Unavailable, "governance unreachable")}
	clock := newFakeClock(time.Now())
	cache := newCachingDecider(inner, time.Minute, clock.Now)
	req := testRequest()

	_, err1 := cache.Decide(context.Background(), req)
	_, err2 := cache.Decide(context.Background(), req)

	if status.Code(err1) != codes.Unavailable || status.Code(err2) != codes.Unavailable {
		t.Fatalf("errors = %v, %v, want both Unavailable", err1, err2)
	}
	if inner.callCount() != 2 {
		t.Fatalf("wrapped Decider called %d times across two errors, want 2 (nothing cached)", inner.callCount())
	}
}

func TestCachingDeciderZeroTTLAlwaysMisses(t *testing.T) {
	inner := &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_ALLOW}}
	clock := newFakeClock(time.Now())
	cache := newCachingDecider(inner, 0, clock.Now)
	req := testRequest()

	if _, err := cache.Decide(context.Background(), req); err != nil {
		t.Fatalf("first Decide: %v", err)
	}
	if _, err := cache.Decide(context.Background(), req); err != nil {
		t.Fatalf("second Decide: %v", err)
	}

	if inner.callCount() != 2 {
		t.Fatalf("wrapped Decider called %d times with ttl<=0, want 2 (caching disabled)", inner.callCount())
	}
}

func TestCachingDeciderInvalidateRemovesCachedVerdict(t *testing.T) {
	inner := &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_ALLOW}}
	clock := newFakeClock(time.Now())
	cache := newCachingDecider(inner, time.Minute, clock.Now)
	req := testRequest()

	if _, err := cache.Decide(context.Background(), req); err != nil {
		t.Fatalf("first Decide: %v", err)
	}
	cache.Invalidate(req.GetParent(), req.GetProduct(), req.GetUser(), req.GetAgent())
	if _, err := cache.Decide(context.Background(), req); err != nil {
		t.Fatalf("second Decide: %v", err)
	}

	if inner.callCount() != 2 {
		t.Fatalf("wrapped Decider called %d times after Invalidate, want 2", inner.callCount())
	}
}

// seedInvalidateScopeCache stores one verdict for each of several distinct
// decision contexts under the same organisation, so a test can invalidate a
// scope and check exactly which of them survive.
func seedInvalidateScopeCache(t *testing.T) *CachingDecider {
	t.Helper()
	cache := newCachingDecider(&fakeDecider{}, time.Minute, time.Now)
	resp := &governancepb.DecideResponse{Decision: governancepb.DecideResponse_ALLOW}

	cache.Store("organisations/techbridge", "rezco", "users/jane", "organisations/techbridge/agents/atlas", resp)
	cache.Store("organisations/techbridge", "rezco", "users/moses", "organisations/techbridge/agents/atlas", resp)
	cache.Store("organisations/techbridge", "dealade", "users/jane", "organisations/techbridge/agents/russell", resp)
	cache.Store("organisations/other", "rezco", "users/jane", "organisations/techbridge/agents/atlas", resp)
	return cache
}

func assertCached(t *testing.T, cache *CachingDecider, wantCached bool, parent, product, user, agent string) {
	t.Helper()
	_, ok := cache.Get(parent, product, user, agent)
	if ok != wantCached {
		t.Fatalf("Get(%q, %q, %q, %q) cached = %v, want %v", parent, product, user, agent, ok, wantCached)
	}
}

func TestInvalidateScopeOrganisationWideRemovesEveryEntryForThatOrganisation(t *testing.T) {
	cache := seedInvalidateScopeCache(t)

	cache.InvalidateScope("organisations/techbridge", "", "", "")

	assertCached(t, cache, false, "organisations/techbridge", "rezco", "users/jane", "organisations/techbridge/agents/atlas")
	assertCached(t, cache, false, "organisations/techbridge", "rezco", "users/moses", "organisations/techbridge/agents/atlas")
	assertCached(t, cache, false, "organisations/techbridge", "dealade", "users/jane", "organisations/techbridge/agents/russell")
	// A different organisation is untouched by a scope naming another one.
	assertCached(t, cache, true, "organisations/other", "rezco", "users/jane", "organisations/techbridge/agents/atlas")
}

func TestInvalidateScopeByProductRemovesOnlyThatProduct(t *testing.T) {
	cache := seedInvalidateScopeCache(t)

	cache.InvalidateScope("organisations/techbridge", "rezco", "", "")

	assertCached(t, cache, false, "organisations/techbridge", "rezco", "users/jane", "organisations/techbridge/agents/atlas")
	assertCached(t, cache, false, "organisations/techbridge", "rezco", "users/moses", "organisations/techbridge/agents/atlas")
	// A different product under the same organisation is unrelated and stays cached.
	assertCached(t, cache, true, "organisations/techbridge", "dealade", "users/jane", "organisations/techbridge/agents/russell")
	assertCached(t, cache, true, "organisations/other", "rezco", "users/jane", "organisations/techbridge/agents/atlas")
}

func TestInvalidateScopeByProductAndUserRemovesOnlyThatOne(t *testing.T) {
	cache := seedInvalidateScopeCache(t)

	cache.InvalidateScope("organisations/techbridge", "rezco", "users/jane", "")

	assertCached(t, cache, false, "organisations/techbridge", "rezco", "users/jane", "organisations/techbridge/agents/atlas")
	// Same organisation and product, different user: unrelated, stays cached.
	assertCached(t, cache, true, "organisations/techbridge", "rezco", "users/moses", "organisations/techbridge/agents/atlas")
	assertCached(t, cache, true, "organisations/techbridge", "dealade", "users/jane", "organisations/techbridge/agents/russell")
}

func TestInvalidateScopeByAgentRemovesOnlyThatAgent(t *testing.T) {
	cache := seedInvalidateScopeCache(t)

	cache.InvalidateScope("organisations/techbridge", "", "", "organisations/techbridge/agents/russell")

	assertCached(t, cache, false, "organisations/techbridge", "dealade", "users/jane", "organisations/techbridge/agents/russell")
	// A different agent under the same organisation is unrelated and stays cached.
	assertCached(t, cache, true, "organisations/techbridge", "rezco", "users/jane", "organisations/techbridge/agents/atlas")
	assertCached(t, cache, true, "organisations/techbridge", "rezco", "users/moses", "organisations/techbridge/agents/atlas")
}

func TestCachingDeciderGetAndStore(t *testing.T) {
	inner := &fakeDecider{}
	clock := newFakeClock(time.Now())
	cache := newCachingDecider(inner, time.Minute, clock.Now)

	if _, ok := cache.Get("organisations/techbridge", "rezco", "users/jane", "organisations/techbridge/agents/atlas"); ok {
		t.Fatalf("Get on an empty cache reported a hit")
	}

	seeded := &governancepb.DecideResponse{Decision: governancepb.DecideResponse_NOTIFY, Reason: "pushed by governance"}
	cache.Store("organisations/techbridge", "rezco", "users/jane", "organisations/techbridge/agents/atlas", seeded)

	got, ok := cache.Get("organisations/techbridge", "rezco", "users/jane", "organisations/techbridge/agents/atlas")
	if !ok {
		t.Fatalf("Get did not find the entry Store just wrote")
	}
	if got != seeded {
		t.Fatalf("Get returned a different response than Store wrote")
	}
	if inner.callCount() != 0 {
		t.Fatalf("Store/Get called the wrapped Decider %d times, want 0", inner.callCount())
	}
}

func TestCachingDeciderConcurrentMissesCoalesce(t *testing.T) {
	inner := &fakeDecider{
		resp:  &governancepb.DecideResponse{Decision: governancepb.DecideResponse_ALLOW},
		block: make(chan struct{}),
	}
	clock := newFakeClock(time.Now())
	cache := newCachingDecider(inner, time.Minute, clock.Now)
	req := testRequest()

	const callers = 5
	results := make(chan *governancepb.DecideResponse, callers)
	errs := make(chan error, callers)
	var ready sync.WaitGroup
	ready.Add(callers)

	for i := 0; i < callers; i++ {
		go func() {
			ready.Done()
			resp, err := cache.Decide(context.Background(), req)
			results <- resp
			errs <- err
		}()
	}

	// Let every goroutine reach Decide before releasing the one call that
	// is actually in flight, so this genuinely tests coalescing rather
	// than a race that happens not to overlap.
	ready.Wait()
	time.Sleep(20 * time.Millisecond)
	close(inner.block)

	for i := 0; i < callers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		if resp := <-results; resp.GetDecision() != governancepb.DecideResponse_ALLOW {
			t.Fatalf("caller %d decision = %v, want ALLOW", i, resp.GetDecision())
		}
	}

	if inner.callCount() != 1 {
		t.Fatalf("wrapped Decider called %d times for %d concurrent callers on the same key, want 1", inner.callCount(), callers)
	}
}

// A budget can be narrowed to a workspace or a project, so two calls that
// differ only in which tenant they are for are two questions. Reusing one
// answer for the other is one tenant's spend decided by another's budget.
func TestACachedVerdictIsNotReusedForAnotherWorkspaceOrProject(t *testing.T) {
	inner := &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_ALLOW}}
	clock := newFakeClock(time.Now())
	cache := newCachingDecider(inner, time.Minute, clock.Now)

	acme := testRequest()
	acme.Workspace = "organisations/techbridge/workspaces/acme"

	globex := testRequest()
	globex.Workspace = "organisations/techbridge/workspaces/globex"

	matter := testRequest()
	matter.Workspace = acme.Workspace
	matter.Project = "matter-1183"

	for _, req := range []*governancepb.DecideRequest{acme, globex, matter, acme} {
		if _, err := cache.Decide(context.Background(), req); err != nil {
			t.Fatalf("Decide: %v", err)
		}
	}

	if inner.callCount() != 3 {
		t.Fatalf("wrapped Decider called %d times, want one per distinct tenant and project, and none for the repeat", inner.callCount())
	}
}

// A scope from governance clears every tenant's verdict within it. That costs a
// call to ask again; keeping one would cost money.
func TestInvalidateScopeClearsAWorkspacesVerdictToo(t *testing.T) {
	inner := &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_ALLOW}}
	cache := newCachingDecider(inner, time.Minute, time.Now)

	req := testRequest()
	req.Workspace = "organisations/techbridge/workspaces/acme"
	if _, err := cache.Decide(context.Background(), req); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	cache.InvalidateScope("organisations/techbridge", "", "", "")

	if _, err := cache.Decide(context.Background(), req); err != nil {
		t.Fatalf("Decide after InvalidateScope: %v", err)
	}
	if inner.callCount() != 2 {
		t.Fatalf("wrapped Decider called %d times after an organisation-wide invalidation, want 2", inner.callCount())
	}
}
