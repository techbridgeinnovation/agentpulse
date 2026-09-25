// Package adkv2hooks wires the recorder into an agent on the second major version of Google's ADK.
//
// It sits beside adkhooks rather than replacing it because the two majors are different libraries as far as Go is concerned: they have different import paths and their types are unrelated, so one package cannot serve both. Agents already in production are on the first, and the platform's own agent blocks scaffold onto the second, so both are live at once and the library has to reach both.
//
// The four callbacks and everything they read are the same. The one difference the port turns on is that the first major hands a model callback a CallbackContext and a tool callback a ToolContext, while this one hands them all a single Context. Every field the record needs is on it either way.
//
// Which tenant the turn is for, and which unit of work inside it, are the two the framework cannot know. They are read from the callback context where the product put them, at its sign-in, and never asked for here — see recorder.WithWorkspace.
//
// Kept as its own package for the same reason adkhooks is: service code that uses no framework can take the recorder without taking a framework with it.
package adkv2hooks

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
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
// governance service SLO. Kept identical to adkhooks' own constant so the
// two integrations behave the same by default.
const defaultDecideTimeout = 500 * time.Millisecond

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

	// Service names the service the agent runs in, e.g. "pulseagent-v1".
	Service string

	// BilledBy is who bills for the model calls, e.g. "VERTEX_AI", "ANTHROPIC". Vertex unless an agent has been pointed at a provider directly.
	//
	// A string so that calling a provider this library has never heard of needs no release of it. Recognised or not, the value reaches the rate card as written.
	BilledBy string

	// Deprecated: set BilledBy. Kept so an agent configured before it existed still attributes its spend correctly.
	Provider pb.Activity_Provider

	// Model is the model this agent is configured to call, and stands in when the framework does not say which model answered.
	//
	// A streamed answer arrives as pieces which the framework assembles into one summary, and that summary carries the token counts but not the name of the model that produced them. Both majors do this. Pricing looks a rate up by model, so a record with no model prices against nothing — accurate counts attached to a cost of zero.
	Model string

	// Skill is the area of the product the user is in, where the product wants cost sliced that way. Optional, and set once here rather than per call because the framework cannot infer a product concept.
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
	// the same way an empty Sinks list means recording is skipped.
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
	// every model call. There is no default duration here — how long a
	// governance verdict may be reused is a policy choice this library does
	// not make on a caller's behalf.
	DecideCacheTTL time.Duration

	// DeniedMessage is what the model's caller sees when governance answers
	// DENY, in the model's own voice. Adopter-configurable because the
	// words a user reads on a refusal are product copy, not something this
	// library should assume. Defaults to DefaultDeniedMessage when empty.
	DeniedMessage string
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

func (o Options) deniedMessage() string {
	if o.DeniedMessage == "" {
		return DefaultDeniedMessage
	}
	return o.DeniedMessage
}

// organisationOf returns the organisation an agent resource name belongs to,
// e.g. "organisations/acme" from "organisations/acme/agents/atlas".
func organisationOf(agentName string) (string, error) {
	const sep = "/agents/"
	i := strings.Index(agentName, sep)
	if i < 0 {
		return "", fmt.Errorf("%q is not an agent resource name", agentName)
	}
	return agentName[:i], nil
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
// Per call rather than per turn: a turn makes as many calls as the loop needs, and they all carry the same request id, so the rows answer both what the turn cost and what each call cost. Partial responses are skipped — in streaming mode every chunk arrives here, and counting them would multiply a turn's usage by however many chunks it happened to be split into.
func AfterModel(r *recorder.Recorder, opts Options) llmagent.AfterModelCallback {
	return func(ctx agent.Context, response *adkmodel.LLMResponse, callErr error) (*adkmodel.LLMResponse, error) {
		if response == nil || response.Partial {
			return nil, nil
		}

		// What the provider said about a failure, beside the one word the code
		// reduces to. A framework that flattens its model's error to a message
		// before this callback sees it leaves nothing to read, and the record then
		// says so rather than carrying a guess.
		reported := recorder.ReportedErrorFor(callErr, opts.billedBy())

		activity := &pb.Activity{
			Agent:           opts.Agent,
			Request:         requestOf(ctx),
			Session:         ctx.SessionID(),
			User:            userOf(r, ctx),
			CallerService:   opts.Service,
			ObservedAs:      pb.Agent_AGENT,
			CallerComponent: componentOf(ctx),
			Skill:           opts.Skill,
			Project:         recorder.ProjectFrom(ctx),
			Model:           modelOf(response, opts),
			BilledBy:        opts.billedBy(),
			Status:          statusOf(response, callErr),
			ErrorCode:       errorCodeOf(response, callErr),
			ErrorFormat:     reported.Format,
			ReportedError:   reported.Fields,
			OccurredAt:      timestamppb.Now(),
		}
		applyUsage(activity, response.UsageMetadata)

		r.RecordIn(ctx, activity)
		return nil, nil
	}
}

// AfterTool records one activity per tool call.
//
// A tool call is a priced unit of work in its own right: a search or a lookup is charged per request, not per token, and those charges are invisible to anything that only counts tokens. Recorded on completion, so a tool that never ran is not counted.
//
// The error is read for its code, not its message, the same way a model call's is: a report that says a tool failed and not how it failed leaves a team to guess between a timeout, a bad argument and an outage.
func AfterTool(r *recorder.Recorder, opts Options) llmagent.AfterToolCallback {
	return func(ctx agent.Context, t tool.Tool, _, result map[string]any, callErr error) (map[string]any, error) {
		name := ""
		if t != nil {
			name = t.Name()
		}

		status, code := toolOutcome(r, opts, name, result, callErr)

		r.RecordIn(ctx, &pb.Activity{
			Agent:           opts.Agent,
			Request:         requestOf(ctx),
			Session:         ctx.SessionID(),
			User:            userOf(r, ctx),
			CallerService:   opts.Service,
			ObservedAs:      pb.Agent_AGENT,
			CallerComponent: "tool:" + name,
			Tool:            name,
			Skill:           opts.Skill,
			Project:         recorder.ProjectFrom(ctx),
			BilledBy:        opts.billedBy(),
			Status:          status,
			ErrorCode:       code,
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

// BeforeModel labels the outgoing request with who it belongs to, then — if
// a Decider is configured — asks governance whether the call may proceed.
//
// Labels set here are carried into the cloud billing export, which is what makes a charge that is not measured in tokens attributable at all. Only opaque identifiers go on a label — never an email — so nothing reaches billing that would be PII.
//
// ALLOW and NOTIFY both let the call proceed: NOTIFY increments the Notified
// counter first, since it is advisory and must never block. DENY is the one
// verdict that preempts the call — BeforeModel returns a synthetic response
// in that case, which is what makes ADK skip the model call entirely (see
// llmagent's own doc on BeforeModelCallbacks). Everything else — a nil
// Decider, a malformed opts.Agent, a Decide call that timed out or errored,
// and any decision value this build does not recognize (including
// DOWNGRADE) — proceeds exactly like ALLOW. An unrecognized or failed
// decision is never treated as DENY.
//
// If opts.DecideCacheTTL is positive, opts.Decider is wrapped in a
// recorder.CachingDecider exactly once, here, before the callback is
// returned — so the same cache is reused for every model call this agent
// makes for the rest of its process lifetime.
func BeforeModel(r *recorder.Recorder, opts Options) llmagent.BeforeModelCallback {
	decider := opts.Decider
	if decider != nil && opts.DecideCacheTTL > 0 {
		decider = recorder.NewCachingDecider(decider, opts.DecideCacheTTL)
	}

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
		if len(labels) > 0 {
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
		}

		if denied := decide(ctx, r, opts, decider); denied != nil {
			return denied, nil
		}
		return nil, nil
	}
}

// decide asks governance whether ctx's call may proceed, using decider,
// when one is configured. It returns a non-nil response only on a genuine
// DENY verdict — the synthetic response BeforeModel should return to skip
// the model call — and nil in every other case, including one this build
// does not recognize.
//
// Bounded by opts.decideTimeout() against ctx itself, so a cancelled turn
// cancels this call too rather than outliving it.
func decide(ctx agent.Context, r *recorder.Recorder, opts Options, decider recorder.Decider) *adkmodel.LLMResponse {
	if decider == nil {
		return nil
	}

	organisation, err := organisationOf(opts.Agent)
	if err != nil {
		r.NoteDecisionError()
		return nil
	}

	dctx, cancel := context.WithTimeout(ctx, opts.decideTimeout())
	defer cancel()

	resp, err := decider.Decide(dctx, &governancepb.DecideRequest{
		Parent:    organisation,
		Agent:     opts.Agent,
		User:      userIDOf(ctx),
		Workspace: recorder.WorkspaceName(organisation, recorder.WorkspaceFrom(ctx)),
		Project:   recorder.ProjectFrom(ctx),
	})
	if err != nil {
		r.NoteDecisionError()
		return nil
	}

	switch resp.GetDecision() {
	case governancepb.DecideResponse_NOTIFY:
		r.NoteNotified()
		return nil
	case governancepb.DecideResponse_DENY:
		r.NoteDenied()
		recordDenied(r, ctx, opts)
		return deniedResponse(opts)
	default:
		// ALLOW, and anything this build does not recognize (including
		// DOWNGRADE, which this integration does not implement yet),
		// proceeds exactly like ALLOW. An unrecognized value is never
		// treated as DENY.
		return nil
	}
}

// deniedResponse is what BeforeModel returns on a genuine DENY. Returning a
// non-nil response from a BeforeModelCallback is what makes ADK skip the
// actual model call and use this response instead.
func deniedResponse(opts Options) *adkmodel.LLMResponse {
	return &adkmodel.LLMResponse{
		Content:      genai.NewContentFromText(opts.deniedMessage(), genai.RoleModel),
		FinishReason: genai.FinishReasonStop,
	}
}

// recordDenied files the refused call as a zero-cost activity. Nothing was
// spent — the call never reached the provider — but a refusal still has to
// be countable, which is what Activity_DENIED is for.
//
// AfterModel never runs for a short-circuited response — ADK skips it along
// with the model call — so this is the only place a denied call is
// recorded.
func recordDenied(r *recorder.Recorder, ctx agent.Context, opts Options) {
	r.RecordIn(ctx, &pb.Activity{
		Agent:           opts.Agent,
		Request:         requestOf(ctx),
		Session:         ctx.SessionID(),
		User:            userOf(r, ctx),
		CallerService:   opts.Service,
		ObservedAs:      pb.Agent_AGENT,
		CallerComponent: componentOf(ctx),
		Skill:           opts.Skill,
		Project:         recorder.ProjectFrom(ctx),
		BilledBy:        opts.billedBy(),
		Status:          pb.Activity_DENIED,
		OccurredAt:      timestamppb.Now(),
	})
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

// errorCodeOf names why a call did not succeed.
//
// The framework's own code first, then the error, then the reason a provider gave for ending a call it answered successfully — which is the only trace a safety block leaves.
func errorCodeOf(response *adkmodel.LLMResponse, callErr error) string {
	switch {
	case response.ErrorCode != "":
		return response.ErrorCode
	case callErr != nil:
		return recorder.ErrorCode(callErr)
	case recorder.BlockedFinish(string(response.FinishReason)):
		return string(response.FinishReason)
	}
	return ""
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
	case recorder.BlockedFinish(string(response.FinishReason)):
		// A safety block, a refusal or a malformed tool call arrives on a
		// successful response with no content and a bill for the prompt.
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
		opts.Agent, opts.Service, opts.billedBy())
}

// toolOutcome reads how a tool call ended.
//
// A raised error is a failure whatever else is true, so it is read first and the
// adopter's own judgement is not consulted: an error already says what went
// wrong, in a code, and a result returned alongside one says nothing. Where
// nothing was raised, the adopter decides, because only the product knows the
// shape of its own tool's answer.
func toolOutcome(r *recorder.Recorder, opts Options, tool string, result map[string]any, callErr error) (pb.Activity_Status, string) {
	if callErr != nil {
		return pb.Activity_FAILED, recorder.ErrorCode(callErr)
	}
	if opts.ToolFailed == nil {
		return pb.Activity_OK, ""
	}
	if failed, code := judgeTool(r, opts, tool, result); failed {
		// A failure the adopter named but gave no code for is still a failure. It is
		// recorded under one the server can classify rather than under nothing, which
		// would read as a success that happened to be marked.
		if code == "" {
			code = "TOOL_ERROR"
		}
		return pb.Activity_FAILED, code
	}
	return pb.Activity_OK, ""
}

// judgeTool runs the adopter's own judgement of a tool's result, and survives it
// panicking.
//
// It is the adopter's code running inside the framework's callback, so a panic in
// it would otherwise fail the tool call it was only meant to describe. A judgement
// that panics is counted and the call is recorded as the framework saw it.
func judgeTool(r *recorder.Recorder, opts Options, tool string, result map[string]any) (failed bool, code string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			r.NotePanicked()
			failed, code = false, ""
		}
	}()
	return opts.ToolFailed(tool, result)
}
