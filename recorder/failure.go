package recorder

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/genai"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

// The conventions a failure can be reported in, named on a record beside what
// was reported.
//
// Which shape a failure is in decides where its identity lives, and providers
// put it in different places: Gemini states a status with a reason beneath it,
// OpenAI states a type and a code that mean different things, Anthropic states
// one type. Stated rather than guessed, for the same reason a usage format is.
const (
	ErrorFormatVertex     = "VERTEX"
	ErrorFormatAnthropic  = "ANTHROPIC"
	ErrorFormatOpenAI     = "OPENAI"
	ErrorFormatPerplexity = "PERPLEXITY"

	// ErrorFormatGRPC is a failure that never reached a provider, or reached one
	// through something that reduced it to a transport status: a deadline, a
	// cancelled turn, an unreachable host.
	ErrorFormatGRPC = "GRPC"
)

// maxReportedErrorValue bounds one value. A failure is named in a word or two,
// so anything longer is prose that has arrived where prose must never be, and it
// is cut rather than carried.
const maxReportedErrorValue = 200

// ReportedFailure is what a caller was told about a failure, under the names it
// was told in, and which convention those names belong to.
type ReportedFailure struct {
	Format string
	Fields []*pb.ReportedField
}

// ReportedErrorFrom states what an error said about itself, without deciding
// what it means.
//
// The deciding happens on the server, which is the whole point: this library is
// compiled into somebody else's agent, so a judgement made here can only be
// corrected by every adopter upgrading, and one made there is corrected by one
// deploy and reapplied to records already written. A failure reduced to a single
// word before it leaves the process cannot be reread, and a word this library
// could not place is a record that says `Unknown` for ever.
//
// Nothing here is a message. A provider's error text quotes the prompt back, and
// the fields that identify a failure — a status, a reason, an http code — never
// do.
func ReportedErrorFrom(err error) ReportedFailure {
	if err == nil {
		return ReportedFailure{}
	}

	var apiErr genai.APIError
	if errors.As(err, &apiErr) {
		return reportedGenAI(apiErr)
	}
	var apiErrPtr *genai.APIError
	if errors.As(err, &apiErrPtr) && apiErrPtr != nil {
		return reportedGenAI(*apiErrPtr)
	}

	return reportedTransport(err)
}

// reportedGenAI states a Gemini or Vertex failure under Google's own names.
//
// The reason beneath the status is the field worth carrying and the one most
// easily lost: `RESOURCE_EXHAUSTED` is the same status for a rate that will
// clear on its own and a quota that will not, and `rateLimitExceeded` beside it
// is what tells them apart.
func reportedGenAI(err genai.APIError) ReportedFailure {
	fields := make([]*pb.ReportedField, 0, 4)
	add := func(name, value string) {
		if v := strings.TrimSpace(value); v != "" {
			if len(v) > maxReportedErrorValue {
				v = v[:maxReportedErrorValue]
			}
			fields = append(fields, &pb.ReportedField{Name: name, Value: v})
		}
	}

	if err.Code != 0 {
		add("http_status", strconv.Itoa(err.Code))
	}
	add("status", err.Status)
	for _, detail := range err.Details {
		add("reason", stringOf(detail["reason"]))
		add("domain", stringOf(detail["domain"]))
	}

	if len(fields) == 0 {
		return ReportedFailure{}
	}
	return ReportedFailure{Format: ErrorFormatVertex, Fields: fields}
}

// reportedTransport states a failure that never carried a provider's own answer.
//
// A deadline, a cancelled turn, an unreachable host: the call did not get far
// enough to be refused, so what there is to say is what the transport said. It
// is reported in its own convention rather than as a provider's, because reading
// a grpc code as a provider status would file an unreachable host under whatever
// that provider happens to call the same word.
func reportedTransport(err error) ReportedFailure {
	code := transportCode(err)
	if code == "" {
		return ReportedFailure{}
	}
	return ReportedFailure{Format: ErrorFormatGRPC, Fields: []*pb.ReportedField{{Name: "code", Value: code}}}
}

// transportCode names a failure that arrived without a provider's answer, and is
// empty for one that did not arrive that way at all.
func transportCode(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return codes.DeadlineExceeded.String()
	case errors.Is(err, context.Canceled):
		return codes.Canceled.String()
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return codes.DeadlineExceeded.String()
	}
	if s, ok := status.FromError(err); ok && s.Code() != codes.Unknown {
		return s.Code().String()
	}
	return ""
}

// ReportedErrorOf states a failure a caller has already read for itself, under
// the provider's own names.
//
// For a service calling a provider this library has no reader for — Anthropic,
// OpenAI, anything else — where the caller holds the error object and this does
// not. The names are the provider's own, not ours: `type`, `code`, `status`,
// `reason`, `http_status`.
func ReportedErrorOf(format string, fields map[string]string) ReportedFailure {
	if strings.TrimSpace(format) == "" || len(fields) == 0 {
		return ReportedFailure{}
	}
	out := make([]*pb.ReportedField, 0, len(fields))
	for name, value := range fields {
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if name == "" || value == "" {
			continue
		}
		if len(value) > maxReportedErrorValue {
			value = value[:maxReportedErrorValue]
		}
		out = append(out, &pb.ReportedField{Name: name, Value: value})
	}
	if len(out) == 0 {
		return ReportedFailure{}
	}
	sortFields(out)
	return ReportedFailure{Format: format, Fields: out}
}

// stringOf reads a detail value that a provider stated as a string, and is empty
// for one it stated as anything else.
func stringOf(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

// sortFields puts the names in a fixed order, so two recordings of one failure
// are the same record and can be compared.
func sortFields(fields []*pb.ReportedField) {
	sort.Slice(fields, func(i, j int) bool { return fields[i].GetName() < fields[j].GetName() })
}
