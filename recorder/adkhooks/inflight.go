package adkhooks

import (
	"slices"
	"sync"
	"time"
)

const (
	// maxInFlightCalls is the most calls one store holds at once.
	//
	// Fixed rather than configurable, for the same reason the recorder's running totals are: it is what makes the store incapable of growing without bound whatever an adopter does. An entry is a key and a little beside it, so a store at its cap is a fraction of a megabyte.
	maxInFlightCalls = 4096

	// inFlightCallTTL is how long a call is held without being heard from.
	//
	// Far longer than a streamed call or a tool call lasts, so a live call is never forgotten under it. Short enough that a call which never reached its second callback — cut short by another callback before it, or by a panic in the tool — is reclaimed while the process is still the same size.
	inFlightCallTTL = 15 * time.Minute

	// inFlightEvictionFraction is the share of a store dropped when it is full, as a divisor, so reaching the cap pays for one scan rather than one per call.
	inFlightEvictionFraction = 8
)

// inFlightStore holds what one callback learned until the callback that pairs with it runs.
//
// The two callbacks in a pair are built separately and are handed nothing by the framework that would let them recognise each other's work, so what the first knows reaches the second through here or not at all. Bounded in both directions — a cap on how many calls are held and a life on each — because a callback that never fires must cost a fixed amount of memory rather than a growing one.
type inFlightStore[T any] struct {
	max int
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	entries map[string]inFlightEntry[T]
}

type inFlightEntry[T any] struct {
	value     T
	touchedAt time.Time
}

func newInFlightStore[T any](max int, ttl time.Duration, now func() time.Time) *inFlightStore[T] {
	return &inFlightStore[T]{max: max, ttl: ttl, now: now, entries: make(map[string]inFlightEntry[T])}
}

// put writes what is known about a call, replacing anything known about it before.
func (s *inFlightStore[T]) put(key string, value T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store(key, value)
}

// update changes what is held for a call in one step, so two callbacks writing about the same call cannot read and write around each other.
func (s *inFlightStore[T]) update(key string, change func(T) T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store(key, change(s.entries[key].value))
}

// forget drops a call without reading it.
func (s *inFlightStore[T]) forget(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, key)
}

// take returns what is known about a call and forgets it, since the callback that pairs with it has now run. An entry older than the store's life is returned as though it were never there: it belongs to a call nothing is coming back for.
func (s *inFlightStore[T]) take(key string) (T, bool) {
	var zero T
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok {
		return zero, false
	}
	delete(s.entries, key)
	if !s.now().Before(entry.touchedAt.Add(s.ttl)) {
		return zero, false
	}
	return entry.value, true
}

// store writes an entry, making room first when a new call would take the store past its cap. Callers hold s.mu.
func (s *inFlightStore[T]) store(key string, value T) {
	now := s.now()
	if _, exists := s.entries[key]; !exists && len(s.entries) >= s.max {
		s.makeRoom(now)
	}
	s.entries[key] = inFlightEntry[T]{value: value, touchedAt: now}
}

// makeRoom drops expired calls, then the longest-quiet share of the rest if that was not enough. Callers hold s.mu.
func (s *inFlightStore[T]) makeRoom(now time.Time) {
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
func (s *inFlightStore[T]) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}
