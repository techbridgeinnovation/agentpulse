// Package adkhooks wires the recorder into a Google ADK agent.
//
// Named for what it holds rather than for the framework alone, because ADK is
// Google's and these are ours, and a package called adk sitting next to an
// import called adk reads as though we wrote the framework.
//
// Four callbacks and nothing at the call sites. Everything the record needs —
// which request, which user, which session, which agent, which model, how many
// tokens, whether it succeeded — is already on the callback context or the
// model response, so an adopting team adds the callbacks and changes no other
// code.
//
// Kept in its own package so that service code which does not use ADK can take
// the recorder without taking the framework with it. That code reports through
// the reporter in the parent package instead, because there are no callbacks to
// register where there is no framework.
package adkhooks

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"
	"google.golang.org/protobuf/types/known/timestamppb"

	governancepb "github.com/techbridgeinnovation/agentpulse/recorder/pb/governance"
	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
	"github.com/techbridgeinnovation/agentpulse/recorder"
)

// defaultDecideTimeout bounds how long BeforeModel waits for a decision
// before proceeding anyway. Short, because — unlike everything else this
// library does — a Decide call now sits on the model call's own critical
// path. This is a recorder-side bound chosen for that reason, not a
// governance service SLO.
const defaultDecideTimeout = 500 * time.Millisecond

// Options is what the framework cannot tell us.
type Options struct {
	// Agent is the registered agent these records belong to.
	// Format: organisations/{organisation}/agents/{agent}
	Agent string

	// Service names the service the agent runs in, e.g. "atlas-agent".
	Service string

	// Provider is who bills for the model calls. Vertex unless an agent has
	// been pointed at a provider directly.
	Provider pb.Activity_Provider

	// Skill is the area of the product the user is in, where the product wants
	// cost sliced that way. Optional, and set once here rather than per call
	// because the framework cannot infer a product concept.
	Skill string

	// Product identifies the product the agent belongs to, e.g. "rezco".
	// Supplied once by the adopting product, for the same reason as Skill:
	// the framework cannot infer a product concept. Required for Decide —
	// DecideRequest.product is a required field of that contract.
	Product string

	// Decider asks governance whether a call may proceed, before BeforeModel
	// lets it through. Optional: nil means governance is skipped entirely,
	// the same way an empty Sinks list means recording is skipped — this
	// integration adds no behavior for a team that has not configured one.
	Decider recorder.Decider

	// DecideTimeout bounds how long BeforeModel waits for a decision.
	// Defaults to defaultDecideTimeout when zero.
	DecideTimeout time.Duration

	// DecideCacheTTL, when positive, wraps Decider once in a
	// recorder.CachingDecider for the life of the callback BeforeModel
	// returns, so a repeated call for the same (organisation, product, user,
	// agent) context answers from memory instead of asking governance
	// again.
	//
	// Zero or negative disables caching: Decider is called directly on
	// every model call, exactly as if this field did not exist. There is no
	// default duration here — how long a governance verdict may be reused
	// before it is asked for again is a policy choice this library does not
	// make on a caller's behalf.
	DecideCacheTTL time.Duration
}

func (o Options) provider() pb.Activity_Provider {
	if o.Provider == pb.Activity_PROVIDER_UNSPECIFIED {
		return pb.Activity_VERTEX_AI
	}
	return o.Provider
}

func (o Options) decideTimeout() time.Duration {
	if o.DecideTimeout <= 0 {
		return defaultDecideTimeout
	}
	return o.DecideTimeout
}

// organisationOf returns the organisation an agent resource name belongs to,
// e.g. "organisations/acme" from "organisations/acme/agents/atlas".
//
// Derived from Options.Agent rather than kept as a second Options field:
// Agent is already the canonical resource name and already required, so a
// separate Organisation field would just be a second source of truth for
// the same identity that could drift from it.
func organisationOf(agentName string) (string, error) {
	const sep = "/agents/"
	i := strings.Index(agentName, sep)
	if i < 0 {
		return "", fmt.Errorf("%q is not an agent resource name", agentName)
	}
	return agentName[:i], nil
}

// requestOf returns the id that groups everything done for one end-user
// request.
//
// A request set explicitly on the context wins, so a delegating agent keeps the
// caller's request across the handoff. Otherwise the framework's own invocation
// id is used, which is stable for the length of one turn.
func requestOf(ctx agent.ReadonlyContext) string {
	if request := recorder.RequestFrom(ctx); request != "" {
		return request
	}
	return ctx.InvocationID()
}

// AfterModel records one activity per model call.
//
// Per call rather than per turn: a turn makes as many calls as the loop needs,
// and they all carry the same request id, so the rows answer both what the turn
// cost and what each call cost. Partial responses are skipped — in streaming
// mode every chunk arrives here, and counting them would multiply a turn's
// usage by however many chunks it happened to be split into.
func AfterModel(r *recorder.Recorder, opts Options) llmagent.AfterModelCallback {
	return func(ctx agent.CallbackContext, response *adkmodel.LLMResponse, callErr error) (*adkmodel.LLMResponse, error) {
		if response == nil || response.Partial {
			return nil, nil
		}

		activity := &pb.Activity{
			Agent:           opts.Agent,
			Request:         requestOf(ctx),
			Session:         ctx.SessionID(),
			User:            ctx.UserID(),
			CallerService:   opts.Service,
			CallerComponent: componentOf(ctx),
			Skill:           opts.Skill,
			Model:           response.ModelVersion,
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
// A tool call is a priced unit of work in its own right: a search or a lookup
// is charged per request, not per token, and those charges are invisible to
// anything that only counts tokens. Recorded on completion, so a tool that
// never ran is not counted.
func AfterTool(r *recorder.Recorder, opts Options) llmagent.AfterToolCallback {
	return func(ctx tool.Context, t tool.Tool, _, _ map[string]any, callErr error) (map[string]any, error) {
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
			User:            ctx.UserID(),
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
	return func(ctx agent.CallbackContext) (*genai.Content, error) {
		r.FinishRequest(requestOf(ctx))
		return nil, nil
	}
}

// BeforeModel labels the outgoing request with who it belongs to, then — if
// a Decider is configured — asks governance whether the call may proceed.
//
// Labels set here are carried into the cloud billing export, which is what
// makes a charge that is not measured in tokens attributable at all. Only
// opaque identifiers go on a label — never an email — so nothing reaches
// billing that would be PII.
//
// ALLOW and NOTIFY both let the call proceed in this version — this always
// returns (nil, nil), never preempting the model call. NOTIFY increments the
// Notified counter first; a Decide call that could not be completed
// increments DecisionErrors and proceeds the same way. This is this
// integration's behavior for as long as governance can only produce ALLOW
// or NOTIFY — it is not a settled fail-open policy for the DOWNGRADE/DENY
// verdicts that do not exist yet.
//
// If opts.DecideCacheTTL is positive, opts.Decider is wrapped in a
// recorder.CachingDecider exactly once, here, before the callback is
// returned — so the same cache is reused for every model call this agent
// makes for the rest of its process lifetime, the same way one Recorder is
// built once and reused.
func BeforeModel(r *recorder.Recorder, opts Options) llmagent.BeforeModelCallback {
	decider := opts.Decider
	if decider != nil && opts.DecideCacheTTL > 0 {
		decider = recorder.NewCachingDecider(decider, opts.DecideCacheTTL)
	}

	return func(ctx agent.CallbackContext, request *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
		if request == nil {
			return nil, nil
		}

		labels := map[string]string{}
		if user := labelValue(ctx.UserID()); user != "" {
			labels["ap_user"] = user
		}
		if component := labelValue(componentOf(ctx)); component != "" {
			labels["ap_component"] = component
		}
		if len(labels) > 0 {
			if request.Config == nil {
				request.Config = &genai.GenerateContentConfig{}
			}
			if request.Config.Labels == nil {
				request.Config.Labels = make(map[string]string, len(labels))
			}
			// Additive, so a label set closer to the call site is never
			// replaced.
			for key, value := range labels {
				if _, exists := request.Config.Labels[key]; !exists {
					request.Config.Labels[key] = value
				}
			}
		}

		decide(ctx, r, opts, decider)
		return nil, nil
	}
}

// decide asks governance whether ctx's call may proceed, using decider,
// when one is configured.
//
// decider is BeforeModel's precomputed value — opts.Decider itself, or
// opts.Decider wrapped in a cache — rather than opts.Decider read fresh
// here, so the same cache (and not a new one) is consulted on every call.
//
// Bounded by opts.decideTimeout() against ctx itself (not a detached
// context.Background()), so a cancelled turn cancels this call too rather
// than outliving it. A malformed opts.Agent, a Decide error, or a NOTIFY
// verdict are all handled without ever stopping the model call — see
// BeforeModel's own doc for why.
func decide(ctx agent.CallbackContext, r *recorder.Recorder, opts Options, decider recorder.Decider) {
	if decider == nil {
		return
	}

	organisation, err := organisationOf(opts.Agent)
	if err != nil {
		r.NoteDecisionError()
		return
	}

	dctx, cancel := context.WithTimeout(ctx, opts.decideTimeout())
	defer cancel()

	resp, err := decider.Decide(dctx, &governancepb.DecideRequest{
		Parent:  organisation,
		Agent:   opts.Agent,
		Product: opts.Product,
		User:    ctx.UserID(),
	})
	if err != nil {
		r.NoteDecisionError()
		return
	}

	if resp.GetDecision() == governancepb.DecideResponse_NOTIFY {
		r.NoteNotified()
	}
}

// componentOf derives which part of the calling code spent the money.
//
// Taken from the framework's own agent name, so adopting teams set nothing. A
// delegating run reports the sub-agent that actually made the call rather than
// the one that started the turn.
func componentOf(ctx agent.ReadonlyContext) string {
	if name := ctx.AgentName(); name != "" {
		return name
	}
	return "model_call"
}

// statusOf reads how the call ended.
//
// Truncation is separated from failure because the two mean different things to
// a cost report: a truncated call did partial work and is charged for it, while
// a failed one may have been charged for work that produced nothing.
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
// Each kind is kept apart because each is billed at its own rate. Thoughts are
// reasoning tokens, which the existing implementations do not capture at all —
// on a reasoning model that is a silent undercount.
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

// labelValue coerces a value into what the billing export accepts: lowercase
// letters, digits, dashes and underscores, up to 63 characters.
//
// The whole value is sanitised rather than having a prefix stripped first. That
// keeps the label the sanitised form of exactly what was recorded, so a billing
// row and an activity row join. Doing it the other way is how one product ended
// up splitting one person's spend across two buckets.
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

// Describe reports how the recorder is wired, for a starter example and for a
// team checking their own setup.
func Describe(opts Options) string {
	return fmt.Sprintf("recording as agent %s in service %q, billed to %s",
		opts.Agent, opts.Service, opts.provider())
}
