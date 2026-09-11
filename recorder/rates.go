package recorder

import (
	"context"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

// RateSource supplies the prices the running total is measured against.
//
// Kept separate from Sink rather than folded into it, for the opposite reason Decider is: a sink pushes records out, a rate source pulls prices in, and the pull happens on a background schedule of its own that has nothing to do with when records are delivered.
type RateSource interface {
	// ListRates returns the whole rate card.
	//
	// Called on a background schedule and never on the path of a model call, so it may take as long as its context allows.
	ListRates(ctx context.Context) ([]*pb.PriceableUnit, error)
}
