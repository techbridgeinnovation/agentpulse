// Package adkv2hooks wires the recorder into an agent on the second major version of Google's ADK.
//
// It sits beside adkhooks rather than replacing it because the two majors are different libraries as far as Go is concerned: they have different import paths and their types are unrelated, so one package cannot serve both. Agents already in production are on the first, and the platform's own agent blocks scaffold onto the second, so both are live at once and the library has to reach both.
//
// The four callbacks and everything they read are the same. The one difference the port turns on is that the first major hands a model callback a CallbackContext and a tool callback a ToolContext, while this one hands them all a single Context. Every field the record needs is on it either way.
//
// Kept as its own package for the same reason adkhooks is: service code that uses no framework can take the recorder without taking a framework with it.
package adkv2hooks

import (
	"fmt"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
	"github.com/techbridgeinnovation/agentpulse/recorder"
)

// Options is what the framework cannot tell us.
type Options struct {
	// Agent is the registered agent these records belong to.
	// Format: organisations/{organisation}/agents/{agent}
	Agent string

	// Service names the service the agent runs in, e.g. "pulseagent-v1".
	Service string

	// Provider is who bills for the model calls. Vertex unless an agent has been pointed at a provider directly.
	Provider pb.Activity_Provider

	// Model is the model this agent is configured to call, and stands in when the framework does not say which model answered.
	//
	// A streamed answer arrives as pieces which the framework assembles into one summary, and that summary carries the token counts but not the name of the model that produced them. Both majors do this. Pricing looks a rate up by model, so a record with no model prices against nothing — accurate counts attached to a cost of zero.
	Model string

	// Skill is the area of the product the user is in, where the product wants cost sliced that way. Optional, and set once here rather than per call because the framework cannot infer a product concept.
	Skill string
}

func (o Options) provider() pb.Activity_Provider {
	if o.Provider == pb.Activity_PROVIDER_UNSPECIFIED {
		return pb.Activity_VERTEX_AI
	}
	return o.Provider
}

// requestOf returns the id that groups everything done for one end-user request.
//
// A request set explicitly on the context wins, so a delegating agent keeps the caller's request across the handoff. Otherwise the framework's own invocation id is used, which is stable for the length of one turn.
func requestOf(ctx agent.ReadonlyContext) string {
	if request := recorder.RequestFrom(ctx); request != "" {
		return request
	}
	return ctx.InvocationID()
}

// userOf returns the person the turn is for, and names them.
//
// A user set explicitly on the context wins, for the same reason a request does: it is what the product's own sign-in check put there, name and all. Otherwise the framework's user id is used, which is an identifier and nothing more.
func userOf(r *recorder.Recorder, ctx agent.ReadonlyContext) string {
	if user := recorder.UserFrom(ctx); user.ID != "" {
		r.NoteUser(user)
		return user.ID
	}
	return ctx.UserID()
}

// userIDOf is userOf for the places that label or ask rather than record, where naming someone would be a side effect of the wrong call.
func userIDOf(ctx agent.ReadonlyContext) string {
	if user := recorder.UserFrom(ctx); user.ID != "" {
		return user.ID
	}
	return ctx.UserID()
}

// AfterModel records one activity per model call.
//
// Per call rather than per turn: a turn makes as many calls as the loop needs, and they all carry the same request id, so the rows answer both what the turn cost and what each call cost. Partial responses are skipped — in streaming mode every chunk arrives here, and counting them would multiply a turn's usage by however many chunks it happened to be split into.
func AfterModel(r *recorder.Recorder, opts Options) llmagent.AfterModelCallback {
	return func(ctx agent.Context, response *adkmodel.LLMResponse, callErr error) (*adkmodel.LLMResponse, error) {
		if response == nil || response.Partial {
			return nil, nil
		}

		activity := &pb.Activity{
			Agent:           opts.Agent,
			Request:         requestOf(ctx),
			Session:         ctx.SessionID(),
			User:            userOf(r, ctx),
			CallerService:   opts.Service,
			CallerComponent: componentOf(ctx),
			Skill:           opts.Skill,
			Model:           modelOf(response, opts),
			Provider:        opts.provider(),
			Status:          statusOf(response, callErr),
			ErrorCode:       response.ErrorCode,
			OccurredAt:      timestamppb.Now(),
		}
		applyUsage(activity, response.UsageMetadata)

		r.Record(activity)
		return nil, nil
	}
}

// AfterTool records one activity per tool call.
//
// A tool call is a priced unit of work in its own right: a search or a lookup is charged per request, not per token, and those charges are invisible to anything that only counts tokens. Recorded on completion, so a tool that never ran is not counted.
func AfterTool(r *recorder.Recorder, opts Options) llmagent.AfterToolCallback {
	return func(ctx agent.Context, t tool.Tool, _, _ map[string]any, callErr error) (map[string]any, error) {
		name := ""
		if t != nil {
			name = t.Name()
		}

		status := pb.Activity_OK
		if callErr != nil {
			status = pb.Activity_FAILED
		}

		r.Record(&pb.Activity{
			Agent:           opts.Agent,
			Request:         requestOf(ctx),
			Session:         ctx.SessionID(),
			User:            userOf(r, ctx),
			CallerService:   opts.Service,
			CallerComponent: "tool:" + name,
			Skill:           opts.Skill,
			Provider:        opts.provider(),
			Status:          status,
			OccurredAt:      timestamppb.Now(),
		})
		return nil, nil
	}
}

// AfterAgent releases the running total for the request the turn belonged to.
//
// The turn is over, so the figure is memory nothing will read again. Not calling it does not leak, since an entry expires on its own and the map has a hard cap, but the entry then sits there long after the request it belonged to is gone.
//
// Registered on the agent that owns the turn, which is where a request ends. On a sub-agent it fires when that sub-agent finishes, which is while the turn it was delegated from is still running, and the total for the rest of that turn then starts again from nothing.
func AfterAgent(r *recorder.Recorder, _ Options) agent.AfterAgentCallback {
	return func(ctx agent.Context) (*genai.Content, error) {
		r.FinishRequest(requestOf(ctx))
		return nil, nil
	}
}

// BeforeModel labels the outgoing request with who it belongs to.
//
// Labels set here are carried into the cloud billing export, which is what makes a charge that is not measured in tokens attributable at all. Only opaque identifiers go on a label — never an email — so nothing reaches billing that would be PII.
func BeforeModel(_ *recorder.Recorder, opts Options) llmagent.BeforeModelCallback {
	return func(ctx agent.Context, request *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
		if request == nil {
			return nil, nil
		}

		labels := map[string]string{}
		if user := labelValue(userIDOf(ctx)); user != "" {
			labels["ap_user"] = user
		}
		if component := labelValue(componentOf(ctx)); component != "" {
			labels["ap_component"] = component
		}
		if len(labels) == 0 {
			return nil, nil
		}

		if request.Config == nil {
			request.Config = &genai.GenerateContentConfig{}
		}
		if request.Config.Labels == nil {
			request.Config.Labels = make(map[string]string, len(labels))
		}
		// Additive, so a label set closer to the call site is never replaced.
		for key, value := range labels {
			if _, exists := request.Config.Labels[key]; !exists {
				request.Config.Labels[key] = value
			}
		}
		return nil, nil
	}
}

// componentOf derives which part of the calling code spent the money.
//
// Taken from the framework's own agent name, so adopting teams set nothing. A delegating run reports the sub-agent that actually made the call rather than the one that started the turn.
func componentOf(ctx agent.ReadonlyContext) string {
	if name := ctx.AgentName(); name != "" {
		return name
	}
	return "model_call"
}

// modelOf says which model answered.
//
// The framework reports it on a response it received whole, and does not report it on one it assembled from a stream, so the configured name stands in there. Recording no model would leave the counts unpriceable, since a rate is looked up by model.
func modelOf(response *adkmodel.LLMResponse, opts Options) string {
	if reported := response.ModelVersion; reported != "" {
		return reported
	}
	return opts.Model
}

// statusOf reads how the call ended.
//
// Truncation is separated from failure because the two mean different things to a cost report: a truncated call did partial work and is charged for it, while a failed one may have been charged for work that produced nothing.
func statusOf(response *adkmodel.LLMResponse, callErr error) pb.Activity_Status {
	switch {
	case callErr != nil, response.ErrorCode != "":
		return pb.Activity_FAILED
	case response.Interrupted, response.FinishReason == genai.FinishReasonMaxTokens:
		return pb.Activity_TRUNCATED
	default:
		return pb.Activity_OK
	}
}

// applyUsage copies the token counts the provider reported.
//
// Each kind is kept apart because each is billed at its own rate. Thoughts are reasoning tokens, which the existing implementations do not capture at all — on a reasoning model that is a silent undercount.
func applyUsage(activity *pb.Activity, usage *genai.GenerateContentResponseUsageMetadata) {
	if usage == nil {
		return
	}
	activity.PromptTokens = usage.PromptTokenCount
	activity.CandidateTokens = usage.CandidatesTokenCount
	activity.CachedTokens = usage.CachedContentTokenCount
	activity.ReasoningTokens = usage.ThoughtsTokenCount
	activity.TotalTokens = usage.TotalTokenCount
}

// labelValue coerces a value into what the billing export accepts: lowercase letters, digits, dashes and underscores, up to 63 characters.
//
// The whole value is sanitised rather than having a prefix stripped first. That keeps the label the sanitised form of exactly what was recorded, so a billing row and an activity row join. Doing it the other way is how one product ended up splitting one person's spend across two buckets.
func labelValue(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if len(out) > 63 {
		out = out[:63]
	}
	return out
}

// Describe reports how the recorder is wired, for a starter example and for a team checking their own setup.
func Describe(opts Options) string {
	return fmt.Sprintf("recording as agent %s in service %q, billed to %s",
		opts.Agent, opts.Service, opts.provider())
}
