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

	governancepb "github.com/techbridgeinnovation/agentpulse/recorder/pb/governance"
	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
	"github.com/techbridgeinnovation/agentpulse/recorder"
)

// defaultDecideTimeout bounds how long BeforeModel waits for a decision
// before proceeding anyway. Short, because — unlike everything else this
// library does — a Decide call now sits on the model call's own critical
// path. This is a recorder-side bound chosen for that reason, not a
// governance service SLO.
const defaultDecideTimeout = 1500 * time.Millisecond

// DefaultDeniedMessage is what a caller sees on a DENY verdict when
// Options.DeniedMessage is empty. Generic on purpose: the actual words a
// product wants its user to read are product copy this library has no basis
// to guess, so this exists only so a team that has not set one yet still
// gets a sentence instead of an empty response.
const DefaultDeniedMessage = "This request was declined because it would exceed a configured spending limit."

// Options is what the framework cannot tell us.
type Options struct {
	// Agent is the registered agent these records belong to.
	// Format: organisations/{organisation}/agents/{agent}
	Agent string

	// Service names the service the agent runs in, e.g. "atlas-agent".
	Service string

	// BilledBy is who bills for the model calls, e.g. "VERTEX_AI", "ANTHROPIC". Vertex unless an agent has been pointed at a provider directly.
	//
	// A string so that calling a provider this library has never heard of needs no release of it. Recognised or not, the value reaches the rate card as written.
	BilledBy string

	// Region is where the model calls are processed, e.g. "us-central1" or "global". Defaults to the Vertex location in GOOGLE_CLOUD_LOCATION when the agent runs on Vertex.
	Region string

	// Deprecated: set BilledBy. Kept so an agent configured before it existed still attributes its spend correctly.
	Provider pb.Activity_Provider

	// Skill is the area of the product the user is in, where the product wants
	// cost sliced that way. Optional, and set once here rather than per call
	// because the framework cannot infer a product concept.
	Skill string

	// Product is deprecated and ignored: DecideRequest.product no longer
	// narrows a governance decision, and this library no longer sends it.
	// The field is kept only so a caller already setting it does not fail
	// to compile; it may be removed in a future version.
	//
	// Deprecated: no longer read.
	Product string

	// ToolFailed reads a tool's own result and says whether it worked.
	//
	// A great many tools never raise. They catch what went wrong and hand back an
	// answer that says so — `{"success": false}`, or a partial result with two of
	// its four lookups missing — and the framework, which only sees that a value
	// was returned, reports a success. Recorded that way, the one call in a turn
	// that actually failed is the one nothing reports, and a team reading a clean
	// failure rate has no idea why their answers are poor.
	//
	// Only the adopter can judge this: the shape of a tool's result is the
	// product's own, and a library guessing at it would be wrong differently for
	// every tool. The code returned is stored as the failure's code, so a report
	// can group by it like any other; it is a code and never a sentence, for the
	// same reason no message is ever recorded.
	//
	// Left nil, nothing changes: a tool fails when it raises, as it does today.
	ToolFailed func(tool string, result map[string]any) (failed bool, code string)

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

	// DeniedMessage is what the model's caller sees when governance answers
	// DENY, in the model's own voice. Adopter-configurable because the
	// words a user reads on a refusal are product copy — dealade's would
	// say something about credits — not something this library should
	// assume. Defaults to DefaultDeniedMessage when empty.
	DeniedMessage string
}

func (o Options) deniedMessage() string {
	if o.DeniedMessage == "" {
		return DefaultDeniedMessage
	}
	return o.DeniedMessage
}

// framework is the framework these hooks run in, recorded so its news can reach the teams that use it.
const framework = "google/adk-go"

// frameworkVersion is the ADK release built into the agent.
func frameworkVersion() string {
	return recorder.ModuleVersion("google.golang.org/adk")
}

func (o Options) region() string {
	if o.Region != "" {
		return o.Region
	}
	return recorder.DefaultRegion(o.billedBy())
}

func (o Options) billedBy() string {
	return recorder.BilledByOf(o.BilledBy, o.Provider) //nolint:staticcheck // the enum is read only to keep an agent configured before BilledBy existed attributing correctly
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

		// What the provider said about a failure, beside the one word the code
		// reduces to. A framework that flattens its model's error to a message
		// before this callback sees it leaves nothing to read, and the record then
		// says so rather than carrying a guess.
		reported := recorder.ReportedErrorFor(callErr, opts.billedBy())

		activity := &pb.Activity{
			Agent:            opts.Agent,
			Request:          requestOf(ctx),
			Session:          ctx.SessionID(),
			User:             userOf(r, ctx),
			CallerService:    opts.Service,
			ObservedAs:       pb.Agent_AGENT,
			SubAgent:         ctx.AgentName(),
			CallerComponent:  componentOf(ctx),
			Skill:            opts.Skill,
			Project:          recorder.ProjectFrom(ctx),
			Model:            modelOf(response, known),
			BilledBy:         opts.billedBy(),
			Region:           opts.region(),
			Framework:        framework,
			FrameworkVersion: frameworkVersion(),
			DurationMs:       millisSince(known.startedAt, time.Now),
			Status:           statusOf(response, callErr),
			ErrorCode:        errorCodeOf(response, callErr),
			ErrorFormat:      reported.Format,
			ReportedError:    reported.Fields,
			OccurredAt:       timestamppb.Now(),
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
// ALLOW and NOTIFY both let the call proceed: NOTIFY increments the Notified
// counter first, since it is advisory and must never block. DENY is the one
// verdict that preempts the call — BeforeModel returns a synthetic response
// in that case, which is what makes ADK skip the model call entirely (see
// llmagent's own doc on BeforeModelCallbacks). Every other outcome — a nil
// Decider, a malformed opts.Agent, a Decide call that timed out or errored,
// ALLOW, and any decision value this build does not recognize — proceeds.
// DOWNGRADE proceeds too, and is never treated as DENY: request.Model is
// rewritten to the replacement governance named, in place, before the
// framework ever sends the call, when the replacement's provider matches
// opts.billedBy() — rewriting only the model name for a different provider
// would ask this agent's own model client for a model it cannot serve. Where
// it does not match, or governance named no replacement at all, the call
// proceeds with the model it originally asked for; Stats.DowngradeApplied
// and Stats.DowngradeNotApplied report which happened.
//
// If opts.DecideCacheTTL is positive, opts.Decider is wrapped in a
// recorder.CachingDecider exactly once, here, before the callback is
// returned — so the same cache is reused for every model call this agent
// makes for the rest of its process lifetime, the same way one Recorder is
// built once and reused.
func BeforeModel(r *recorder.Recorder, opts Options) llmagent.BeforeModelCallback {
	decider := recorder.RememberRefusals(opts.Decider)
	if decider != nil && opts.DecideCacheTTL > 0 {
		decider = recorder.NewCachingDecider(decider, opts.DecideCacheTTL)
	}

	return func(ctx agent.CallbackContext, request *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
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

		outcome := decide(ctx, r, opts, decider, request.Model)
		if outcome.denied != nil {
			// AfterModel never runs for a short-circuited response — ADK
			// skips it along with the model call — so this is the only
			// place a denied call is recorded, and the only place this
			// call's in-flight entry is cleared.
			inFlight.take(callKey(ctx))
			return outcome.denied, nil
		}
		if outcome.replacementModel != "" {
			if outcome.replacementProvider != "" && !strings.EqualFold(outcome.replacementProvider, opts.billedBy()) {
				r.NoteDowngradeNotApplied()
			} else {
				request.Model = outcome.replacementModel
				r.NoteDowngradeApplied()
			}
		}
		// Noted once, here, after any downgrade has already been applied to
		// request.Model, so a call whose provider reports no model at all
		// falls back to what was actually sent — the replacement, where one
		// was applied — never to the model that was asked for before it was
		// overridden.
		inFlight.requested(callKey(ctx), request.Model)
		return nil, nil
	}
}

// decideOutcome is what governance decided about one call, and what
// BeforeModel could learn from it before the framework sends the call.
type decideOutcome struct {
	// denied is non-nil only on a genuine DENY verdict — the synthetic
	// response BeforeModel returns to skip the model call.
	denied *adkmodel.LLMResponse

	// replacementProvider and replacementModel are what DOWNGRADE named,
	// both empty unless decision was DOWNGRADE. Whether they can actually be
	// applied to request.Model is BeforeModel's own decision, not decide's —
	// it is the one place opts.billedBy() and request are both in scope.
	replacementProvider string
	replacementModel    string
}

// decide asks governance whether a call naming requestedModel may proceed,
// using decider, when one is configured.
//
// decider is BeforeModel's precomputed value — opts.Decider itself, or
// opts.Decider wrapped in a cache — rather than opts.Decider read fresh
// here, so the same cache (and not a new one) is consulted on every call.
//
// Bounded by opts.decideTimeout() against ctx itself (not a detached
// context.Background()), so a cancelled turn cancels this call too rather
// than outliving it. A nil decider, a malformed opts.Agent, a Decide error,
// and a NOTIFY verdict are all handled without ever stopping the model
// call — see BeforeModel's own doc for why, and for how DOWNGRADE is
// applied once this returns.
//
// The workspace and the project are asked about because a budget can be narrowed to either, and a call is only held to a budget that names the tenant it is for. A turn that names no workspace asks about the organisation's default, which is what the same call spends against.
func decide(ctx agent.CallbackContext, r *recorder.Recorder, opts Options, decider recorder.Decider, requestedModel string) decideOutcome {
	if decider == nil {
		return decideOutcome{}
	}

	organisation, err := organisationOf(opts.Agent)
	if err != nil {
		r.NoteDecisionError()
		return decideOutcome{}
	}

	dctx, cancel := context.WithTimeout(ctx, opts.decideTimeout())
	defer cancel()

	resp, err := decider.Decide(dctx, &governancepb.DecideRequest{
		Parent:            organisation,
		Agent:             opts.Agent,
		User:              userIDOf(ctx),
		Workspace:         recorder.WorkspaceName(organisation, recorder.WorkspaceFrom(ctx)),
		Project:           recorder.ProjectFrom(ctx),
		RequestedProvider: opts.billedBy(),
		RequestedModel:    requestedModel,
	})
	if err != nil {
		r.NoteDecisionError()
		return decideOutcome{}
	}

	switch resp.GetDecision() {
	case governancepb.DecideResponse_NOTIFY:
		r.NoteNotified()
		return decideOutcome{}
	case governancepb.DecideResponse_DENY:
		r.NoteDenied()
		recordDenied(r, ctx, opts)
		return decideOutcome{denied: deniedResponse(opts)}
	case governancepb.DecideResponse_DOWNGRADE:
		r.NoteDowngraded()
		return decideOutcome{replacementProvider: resp.GetReplacementProvider(), replacementModel: resp.GetReplacementModel()}
	default:
		// ALLOW, and anything this build does not recognize, proceeds
		// exactly like ALLOW. An unrecognized value is never treated as
		// DENY.
		return decideOutcome{}
	}
}

// deniedResponse is what BeforeModel returns on a genuine DENY. Returning a
// non-nil response from a BeforeModelCallback is what makes ADK skip the
// actual model call and use this response instead — see llmagent's doc on
// BeforeModelCallbacks.
func deniedResponse(opts Options) *adkmodel.LLMResponse {
	return &adkmodel.LLMResponse{
		Content:      genai.NewContentFromText(opts.deniedMessage(), genai.RoleModel),
		FinishReason: genai.FinishReasonStop,
	}
}

// recordDenied files the refused call as a zero-cost activity. Nothing was
// spent — the call never reached the provider — but a refusal still has to
// be countable, which is what Activity_DENIED is for.
func recordDenied(r *recorder.Recorder, ctx agent.CallbackContext, opts Options) {
	r.RecordIn(ctx, &pb.Activity{
		Agent:            opts.Agent,
		Request:          requestOf(ctx),
		Session:          ctx.SessionID(),
		User:             userOf(r, ctx),
		CallerService:    opts.Service,
		ObservedAs:       pb.Agent_AGENT,
		SubAgent:         ctx.AgentName(),
		CallerComponent:  componentOf(ctx),
		Skill:            opts.Skill,
		Project:          recorder.ProjectFrom(ctx),
		BilledBy:         opts.billedBy(),
		Region:           opts.region(),
		Framework:        framework,
		FrameworkVersion: frameworkVersion(),
		Status:           pb.Activity_DENIED,
		OccurredAt:       timestamppb.Now(),
	})
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

// applyUsage records what the provider said about a call, and leaves reading it to the server.
//
// The counts go on as reported, under the provider's own names, with `usage_format` saying which convention they are in. The token classes an activity is priced on are written server-side from those, because the arithmetic differs by provider and gets corrected: Gemini's prompt count already contains the tokens served from cache, so subtracting here and being wrong means every adopter has to upgrade and redeploy before a bill comes right, while being wrong on the server costs one deploy and can be reapplied to records already written.
//
// The classes are also filled in here, and are deliberately the cruder reading. They are what the in-process running total is priced from, which is the half of a spend limit that needs no network call, so they have to exist before the record reaches anything. The server overwrites them from the reported quantities, so what is stored, shown and billed is the server's reading and never this one. Where the two differ this one reads high — it counts a cached token as ordinary input — and a running total that is too high stops a budget slightly early, which is the safe direction for the one figure that refuses work.
func applyUsage(activity *pb.Activity, usage *genai.GenerateContentResponseUsageMetadata) {
	if usage == nil {
		return
	}
	activity.UsageFormat = recorder.FormatVertex
	activity.ReportedUsage = recorder.ReportedFromGenAI(usage)

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
		opts.Agent, opts.Service, opts.billedBy())
}
