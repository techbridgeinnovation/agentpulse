package recorder

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	governancepb "github.com/techbridgeinnovation/agentpulse/recorder/pb/governance"
)

// fakeGovernance stands in for the governance service over a real gRPC
// connection, so the decider is exercised through actual serialisation
// rather than a stub.
type fakeGovernance struct {
	governancepb.UnimplementedDecisionsServiceServer

	got  *governancepb.DecideRequest
	resp *governancepb.DecideResponse
	err  error
}

func (f *fakeGovernance) Decide(_ context.Context, req *governancepb.DecideRequest) (*governancepb.DecideResponse, error) {
	f.got = req
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func serveGovernance(t *testing.T, service *fakeGovernance) grpc.ClientConnInterface {
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

func TestGRPCDeciderCallsDecideOverGRPC(t *testing.T) {
	service := &fakeGovernance{resp: &governancepb.DecideResponse{
		Decision:      governancepb.DecideResponse_ALLOW,
		Reason:        "within budget",
		PolicyVersion: "monthly-notify-v1",
	}}
	decider := NewGRPCDecider(serveGovernance(t, service))

	req := &governancepb.DecideRequest{
		Parent:  "organisations/techbridge",
		Agent:   "organisations/techbridge/agents/atlas",
		Product: "rezco",
		User:    "users/jane",
	}
	resp, err := decider.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if service.got.GetParent() != req.GetParent() || service.got.GetAgent() != req.GetAgent() ||
		service.got.GetProduct() != req.GetProduct() || service.got.GetUser() != req.GetUser() {
		t.Fatalf("service received %+v, want %+v", service.got, req)
	}
	if resp.GetDecision() != governancepb.DecideResponse_ALLOW {
		t.Fatalf("decision = %v, want ALLOW", resp.GetDecision())
	}
}

func TestGRPCDeciderPropagatesAnError(t *testing.T) {
	service := &fakeGovernance{err: status.Error(codes.FailedPrecondition, "budget has no configured accounting period")}
	decider := NewGRPCDecider(serveGovernance(t, service))

	_, err := decider.Decide(context.Background(), &governancepb.DecideRequest{
		Parent:  "organisations/techbridge",
		Agent:   "organisations/techbridge/agents/atlas",
		Product: "rezco",
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("got %v, want FailedPrecondition", status.Code(err))
	}
}
