package recorder

import (
	"context"
	"errors"
	"testing"
	"time"

	governancepb "github.com/techbridgeinnovation/agentpulse/recorder/pb/governance"
)

type scriptedDecider struct {
	resp *governancepb.DecideResponse
	err  error
}

func (s *scriptedDecider) Decide(context.Context, *governancepb.DecideRequest) (*governancepb.DecideResponse, error) {
	return s.resp, s.err
}

func TestARefusalIsRepeatedWhenGovernanceDoesNotAnswer(t *testing.T) {
	inner := &scriptedDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_DENY}}
	now := time.Unix(1_000_000, 0)
	d := RememberRefusals(inner).(*rememberingDecider)
	d.now = func() time.Time { return now }
	req := &governancepb.DecideRequest{Parent: "organisations/acme", Agent: "organisations/acme/agents/reviewer"}
	other := &governancepb.DecideRequest{Parent: "organisations/acme", Agent: "organisations/acme/agents/other"}

	if resp, err := d.Decide(context.Background(), req); err != nil || resp.GetDecision() != governancepb.DecideResponse_DENY {
		t.Fatalf("first answer = %v, %v", resp, err)
	}

	inner.resp, inner.err = nil, context.DeadlineExceeded
	if resp, err := d.Decide(context.Background(), req); err != nil || resp.GetDecision() != governancepb.DecideResponse_DENY {
		t.Fatalf("a timeout after a refusal = %v, %v, want the refusal repeated", resp, err)
	}
	if _, err := d.Decide(context.Background(), other); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("another agent's timeout = %v, want it left to go ahead", err)
	}

	now = now.Add(refusalWindow)
	if _, err := d.Decide(context.Background(), req); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a timeout an hour on = %v, want the refusal forgotten", err)
	}
}

func TestAnAllowForgetsTheRefusal(t *testing.T) {
	inner := &scriptedDecider{resp: &governancepb.DecideResponse{Decision: governancepb.DecideResponse_DENY}}
	d := RememberRefusals(inner)
	req := &governancepb.DecideRequest{Parent: "organisations/acme", Agent: "organisations/acme/agents/reviewer"}

	_, _ = d.Decide(context.Background(), req)
	inner.resp = &governancepb.DecideResponse{Decision: governancepb.DecideResponse_ALLOW}
	_, _ = d.Decide(context.Background(), req)
	inner.resp, inner.err = nil, errors.New("unavailable")
	if _, err := d.Decide(context.Background(), req); err == nil {
		t.Fatal("a failure after the limit was raised was refused, want it left to go ahead")
	}
}
