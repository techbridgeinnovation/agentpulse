package recorder

import (
	"sort"
	"strings"

	"google.golang.org/genai"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

// The reporting conventions a set of token counts can arrive in.
//
// Which one a record is in decides how its numbers are read, and it is not the same question as who billed: Claude served through Vertex is billed by Google and reports Vertex-shaped usage. Stated on the record because providers disagree about what their own headline numbers contain, and reading one convention as another charges for the same token twice.
const (
	FormatVertex          = "VERTEX"
	FormatAnthropic       = "ANTHROPIC"
	FormatOpenAIChat      = "OPENAI_CHAT"
	FormatOpenAIResponses = "OPENAI_RESPONSES"

	// FormatPerplexity is OpenAI's chat field names with Perplexity's own counts beside them, such as the citation tokens its search charges for.
	FormatPerplexity = "PERPLEXITY"
)

// The providers that bill for a call, in the vocabulary the rate card prices against.
//
// Constants for the ones we call today, and a plain string on the wire so that calling a provider nobody has called before needs no contract change and no release of this library.
const (
	ProviderVertexAI   = "VERTEX_AI"
	ProviderAnthropic  = "ANTHROPIC"
	ProviderOpenAI     = "OPENAI"
	ProviderPerplexity = "PERPLEXITY"
)

// reportedQuantities turns what a caller read off its provider into what the record carries.
//
// Sorted by unit, because a record whose fields land in a different order on every call is one nothing can be diffed against, and comparing two recordings of the same call is how a convention gets checked.
func reportedQuantities(reported map[string]int64) []*pb.ReportedQuantity {
	if len(reported) == 0 {
		return nil
	}
	units := make([]string, 0, len(reported))
	for unit := range reported {
		if strings.TrimSpace(unit) != "" && reported[unit] != 0 {
			units = append(units, unit)
		}
	}
	sort.Strings(units)

	quantities := make([]*pb.ReportedQuantity, 0, len(units))
	for _, unit := range units {
		quantities = append(quantities, &pb.ReportedQuantity{Unit: unit, Quantity: reported[unit]})
	}
	return quantities
}

// BilledByOf names the provider that billed for a call.
//
// Takes what the adopter stated, and falls back to reading the enum the same thing used to be stated in, so that an agent configured before `BilledBy` existed keeps attributing its spend to the right provider without being touched.
//
//nolint:staticcheck // Reading the deprecated enum is the whole job of this function.
func BilledByOf(billedBy string, provider pb.Activity_Provider) string {
	if billedBy != "" {
		return billedBy
	}
	switch provider {
	case pb.Activity_ANTHROPIC:
		return ProviderAnthropic
	case pb.Activity_OPENAI:
		return ProviderOpenAI
	case pb.Activity_PERPLEXITY:
		return ProviderPerplexity
	default:
		return ProviderVertexAI
	}
}

// ReportedFromGenAI states what the genai client said about a call, under the names it said them.
//
// Nothing is added, subtracted or renamed here, and that is the point. This library runs inside somebody else's agent, so an interpretation it bakes into a number can only be corrected by every adopter upgrading and redeploying; one the server makes can be corrected by one deploy of ours, and can be reapplied to records already written because what the provider actually said is still on them.
//
// Quantities reported as zero are left out. A provider reporting nothing and a provider reporting none are the same thing to a bill.
func ReportedFromGenAI(usage *genai.GenerateContentResponseUsageMetadata) []*pb.ReportedQuantity {
	if usage == nil {
		return nil
	}

	reported := make([]*pb.ReportedQuantity, 0, 8)
	add := func(unit string, quantity int32) {
		if quantity != 0 {
			reported = append(reported, &pb.ReportedQuantity{Unit: unit, Quantity: int64(quantity)})
		}
	}

	add("promptTokenCount", usage.PromptTokenCount)
	add("candidatesTokenCount", usage.CandidatesTokenCount)
	add("cachedContentTokenCount", usage.CachedContentTokenCount)
	add("thoughtsTokenCount", usage.ThoughtsTokenCount)
	add("toolUsePromptTokenCount", usage.ToolUsePromptTokenCount)
	add("totalTokenCount", usage.TotalTokenCount)

	// The modality breakdowns, which are the only place it is visible that some of these tokens were audio or image rather than text. They are priced at their own rates, several times the text rate on both providers that report them, so a call that carries them and says so is the difference between a voice agent's bill being right and being a guess.
	addModalities(add, "promptTokensDetails", usage.PromptTokensDetails)
	addModalities(add, "candidatesTokensDetails", usage.CandidatesTokensDetails)
	addModalities(add, "cacheTokensDetails", usage.CacheTokensDetails)
	addModalities(add, "toolUsePromptTokensDetails", usage.ToolUsePromptTokensDetails)

	if len(reported) == 0 {
		return nil
	}
	return reported
}

// addModalities names each modality's share of a count, e.g. `candidatesTokensDetails.AUDIO`.
//
// A breakdown of a count already reported rather than a count of its own, which is why the parent name is kept as a prefix: whatever reads these has to be able to tell a part from a whole, and the name is the only thing that says which it is.
func addModalities(add func(string, int32), parent string, counts []*genai.ModalityTokenCount) {
	for _, count := range counts {
		if count == nil {
			continue
		}
		modality := strings.TrimSpace(string(count.Modality))
		if modality == "" {
			continue
		}
		add(parent+"."+modality, count.TokenCount)
	}
}
