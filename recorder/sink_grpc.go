package recorder

import (
	"context"
	"fmt"
	"strings"

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
//
// An empty organisation is refused with a panic, at construction and never
// later. Every record this sink sends is filed under the organisation, and
// with none named metering rejects them all, which the recorder would count
// quietly for the life of the process. A setting that is missing should stop
// the process where a person is watching it start, not drop records where
// nobody is.
func NewGRPCSink(conn grpc.ClientConnInterface, organisation string) *GRPCSink {
	if strings.TrimSpace(organisation) == "" {
		panic("recorder: NewGRPCSink needs the organisation the records are filed under")
	}
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
