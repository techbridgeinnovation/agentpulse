package recorder

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"

	"google.golang.org/genai"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

// This is the way in for code that is not an agent on a framework.
//
// There is nothing to observe such code from, so it reports for itself. What it
// must not have to do is know the shape of a record: the fields that are easy to
// fill in wrongly are the ones that quietly cost money, and every service
// filling them by hand is every service getting them wrong differently. So the
// parts that do not change are given once, the parts that do are named, and the
// mapping onto a record happens here.
//
// `component` is asked for rather than derived, which diverges from how the
// framework path works. Nothing here can infer which part of a service made a
// call, and a single bucket for a whole service answers no question worth
// asking.

// Attribution is what every record from one service has in common.
type Attribution struct {
	// Agent is the registered agent this service records as.
	// Format: organisations/{organisation}/agents/{agent}
	Agent string

	// Service names the service itself, e.g. "sources-service".
	Service string

	// BilledBy is who bills for the calls, e.g. "VERTEX_AI", "ANTHROPIC". Vertex unless the service calls a provider directly, which decides whether the spend can ever be checked against an invoice.
	//
	// A string so that calling a provider this library has never heard of needs no release of it. Recognised or not, the value reaches the rate card as written.
	BilledBy string

	// Deprecated: set BilledBy. Kept so a service configured before it existed still attributes its spend correctly.
	Provider pb.Activity_Provider

	// Skill is the area of the product the work belongs to, where the product
	// wants cost sliced that way. Optional.
	Skill string
}

func (a Attribution) billedBy() string {
	return BilledByOf(a.BilledBy, a.Provider) //nolint:staticcheck // the enum is read only to keep an agent configured before BilledBy existed attributing correctly
}

// Tokens is a split a caller has already made, for a caller who is certain the classes do not overlap.
//
// Prefer ModelCall.Reported. Filling this in means deciding, at the call site, what a provider's numbers mean — and providers disagree about that in ways that are easy to miss and expensive to get wrong. Gemini's prompt count already contains the tokens served from cache, so copying it into Prompt and the cache figure into Cached charges the same tokens twice, once at roughly ten times the rate they earned. OpenAI does the same with cached, and counts reasoning inside its output figure besides.
//
// Reported hands the provider's own numbers to the server and lets the split be made in one place that can be corrected. This stays for a caller who has genuinely already done the arithmetic, and for one whose provider reports classes that never overlapped.
//
// Each class is separate because each is billed at its own rate, and the ones that get forgotten are the expensive ones: cache writes cost more than ordinary input, and reasoning is invisible to anything that only adds up input and output.
type Tokens struct {
	Prompt     int32
	Candidate  int32
	Cached     int32
	CacheWrite int32
	Reasoning  int32

	// Total as the provider reported it. Left at zero it is the sum of the
	// others, which is right often enough and wrong for a provider that counts
	// something we have no field for.
	Total int32
}

func (t Tokens) total() int32 {
	if t.Total > 0 {
		return t.Total
	}
	return t.Prompt + t.Candidate + t.Cached + t.CacheWrite + t.Reasoning
}

// Charge is a cost that is not measured in tokens, such as one web search.
//
// The quantity is what happened; what it costs is decided by the server against
// the rate card, so a caller never states a price.
type Charge struct {
	// PriceableUnit is the rate card entry this is charged against.
	// Format: priceableUnits/{id}
	PriceableUnit string
	Quantity      int64
}

// ModelCall is one finished call to a model.
type ModelCall struct {
	// Model as the provider names it, e.g. "gemini-2.5-pro".
	Model string

	// Component is the part of the service that spent this, e.g. "asset_summary".
	Component string

	// Reported is what the provider said, keyed by the provider's own name for each quantity, e.g. {"input_tokens": 4211, "cache_read_input_tokens": 21847}.
	//
	// The way to report usage from a service that is not on a framework. Hand over the numbers as the provider gave them, name them as the provider named them, say which convention they are in with Format, and the split into priced classes is made server-side — in one place, correctable without this service being touched, and reapplicable to records already written.
	Reported map[string]int64

	// Format names the convention Reported is in, e.g. recorder.FormatAnthropic.
	//
	// Required alongside Reported, and the whole reason Reported can be read at all: the same field name means different things to different providers, and a set of counts with no convention attached cannot be split without guessing.
	Format string

	// Tokens is a split already made, for a caller who has genuinely made it. Prefer Reported.
	Tokens   Tokens
	Duration time.Duration

	// CacheWriteTTL is how long a cache entry this call wrote is kept.
	//
	// Stated by the caller because no provider reports it back: it is a request parameter, so the only place it is known is the code that set it. Worth stating because a longer-lived entry is charged a premium — an hour against the usual few minutes — and a write priced at the short rate is wrong by that difference.
	CacheWriteTTL time.Duration

	// CostMicros is what the provider said the call cost, in millionths of a dollar.
	//
	// A few providers return a price with the response. Where one does there is no reason for us to estimate over the top of it, so it is carried through and preferred.
	CostMicros int64

	// Tier is how the call was served, where a provider prices the same tokens differently by tier, e.g. "BATCH".
	//
	// Stated by the caller for the same reason as the lifetime: the tier is chosen when the request is made and does not come back on the response. Batch is priced at around half, so a batched call recorded without it is charged about twice what it cost.
	Tier string

	// Err is read for whether the call failed and for its code. The message is
	// never recorded: a provider error routinely quotes the prompt back.
	Err error

	// Truncated marks a call that hit a limit and did partial work. It is kept
	// apart from a failure because a truncated call was charged for what it did
	// and a failed one may have been charged for nothing.
	Truncated bool

	// FinishReason is what the provider said ended the call, where it said
	// anything at all, e.g. "SAFETY", "MAX_TOKENS", "content_filter".
	//
	// Worth reporting because the failures that cost the most are the ones a
	// provider reports on a successful response: a call blocked for safety
	// comes back 200 with no error, no content and a bill for the prompt, and
	// a recorder that reads only Err files it as an ordinary success.
	FinishReason string

	// Charges are costs on this call that tokens do not describe.
	Charges []Charge
}

// ToolCall is one finished call to a tool.
//
// Worth recording on its own because a search or a lookup is charged per use,
// which is spend that counting tokens can never see.
type ToolCall struct {
	// Tool is the name of the tool that ran.
	Tool string

	Duration time.Duration
	Err      error
	Charges  []Charge
}

// Reporter records on behalf of one service.
//
// Cheap to hold and safe to share: it keeps no state beyond the attribution it
// was built with.
type Reporter struct {
	recorder    *Recorder
	attribution Attribution
}

// For returns a reporter that stamps every record with the same attribution.
//
// Built once where the service starts, so a call site names only what is
// different about its own call.
func (r *Recorder) For(attribution Attribution) *Reporter {
	return &Reporter{recorder: r, attribution: attribution}
}

// ModelCall records a finished model call, and returns immediately.
//
// It never blocks, never returns an error and never panics, which is the whole
// contract of this library: recording is not allowed to be the reason a request
// failed.
func (rp *Reporter) ModelCall(ctx context.Context, call ModelCall) {
	if rp == nil {
		return
	}

	activity := rp.activity(ctx, call.Component, call.Duration, call.Err, call.Charges)
	activity.Model = call.Model
	activity.UsageFormat = call.Format
	activity.ReportedUsage = reportedQuantities(call.Reported)
	activity.ServiceTier = call.Tier
	activity.ProviderCostMicros = call.CostMicros
	activity.CacheWriteTtlSeconds = int32(call.CacheWriteTTL.Seconds())

	// A split the caller made is carried as given. Both can be present: a caller migrating onto Reported keeps its own figures until the server's reading of the same call is confirmed to agree with them, which is the cheapest way to prove a convention is read right.
	if call.Tokens != (Tokens{}) {
		activity.PromptTokens = call.Tokens.Prompt
		activity.CandidateTokens = call.Tokens.Candidate
		activity.CachedTokens = call.Tokens.Cached
		activity.CacheWriteTokens = call.Tokens.CacheWrite
		activity.ReasoningTokens = call.Tokens.Reasoning
		activity.TotalTokens = call.Tokens.total()
	}

	switch {
	case call.Err != nil:
		// Already failed, with its code, in activity().
	case call.Truncated, truncatedFinish(call.FinishReason):
		activity.Status = pb.Activity_TRUNCATED
	case BlockedFinish(call.FinishReason):
		activity.Status = pb.Activity_FAILED
		activity.ErrorCode = call.FinishReason
	}

	rp.recorder.RecordIn(ctx, activity)
}

// ToolCall records a finished tool call, and returns immediately.
func (rp *Reporter) ToolCall(ctx context.Context, call ToolCall) {
	if rp == nil {
		return
	}

	activity := rp.activity(ctx, "tool:"+call.Tool, call.Duration, call.Err, call.Charges)
	rp.recorder.RecordIn(ctx, activity)
}

// SpentOn returns what a request has cost so far in this process, in millionths
// of a dollar.
//
// The same answer the recorder gives, offered here so a handler holds one thing
// rather than two.
func (rp *Reporter) SpentOn(request string) int64 {
	if rp == nil {
		return 0
	}
	return rp.recorder.SpentOn(request)
}

// FinishRequest releases the running total for a request.
//
// Call it when a request is done. Not calling it leaks one entry per request for
// the life of the process.
func (rp *Reporter) FinishRequest(request string) {
	if rp == nil {
		return
	}
	rp.recorder.FinishRequest(request)
}

// activity fills in everything the two kinds of call have in common.
func (rp *Reporter) activity(ctx context.Context, component string, duration time.Duration, err error, charges []Charge) *pb.Activity {
	user := UserFrom(ctx)
	rp.recorder.NoteUserIn(ctx, user)

	activity := &pb.Activity{
		Agent:           rp.attribution.Agent,
		Request:         RequestFrom(ctx),
		Session:         SessionFrom(ctx),
		User:            user.ID,
		Project:         ProjectFrom(ctx),
		CallerService:   rp.attribution.Service,
		CallerComponent: component,
		Skill:           rp.attribution.Skill,
		BilledBy:        rp.attribution.billedBy(),
		DurationMs:      duration.Milliseconds(),
		Status:          pb.Activity_OK,
		OccurredAt:      timestamppb.Now(),
	}

	if err != nil {
		activity.Status = pb.Activity_FAILED
		activity.ErrorCode = ErrorCode(err)
	}

	for _, charge := range charges {
		activity.Charges = append(activity.Charges, &pb.Charge{
			PriceableUnit: charge.PriceableUnit,
			Quantity:      charge.Quantity,
		})
	}
	return activity
}

// blockedFinishReasons are the reasons a provider gives for a call that ended
// without producing what it was asked for.
//
// Every one of these arrives on a successful response — 200, no error, usually
// no content and a bill for the prompt — so nothing that reads only the error
// will ever see them. Held as a set of strings rather than a provider's own
// enum so that one recorder can report Gemini's SAFETY, OpenAI's
// content_filter and Bedrock's guardrail_intervened through the same field.
//
// MAX_TOKENS and its siblings are deliberately absent: a call cut short by a
// limit did the work it was paid for, which is what TRUNCATED says.
var blockedFinishReasons = map[string]bool{
	"SAFETY":                   true,
	"RECITATION":               true,
	"BLOCKLIST":                true,
	"PROHIBITED_CONTENT":       true,
	"SPII":                     true,
	"IMAGE_SAFETY":             true,
	"IMAGE_PROHIBITED_CONTENT": true,
	"IMAGE_RECITATION":         true,
	"LANGUAGE":                 true,
	"MALFORMED_FUNCTION_CALL":  true,
	"UNEXPECTED_TOOL_CALL":     true,
	"TOO_MANY_TOOL_CALLS":      true,
	"NO_IMAGE":                 true,
	"CONTENT_FILTER":           true,
	"CONTENT_FILTERED":         true,
	"GUARDRAIL_INTERVENED":     true,
	"REFUSAL":                  true,
	"ERROR_TOXIC":              true,
	"MALFORMED_MODEL_OUTPUT":   true,
	"MALFORMED_TOOL_USE":       true,
	// A model that stopped for a reason it would not name did not answer
	// either. Recorded as a failure under its own code, which classifies as
	// unknown, so it is visible as something to look into rather than counted
	// as a cheap success.
	"OTHER":       true,
	"IMAGE_OTHER": true,
}

// truncatedReasons are the reasons that mean a limit was reached, not a refusal.
var truncatedReasons = map[string]bool{
	"MAX_TOKENS":        true,
	"LENGTH":            true,
	"MAX_OUTPUT_TOKENS": true,
}

// BlockedFinish reports whether a provider's finish reason means the call
// produced nothing usable, and should therefore be recorded as a failure
// rather than as a success that happened to be cheap.
//
// Exported because a service calling a provider directly has to answer the
// same question this library's framework hooks answer, and two answers to it
// would put the same failure on two sides of a report.
func BlockedFinish(reason string) bool {
	return blockedFinishReasons[canonicalReason(reason)]
}

// truncatedFinish reports whether a finish reason means a limit was hit.
func truncatedFinish(reason string) bool {
	return truncatedReasons[canonicalReason(reason)]
}

// canonicalReason upper-cases a reason so that a provider writing
// content_filter and one writing CONTENT_FILTER are read as the same thing.
func canonicalReason(reason string) string {
	return strings.ToUpper(strings.TrimSpace(reason))
}

// ErrorCode reduces an error to the one part of it that is safe to keep.
//
// A code says what went wrong in a way a report can group by. A message says the
// same thing in prose and often quotes the prompt that caused it, which is the
// one thing this library must never carry. Taking the error and returning only
// its code is also what stops a caller reaching for err.Error() themselves.
//
// The code is whatever the layer that failed calls it — a provider's own status,
// the http status where that is all there is, or a grpc code — rather than a
// taxonomy of our own. A deadline is read first whichever layer reported it, so
// that the one failure a report is always asked about groups as one row.
//
// Exported because a service that records through something other than this
// library's own hooks still has to reduce an error the same way, and the whole
// point of doing it here is that it is done identically everywhere.
func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return codes.DeadlineExceeded.String()
	case errors.Is(err, context.Canceled):
		return codes.Canceled.String()
	}
	if code := providerCode(err); code != "" {
		return code
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return codes.DeadlineExceeded.String()
	}
	return status.Code(err).String()
}

// providerCode reads the code out of a provider sdk's own error.
//
// Without this, every failure that is not a grpc status reaches a report as
// Unknown: a rate limit, a malformed request and an outage arrive as the same
// word, and a failures panel built on it has nothing to say. The genai client
// returns its error by value; the pointer is tried too because the type is
// exported and a caller is free to wrap one.
func providerCode(err error) string {
	var byValue genai.APIError
	if errors.As(err, &byValue) {
		return apiErrorCode(byValue)
	}
	var byPointer *genai.APIError
	if errors.As(err, &byPointer) && byPointer != nil {
		return apiErrorCode(*byPointer)
	}
	return ""
}

// apiErrorCode prefers the status the provider named over the http status it
// arrived with, because the status is what that provider's own documentation
// calls the failure and therefore what someone reading the report will search
// for.
//
// Only when that status is one. The genai client fills the same field with the
// http reason phrase — `429 Too Many Requests` — whenever the body it got was
// not json, which happens on whole classes of failure, and storing that as the
// code would put a different string in the report for the same failure
// depending on what the provider happened to return. The http status is the
// honest answer in that case, and it is the one every provider agrees on.
func apiErrorCode(err genai.APIError) string {
	if canonicalStatus(err.Status) {
		return err.Status
	}
	if err.Code != 0 {
		return "HTTP_" + strconv.Itoa(err.Code)
	}
	return err.Status
}

// canonicalStatus reports whether a provider named the failure, as opposed to
// handing back an http reason phrase. A named status is letters and
// underscores — RESOURCE_EXHAUSTED, FAILED_PRECONDITION — and never carries
// the digits or the spaces a phrase does.
func canonicalStatus(status string) bool {
	if status == "" {
		return false
	}
	for _, r := range status {
		if r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			continue
		}
		return false
	}
	return true
}
