package recorder

import (
	"context"

	governancepb "github.com/techbridgeinnovation/agentpulse/recorder/pb/governance"
)

// Decider asks governance whether a call may proceed, before it is made.
//
// Kept separate from Sink rather than folded into it: deciding is a
// synchronous request/response on the calling turn's own critical path,
// nothing like a batch handed to the background worker and delivered
// whenever it gets there, so it has no place on Recorder's queue at all.
type Decider interface {
	// Decide asks whether req's call may proceed.
	Decide(ctx context.Context, req *governancepb.DecideRequest) (*governancepb.DecideResponse, error)
}
