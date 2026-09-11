package recorder

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

// The figures asserted here are metering's own.
//
// Every case below is one of the cases in metering/v1/internal/pricing, with the same rates, the same counts and the same expected total. That package is internal to its module and cannot be imported, so agreement between the two is held by asserting the same numbers rather than by calling the same code. A change to either side that moves a figure breaks one of the two suites, which is the point: a local figure that disagrees with the stored one is worse than no local figure.

func rate(name, provider, model string, kind pb.PriceableUnit_Kind, nanos int64, from, to time.Time) *pb.PriceableUnit {
	u := &pb.PriceableUnit{
		Name:            name,
		Provider:        provider,
		Model:           model,
		Kind:            kind,
		UnitCostNanos:   nanos,
		RateCardVersion: "2026-09",
		EffectiveFrom:   timestamppb.New(from),
	}
	if !to.IsZero() {
		u.EffectiveTo = timestamppb.New(to)
	}
	return u
}

func TestEachTokenKindIsMultipliedByItsOwnRate(t *testing.T) {
	now := time.Now()
	card := &rateCard{units: []*pb.PriceableUnit{
		rate("priceableUnits/in", "VERTEX_AI", "gemini-2.5-pro", pb.PriceableUnit_PROMPT_TOKENS, 2000, now.Add(-time.Hour), time.Time{}),
		rate("priceableUnits/out", "VERTEX_AI", "gemini-2.5-pro", pb.PriceableUnit_CANDIDATE_TOKENS, 10000, now.Add(-time.Hour), time.Time{}),
		rate("priceableUnits/cached", "VERTEX_AI", "gemini-2.5-pro", pb.PriceableUnit_CACHED_TOKENS, 1000, now.Add(-time.Hour), time.Time{}),
	}}

	got := card.priceOf(&pb.Activity{
		Provider:        pb.Activity_VERTEX_AI,
		Model:           "gemini-2.5-pro",
		PromptTokens:    1000,
		CandidateTokens: 500,
		CachedTokens:    200,
		OccurredAt:      timestamppb.New(now),
	})

	// 1000*2 + 500*10 + 200*1
	if want := int64(7200); got != want {
		t.Fatalf("priced at %d, want %d", got, want)
	}
}

// Cached tokens are billed differently from ordinary prompt tokens. Charging them at the prompt rate is a real defect elsewhere in the estate, so it is worth a test that would catch it here.
func TestCachedTokensAreNotChargedAtThePromptRate(t *testing.T) {
	now := time.Now()
	card := &rateCard{units: []*pb.PriceableUnit{
		rate("priceableUnits/in", "ANTHROPIC", "", pb.PriceableUnit_PROMPT_TOKENS, 100000, now.Add(-time.Hour), time.Time{}),
		rate("priceableUnits/cached", "ANTHROPIC", "", pb.PriceableUnit_CACHED_TOKENS, 10000, now.Add(-time.Hour), time.Time{}),
	}}

	got := card.priceOf(&pb.Activity{
		Provider:     pb.Activity_ANTHROPIC,
		CachedTokens: 1000,
		OccurredAt:   timestamppb.New(now),
	})

	if want := int64(10000); got != want {
		t.Fatalf("cached tokens priced at %d, want %d: they must not use the prompt rate", got, want)
	}
}

func TestOnlyRatesInForceWhenTheWorkHappenedArePriced(t *testing.T) {
	now := time.Now()
	card := &rateCard{units: []*pb.PriceableUnit{
		rate("priceableUnits/old", "OPENAI", "", pb.PriceableUnit_PROMPT_TOKENS, 5000, now.Add(-48*time.Hour), now.Add(-24*time.Hour)),
		rate("priceableUnits/new", "OPENAI", "", pb.PriceableUnit_PROMPT_TOKENS, 9000, now.Add(-24*time.Hour), time.Time{}),
	}}

	yesterday := &pb.Activity{
		Provider:     pb.Activity_OPENAI,
		PromptTokens: 100,
		OccurredAt:   timestamppb.New(now.Add(-36 * time.Hour)),
	}
	if got := card.priceOf(yesterday); got != 500 {
		t.Fatalf("work done under the old rate priced at %d, want 500", got)
	}

	today := &pb.Activity{
		Provider:     pb.Activity_OPENAI,
		PromptTokens: 100,
		OccurredAt:   timestamppb.New(now),
	}
	if got := card.priceOf(today); got != 900 {
		t.Fatalf("work done under the current rate priced at %d, want 900", got)
	}
}

// A model with no rate must not stop an agent recording that it ran, and must not make the running total wrong in the direction that stops work.
func TestAModelWithNoRateCostsNothingRatherThanFailing(t *testing.T) {
	now := time.Now()
	card := &rateCard{units: []*pb.PriceableUnit{
		rate("priceableUnits/in", "VERTEX_AI", "gemini-2.5-pro", pb.PriceableUnit_PROMPT_TOKENS, 2000, now.Add(-time.Hour), time.Time{}),
	}}

	got := card.priceOf(&pb.Activity{
		Provider:     pb.Activity_VERTEX_AI,
		Model:        "some-model-we-have-no-price-for",
		PromptTokens: 1000,
		OccurredAt:   timestamppb.New(now),
	})

	if got != 0 {
		t.Fatalf("unpriced model returned %d, want 0", got)
	}
}

func TestChargesNotMeasuredInTokensArePriced(t *testing.T) {
	now := time.Now()
	card := &rateCard{units: []*pb.PriceableUnit{
		rate("priceableUnits/search", "VERTEX_AI", "", pb.PriceableUnit_REQUEST, 35000000, now.Add(-time.Hour), time.Time{}),
	}}

	got := card.priceOf(&pb.Activity{
		Provider:   pb.Activity_VERTEX_AI,
		Charges:    []*pb.Charge{{PriceableUnit: "priceableUnits/search", Quantity: 3}},
		OccurredAt: timestamppb.New(now),
	})

	if want := int64(105000); got != want {
		t.Fatalf("priced at %d, want %d", got, want)
	}
}

// Gemini input is 1.25 millionths of a dollar per token. Held as a whole number of millionths it rounds to 1 and undercounts by a fifth, which is why a rate is billionths.
func TestAFractionalRatePricesExactlyRatherThanRoundingDown(t *testing.T) {
	now := time.Now()
	card := &rateCard{units: []*pb.PriceableUnit{
		rate("priceableUnits/gemini-in", "VERTEX_AI", "gemini-2.5-pro", pb.PriceableUnit_PROMPT_TOKENS, 1250, now.Add(-time.Hour), time.Time{}),
	}}

	got := card.priceOf(&pb.Activity{
		Provider:     pb.Activity_VERTEX_AI,
		Model:        "gemini-2.5-pro",
		PromptTokens: 4000,
		OccurredAt:   timestamppb.New(now),
	})

	if want := int64(5000); got != want {
		t.Fatalf("priced at %d, want %d: a fractional rate was rounded away", got, want)
	}
}

// A charge small enough to fall below a millionth must not become free.
func TestAChargeBelowOneMillionthIsNotFree(t *testing.T) {
	now := time.Now()
	card := &rateCard{units: []*pb.PriceableUnit{
		rate("priceableUnits/tiny", "OPENAI", "", pb.PriceableUnit_PROMPT_TOKENS, 600, now.Add(-time.Hour), time.Time{}),
	}}

	got := card.priceOf(&pb.Activity{
		Provider:     pb.Activity_OPENAI,
		PromptTokens: 1,
		OccurredAt:   timestamppb.New(now),
	})

	// 0.6 of a millionth rounds to 1 rather than to nothing.
	if got != 1 {
		t.Fatalf("priced at %d, want 1: a sub-millionth charge was rounded to free", got)
	}
}

// A card that was never fetched prices at nothing rather than reaching through a nil pointer, which is the state every process is in for its first moments.
func TestNoCardPricesAtNothing(t *testing.T) {
	var card *rateCard

	if got := card.priceOf(&pb.Activity{Provider: pb.Activity_VERTEX_AI, PromptTokens: 1000}); got != 0 {
		t.Fatalf("priced at %d with no card, want 0", got)
	}
}
