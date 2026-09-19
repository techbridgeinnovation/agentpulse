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
// Which tenant the turn is for, and which unit of work inside it, are the two the framework cannot know. They are read from the callback context where the product put them, at its sign-in, and never asked for here — see recorder.WithWorkspace.
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
	"google.golang.org/genai"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/techbridgeinnovation/agentpulse/recorder"
	governancepb "github.com/techbridgeinnovation/agentpulse/recorder/pb/governance"
	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
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

// userOf returns the person the turn is for, and names them.
//
// A user set explicitly on the context wins, for the same reason a request does: it is what the product's own sign-in check put there, name and all. Otherwise the framework's user id is used, which is an identifier and nothing more.
func userOf(r *recorder.Recorder, ctx agent.ReadonlyContext) string {
	if user := recorder.UserFrom(ctx); user.ID != "" {
		r.NoteUserIn(ctx, user)
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
// Per call rather than per turn: a turn makes as many calls as the loop needs,
// and they all carry the same request id, so the rows answer both what the turn
// cost and what each call cost. Partial responses are skipped — in streaming
// mode every chunk arrives here, and counting them would multiply a turn's
// usage by however many chunks it happened to be split into.
//
// A chunk is still read for the model it reports, because the final response
// of a streamed call arrives without one (see inFlightModels).
func AfterModel(r *recorder.Recorder, opts Options) llmagent.AfterModelCallback {
	return func(ctx agent.CallbackContext, response *adkmodel.LLMResponse, callErr error) (*adkmodel.LLMResponse, error) {
		if response == nil {
			return nil, nil
		}
		key := callKey(ctx)
		if response.Partial {
			inFlight.served(key, response.ModelVersion)
			return nil, nil
		}
		known := inFlight.take(key)

		activity := &pb.Activity{
			Agent:           opts.Agent,
			Request:         requestOf(ctx),
			Session:         ctx.SessionID(),
			User:            userOf(r, ctx),
			CallerService:   opts.Service,
			CallerComponent: componentOf(ctx),
			Skill:           opts.Skill,
			Project:         recorder.ProjectFrom(ctx),
			Model:           modelOf(response, known),
			Provider:        opts.provider(),
			DurationMs:      millisSince(known.startedAt, time.Now),
			Status:          statusOf(response, callErr),
			ErrorCode:       errorCodeOf(response, callErr),
			OccurredAt:      timestamppb.Now(),
		}
		applyUsage(activity, response.UsageMetadata)

		r.RecordIn(ctx, activity)
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
// It also notes which model the call asks for, so a call whose provider reports
// no model at all is still recorded against the one requested.
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
		inFlight.requested(callKey(ctx), request.Model)

		labels := map[string]string{}
		if user := labelValue(userIDOf(ctx)); user != "" {
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
//
// The workspace and the project are asked about because a budget can be narrowed to either, and a call is only held to a budget that names the tenant it is for. A turn that names no workspace asks about the organisation's default, which is what the same call spends against.
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
		Parent:    organisation,
		Agent:     opts.Agent,
		Product:   opts.Product,
		User:      userIDOf(ctx),
		Workspace: recorder.WorkspaceName(organisation, recorder.WorkspaceFrom(ctx)),
		Project:   recorder.ProjectFrom(ctx),
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

// errorCodeOf names why a call did not succeed.
//
// The framework's own code is preferred, since it is the one the provider reported through it. A call that failed before there was a response has only the error, which is reduced the same way every other failure in this library is.
func errorCodeOf(response *adkmodel.LLMResponse, callErr error) string {
	if response.ErrorCode != "" {
		return response.ErrorCode
	}
	if callErr != nil {
		return recorder.ErrorCode(callErr)
	}
	if recorder.BlockedFinish(string(response.FinishReason)) {
		return string(response.FinishReason)
	}
	return ""
}

// statusOf reads how the call ended.
//
// Truncation is separated from failure because the two mean different things to
// a cost report: a truncated call did partial work and is charged for it, while
// a failed one may have been charged for work that produced nothing.
//
// A blocked finish reason is read last and counts as a failure. It is the one
// kind of failure that arrives on a perfectly successful response: safety
// blocks, refusals and malformed tool calls come back with no error and no
// content, and the prompt is billed all the same. Read only for the error,
// such a call is recorded as a cheap success — which is exactly what it is
// not. The limit reasons are checked first, so a call cut short by a token
// ceiling stays truncated rather than becoming a failure.
func statusOf(response *adkmodel.LLMResponse, callErr error) pb.Activity_Status {
	switch {
	case callErr != nil, response.ErrorCode != "":
		return pb.Activity_FAILED
	case response.Interrupted, response.FinishReason == genai.FinishReasonMaxTokens:
		return pb.Activity_TRUNCATED
	case recorder.BlockedFinish(string(response.FinishReason)):
		return pb.Activity_FAILED
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
