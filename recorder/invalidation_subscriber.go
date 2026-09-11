package recorder

import (
	"context"
	"time"

	"google.golang.org/grpc"

	governancepb "github.com/techbridgeinnovation/agentpulse/recorder/pb/governance"
)

// reconnectDelay bounds how long Run waits before retrying a stream that
// ended, whether from an error or a clean close. Fixed and short rather
// than backed off: this stream is a latency optimisation on top of a
// cache's own TTL, never a guarantee, so there is nothing to protect by
// backing off harder — see InvalidationSubscriber's own doc.
const reconnectDelay = 50 * time.Millisecond

// InvalidationSubscriber keeps a CachingDecider's cached verdicts fresh by
// consuming governance's StreamDecisionInvalidations and applying every
// message to the cache via InvalidateScope.
//
// Optional and separate from Decider/CachingDecider themselves: nothing
// here is required for the cache to work correctly. Without a running
// subscriber, cached verdicts simply expire on their own TTL exactly as
// they would if this type did not exist — streaming only ever shortens
// that window, it does not replace it. This is what keeps it opt-in: a
// caller wires one in only if they want fresher invalidation, and its
// absence changes nothing about existing Decide/cache/fail-open behavior.
type InvalidationSubscriber struct {
	client governancepb.DecisionsServiceClient
	cache  *CachingDecider
}

// NewInvalidationSubscriber wraps an existing connection.
//
// The connection is the caller's, following the same pattern as
// NewGRPCDecider and NewGRPCSink: an agent already has one to the
// platform, and dialling a second would double the sockets for no reason.
// The subscriber never closes it.
func NewInvalidationSubscriber(conn grpc.ClientConnInterface, cache *CachingDecider) *InvalidationSubscriber {
	return &InvalidationSubscriber{client: governancepb.NewDecisionsServiceClient(conn), cache: cache}
}

// Run subscribes to invalidations for parent and applies them to the cache
// until ctx is done, reconnecting after reconnectDelay whenever the stream
// ends for any reason.
//
// Run blocks and is meant to be started in its own goroutine
// (`go subscriber.Run(ctx, parent)`); the caller stops it by cancelling
// ctx, the same way every other lifecycle in this library is caller-owned
// (Recorder.Close, GRPCDecider/GRPCSink never closing their connection).
// There is no other shutdown signal and none is needed: Run never holds
// anything that must be released beyond ctx being done.
func (s *InvalidationSubscriber) Run(ctx context.Context, parent string) {
	for ctx.Err() == nil {
		s.consume(ctx, parent)

		select {
		case <-ctx.Done():
			return
		case <-time.After(reconnectDelay):
		}
	}
}

// consume opens one stream and applies every invalidation it delivers,
// returning as soon as the stream ends — from an error, a clean close, or
// ctx being done. Never returns an error itself: whatever ended the stream,
// Run's own loop decides what happens next.
func (s *InvalidationSubscriber) consume(ctx context.Context, parent string) {
	stream, err := s.client.StreamDecisionInvalidations(ctx, &governancepb.StreamDecisionInvalidationsRequest{Parent: parent})
	if err != nil {
		return
	}

	for {
		invalidation, err := stream.Recv()
		if err != nil {
			return
		}
		s.cache.InvalidateScope(invalidation.GetParent(), invalidation.GetProduct(), invalidation.GetUser(), invalidation.GetAgent())
	}
}
