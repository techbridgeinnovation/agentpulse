package recorder

import (
	"testing"

	"google.golang.org/genai"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

// The turn dealade tested against on 20 September 2026, which is the case that
// found the double count: the prompt count already contains the cached tokens,
// and reading the two as separate piles charges 21,847 tokens at the input rate
// on top of the cache rate they actually earned.
//
// Kept as a fixture rather than invented numbers because a real call is the
// only thing that proves the arithmetic against a provider that exists.
func dealadeTurn() *genai.GenerateContentResponseUsageMetadata {
	return &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:        70679,
		CandidatesTokenCount:    460,
		CachedContentTokenCount: 21847,
		ThoughtsTokenCount:      1653,
		TotalTokenCount:         72792,
	}
}

func quantities(reported []*pb.ReportedQuantity) map[string]int64 {
	out := map[string]int64{}
	for _, q := range reported {
		out[q.GetUnit()] = q.GetQuantity()
	}
	return out
}

func TestTheProvidersOwnNumbersSurviveUntouched(t *testing.T) {
	got := quantities(ReportedFromGenAI(dealadeTurn()))

	for unit, want := range map[string]int64{
		"promptTokenCount":        70679,
		"candidatesTokenCount":    460,
		"cachedContentTokenCount": 21847,
		"thoughtsTokenCount":      1653,
		"totalTokenCount":         72792,
	} {
		if got[unit] != want {
			t.Errorf("%s = %d, want %d as the provider stated it", unit, got[unit], want)
		}
	}
}

// The prompt count is passed on whole, cached tokens and all. Subtracting here
// is the thing this design exists to stop: the split belongs where it can be
// corrected without every adopter shipping a release.
func TestNothingIsSubtractedBeforeTheRecordLeaves(t *testing.T) {
	got := quantities(ReportedFromGenAI(dealadeTurn()))

	if got["promptTokenCount"] != 70679 {
		t.Errorf("promptTokenCount = %d, want the provider's own figure of 70679", got["promptTokenCount"])
	}
	if got["promptTokenCount"]-got["cachedContentTokenCount"] != 48832 {
		t.Error("the server has nothing to subtract from, so it cannot arrive at the 48832 uncached tokens that were actually billed at the input rate")
	}
}

// Audio and image tokens are priced at several times the text rate and are
// reported nowhere but the modality breakdown. A record that drops it prices a
// voice turn as though it were text.
func TestModalitySurvivesSoAVoiceTurnCanBePricedAtAll(t *testing.T) {
	usage := dealadeTurn()
	usage.CandidatesTokensDetails = []*genai.ModalityTokenCount{
		{Modality: genai.MediaModalityAudio, TokenCount: 1840},
		{Modality: genai.MediaModalityText, TokenCount: 460},
	}
	usage.PromptTokensDetails = []*genai.ModalityTokenCount{
		{Modality: genai.MediaModalityImage, TokenCount: 258},
	}

	got := quantities(ReportedFromGenAI(usage))

	for unit, want := range map[string]int64{
		"candidatesTokensDetails.AUDIO": 1840,
		"candidatesTokensDetails.TEXT":  460,
		"promptTokensDetails.IMAGE":     258,
	} {
		if got[unit] != want {
			t.Errorf("%s = %d, want %d", unit, got[unit], want)
		}
	}
}

// Tool results are charged as input and are counted in the provider's total, so
// leaving them out makes the parts disagree with the whole — which is the check
// that tells us a convention was read correctly.
func TestToolResultTokensAreCarriedSoThePartsCanMeetTheTotal(t *testing.T) {
	usage := dealadeTurn()
	usage.ToolUsePromptTokenCount = 1919
	usage.TotalTokenCount = 74711

	got := quantities(ReportedFromGenAI(usage))

	if got["toolUsePromptTokenCount"] != 1919 {
		t.Errorf("toolUsePromptTokenCount = %d, want 1919", got["toolUsePromptTokenCount"])
	}

	// Cached is inside the prompt count, so the whole is prompt, candidates,
	// tool-use and thoughts — exactly what the provider documents its total to be.
	parts := got["promptTokenCount"] + got["candidatesTokenCount"] + got["toolUsePromptTokenCount"] + got["thoughtsTokenCount"]
	if parts != got["totalTokenCount"] {
		t.Errorf("parts sum to %d against a stated total of %d", parts, got["totalTokenCount"])
	}
}

func TestAQuantityTheProviderDidNotReportIsAbsentRatherThanZero(t *testing.T) {
	got := quantities(ReportedFromGenAI(dealadeTurn()))

	if _, present := got["toolUsePromptTokenCount"]; present {
		t.Error("a quantity the provider never reported is on the record, which makes a gap look like a measurement")
	}
}

func TestNoUsageIsNoQuantitiesRatherThanAnEmptyList(t *testing.T) {
	if got := ReportedFromGenAI(nil); got != nil {
		t.Errorf("got %v, want nothing at all", got)
	}
}

func TestACallerReportingByHandIsSortedSoTwoRecordingsCanBeCompared(t *testing.T) {
	got := reportedQuantities(map[string]int64{
		"output_tokens":           460,
		"cache_read_input_tokens": 21847,
		"input_tokens":            48832,
		"blank":                   0,
	})

	want := []string{"cache_read_input_tokens", "input_tokens", "output_tokens"}
	if len(got) != len(want) {
		t.Fatalf("got %d quantities, want %d — a zero is not a measurement and does not belong on the record", len(got), len(want))
	}
	for i, unit := range want {
		if got[i].GetUnit() != unit {
			t.Errorf("quantity %d is %q, want %q", i, got[i].GetUnit(), unit)
		}
	}
}

func TestAProviderStatedByNameWinsOverTheEnumItUsedToBeStatedIn(t *testing.T) {
	if got := BilledByOf(ProviderAnthropic, pb.Activity_VERTEX_AI); got != ProviderAnthropic { //nolint:staticcheck // the enum is what this function exists to read
		t.Errorf("billed_by = %q, want %q", got, ProviderAnthropic)
	}
	if got := BilledByOf("", pb.Activity_OPENAI); got != ProviderOpenAI { //nolint:staticcheck // as above
		t.Errorf("billed_by = %q, want an agent configured before the string existed to still attribute to %q", got, ProviderOpenAI)
	}
	if got := BilledByOf("", pb.Activity_PROVIDER_UNSPECIFIED); got != ProviderVertexAI { //nolint:staticcheck // as above
		t.Errorf("billed_by = %q, want %q", got, ProviderVertexAI)
	}
}
