package recorder

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

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

// maxReportedErrorFields bounds one failure. Every provider names a failure in
// under a dozen fields, so a caller sending more is sending something else.
const maxReportedErrorFields = 16

// proseFieldNames are the names providers give their error text. A caller
// stating a failure by hand passes whatever its sdk handed it, and the text
// under these names quotes the request back, so they are dropped whoever sent
// them.
var proseFieldNames = map[string]bool{
	"message":           true,
	"localized_message": true,
	"localizedmessage":  true,
	"description":       true,
	"detail":            true,
	"details":           true,
	"error":             true,
	"error_description": true,
}

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
// the fields that identify a failure — a status, a reason, a field path, an http
// code — never do.
//
// An error from an OpenAI-shaped sdk is stated as OpenAI's convention. Use
// ReportedErrorFor where who billed the call is known, because one provider fills
// OpenAI's field names with different meanings.
func ReportedErrorFrom(err error) ReportedFailure {
	return ReportedErrorFor(err, "")
}

// ReportedErrorFor states what an error said about itself, for a call billed by
// billedBy.
//
// Each provider's sdk is read in its own way. The genai client hands back its
// error with every detail Google attached. The OpenAI and Anthropic sdks hand
// back an error holding the raw body and the response it arrived in, which is
// read by the methods both expose rather than by importing either: adopting this
// library must not mean taking on an sdk the adopter does not use.
func ReportedErrorFor(err error, billedBy string) ReportedFailure {
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

	if reported := reportedSDK(err, billedBy); reported.Format != "" {
		return reported
	}

	return reportedTransport(err)
}

// fieldSet collects what one failure said, bounded and in a fixed order.
type fieldSet struct {
	fields []*pb.ReportedField
}

func (s *fieldSet) add(name, value string) {
	name, value = strings.TrimSpace(name), strings.TrimSpace(value)
	if name == "" || value == "" || proseFieldNames[strings.ToLower(name)] {
		return
	}
	if len(s.fields) == maxReportedErrorFields {
		return
	}
	s.fields = append(s.fields, &pb.ReportedField{Name: name, Value: boundValue(value)})
}

// failure is the set stated in format, or nothing where nothing was said.
func (s *fieldSet) failure(format string) ReportedFailure {
	if len(s.fields) == 0 {
		return ReportedFailure{}
	}
	sortFields(s.fields)
	return ReportedFailure{Format: format, Fields: s.fields}
}

// reportedGenAI states a Gemini or Vertex failure under Google's own names.
//
// Google attaches typed details to a failure, and each type answers a different
// question: ErrorInfo names the reason beneath the status, BadRequest names the
// field that was wrong, QuotaFailure names the quota that was hit and its limit,
// RetryInfo says how long to wait, and RequestInfo carries the id Google's
// support looks a call up by. A nested name is written as the path Google gives
// it, e.g. `fieldViolations.field`, so it reads back under the name Google's own
// documentation uses.
//
// The descriptions inside those details are left behind. A field violation's
// description quotes the value that was rejected.
func reportedGenAI(err genai.APIError) ReportedFailure {
	var s fieldSet
	if err.Code != 0 {
		s.add("http_status", strconv.Itoa(err.Code))
	}
	s.add("status", err.Status)
	for _, detail := range err.Details {
		s.add("reason", stringOf(detail["reason"]))
		s.add("domain", stringOf(detail["domain"]))
		s.add("retryDelay", stringOf(detail["retryDelay"]))
		s.add("requestId", stringOf(detail["requestId"]))
		for _, violation := range mapsOf(detail["fieldViolations"]) {
			s.add("fieldViolations.field", stringOf(violation["field"]))
			s.add("fieldViolations.reason", stringOf(violation["reason"]))
		}
		for _, violation := range mapsOf(detail["violations"]) {
			s.add("violations.quotaId", stringOf(violation["quotaId"]))
			s.add("violations.quotaMetric", stringOf(violation["quotaMetric"]))
			s.add("violations.quotaValue", stringOf(violation["quotaValue"]))
		}
	}
	return s.failure(ErrorFormatVertex)
}

// sdkError is an error from an sdk generated the way OpenAI's and Anthropic's
// Go sdks are: it holds the raw error body and can print the response it came
// in. Both expose exactly these two methods.
type sdkError interface {
	RawJSON() string
	DumpResponse(body bool) []byte
}

// reportedSDK states a failure from an OpenAI or Anthropic sdk under that
// provider's own names.
//
// The two bodies differ in where the failure's name sits. Anthropic's is an
// envelope, `{"type": "error", "error": {"type": ...}, "request_id": ...}`, with
// one type and nothing beneath it. OpenAI's error names a `type`, a `code` and
// the `param` that was wrong, and is the shape a great many other providers
// copy. Perplexity copies it and puts an http status where OpenAI puts a word,
// which is why who billed the call decides the convention an OpenAI-shaped body
// is stated in, and the shape alone does not.
//
// The retry wait and the request id arrive as headers rather than in the body,
// and are read from the response the error kept.
func reportedSDK(err error, billedBy string) ReportedFailure {
	var sdk sdkError
	if !errors.As(err, &sdk) {
		return ReportedFailure{}
	}

	var body map[string]any
	if raw := sdk.RawJSON(); raw != "" {
		_ = json.Unmarshal([]byte(raw), &body)
	}
	inner := body
	if nested, ok := body["error"].(map[string]any); ok {
		inner = nested
	}

	var s fieldSet
	status, header := responseOf(sdk)
	if status != 0 {
		s.add("http_status", strconv.Itoa(status))
	}
	s.add("retry_after", header.Get("Retry-After"))

	if stringOf(body["type"]) == "error" {
		s.add("type", stringOf(inner["type"]))
		s.add("request_id", firstOf(stringOf(body["request_id"]), header.Get("Request-Id")))
		return s.failure(ErrorFormatAnthropic)
	}

	s.add("type", stringOf(inner["type"]))
	s.add("code", stringOf(inner["code"]))
	s.add("param", stringOf(inner["param"]))
	s.add("request_id", header.Get("X-Request-Id"))
	if strings.EqualFold(billedBy, ProviderPerplexity) {
		return s.failure(ErrorFormatPerplexity)
	}
	return s.failure(ErrorFormatOpenAI)
}

// responseOf reads the status and headers an sdk error arrived with, without
// its body.
func responseOf(sdk sdkError) (int, http.Header) {
	dump := sdk.DumpResponse(false)
	if len(dump) == 0 {
		return 0, http.Header{}
	}
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(dump)), nil)
	if err != nil {
		return 0, http.Header{}
	}
	return resp.StatusCode, resp.Header
}

// sdkCode is the name an OpenAI or Anthropic sdk error gives its failure, for a
// record's code: the specific code where the provider gave one, and its type
// where it did not.
func sdkCode(err error, billedBy string) string {
	reported := reportedSDK(err, billedBy)
	fields := map[string]string{}
	for _, f := range reported.Fields {
		fields[f.GetName()] = f.GetValue()
	}
	switch reported.Format {
	case ErrorFormatAnthropic, ErrorFormatPerplexity:
		return fields["type"]
	case ErrorFormatOpenAI:
		return firstOf(fields["code"], fields["type"])
	}
	return ""
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
// For a service calling a provider through something this library cannot read —
// an sdk in another shape, a plain http client — where the caller holds the
// error and this does not. The names are the provider's own, not ours: `type`,
// `code`, `param`, `status`, `reason`, `http_status`, `retry_after`,
// `request_id`. A provider's message is dropped whatever it is passed under.
func ReportedErrorOf(format string, fields map[string]string) ReportedFailure {
	if strings.TrimSpace(format) == "" || len(fields) == 0 {
		return ReportedFailure{}
	}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)

	var s fieldSet
	for _, name := range names {
		s.add(name, fields[name])
	}
	return s.failure(strings.TrimSpace(format))
}

// boundValue cuts a value to the length a failure is ever named in, on a
// character boundary: a value cut through the middle of one is not valid text,
// and a record holding it cannot be stored.
func boundValue(value string) string {
	if len(value) <= maxReportedErrorValue {
		return value
	}
	cut := maxReportedErrorValue
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

// stringOf reads a detail value that a provider stated as a string, and is empty
// for one it stated as a structure. A number is written as the digits it was.
func stringOf(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool, int, int64, json.Number:
		return fmt.Sprint(v)
	default:
		return ""
	}
}

// mapsOf reads a detail value that a provider stated as a list of objects.
func mapsOf(value any) []map[string]any {
	list, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// sortFields puts the fields in a fixed order, so two recordings of one failure
// are the same record and can be compared.
func sortFields(fields []*pb.ReportedField) {
	sort.SliceStable(fields, func(i, j int) bool { return fields[i].GetName() < fields[j].GetName() })
}
