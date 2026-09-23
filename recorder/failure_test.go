package recorder

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"google.golang.org/genai"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func fieldsOf(t *testing.T, f ReportedFailure) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, field := range f.Fields {
		out[field.GetName()] = field.GetValue()
	}
	return out
}

// The reason beneath the status is the field that tells a rate which clears on
// its own from a quota which does not, and both arrive as RESOURCE_EXHAUSTED.
func TestAProviderFailureKeepsTheReasonBeneathTheStatus(t *testing.T) {
	got := ReportedErrorFrom(genai.APIError{
		Code:    429,
		Status:  "RESOURCE_EXHAUSTED",
		Message: "Resource exhausted. The prompt was: how do I...",
		Details: []map[string]any{{"reason": "rateLimitExceeded", "domain": "googleapis.com"}},
	})

	if got.Format != ErrorFormatVertex {
		t.Fatalf("format = %q, want %q", got.Format, ErrorFormatVertex)
	}
	fields := fieldsOf(t, got)
	for name, want := range map[string]string{
		"http_status": "429",
		"status":      "RESOURCE_EXHAUSTED",
		"reason":      "rateLimitExceeded",
		"domain":      "googleapis.com",
	} {
		if fields[name] != want {
			t.Errorf("%s = %q, want %q", name, fields[name], want)
		}
	}
}

// The one field that must never survive. A provider's error text quotes the
// prompt, which is the thing this library exists not to carry.
func TestAReportedFailureNeverCarriesTheMessage(t *testing.T) {
	got := ReportedErrorFrom(genai.APIError{
		Code:    400,
		Status:  "INVALID_ARGUMENT",
		Message: "the prompt said: my card number is 4111 1111 1111 1111",
	})

	for _, field := range got.Fields {
		if field.GetValue() == "" {
			continue
		}
		if len(field.GetValue()) > maxReportedErrorValue {
			t.Errorf("%s is %d long, want it bounded", field.GetName(), len(field.GetValue()))
		}
		if field.GetName() == "message" {
			t.Fatalf("a message reached the record: %q", field.GetValue())
		}
	}
	if fieldsOf(t, got)["status"] != "INVALID_ARGUMENT" {
		t.Errorf("status missing, want the failure still named")
	}
}

// A call that never reached a provider has no provider answer to state, and
// reading a transport code as one would file an unreachable host under whatever
// that provider calls the same word.
func TestAFailureThatNeverReachedAProviderIsReportedAsTransport(t *testing.T) {
	for _, err := range []error{
		context.DeadlineExceeded,
		fmt.Errorf("dialling: %w", context.Canceled),
		status.Error(codes.Unavailable, "no healthy upstream"),
	} {
		got := ReportedErrorFrom(err)
		if got.Format != ErrorFormatGRPC {
			t.Errorf("%v: format = %q, want %q", err, got.Format, ErrorFormatGRPC)
		}
		if fieldsOf(t, got)["code"] == "" {
			t.Errorf("%v: no code reported", err)
		}
	}
}

// An error this library cannot read states nothing rather than guessing, so the
// record shows a gap someone can close instead of a class that is quietly wrong.
func TestAnUnreadableFailureStatesNothing(t *testing.T) {
	got := ReportedErrorFrom(errors.New("something went wrong"))
	if got.Format != "" || len(got.Fields) != 0 {
		t.Errorf("got %+v, want nothing stated", got)
	}
	if nothing := ReportedErrorFrom(nil); nothing.Format != "" {
		t.Errorf("nil error reported %+v, want nothing", nothing)
	}
}

// A wrapped provider error is still a provider error: the frameworks wrap, and a
// reader that only matched the top of the chain would see none of them.
func TestAWrappedProviderFailureIsStillRead(t *testing.T) {
	got := ReportedErrorFrom(fmt.Errorf("openai: call failed: %w",
		fmt.Errorf("inner: %w", genai.APIError{Code: 503, Status: "UNAVAILABLE"})))

	if got.Format != ErrorFormatVertex || fieldsOf(t, got)["status"] != "UNAVAILABLE" {
		t.Errorf("got %+v, want the wrapped provider failure read", got)
	}
}

// A caller holding an error this library has no reader for states it itself,
// under the provider's own names.
func TestACallerCanStateAFailureThisLibraryCannotRead(t *testing.T) {
	got := ReportedErrorOf(ErrorFormatAnthropic, map[string]string{
		"http_status": "429",
		"type":        "rate_limit_error",
		"retry_after": "absent",
	})

	if got.Format != ErrorFormatAnthropic {
		t.Fatalf("format = %q, want %q", got.Format, ErrorFormatAnthropic)
	}
	fields := fieldsOf(t, got)
	if fields["type"] != "rate_limit_error" || fields["retry_after"] != "absent" {
		t.Errorf("fields = %v, want what the caller stated", fields)
	}
	// Sorted, so two recordings of one failure are the same record.
	if got.Fields[0].GetName() != "http_status" {
		t.Errorf("first field = %q, want the names in a fixed order", got.Fields[0].GetName())
	}
	if empty := ReportedErrorOf("", map[string]string{"type": "x"}); empty.Format != "" {
		t.Errorf("a format nobody named reported %+v, want nothing", empty)
	}
}

// Google attaches typed details to a failure, and each one answers a question
// the status cannot: which field was wrong, which quota was hit, how long to
// wait, and the id its support looks a call up by. The descriptions inside them
// quote the request and are left behind.
func TestGoogleDetailsAreKeptAndTheirDescriptionsAreNot(t *testing.T) {
	got := ReportedErrorFrom(genai.APIError{
		Code:   429,
		Status: "RESOURCE_EXHAUSTED",
		Details: []map[string]any{
			{"@type": "type.googleapis.com/google.rpc.QuotaFailure", "violations": []any{map[string]any{
				"quotaMetric": "generativelanguage.googleapis.com/generate_content_requests",
				"quotaId":     "GenerateRequestsPerMinutePerProjectPerModel",
				"quotaValue":  "60",
				"description": "quota for project 123 on the prompt 'my card is 4111'",
			}}},
			{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "34s"},
			{"@type": "type.googleapis.com/google.rpc.RequestInfo", "requestId": "req-9"},
		},
	})
	fields := fieldsOf(t, got)
	for name, want := range map[string]string{
		"violations.quotaId":     "GenerateRequestsPerMinutePerProjectPerModel",
		"violations.quotaMetric": "generativelanguage.googleapis.com/generate_content_requests",
		"violations.quotaValue":  "60",
		"retryDelay":             "34s",
		"requestId":              "req-9",
	} {
		if fields[name] != want {
			t.Errorf("%s = %q, want %q", name, fields[name], want)
		}
	}

	invalid := fieldsOf(t, ReportedErrorFrom(genai.APIError{
		Code:   400,
		Status: "INVALID_ARGUMENT",
		Details: []map[string]any{{"@type": "type.googleapis.com/google.rpc.BadRequest", "fieldViolations": []any{map[string]any{
			"field":       "contents[3].parts[0].function_response",
			"description": `Invalid value "my card is 4111"`,
		}}}},
	}))
	if invalid["fieldViolations.field"] != "contents[3].parts[0].function_response" {
		t.Errorf("field = %q, want the path Google named", invalid["fieldViolations.field"])
	}
	for name, value := range invalid {
		if strings.Contains(value, "4111") {
			t.Fatalf("%s carried the request back: %q", name, value)
		}
	}
}

// fakeSDKError is what OpenAI's and Anthropic's Go sdks hand back: the raw body
// and the response it came in, behind the two methods both expose.
type fakeSDKError struct {
	body     string
	response string
}

func (e *fakeSDKError) Error() string            { return "sdk error" }
func (e *fakeSDKError) RawJSON() string          { return e.body }
func (e *fakeSDKError) DumpResponse(bool) []byte { return []byte(e.response) }

// OpenAI names the parameter that was wrong and the specific problem with it,
// and the message beside them quotes the value that was sent.
func TestAnOpenAIFailureNamesTheParameterAndNotTheValue(t *testing.T) {
	err := fmt.Errorf("openai: call failed: %w", &fakeSDKError{
		body:     `{"message":"Invalid 'tools[11].function.name': string too long. Got 'my secret tool'","type":"invalid_request_error","param":"tools[11].function.name","code":"string_above_max_length"}`,
		response: "HTTP/1.1 400 Bad Request\r\nX-Request-Id: req_abc\r\n\r\n",
	})

	got := ReportedErrorFor(err, ProviderOpenAI)
	if got.Format != ErrorFormatOpenAI {
		t.Fatalf("format = %q, want %q", got.Format, ErrorFormatOpenAI)
	}
	fields := fieldsOf(t, got)
	for name, want := range map[string]string{
		"http_status": "400",
		"type":        "invalid_request_error",
		"code":        "string_above_max_length",
		"param":       "tools[11].function.name",
		"request_id":  "req_abc",
	} {
		if fields[name] != want {
			t.Errorf("%s = %q, want %q", name, fields[name], want)
		}
	}
	for name, value := range fields {
		if strings.Contains(value, "secret") {
			t.Fatalf("%s carried the request back: %q", name, value)
		}
	}
	if code := ErrorCode(err); code != "string_above_max_length" {
		t.Errorf("code = %q, want the provider's specific code rather than Unknown", code)
	}
}

// Perplexity uses OpenAI's field names with its own meanings, so who billed the
// call decides the convention and the shape of the body does not.
func TestAnOpenAIShapedFailureBilledByPerplexityIsStatedAsPerplexity(t *testing.T) {
	err := &fakeSDKError{body: `{"type":"bad_request","code":400,"message":"At body -> messages"}`}
	if got := ReportedErrorFor(err, ProviderPerplexity); got.Format != ErrorFormatPerplexity {
		t.Errorf("format = %q, want %q", got.Format, ErrorFormatPerplexity)
	}
	if got := fieldsOf(t, ReportedErrorFor(err, ProviderPerplexity)); got["code"] != "400" {
		t.Errorf("code = %q, want the number written as its digits", got["code"])
	}
}

// Anthropic's body is an envelope with one type in it, and the request id it
// gives is the one its support asks for.
func TestAnAnthropicFailureKeepsItsTypeAndRequestID(t *testing.T) {
	got := ReportedErrorFor(&fakeSDKError{
		body:     `{"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your per-minute rate limit"},"request_id":"req_011CS"}`,
		response: "HTTP/1.1 429 Too Many Requests\r\nRetry-After: 12\r\n\r\n",
	}, ProviderAnthropic)

	if got.Format != ErrorFormatAnthropic {
		t.Fatalf("format = %q, want %q", got.Format, ErrorFormatAnthropic)
	}
	fields := fieldsOf(t, got)
	for name, want := range map[string]string{
		"http_status": "429",
		"type":        "rate_limit_error",
		"request_id":  "req_011CS",
		"retry_after": "12",
	} {
		if fields[name] != want {
			t.Errorf("%s = %q, want %q", name, fields[name], want)
		}
	}
	if _, ok := fields["message"]; ok {
		t.Error("a message reached the record")
	}
}

// A caller stating a failure by hand passes whatever its sdk handed it. A
// message under any of the names providers give one is dropped, and a caller
// sending far more than a failure is ever named in is cut off.
func TestAFailureStatedByHandLosesItsMessageAndIsBounded(t *testing.T) {
	stated := map[string]string{"type": "rate_limit_error", "message": "the prompt said...", "Description": "more prose"}
	for i := range 40 {
		stated[fmt.Sprintf("extra_%02d", i)] = "x"
	}
	got := ReportedErrorOf(ErrorFormatAnthropic, stated)

	fields := fieldsOf(t, got)
	if _, ok := fields["message"]; ok {
		t.Error("message kept, want it dropped")
	}
	if _, ok := fields["Description"]; ok {
		t.Error("description kept, want it dropped")
	}
	if len(got.Fields) > maxReportedErrorFields {
		t.Errorf("%d fields kept, want at most %d", len(got.Fields), maxReportedErrorFields)
	}
}

// A value cut through the middle of a character is not valid text, and a record
// holding one cannot be stored at all.
func TestALongValueIsCutOnACharacterBoundary(t *testing.T) {
	value := strings.Repeat("a", maxReportedErrorValue-1) + "é" + "tail"
	got := boundValue(value)
	if !utf8.ValidString(got) {
		t.Fatalf("cut value is not valid text: %q", got[len(got)-4:])
	}
	if len(got) > maxReportedErrorValue {
		t.Errorf("length = %d, want at most %d", len(got), maxReportedErrorValue)
	}
}
