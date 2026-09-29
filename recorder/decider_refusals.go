package recorder

import (
	"context"
	"sync"
	"time"

	governancepb "github.com/techbridgeinnovation/agentpulse/recorder/pb/governance"
)

// refusalWindow is how long a refusal is still honoured when governance does not answer. A budget that ran out rarely comes back within the hour, and a check slowed by governance waking from idle must not let the call through.
const refusalWindow = time.Hour

// maxRememberedRefusals bounds the memory: past it everything is forgotten and relearned on the next answer.
const maxRememberedRefusals = 4096

// reasonRememberedRefusal is the reason on a refusal repeated from memory rather than given by governance.
const reasonRememberedRefusal = "refused within the hour and governance did not answer in time"

// RememberRefusals wraps a Decider so that a call governance refused in the last hour, for the same agent, person, tenant and model, is refused again when governance cannot answer in time. Any other failure still lets the call go ahead.
func RememberRefusals(inner Decider) Decider {
	if inner == nil {
		return nil
	}
	if _, already := inner.(*rememberingDecider); already {
		return inner
	}
	return &rememberingDecider{inner: inner, refused: map[decisionCacheKey]time.Time{}, now: time.Now}
}

type rememberingDecider struct {
	inner   Decider
	mu      sync.Mutex
	refused map[decisionCacheKey]time.Time
	now     func() time.Time
}

// Decide asks the wrapped Decider, and answers from memory only when it fails.
func (d *rememberingDecider) Decide(ctx context.Context, req *governancepb.DecideRequest) (*governancepb.DecideResponse, error) {
	key := decisionCacheKeyFor(req)
	resp, err := d.inner.Decide(ctx, req)

	d.mu.Lock()
	defer d.mu.Unlock()
	if err == nil {
		if resp.GetDecision() == governancepb.DecideResponse_DENY {
			if len(d.refused) >= maxRememberedRefusals {
				d.refused = map[decisionCacheKey]time.Time{}
			}
			d.refused[key] = d.now()
		} else {
			delete(d.refused, key)
		}
		return resp, nil
	}
	if at, ok := d.refused[key]; ok && d.now().Sub(at) < refusalWindow {
		return &governancepb.DecideResponse{Decision: governancepb.DecideResponse_DENY, Reason: reasonRememberedRefusal}, nil
	}
	return nil, err
}
