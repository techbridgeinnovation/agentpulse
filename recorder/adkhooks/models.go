package adkhooks

import (
	"time"

	"google.golang.org/adk/agent"
	adkmodel "google.golang.org/adk/model"
)

// inFlight is shared by BeforeModel and AfterModel, which are built separately and have no other way to hand each other what they learned about a call.
var inFlight = newInFlightModels(maxInFlightCalls, inFlightCallTTL, time.Now)

// calledModel is what is known about one model call between the callback that starts it and the one that records it.
type calledModel struct {
	requested string
	served    string
	startedAt time.Time
}

// inFlightModels holds the model behind each model call, and when it began, until its final response is recorded.
//
// ADK drops the model from a streamed call's final response. Its stream aggregator assembles that response from the chunks and copies their content, usage and finish reason, but not ModelVersion, even though every chunk carried it. The final response is the only one recorded, so without this a streamed call is stored with no model — and metering prices a call by its model, so it is stored as having cost nothing.
//
// The framework reports no duration either, so the start is kept here for the same reason: a figure that exists at the first callback and nowhere after it.
type inFlightModels struct {
	*inFlightStore[calledModel]
}

func newInFlightModels(max int, ttl time.Duration, now func() time.Time) *inFlightModels {
	return &inFlightModels{newInFlightStore[calledModel](max, ttl, now)}
}

// callKey identifies one agent's model calls within one invocation. An agent makes its model calls one after another, so the key names the call currently in flight; the agent and branch keep a sub-agent's or a parallel branch's calls apart from the rest of the turn.
func callKey(ctx agent.ReadonlyContext) string {
	return ctx.InvocationID() + "\x00" + ctx.AgentName() + "\x00" + ctx.Branch()
}

// requested notes the model a call is about to ask for, and when it was asked for. It starts what is known about the call afresh, so nothing learned about the previous call carries over.
func (m *inFlightModels) requested(key, model string) {
	if model == "" {
		m.forget(key)
		return
	}
	m.put(key, calledModel{requested: model, startedAt: m.now()})
}

// served notes the model the provider reported on a streamed chunk.
func (m *inFlightModels) served(key, model string) {
	if model == "" {
		return
	}
	m.update(key, func(known calledModel) calledModel {
		known.served = model
		return known
	})
}

// take returns what is known about a call and forgets it, since its final response has arrived.
func (m *inFlightModels) take(key string) calledModel {
	known, _ := m.inFlightStore.take(key)
	return known
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

// millisSince is how long a call took, where the callback that started it ran.
//
// Zero when it did not, which is how a record says the duration is unknown rather than instant: an agent whose BeforeModel callback is not registered still records everything else about its calls.
func millisSince(started time.Time, now func() time.Time) int64 {
	if started.IsZero() {
		return 0
	}
	elapsed := now().Sub(started)
	if elapsed <= 0 {
		return 0
	}
	return elapsed.Milliseconds()
}
