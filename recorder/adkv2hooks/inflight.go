package adkv2hooks

import (
	"slices"
	"sync"
	"time"

	"google.golang.org/adk/v2/agent"
)

const (
	// maxInFlightDowngrades is the most calls this store holds at once.
	//
	// Fixed rather than configurable, so the store is incapable of growing
	// without bound whatever an adopter does. An entry is a key and a model
	// name beside it, so a store at its cap is a fraction of a megabyte.
	maxInFlightDowngrades = 4096

	// inFlightDowngradeTTL is how long a call is held without being heard
	// from. Far longer than a streamed call lasts, so a live call is never
	// forgotten under it; short enough that a call whose AfterModel never
	// runs — cut short by another callback, or by a panic — is reclaimed
	// while the process is still the same size.
	inFlightDowngradeTTL = 15 * time.Minute

	// inFlightEvictionFraction is the share of the store dropped when it is
	// full, as a divisor, so reaching the cap pays for one scan rather than
	// one per call.
	inFlightEvictionFraction = 8
)

// inFlightDowngrades holds the model BeforeModel actually sent for a call —
// after a downgrade was applied — until AfterModel reads it back.
//
// BeforeModel and AfterModel are built separately and the framework hands
// neither anything that would let them recognise each other's work, so what
// BeforeModel learned reaches AfterModel through here or not at all — the
// same problem adkhooks solves with its own inFlightModels, ported here in
// the narrower shape this package actually needs: modelOf already has a
// static fallback (opts.Model) for the ordinary case, so this only ever
// needs to hold the one exception a downgrade introduces.
//
// Written to only when a downgrade was actually applied: a call BeforeModel
// never downgraded has no entry here, and AfterModel's existing fallback to
// opts.Model covers it exactly as it always has.
var inFlightDowngrades = newInFlightDowngradeStore(maxInFlightDowngrades, inFlightDowngradeTTL, time.Now)

type inFlightDowngradeEntry struct {
	model     string
	touchedAt time.Time
}

type inFlightDowngradeStore struct {
	max int
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	entries map[string]inFlightDowngradeEntry
}

func newInFlightDowngradeStore(max int, ttl time.Duration, now func() time.Time) *inFlightDowngradeStore {
	return &inFlightDowngradeStore{max: max, ttl: ttl, now: now, entries: make(map[string]inFlightDowngradeEntry)}
}

// put records the model applied for key, replacing anything held for it before.
func (s *inFlightDowngradeStore) put(key, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if _, exists := s.entries[key]; !exists && len(s.entries) >= s.max {
		s.makeRoom(now)
	}
	s.entries[key] = inFlightDowngradeEntry{model: model, touchedAt: now}
}

// take returns the model applied for key, if any, and forgets it. An entry
// older than ttl is returned as though it were never there: it belongs to a
// call nothing is coming back for.
func (s *inFlightDowngradeStore) take(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok {
		return "", false
	}
	delete(s.entries, key)
	if !s.now().Before(entry.touchedAt.Add(s.ttl)) {
		return "", false
	}
	return entry.model, true
}

// makeRoom drops expired calls, then the longest-quiet share of the rest if
// that was not enough. Callers hold s.mu.
func (s *inFlightDowngradeStore) makeRoom(now time.Time) {
	for key, entry := range s.entries {
		if !now.Before(entry.touchedAt.Add(s.ttl)) {
			delete(s.entries, key)
		}
	}
	if len(s.entries) < s.max {
		return
	}

	keys := make([]string, 0, len(s.entries))
	for key := range s.entries {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b string) int {
		return s.entries[a].touchedAt.Compare(s.entries[b].touchedAt)
	})
	drop := max(1, len(keys)/inFlightEvictionFraction)
	for _, key := range keys[:drop] {
		delete(s.entries, key)
	}
}

// size reports how many calls are held, for tests.
func (s *inFlightDowngradeStore) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// callKey identifies one agent's model calls within one invocation. An agent
// makes its model calls one after another, so the key names the call
// currently in flight; the agent and branch keep a sub-agent's or a parallel
// branch's calls apart from the rest of the turn. Mirrors adkhooks' own
// callKey exactly, for the same reason.
func callKey(ctx agent.ReadonlyContext) string {
	return ctx.InvocationID() + "\x00" + ctx.AgentName() + "\x00" + ctx.Branch()
}
