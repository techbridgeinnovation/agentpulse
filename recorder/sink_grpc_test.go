package recorder

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

// fakeMetering stands in for the metering service over a real gRPC connection,
// so the sink is exercised through actual serialisation rather than a stub.
type fakeMetering struct {
	pb.UnimplementedActivitiesServiceServer
	pb.UnimplementedUsersServiceServer

	mu sync.Mutex
	// parents is the parent of each call, one entry per call, so a test can
	// see how a flush was split as well as where it landed.
	seen        []*pb.Activity
	parents     []string
	users       []*pb.User
	userParents []string
	err         error
}

func (f *fakeMetering) BatchCreateActivities(_ context.Context, req *pb.BatchCreateActivitiesRequest) (*pb.BatchCreateActivitiesResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parents = append(f.parents, req.GetParent())
	f.seen = append(f.seen, req.GetActivities()...)
	return &pb.BatchCreateActivitiesResponse{Activities: req.GetActivities()}, nil
}

func (f *fakeMetering) BatchUpsertUsers(_ context.Context, req *pb.BatchUpsertUsersRequest) (*pb.BatchUpsertUsersResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.userParents = append(f.userParents, req.GetParent())
	f.users = append(f.users, req.GetUsers()...)
	return &pb.BatchUpsertUsersResponse{Users: req.GetUsers()}, nil
}

func (f *fakeMetering) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen)
}

// filedUnder is the parent of each batch of activities the service was given.
func (f *fakeMetering) filedUnder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.parents...)
}

// namedUnder is the parent of each batch of names the service was given.
func (f *fakeMetering) namedUnder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.userParents...)
}

func serve(t *testing.T, service *fakeMetering) grpc.ClientConnInterface {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	pb.RegisterActivitiesServiceServer(server, service)
	pb.RegisterUsersServiceServer(server, service)
	go func() { _ = server.Serve(listener) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dialling the fake service: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		server.Stop()
	})
	return conn
}

func TestGRPCSinkSendsOneCallPerBatchNotPerRecord(t *testing.T) {
	service := &fakeMetering{}
	sink := NewGRPCSink(serve(t, service), "organisations/techbridge")

	err := sink.Send(context.Background(), []*pb.Activity{
		activity("a"), activity("b"), activity("c"),
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := service.count(); got != 3 {
		t.Fatalf("service saw %d activities, want 3", got)
	}
	if got := service.filedUnder(); len(got) != 1 || got[0] != "organisations/techbridge" {
		t.Fatalf("filed under %v, want one batch under the configured organisation", got)
	}
}

func TestGRPCSinkReportsARejectedBatchWithoutRetrying(t *testing.T) {
	service := &fakeMetering{err: status.Error(codes.PermissionDenied, "no")}
	sink := NewGRPCSink(serve(t, service), "organisations/techbridge")

	// Reported, not retried: the queue has already accounted for these, and a
	// duplicate record costs more than a gap.
	if err := sink.Send(context.Background(), []*pb.Activity{activity("a")}); err == nil {
		t.Fatal("Send returned nil for a rejected batch")
	}
	if got := service.count(); got != 0 {
		t.Fatalf("service stored %d activities despite refusing, want 0", got)
	}
}

func TestGRPCSinkSendingNothingIsNotACall(t *testing.T) {
	service := &fakeMetering{err: errors.New("must not be called")}
	sink := NewGRPCSink(serve(t, service), "organisations/techbridge")

	if err := sink.Send(context.Background(), nil); err != nil {
		t.Fatalf("Send with no activities returned %v, want nil", err)
	}
}

func TestARecorderWiredToTheServiceDeliversThroughIt(t *testing.T) {
	service := &fakeMetering{}
	sink := NewGRPCSink(serve(t, service), "organisations/techbridge")

	r := New(Config{Sinks: []Sink{sink}, FlushEvery: 10 * time.Millisecond})
	r.Record(activity("a"))
	r.Record(activity("b"))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r.Close(ctx)

	if got := service.count(); got != 2 {
		t.Fatalf("service saw %d activities, want 2", got)
	}
}

func TestGRPCSinkFilesAWorkspacesRecordsUnderThatWorkspace(t *testing.T) {
	service := &fakeMetering{}
	sink := NewGRPCSink(serve(t, service), "organisations/techbridge")

	ctx := WithWorkspace(context.Background(), "acme")
	if err := sink.Send(ctx, []*pb.Activity{activity("a")}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	want := "organisations/techbridge/workspaces/acme"
	if got := service.filedUnder(); len(got) != 1 || got[0] != want {
		t.Fatalf("filed under %v, want %q", got, want)
	}
}

// A recorder that names no workspace goes on filing under the organisation, and
// one that names several sends a batch for each. Both in one flush, because
// both are the same flush in a process serving one tenant among many.
func TestARecorderSendsOneBatchPerWorkspaceWithTheRightParent(t *testing.T) {
	service := &fakeMetering{}
	sink := NewGRPCSink(serve(t, service), "organisations/techbridge")

	r := New(Config{Sinks: []Sink{sink}, FlushEvery: time.Hour})
	r.Record(activity("a"))
	r.RecordIn(WithWorkspace(context.Background(), "acme"), activity("b"))
	r.RecordIn(WithWorkspace(context.Background(), "globex"), activity("c"))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r.Close(ctx)

	got := service.filedUnder()
	slices.Sort(got)
	want := []string{
		"organisations/techbridge",
		"organisations/techbridge/workspaces/acme",
		"organisations/techbridge/workspaces/globex",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("filed under %v, want %v", got, want)
	}
	if n := service.count(); n != 3 {
		t.Fatalf("service saw %d activities, want 3", n)
	}
}

func TestGRPCSinkNamesAPersonInTheWorkspaceTheyWereSeenIn(t *testing.T) {
	service := &fakeMetering{}
	sink := NewGRPCSink(serve(t, service), "organisations/techbridge")

	if err := sink.SendUsers(context.Background(), []User{ada}); err != nil {
		t.Fatalf("SendUsers: %v", err)
	}
	if err := sink.SendUsers(WithWorkspace(context.Background(), "acme"), []User{ada}); err != nil {
		t.Fatalf("SendUsers in a workspace: %v", err)
	}

	want := []string{"organisations/techbridge", "organisations/techbridge/workspaces/acme"}
	if got := service.namedUnder(); !slices.Equal(got, want) {
		t.Fatalf("named under %v, want %v", got, want)
	}

	// The row's own name sits under the same parent, which is what makes it the
	// row the workspace's records join to.
	wantNames := []string{
		"organisations/techbridge/users/" + ada.ID,
		"organisations/techbridge/workspaces/acme/users/" + ada.ID,
	}
	for i, user := range service.users {
		if user.GetName() != wantNames[i] {
			t.Errorf("user %d is named %q, want %q", i, user.GetName(), wantNames[i])
		}
	}
}
