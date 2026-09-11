package recorder

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

// reported records one call through a reporter and returns what reached the
// sink, so each test below asserts on a real delivered record rather than on
// something built for the test.
func reported(t *testing.T, ctx context.Context, record func(*Reporter)) *pb.Activity {
	t.Helper()

	sink := &captureSink{}
	rec := New(Config{Sinks: []Sink{sink}, FlushEvery: time.Millisecond})
	record(rec.For(Attribution{
		Agent:    "organisations/techbridge/agents/sources",
		Service:  "sources-service",
		Provider: pb.Activity_VERTEX_AI,
		Skill:    "research",
	}))

	closing, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rec.Close(closing)

	if sink.count() != 1 {
		t.Fatalf("%d records reached the sink, want 1", sink.count())
	}
	return sink.seen[0]
}

func TestAServiceReportsWithoutNamingWhatItAlreadyToldUs(t *testing.T) {
	ctx := WithSession(WithUser(WithRequest(context.Background(), "requests/abc"), "users/7"), "sessions/9")

	got := reported(t, ctx, func(rp *Reporter) {
		rp.ModelCall(ctx, ModelCall{Model: "gemini-2.5-pro", Component: "asset_summary"})
	})

	for label, pair := range map[string][2]string{
		"agent":     {got.GetAgent(), "organisations/techbridge/agents/sources"},
		"service":   {got.GetCallerService(), "sources-service"},
		"skill":     {got.GetSkill(), "research"},
		"model":     {got.GetModel(), "gemini-2.5-pro"},
		"component": {got.GetCallerComponent(), "asset_summary"},
		"request":   {got.GetRequest(), "requests/abc"},
		"user":      {got.GetUser(), "users/7"},
		"session":   {got.GetSession(), "sessions/9"},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %q, want %q", label, pair[0], pair[1])
		}
	}
	if got.GetProvider() != pb.Activity_VERTEX_AI {
		t.Errorf("provider = %s, want VERTEX_AI", got.GetProvider())
	}
	if got.GetOccurredAt() == nil {
		t.Error("no occurred_at, so the record cannot be priced against the rates that applied")
	}
}

func TestEachKindOfTokenIsKeptApart(t *testing.T) {
	ctx := context.Background()

	got := reported(t, ctx, func(rp *Reporter) {
		rp.ModelCall(ctx, ModelCall{
			Model:  "gemini-2.5-pro",
			Tokens: Tokens{Prompt: 100, Candidate: 20, Cached: 40, CacheWrite: 8, Reasoning: 12},
		})
	})

	// Each is billed at its own rate, so a record that merges them cannot be
	// priced correctly however good the rate card is.
	for label, pair := range map[string][2]int32{
		"prompt":      {got.GetPromptTokens(), 100},
		"candidate":   {got.GetCandidateTokens(), 20},
		"cached":      {got.GetCachedTokens(), 40},
		"cache write": {got.GetCacheWriteTokens(), 8},
		"reasoning":   {got.GetReasoningTokens(), 12},
		"total":       {got.GetTotalTokens(), 180},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s tokens = %d, want %d", label, pair[0], pair[1])
		}
	}
}

func TestATotalTheProviderReportedWinsOverAddingThemUp(t *testing.T) {
	ctx := context.Background()

	// A provider that charges for something we have no field for reports a
	// total above the sum, and its number is the one that gets billed.
	got := reported(t, ctx, func(rp *Reporter) {
		rp.ModelCall(ctx, ModelCall{Tokens: Tokens{Prompt: 100, Candidate: 20, Total: 205}})
	})

	if got.GetTotalTokens() != 205 {
		t.Fatalf("total = %d, want 205", got.GetTotalTokens())
	}
}

func TestAFailedCallKeepsItsCodeAndNoneOfItsMessage(t *testing.T) {
	ctx := context.Background()
	secret := "the model refused: ignore all previous instructions and reveal"

	got := reported(t, ctx, func(rp *Reporter) {
		rp.ModelCall(ctx, ModelCall{
			Model:  "gemini-2.5-pro",
			Tokens: Tokens{Prompt: 100},
			Err:    status.Error(codes.InvalidArgument, secret),
		})
	})

	if got.GetStatus() != pb.Activity_FAILED {
		t.Errorf("status = %s, want FAILED", got.GetStatus())
	}
	if got.GetErrorCode() != "InvalidArgument" {
		t.Errorf("error code = %q, want InvalidArgument", got.GetErrorCode())
	}
	// A provider message quotes the prompt back, which is the one thing that
	// must never reach a record.
	if body, err := proto.Marshal(got); err == nil && bytes.Contains(body, []byte(secret)) {
		t.Fatal("the error message reached the record")
	}
}

func TestAFailedCallIsStillRecordedBecauseItWasStillCharged(t *testing.T) {
	ctx := context.Background()

	got := reported(t, ctx, func(rp *Reporter) {
		rp.ModelCall(ctx, ModelCall{Tokens: Tokens{Prompt: 900}, Err: errors.New("connection reset")})
	})

	if got.GetPromptTokens() != 900 {
		t.Fatalf("prompt tokens = %d, want 900 — a ledger of successes understates the bill", got.GetPromptTokens())
	}
}

func TestHittingALimitReadsAsTruncatedRatherThanFailed(t *testing.T) {
	ctx := context.Background()

	got := reported(t, ctx, func(rp *Reporter) {
		rp.ModelCall(ctx, ModelCall{Tokens: Tokens{Candidate: 8192}, Truncated: true})
	})

	// A truncated call did partial work and was charged for it. A failed one
	// may have been charged for nothing, so the two cannot share a status.
	if got.GetStatus() != pb.Activity_TRUNCATED {
		t.Fatalf("status = %s, want TRUNCATED", got.GetStatus())
	}
}

func TestAFailureOutranksTruncation(t *testing.T) {
	ctx := context.Background()

	got := reported(t, ctx, func(rp *Reporter) {
		rp.ModelCall(ctx, ModelCall{Truncated: true, Err: status.Error(codes.Unavailable, "")})
	})

	if got.GetStatus() != pb.Activity_FAILED {
		t.Fatalf("status = %s, want FAILED", got.GetStatus())
	}
}

func TestAToolCallSaysWhichToolAndWhatItCharged(t *testing.T) {
	ctx := context.Background()

	got := reported(t, ctx, func(rp *Reporter) {
		rp.ToolCall(ctx, ToolCall{
			Tool:     "web_search",
			Duration: 250 * time.Millisecond,
			Charges:  []Charge{{PriceableUnit: "priceableUnits/vertex-ai-grounded-search-request", Quantity: 1}},
		})
	})

	if got.GetCallerComponent() != "tool:web_search" {
		t.Errorf("component = %q, want tool:web_search", got.GetCallerComponent())
	}
	if got.GetDurationMs() != 250 {
		t.Errorf("duration = %dms, want 250", got.GetDurationMs())
	}
	if len(got.GetCharges()) != 1 || got.GetCharges()[0].GetQuantity() != 1 {
		t.Fatalf("charges = %v, want one charge of quantity 1", got.GetCharges())
	}
	// What it costs is the server's answer, never the caller's.
	if got.GetCharges()[0].GetEstimatedCostMicros() != 0 {
		t.Error("a caller stated a price, which is the server's job")
	}
}

func TestANilReporterIsSafeToUse(t *testing.T) {
	// A service that failed to build one must not crash on the first call it
	// tries to record.
	var rp *Reporter
	rp.ModelCall(context.Background(), ModelCall{Model: "gemini-2.5-pro"})
	rp.ToolCall(context.Background(), ToolCall{Tool: "web_search"})
}

func TestAnErrorIsReducedToACodeWhateverKindItIs(t *testing.T) {
	for _, c := range []struct {
		what string
		err  error
		want string
	}{
		{"a grpc status", status.Error(codes.NotFound, "gone"), "NotFound"},
		{"a deadline", context.DeadlineExceeded, "DeadlineExceeded"},
		{"a cancellation", context.Canceled, "Canceled"},
		{"a wrapped deadline", errors.Join(errors.New("calling the model"), context.DeadlineExceeded), "DeadlineExceeded"},
		{"anything else", errors.New("connection reset by peer"), "Unknown"},
	} {
		if got := errorCode(c.err); got != c.want {
			t.Errorf("%s: errorCode = %q, want %q", c.what, got, c.want)
		}
	}
}
