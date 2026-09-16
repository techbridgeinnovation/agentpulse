package recorder

import (
	"context"
	"errors"
	"time"

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

	// Provider is who bills for the calls. Vertex unless the service calls a
	// provider directly, which decides whether the spend can ever be checked
	// against an invoice.
	Provider pb.Activity_Provider

	// Skill is the area of the product the work belongs to, where the product
	// wants cost sliced that way. Optional.
	Skill string
}

func (a Attribution) provider() pb.Activity_Provider {
	if a.Provider == pb.Activity_PROVIDER_UNSPECIFIED {
		return pb.Activity_VERTEX_AI
	}
	return a.Provider
}

// Tokens is what the provider reported it will charge for.
//
// Each kind is separate because each is billed at its own rate, and the ones
// that get forgotten are the expensive ones: cache writes cost more than
// ordinary input, and reasoning tokens are invisible to anything that only adds
// up input and output.
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

	Tokens   Tokens
	Duration time.Duration

	// Err is read for whether the call failed and for its code. The message is
	// never recorded: a provider error routinely quotes the prompt back.
	Err error

	// Truncated marks a call that hit a limit and did partial work. It is kept
	// apart from a failure because a truncated call was charged for what it did
	// and a failed one may have been charged for nothing.
	Truncated bool

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
	activity.PromptTokens = call.Tokens.Prompt
	activity.CandidateTokens = call.Tokens.Candidate
	activity.CachedTokens = call.Tokens.Cached
	activity.CacheWriteTokens = call.Tokens.CacheWrite
	activity.ReasoningTokens = call.Tokens.Reasoning
	activity.TotalTokens = call.Tokens.total()

	if call.Err == nil && call.Truncated {
		activity.Status = pb.Activity_TRUNCATED
	}

	rp.recorder.Record(activity)
}

// ToolCall records a finished tool call, and returns immediately.
func (rp *Reporter) ToolCall(ctx context.Context, call ToolCall) {
	if rp == nil {
		return
	}

	activity := rp.activity(ctx, "tool:"+call.Tool, call.Duration, call.Err, call.Charges)
	rp.recorder.Record(activity)
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
	rp.recorder.NoteUser(user)

	activity := &pb.Activity{
		Agent:           rp.attribution.Agent,
		Request:         RequestFrom(ctx),
		Session:         SessionFrom(ctx),
		User:            user.ID,
		CallerService:   rp.attribution.Service,
		CallerComponent: component,
		Skill:           rp.attribution.Skill,
		Provider:        rp.attribution.provider(),
		DurationMs:      duration.Milliseconds(),
		Status:          pb.Activity_OK,
		OccurredAt:      timestamppb.Now(),
	}

	if err != nil {
		activity.Status = pb.Activity_FAILED
		activity.ErrorCode = errorCode(err)
	}

	for _, charge := range charges {
		activity.Charges = append(activity.Charges, &pb.Charge{
			PriceableUnit: charge.PriceableUnit,
			Quantity:      charge.Quantity,
		})
	}
	return activity
}

// errorCode reduces an error to the one part of it that is safe to keep.
//
// A code says what went wrong in a way a report can group by. A message says the
// same thing in prose and often quotes the prompt that caused it, which is the
// one thing this library must never carry. Taking the error and returning only
// its code is also what stops a caller reaching for err.Error() themselves.
func errorCode(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return codes.DeadlineExceeded.String()
	case errors.Is(err, context.Canceled):
		return codes.Canceled.String()
	}
	return status.Code(err).String()
}
