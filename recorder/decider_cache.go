package recorder

import (
	"context"
	"sync"
	"time"

	governancepb "github.com/techbridgeinnovation/agentpulse/recorder/pb/governance"
)

var _ Decider = (*CachingDecider)(nil)

// decisionCacheKey is the full decision context a governance verdict was
// reached for. A cached response is only ever reused for another call with
// exactly this same (organisation, workspace, project, product, user, agent)
// tuple.
//
// The workspace and the project are part of it because a budget can be narrowed to either, so two calls that differ only in which tenant they are for are two questions with two answers. Leaving them out would let one tenant's verdict answer another's call for as long as the entry lived, which is the whole of what a workspace exists to prevent.
type decisionCacheKey struct {
	parent    string
	workspace string
	project   string
	product   string
	user      string
	agent     string
}

func decisionCacheKeyOf(parent, workspace, project, product, user, agent string) decisionCacheKey {
	return decisionCacheKey{parent: parent, workspace: workspace, project: project, product: product, user: user, agent: agent}
}

// decisionCacheKeyFor is the key a request's own decision context makes.
func decisionCacheKeyFor(req *governancepb.DecideRequest) decisionCacheKey {
	return decisionCacheKeyOf(req.GetParent(), req.GetWorkspace(), req.GetProject(), req.GetProduct(), req.GetUser(), req.GetAgent())
}

// cacheEntry is one cached verdict and when it stops being usable.
type cacheEntry struct {
	resp      *governancepb.DecideResponse
	expiresAt time.Time
}

// inflightCall lets concurrent misses for the same key share one call to the
// wrapped Decider instead of each dialling governance separately.
type inflightCall struct {
	done chan struct{}
	resp *governancepb.DecideResponse
	err  error
}

// CachingDecider wraps a Decider with a local cache of its verdicts, so a
// warm call answers from memory instead of asking governance again.
//
// Only successful verdicts are cached. A wrapped-Decider error is never
// cached and is returned exactly as the wrapped Decider produced it, so the
// existing fail-open handling in adk.BeforeModel — proceed, count a
// DecisionError — sees the same errors it always has.
//
// Get, Store and Invalidate are exported deliberately: a later streaming
// mechanism that lets governance push a changed or invalidated verdict into
// a running agent has exactly the operations it needs here, without this
// type changing shape.
type CachingDecider struct {
	inner Decider
	ttl   time.Duration
	now   func() time.Time

	mu      sync.Mutex
	entries map[decisionCacheKey]cacheEntry

	inflightMu sync.Mutex
	inflight   map[decisionCacheKey]*inflightCall
}

// NewCachingDecider wraps inner with a local cache of its verdicts.
//
// ttl is required and not defaulted: how long a governance verdict may be
// reused before it is asked for again is a policy choice this package does
// not make on a caller's behalf. ttl <= 0 makes every read a miss — safe,
// not silently a different cache policy.
func NewCachingDecider(inner Decider, ttl time.Duration) *CachingDecider {
	return newCachingDecider(inner, ttl, time.Now)
}

// newCachingDecider is NewCachingDecider with an injectable clock, so tests
// can control expiry without sleeping.
func newCachingDecider(inner Decider, ttl time.Duration, now func() time.Time) *CachingDecider {
	return &CachingDecider{
		inner:    inner,
		ttl:      ttl,
		now:      now,
		entries:  make(map[decisionCacheKey]cacheEntry),
		inflight: make(map[decisionCacheKey]*inflightCall),
	}
}

// Decide answers from the cache when a live verdict is cached for req's
// exact decision context, otherwise asks the wrapped Decider and caches a
// successful result.
func (c *CachingDecider) Decide(ctx context.Context, req *governancepb.DecideRequest) (*governancepb.DecideResponse, error) {
	if resp, ok := c.get(decisionCacheKeyFor(req)); ok {
		return resp, nil
	}
	return c.resolve(ctx, req)
}

// resolve asks the wrapped Decider for req's decision context, coalescing
// concurrent callers for the same key into one call.
//
// A caller waiting on someone else's in-flight call still respects its own
// ctx: it selects on that call finishing or its own ctx being done, rather
// than blocking unconditionally on another call's deadline.
func (c *CachingDecider) resolve(ctx context.Context, req *governancepb.DecideRequest) (*governancepb.DecideResponse, error) {
	key := decisionCacheKeyFor(req)

	c.inflightMu.Lock()
	if call, ok := c.inflight[key]; ok {
		c.inflightMu.Unlock()
		select {
		case <-call.done:
			return call.resp, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &inflightCall{done: make(chan struct{})}
	c.inflight[key] = call
	c.inflightMu.Unlock()

	resp, err := c.inner.Decide(ctx, req)
	if err == nil {
		c.store(key, resp)
	}
	call.resp, call.err = resp, err

	c.inflightMu.Lock()
	delete(c.inflight, key)
	c.inflightMu.Unlock()
	close(call.done)

	return resp, err
}

// Get reads a cached verdict for the given decision context, if one is
// cached and not expired.
//
// The decision context it reads is the one naming no workspace and no
// project, which is what an agent serving a single tenant asks with.
func (c *CachingDecider) Get(parent, product, user, agent string) (*governancepb.DecideResponse, bool) {
	return c.get(decisionCacheKeyOf(parent, "", "", product, user, agent))
}

func (c *CachingDecider) get(key decisionCacheKey) (*governancepb.DecideResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if !c.now().Before(entry.expiresAt) {
		delete(c.entries, key)
		return nil, false
	}
	return entry.resp, true
}

// Store caches resp for the given decision context, for this cache's
// configured ttl.
//
// A ttl <= 0 is a no-op rather than an entry that expires immediately: the
// two are equivalent in practice, but a real expiresAt of exactly now()
// depends on the clock ticking between Store and the next Get to be
// treated as expired, which is not guaranteed — an injected test clock in
// particular may not advance at all between the two calls.
//
// A nil resp is also a no-op: a cached "successful nil" would answer a
// later Get with a hit but nothing behind it — worse than the miss it
// would have been otherwise.
//
// Exported so a later streaming mechanism can seed or refresh a verdict
// governance pushed, without going through the wrapped Decider at all.
//
// The decision context it writes, like Get's, is the one naming no workspace
// and no project.
func (c *CachingDecider) Store(parent, product, user, agent string, resp *governancepb.DecideResponse) {
	c.store(decisionCacheKeyOf(parent, "", "", product, user, agent), resp)
}

func (c *CachingDecider) store(key decisionCacheKey, resp *governancepb.DecideResponse) {
	if c.ttl <= 0 || resp == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cacheEntry{resp: resp, expiresAt: c.now().Add(c.ttl)}
}

// Invalidate removes any cached verdict for the given decision context.
//
// Exported for the same reason as Store: a later streaming mechanism needs
// to drop a verdict governance has told this agent is no longer current.
//
// The decision context it drops, like Get's and Store's, is the one naming no
// workspace and no project. InvalidateScope is what reaches the rest.
func (c *CachingDecider) Invalidate(parent, product, user, agent string) {
	key := decisionCacheKeyOf(parent, "", "", product, user, agent)

	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// InvalidateScope removes every cached verdict matching the given scope,
// where an empty product, user or agent means "any" — mirroring
// governance's DecisionInvalidation semantics exactly: parent alone
// invalidates every cached decision for that organisation; parent and
// product invalidate every user and agent under that product; and so on.
// parent is always matched exactly.
//
// This exists because a scope-shaped invalidation from governance cannot
// be applied through Invalidate: governance knows which budget scope
// changed, but has no way to know which exact (parent, product, user,
// agent) tuples a given recorder happens to have cached — only this cache
// does.
//
// The workspace and the project a verdict was reached for are not filters here, so a scope clears every tenant's verdict within it rather than one tenant's. That errs towards asking governance again, which costs a call; the other way round would be keeping a verdict governance has said is stale, which costs money.
func (c *CachingDecider) InvalidateScope(parent, product, user, agent string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.entries {
		if key.parent != parent {
			continue
		}
		if product != "" && key.product != product {
			continue
		}
		if user != "" && key.user != user {
			continue
		}
		if agent != "" && key.agent != agent {
			continue
		}
		delete(c.entries, key)
	}
}
