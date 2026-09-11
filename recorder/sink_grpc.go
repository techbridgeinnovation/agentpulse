package recorder

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

// GRPCSink delivers records to the metering service.
type GRPCSink struct {
	client pb.ActivitiesServiceClient
	parent string
}

// NewGRPCSink wraps an existing connection.
//
// The connection is the caller's, because an agent already has one to the
// platform and dialling a second would double the sockets for no reason. The
// recorder never closes it.
func NewGRPCSink(conn grpc.ClientConnInterface, organisation string) *GRPCSink {
	return &GRPCSink{
		client: pb.NewActivitiesServiceClient(conn),
		parent: organisation,
	}
}

func (s *GRPCSink) Name() string { return "metering" }

// Send writes the batch in one call.
//
// Batched rather than one call per record: an agent turn can make a dozen model
// calls, and a dozen round trips to record them would be the slowest thing in
// the turn. A rejected batch is reported as an error and the recorder counts it
// — retrying here would mean holding records the queue has already accounted
// for, and a duplicate costs more than a gap.
func (s *GRPCSink) Send(ctx context.Context, activities []*pb.Activity) error {
	if len(activities) == 0 {
		return nil
	}

	_, err := s.client.BatchCreateActivities(ctx, &pb.BatchCreateActivitiesRequest{
		Parent:     s.parent,
		Activities: activities,
	})
	if err != nil {
		return fmt.Errorf("recording %d activities: %w", len(activities), err)
	}
	return nil
}
