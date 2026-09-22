package recorder

import (
	"context"
	"errors"
	"fmt"
	"testing"

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
