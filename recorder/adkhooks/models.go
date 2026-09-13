package adkhooks

import (
	"slices"
	"sync"
	"time"

	"google.golang.org/adk/agent"
	adkmodel "google.golang.org/adk/model"
)

const (
	// maxInFlightCalls is the most model calls a model name is held for at once.
	//
	// Fixed rather than configurable, for the same reason the recorder's running totals are: it is what makes the store incapable of growing without bound whatever an adopter does. An entry is a key and two short strings, so the store at its cap is a fraction of a megabyte.
	maxInFlightCalls = 4096

	// inFlightCallTTL is how long a call is held without being heard from.
	//
	// Far longer than a streamed call lasts, so a live call is never forgotten under it. Short enough that a call which never reached the model — cut short by another before-model callback — is reclaimed while the process is still the same size.
	inFlightCallTTL = 15 * time.Minute

	// inFlightEvictionFraction is the share of the store dropped when it is full, as a divisor, so reaching the cap pays for one scan rather than one per call.
	inFlightEvictionFraction = 8
)

// inFlight is shared by BeforeModel and AfterModel, which are built separately and have no other way to hand each other what they learned about a call.
var inFlight = newInFlightModels(maxInFlightCalls, inFlightCallTTL, time.Now)

// calledModel is what is known about the model behind one call.
type calledModel struct {
	requested string
	served    string
	touchedAt time.Time
}

// inFlightModels holds the model behind each model call until its final response is recorded.
//
// ADK drops the model from a streamed call's final response. Its stream aggregator assembles that response from the chunks and copies their content, usage and finish reason, but not ModelVersion, even though every chunk carried it. The final response is the only one recorded, so without this a streamed call is stored with no model — and metering prices a call by its model, so it is stored as having cost nothing.
type inFlightModels struct {
	max int
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	entries map[string]calledModel
}

func newInFlightModels(max int, ttl time.Duration, now func() time.Time) *inFlightModels {
	return &inFlightModels{max: max, ttl: ttl, now: now, entries: make(map[string]calledModel)}
}

// callKey identifies one agent's model calls within one invocation. An agent makes its model calls one after another, so the key names the call currently in flight; the agent and branch keep a sub-agent's or a parallel branch's calls apart from the rest of the turn.
func callKey(ctx agent.ReadonlyContext) string {
	return ctx.InvocationID() + "\x00" + ctx.AgentName() + "\x00" + ctx.Branch()
}

// requested notes the model a call is about to ask for. It starts what is known about the call afresh, so nothing learned about the previous call carries over.
func (m *inFlightModels) requested(key, model string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if model == "" {
		delete(m.entries, key)
		return
	}
	m.store(key, calledModel{requested: model})
}

// served notes the model the provider reported on a streamed chunk.
func (m *inFlightModels) served(key, model string) {
	if model == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.entries[key]
	entry.served = model
	m.store(key, entry)
}

// take returns what is known about a call and forgets it, since its final response has arrived.
func (m *inFlightModels) take(key string) calledModel {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[key]
	if !ok {
		return calledModel{}
	}
	delete(m.entries, key)
	if !m.now().Before(entry.touchedAt.Add(m.ttl)) {
		return calledModel{}
	}
	return entry
}

// store writes an entry, making room first when a new call would take the store past its cap. Callers hold m.mu.
func (m *inFlightModels) store(key string, entry calledModel) {
	now := m.now()
	if _, exists := m.entries[key]; !exists && len(m.entries) >= m.max {
		m.makeRoom(now)
	}
	entry.touchedAt = now
	m.entries[key] = entry
}

// makeRoom drops expired calls, then the longest-quiet share of the rest if that was not enough. Callers hold m.mu.
func (m *inFlightModels) makeRoom(now time.Time) {
	for key, entry := range m.entries {
		if !now.Before(entry.touchedAt.Add(m.ttl)) {
			delete(m.entries, key)
		}
	}
	if len(m.entries) < m.max {
		return
	}

	keys := make([]string, 0, len(m.entries))
	for key := range m.entries {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b string) int {
		return m.entries[a].touchedAt.Compare(m.entries[b].touchedAt)
	})
	drop := max(1, len(keys)/inFlightEvictionFraction)
	for _, key := range keys[:drop] {
		delete(m.entries, key)
	}
}

// size reports how many calls are held, for tests.
func (m *inFlightModels) size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

// modelOf names the model a final response is recorded against.
//
// A model the response itself reports is never replaced. Otherwise the model the provider reported while streaming is preferred over the one the agent asked for, since it is what was actually served and billed.
func modelOf(response *adkmodel.LLMResponse, known calledModel) string {
	switch {
	case response.ModelVersion != "":
		return response.ModelVersion
	case known.served != "":
		return known.served
	default:
		return known.requested
	}
}
