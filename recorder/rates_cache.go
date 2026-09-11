package recorder

import (
	"context"
	"sync/atomic"
)

// rateCache holds the rate card in force for this process.
//
// One pointer, swapped wholesale on a refresh. Reading it is a single atomic load, which is what lets the recording path price a record without taking a lock the refresh could be holding.
//
// A cache with no card yet is the ordinary state of a process that has just started, and it is also the state a failed refresh leaves behind. Both price at nothing. Degrading to nothing is the correct failure mode here: the running total is a local convenience on top of what metering prices authoritatively, so an agent with no rates carries on exactly as an agent with no rate source configured at all.
type rateCache struct {
	source RateSource
	card   atomic.Pointer[rateCard]
}

func newRateCache(source RateSource) *rateCache {
	return &rateCache{source: source}
}

// current returns the card in hand, or nil when no fetch has succeeded yet.
//
// Safe on a nil cache, because a recorder configured with no rate source holds none and the recording path calls this on every record either way.
func (c *rateCache) current() *rateCard {
	if c == nil {
		return nil
	}
	return c.card.Load()
}

// refresh fetches the card once and replaces what is held.
//
// An error, or a card with nothing in it, leaves the previous card in place. A rate card that has gone briefly unreadable is not a reason to stop pricing against the prices already known to be right.
func (c *rateCache) refresh(ctx context.Context) error {
	units, err := c.source.ListRates(ctx)
	if err != nil {
		return err
	}
	if len(units) == 0 {
		return nil
	}
	c.card.Store(&rateCard{units: units})
	return nil
}
