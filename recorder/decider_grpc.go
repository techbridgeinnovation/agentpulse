package recorder

import (
	"context"

	"google.golang.org/grpc"

	governancepb "github.com/techbridgeinnovation/agentpulse/recorder/pb/governance"
)

// GRPCDecider asks the governance service for a decision.
type GRPCDecider struct {
	client governancepb.DecisionsServiceClient
}

// NewGRPCDecider wraps an existing connection.
//
// The connection is the caller's, following the same pattern as
// NewGRPCSink: an agent already has one to the platform, and dialling a
// second would double the sockets for no reason. The recorder never closes
// it.
func NewGRPCDecider(conn grpc.ClientConnInterface) *GRPCDecider {
	return &GRPCDecider{client: governancepb.NewDecisionsServiceClient(conn)}
}

// Decide asks governance for a verdict.
func (d *GRPCDecider) Decide(ctx context.Context, req *governancepb.DecideRequest) (*governancepb.DecideResponse, error) {
	return d.client.Decide(ctx, req)
}
