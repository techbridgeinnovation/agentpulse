package recorder

import (
	"context"
	"errors"
	"net"
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

	mu     sync.Mutex
	seen   []*pb.Activity
	parent string
	err    error
}

func (f *fakeMetering) BatchCreateActivities(_ context.Context, req *pb.BatchCreateActivitiesRequest) (*pb.BatchCreateActivitiesResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parent = req.GetParent()
	f.seen = append(f.seen, req.GetActivities()...)
	return &pb.BatchCreateActivitiesResponse{Activities: req.GetActivities()}, nil
}

func (f *fakeMetering) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen)
}

func serve(t *testing.T, service *fakeMetering) grpc.ClientConnInterface {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	pb.RegisterActivitiesServiceServer(server, service)
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
	if service.parent != "organisations/techbridge" {
		t.Fatalf("parent = %q, want the configured organisation", service.parent)
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
