package adkv2hooks

import (
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// inFlight is shared by BeforeModel and AfterModel, which are built separately and have no other way to hand each other what they learned about a call.
var inFlight = newInFlightModels(maxInFlightCalls, inFlightCallTTL, time.Now)

// calledModel is what is known about one model call between the callback that starts it and the one that records it.
type calledModel struct {
	requested string
	served    string
	startedAt time.Time

	// usage is the latest count a streamed chunk reported, which is all a call that fails partway through leaves behind.
	usage *genai.GenerateContentResponseUsageMetadata

	// failed marks a call OnModelError has already recorded, so a response another error callback substitutes for it is not recorded again by AfterModel.
	failed bool
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

// requested notes the model a call is about to ask for, and when it was asked for. It starts what is known about the call afresh, so nothing learned about the previous call carries over.
func (m *inFlightModels) requested(key, model string) {
	m.put(key, calledModel{requested: model, startedAt: m.now()})
}

// chunk notes what a streamed chunk reported: the model that served it, and the usage so far.
func (m *inFlightModels) chunk(key, model string, usage *genai.GenerateContentResponseUsageMetadata) {
	if model == "" && usage == nil {
		return
	}
	m.update(key, func(known calledModel) calledModel {
		if model != "" {
			known.served = model
		}
		if usage != nil {
			known.usage = usage
		}
		return known
	})
}

// fail returns what is known about a call that has just failed, and leaves in its place only the mark that it was recorded.
func (m *inFlightModels) fail(key string) calledModel {
	var known calledModel
	m.update(key, func(held calledModel) calledModel {
		known = held
		return calledModel{failed: true}
	})
	return known
}

// take returns what is known about a call and forgets it, since its final response has arrived.
func (m *inFlightModels) take(key string) calledModel {
	known, _ := m.inFlightStore.take(key)
	return known
}

// modelOf names the model a final response is recorded against.
//
// A model the response itself reports is never replaced. Otherwise the model the provider reported while streaming is preferred over the one the agent asked for, since it is what was actually served and billed. The model the agent is configured with stands in last, for an agent whose BeforeModel callback is not registered, because a record with no model prices against nothing.
func modelOf(response *adkmodel.LLMResponse, known calledModel, configured string) string {
	switch {
	case response.ModelVersion != "":
		return response.ModelVersion
	case known.served != "":
		return known.served
	case known.requested != "":
		return known.requested
	default:
		return configured
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
