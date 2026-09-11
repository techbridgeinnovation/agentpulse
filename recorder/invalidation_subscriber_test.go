package recorder

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	governancepb "github.com/techbridgeinnovation/agentpulse/recorder/pb/governance"
)

// fakeInvalidationServer stands in for governance's streaming RPC. It
// forwards whatever a test sends on send to the connected client, and ends
// the stream — however many times it is called — as soon as the request
// context is done, err is set, or send is closed.
type fakeInvalidationServer struct {
	governancepb.UnimplementedDecisionsServiceServer

	send chan *governancepb.DecisionInvalidation
	err  error
}

func (f *fakeInvalidationServer) StreamDecisionInvalidations(_ *governancepb.StreamDecisionInvalidationsRequest, stream governancepb.DecisionsService_StreamDecisionInvalidationsServer) error {
	if f.err != nil {
		return f.err
	}
	for {
		select {
		case invalidation, ok := <-f.send:
			if !ok {
				return nil
			}
			if err := stream.Send(invalidation); err != nil {
				return err
			}
		case <-stream.Context().Done():
			return nil
		}
	}
}

func serveInvalidations(t *testing.T, service *fakeInvalidationServer) grpc.ClientConnInterface {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	governancepb.RegisterDecisionsServiceServer(server, service)
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

// waitUntil polls cond until it reports true or the deadline passes,
// failing the test in the latter case. Used instead of a fixed sleep
// because delivery through the subscriber's own goroutine and stream is
// not otherwise synchronized with the test.
func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met in time")
}

func TestInvalidationSubscriberRemovesMatchingEntryAndLeavesUnrelatedOnesCached(t *testing.T) {
	inner := &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_ALLOW}}
	cache := NewCachingDecider(inner, time.Minute)

	target := testRequest()
	unrelated := testRequest()
	unrelated.User = "users/moses"
	resp := &governancepb.DecideResponse{Decision: governancepb.DecideResponse_ALLOW}
	cache.Store(target.GetParent(), target.GetProduct(), target.GetUser(), target.GetAgent(), resp)
	cache.Store(unrelated.GetParent(), unrelated.GetProduct(), unrelated.GetUser(), unrelated.GetAgent(), resp)

	server := &fakeInvalidationServer{send: make(chan *governancepb.DecisionInvalidation, 1)}
	conn := serveInvalidations(t, server)
	subscriber := NewInvalidationSubscriber(conn, cache)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go subscriber.Run(ctx, target.GetParent())

	server.send <- &governancepb.DecisionInvalidation{
		Parent:  target.GetParent(),
		Product: target.GetProduct(),
		User:    target.GetUser(),
		Agent:   target.GetAgent(),
	}

	waitUntil(t, func() bool {
		_, ok := cache.Get(target.GetParent(), target.GetProduct(), target.GetUser(), target.GetAgent())
		return !ok
	})

	// The unrelated entry (a different user) was never named by the
	// invalidation and must still be cached.
	if _, ok := cache.Get(unrelated.GetParent(), unrelated.GetProduct(), unrelated.GetUser(), unrelated.GetAgent()); !ok {
		t.Fatal("an unrelated cache entry was removed by an invalidation that did not name it")
	}

	// The next lookup for the invalidated context must go through Decide
	// again rather than answer from a since-removed cache entry.
	before := inner.callCount()
	if _, err := cache.Decide(context.Background(), target); err != nil {
		t.Fatalf("Decide after invalidation: %v", err)
	}
	if inner.callCount() != before+1 {
		t.Fatalf("wrapped Decider called %d times after invalidation, want %d — the cache should have missed and read through", inner.callCount(), before+1)
	}
}

func TestInvalidationSubscriberFailureDoesNotDisruptTheCache(t *testing.T) {
	inner := &fakeDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_ALLOW}}
	cache := NewCachingDecider(inner, time.Minute)

	server := &fakeInvalidationServer{err: status.Error(codes.Unavailable, "governance unreachable")}
	conn := serveInvalidations(t, server)
	subscriber := NewInvalidationSubscriber(conn, cache)

	ctx, cancel := context.WithCancel(context.Background())
	go subscriber.Run(ctx, "organisations/techbridge")
	// Let the subscriber fail and retry a few times in the background.
	time.Sleep(5 * reconnectDelay)

	req := testRequest()
	if _, err := cache.Decide(context.Background(), req); err != nil {
		t.Fatalf("Decide with a failing subscriber running: %v", err)
	}
	if _, ok := cache.Get(req.GetParent(), req.GetProduct(), req.GetUser(), req.GetAgent()); !ok {
		t.Fatal("a failing subscriber prevented the cache from working normally")
	}
	if inner.callCount() != 1 {
		t.Fatalf("wrapped Decider called %d times, want 1 — the cache miss/store path is unaffected by the subscriber", inner.callCount())
	}

	cancel()
}
