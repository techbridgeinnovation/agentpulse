package recorder

import (
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
// This is the same arithmetic metering runs server-side, and it has to stay the same. The server figure is the one that is stored and billed against; this one exists so a spend decision can read what a request has spent without a network call per model call. Two figures for the same work that disagree are worse than one figure and a gap.
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

	var total int64
	for _, unit := range c.units {
		if !inForce(unit, at) {
			continue
		}
		if unit.GetProvider() != BilledByOf(a.GetBilledBy(), a.GetProvider()) { //nolint:staticcheck // a record written before billed_by existed states its provider only in the enum
			continue
		}
		// An empty model on a rate means it applies whatever the model.
		if unit.GetModel() != "" && unit.GetModel() != a.GetModel() {
			continue
		}
		quantity, priced := quantityOf(a, unit.GetKind())
		if !priced || quantity == 0 {
			continue
		}
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
