package recorder

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	governancepb "github.com/techbridgeinnovation/agentpulse/recorder/pb/governance"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

// ErrDenied is what a call made through an instrumented genai client returns when a spend limit refused it before it was sent. Test for it with Denied, which also recognises the refusal the OpenAI and Anthropic sdks raise.
var ErrDenied = errors.New("agentpulse: the call was declined because it would exceed a spend limit")

// deniedType is the error type a refused call carries in the sdk error an OpenAI or Anthropic client raises for it. Not a type either provider uses, so a refusal by a spend limit is never read as the provider's own.
const deniedType = "agentpulse_denied"

// defaultDirectDecideTimeout bounds how long a call waits for governance before it goes ahead without an answer.
const defaultDirectDecideTimeout = 1500 * time.Millisecond

// Governance is how an instrumented client asks whether a call may proceed.
type Governance struct {
	// Decider asks governance. Nil leaves every call ungoverned.
	Decider Decider

	// CacheTTL, when positive, keeps each answer this long, so a service making many calls for one workspace asks once rather than every time.
	CacheTTL time.Duration

	// Timeout bounds how long a call waits for an answer. Zero is a second and a half. A call whose answer does not arrive in time goes ahead, unless the same call was refused within the hour.
	Timeout time.Duration
}

// Governed returns a reporter whose instrumented clients ask governance before every call, the way an agent's callbacks do, so a budget that stops an agent stops a service spending against the same budget too.
//
// A call governance refuses is never sent, and is recorded as refused. Every other answer, and no answer — governance unreachable, slow, or saying something this build does not know — lets the call go ahead: a spend decision that cannot be made must not be the reason the product stops working.
//
// A DOWNGRADE is applied to the outbound call where the instrumented client's protocol can safely rewrite the model this call names — see protocol.downgrade in direct.go for exactly which clients that covers today. Where it cannot, the original request proceeds unchanged rather than being silently claimed as downgraded; Stats.DowngradeApplied and Stats.DowngradeNotApplied report which happened. A DOWNGRADE is decided once, from this single call's own request — nothing here asks governance again for the replacement.
//
// The caller sees a refusal as an error. From a genai client it is ErrDenied; from an OpenAI or Anthropic client it is the sdk's own error, raised from a refusal the sdk is told not to retry. Denied recognises all three.
func (rp *Reporter) Governed(g Governance) *Reporter {
	if rp == nil {
		return nil
	}
	governed := *rp
	governed.decider = RememberRefusals(g.Decider)
	if governed.decider != nil && g.CacheTTL > 0 {
		governed.decider = NewCachingDecider(governed.decider, g.CacheTTL)
	}
	governed.decideTimeout = g.Timeout
	if governed.decideTimeout <= 0 {
		governed.decideTimeout = defaultDirectDecideTimeout
	}
	return &governed
}

// Denied reports whether err is a call a spend limit refused before it was sent.
func Denied(err error) bool {
	if errors.Is(err, ErrDenied) {
		return true
	}
	var sdk sdkError
	return errors.As(err, &sdk) && strings.Contains(sdk.RawJSON(), `"`+deniedType+`"`)
}

// decideOutcome is what governance decided about one call, and what this
// reporter learned that a caller can act on before sending it.
type decideOutcome struct {
	// proceed is false only for a genuine DENY. Every other outcome — ALLOW,
	// DOWNGRADE, NOTIFY, or no answer at all — proceeds; DOWNGRADE is never
	// treated as DENY.
	proceed bool

	// replacementProvider and replacementModel are what DOWNGRADE named,
	// both empty unless decision was DOWNGRADE. Whether they can actually be
	// applied to the outbound call is a question for the protocol, not this
	// type — see protocol.downgrade.
	replacementProvider string
	replacementModel    string
}

// decide asks governance whether a call naming requestedProvider and
// requestedModel may proceed. Only a genuine DENY refuses it.
func (rp *Reporter) decide(ctx context.Context, requestedProvider, requestedModel string) (out decideOutcome) {
	out.proceed = true
	if rp.decider == nil {
		return out
	}
	// A decider is the host's to supply, and one that panics must cost the call nothing.
	defer func() {
		if recover() != nil {
			rp.recorder.NotePanicked()
			out = decideOutcome{proceed: true}
		}
	}()
	organisation, _, found := strings.Cut(rp.attribution.Agent, "/agents/")
	if !found || organisation == "" {
		rp.recorder.NoteDecisionError()
		return out
	}

	dctx, cancel := context.WithTimeout(ctx, rp.decideTimeout)
	defer cancel()
	resp, err := rp.decider.Decide(dctx, &governancepb.DecideRequest{
		Parent:            organisation,
		Agent:             rp.attribution.Agent,
		User:              decidableUser(UserFrom(ctx).ID),
		Workspace:         WorkspaceName(organisation, WorkspaceFrom(ctx)),
		Project:           ProjectFrom(ctx),
		RequestedProvider: requestedProvider,
		RequestedModel:    requestedModel,
	})
	if err != nil {
		rp.recorder.NoteDecisionError()
		return out
	}
	switch resp.GetDecision() {
	case governancepb.DecideResponse_NOTIFY:
		rp.recorder.NoteNotified()
	case governancepb.DecideResponse_DENY:
		rp.recorder.NoteDenied()
		out.proceed = false
	case governancepb.DecideResponse_DOWNGRADE:
		rp.recorder.NoteDowngraded()
		out.replacementProvider = resp.GetReplacementProvider()
		out.replacementModel = resp.GetReplacementModel()
	}
	return out
}

// recordDenied files a refused call. Nothing was spent, but a refusal still has to be countable.
func (rp *Reporter) recordDenied(ctx context.Context, component, model, billedBy string) {
	activity := rp.activity(ctx, component, 0, nil, nil)
	activity.Model = model
	activity.BilledBy = billedBy
	activity.Status = pb.Activity_DENIED
	rp.recorder.RecordIn(ctx, activity)
}

// deniedReply is the refusal an OpenAI or Anthropic sdk reads for a call a spend limit declined: a response it raises as its own error, marked not to be retried, because a retry would only be refused again.
func deniedReply(req *http.Request, body string) *http.Response {
	return &http.Response{
		Status:        "403 Forbidden",
		StatusCode:    http.StatusForbidden,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": {"application/json"}, "X-Should-Retry": {"false"}},
		Body:          io.NopCloser(bytes.NewReader([]byte(body))),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

// decidableUser is the identifier a spend decision is asked about, with one shaped like an email address left out for the same reason a record leaves it out.
func decidableUser(id string) string {
	if LooksLikeEmail(id) {
		return ""
	}
	return id
}
