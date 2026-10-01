package recorder

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

// GRPCSink delivers records, and the names behind them, to the metering
// service.
type GRPCSink struct {
	client pb.ActivitiesServiceClient
	users  pb.UsersServiceClient
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
//
// The organisation is fixed here because one process bills one organisation. The workspace is not, because one process can serve many tenants of it, so it comes from each record instead — see WithWorkspace.
func NewGRPCSink(conn grpc.ClientConnInterface, organisation string) *GRPCSink {
	if strings.TrimSpace(organisation) == "" {
		panic("recorder: NewGRPCSink needs the organisation the records are filed under")
	}
	return &GRPCSink{
		client: pb.NewActivitiesServiceClient(conn),
		users:  pb.NewUsersServiceClient(conn),
		parent: organisation,
	}
}

func (s *GRPCSink) Name() string { return "metering" }

// parentFor is what a batch for the given workspace is filed under.
//
// The workspace's own name where the batch named one, and the organisation on its own where it did not. The second is what a recorder that knows nothing of workspaces sends, and it is accepted: the batch lands in the organisation's default workspace. A tenant a caller did not name is never invented here.
func (s *GRPCSink) parentFor(workspace string) string {
	if name := WorkspaceName(s.parent, workspace); name != "" {
		return name
	}
	return s.parent
}

// Send writes the batch in one call.
//
// Batched rather than one call per record: an agent turn can make a dozen model
// calls, and a dozen round trips to record them would be the slowest thing in
// the turn. A batch refused for a reason that can pass is sent again within ctx's deadline, under the same request id so that metering writes it once however many times it arrives. Any other refusal is reported as an error and the recorder counts it.
//
// One call carries one parent, because metering takes a batch or refuses it whole and the parent is where the records' tenant is stated. The workspace it belongs to is on ctx; the recorder hands a sink one workspace at a time, so a call that covers two tenants cannot be made from here.
func (s *GRPCSink) Send(ctx context.Context, activities []*pb.Activity) error {
	if len(activities) == 0 {
		return nil
	}

	req := &pb.BatchCreateActivitiesRequest{
		Parent:     s.parentFor(WorkspaceFrom(ctx)),
		Activities: activities,
		RequestId:  newRequestID(),
	}
	err := sendWithRetry(ctx, func(ctx context.Context) error {
		_, err := s.client.BatchCreateActivities(ctx, req)
		return err
	})
	if err != nil {
		return fmt.Errorf("recording %d activities: %w", len(activities), err)
	}
	return nil
}

// SendUsers names a batch of people under one workspace, in one call.
//
// Each identifier becomes the last segment of the name, which is what makes
// the directory row and the activities that carry it the same person. The
// write replaces what the directory held, so what a product knows today is
// what the report shows.
//
// The workspace is ctx's, the same one the person's records were filed under, because a row in one tenant's directory is no use joining to another tenant's records.
func (s *GRPCSink) SendUsers(ctx context.Context, users []User) error {
	if len(users) == 0 {
		return nil
	}

	parent := s.parentFor(WorkspaceFrom(ctx))

	batch := make([]*pb.User, 0, len(users))
	for _, u := range users {
		batch = append(batch, &pb.User{
			Name:        parent + "/users/" + u.ID,
			DisplayName: u.Name,
			Email:       u.Email,
		})
	}

	req := &pb.BatchUpsertUsersRequest{Parent: parent, Users: batch}
	// An upsert written twice holds what it held after once, so it is sent again the same way a batch of records is.
	err := sendWithRetry(ctx, func(ctx context.Context) error {
		_, err := s.users.BatchUpsertUsers(ctx, req)
		return err
	})
	if err != nil {
		return fmt.Errorf("naming %d users: %w", len(users), err)
	}
	return nil
}

// The bounds on sending one batch again. Every attempt shares the one deadline the recorder gives a send, so retrying never holds the worker longer than a single slow send could.
const (
	maxSendAttempts = 4
	firstRetryWait  = 100 * time.Millisecond
)

// retryableCodes are the refusals that can pass: a service unreachable, overloaded, timed out or in conflict a moment ago may accept the same batch now. Any other code would refuse it again.
var retryableCodes = map[codes.Code]bool{
	codes.Unavailable:       true,
	codes.ResourceExhausted: true,
	codes.Aborted:           true,
	codes.DeadlineExceeded:  true,
}

// sendWithRetry makes one send, and again with a doubling wait between while it is refused for a reason that can pass and ctx has time left.
func sendWithRetry(ctx context.Context, send func(context.Context) error) error {
	wait := firstRetryWait
	for attempt := 1; ; attempt++ {
		err := send(ctx)
		if err == nil || attempt == maxSendAttempts || ctx.Err() != nil || !retryableCodes[status.Code(err)] {
			return err
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
		wait *= 2
	}
}

// newRequestID names one batch, in the form of a random uuid, so that every attempt at it is known to metering as the same write.
func newRequestID() string {
	var b [16]byte
	// Never fails: the standard library ends the process rather than return an error from here.
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
