package recorder

import (
	"strings"
	"time"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

// rateCard is the set of prices the running total is measured against.
//
// Immutable once built. A refresh replaces the whole card rather than editing one, so pricing a record never takes a lock and never reads a card halfway through a change.
type rateCard struct {
	units []*pb.PriceableUnit
}

// priceOf returns what an activity is expected to cost, in millionths of a dollar.
//
// Deliberately not the same figure metering arrives at, and the difference is the point of the split. This one is priced from the classes the hooks fill in, which is the cruder reading a library can make without knowing a provider's conventions; metering prices from what the provider actually said. It exists so a spend decision can read what a request has spent without a network call per model call, and it is never stored or billed against.
//
// Where the two differ this one reads high, because it counts a token served from cache as ordinary input. A running total that is too high stops a budget slightly early, which is the safe direction for the one figure that refuses work — and the figure a customer sees is always the server's.
//
// A token kind with no matching rate contributes nothing rather than failing, for the same reason it does server-side: a model there is no price for yet must never stop an agent from running.
func (c *rateCard) priceOf(a *pb.Activity) int64 {
	if c == nil || a == nil {
		return 0
	}

	// Priced against the rates in force when the work happened, so a provider changing its prices never rewrites what history cost.
	at := time.Now()
	if a.GetOccurredAt() != nil {
		at = a.GetOccurredAt().AsTime()
	}

	// One rate per kind, the most specific that applies, as metering chooses it. The card holds a standard, batch, priority, long-prompt and regional rate for the same tokens, and adding them all would count each token several times over.
	billedBy := BilledByOf(a.GetBilledBy(), a.GetProvider()) //nolint:staticcheck // a record written before billed_by existed states its provider only in the enum
	var best [8]*pb.PriceableUnit
	for _, unit := range c.units {
		if !inForce(unit, at) || !applies(unit, a, billedBy) {
			continue
		}
		quantity, priced := quantityOf(a, unit.GetKind())
		if !priced || quantity == 0 {
			continue
		}
		k := int(unit.GetKind())
		if k < 0 || k >= len(best) {
			continue
		}
		if held := best[k]; held == nil || moreSpecific(unit, held) {
			best[k] = unit
		}
	}
	var total int64
	for _, unit := range best {
		if unit == nil {
			continue
		}
		quantity, _ := quantityOf(a, unit.GetKind())
		total += costOf(quantity, unit.GetUnitCostNanos())
	}

	// Charges the caller reported that are not measured in tokens, such as a per-request search fee.
	for _, charge := range a.GetCharges() {
		for _, unit := range c.units {
			if !inForce(unit, at) {
				continue
			}
			if unit.GetName() == charge.GetPriceableUnit() {
				total += costOf(charge.GetQuantity(), unit.GetUnitCostNanos())
				break
			}
		}
	}

	return total
}

// applies mirrors metering's rule for whether a rate is one of the rates for a call.
func applies(unit *pb.PriceableUnit, a *pb.Activity, billedBy string) bool {
	if unit.GetProvider() != billedBy {
		return false
	}
	// An empty model on a rate means it applies whatever the model.
	if unit.GetModel() != "" && unit.GetModel() != a.GetModel() {
		return false
	}
	if unit.GetServiceTier() != "" && unit.GetServiceTier() != a.GetServiceTier() {
		return false
	}
	if int64(unit.GetMinPromptTokens()) > int64(a.GetPromptTokens())+int64(a.GetCachedTokens()) {
		return false
	}
	if unit.GetMinCacheWriteTtlSeconds() > a.GetCacheWriteTtlSeconds() {
		return false
	}
	if !inRegion(unit.GetRegion(), a.GetRegion()) {
		return false
	}
	// Modality rates price quantities an activity does not split out, so none of them applies.
	return unit.GetModality() == ""
}

// inRegion mirrors metering: an empty rate region is any region, REGIONAL is any region but the global default, and anything else must match.
func inRegion(rate, call string) bool {
	switch {
	case rate == "":
		return true
	case strings.EqualFold(rate, "REGIONAL"):
		return call != "" && !strings.EqualFold(call, "global")
	default:
		return strings.EqualFold(rate, call)
	}
}

// moreSpecific mirrors metering's order for choosing between two matching rates.
func moreSpecific(unit, held *pb.PriceableUnit) bool {
	if named, wasNamed := unit.GetModel() != "", held.GetModel() != ""; named != wasNamed {
		return named
	}
	if named, wasNamed := unit.GetServiceTier() != "", held.GetServiceTier() != ""; named != wasNamed {
		return named
	}
	if unit.GetMinPromptTokens() != held.GetMinPromptTokens() {
		return unit.GetMinPromptTokens() > held.GetMinPromptTokens()
	}
	if unit.GetMinCacheWriteTtlSeconds() != held.GetMinCacheWriteTtlSeconds() {
		return unit.GetMinCacheWriteTtlSeconds() > held.GetMinCacheWriteTtlSeconds()
	}
	if named, wasNamed := unit.GetRegion() != "", held.GetRegion() != ""; named != wasNamed {
		return named
	}
	if from, heldFrom := unit.GetEffectiveFrom().AsTime(), held.GetEffectiveFrom().AsTime(); !from.Equal(heldFrom) {
		return from.After(heldFrom)
	}
	return unit.GetName() < held.GetName()
}

// inForce says whether a rate applied at the given moment.
func inForce(unit *pb.PriceableUnit, at time.Time) bool {
	if unit.GetEffectiveFrom() != nil && unit.GetEffectiveFrom().AsTime().After(at) {
		return false
	}
	if unit.GetEffectiveTo() != nil && !unit.GetEffectiveTo().AsTime().After(at) {
		return false
	}
	return true
}

// quantityOf returns how much of a priceable kind an activity used, and whether that kind is one an activity carries a count for at all.
//
// A switch rather than a map built per call, because this runs in the host's own goroutine on the way out of a model call and has no business allocating there.
func quantityOf(a *pb.Activity, kind pb.PriceableUnit_Kind) (int64, bool) {
	switch kind {
	case pb.PriceableUnit_PROMPT_TOKENS:
		return int64(a.GetPromptTokens()), true
	case pb.PriceableUnit_CANDIDATE_TOKENS:
		return int64(a.GetCandidateTokens()), true
	case pb.PriceableUnit_CACHED_TOKENS:
		return int64(a.GetCachedTokens()), true
	case pb.PriceableUnit_CACHE_WRITE_TOKENS:
		return int64(a.GetCacheWriteTokens()), true
	case pb.PriceableUnit_REASONING_TOKENS:
		return int64(a.GetReasoningTokens()), true
	}
	return 0, false
}

// costOf multiplies a quantity by a rate in billionths and returns millionths.
//
// Rounded to the nearest millionth rather than truncated. Truncating would make every small charge free, and a thousand free charges is a real number missing from a bill.
func costOf(quantity, nanos int64) int64 {
	total := quantity * nanos
	if total == 0 {
		return 0
	}
	return (total + 500) / 1000
}
