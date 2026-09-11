package recorder

import (
	"context"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

// Sink is where records go. A team picks one — our metering service, an
// OpenTelemetry collector, BigQuery, a log file, or nothing at all.
//
// A sink is handed a batch and may take as long as its own deadline allows; it
// runs on the recorder's worker, never on the caller's turn. Returning an error
// is fine and expected — the recorder counts it and carries on. A sink must not
// assume it is the only one.
type Sink interface {
	// Send delivers a batch. The slice must not be retained after returning.
	Send(ctx context.Context, activities []*pb.Activity) error

	// Name identifies the sink in the recorder's counters, so a team can tell
	// which destination is failing.
	Name() string
}

// discardSink accepts everything and keeps nothing.
//
// The default, so that a misconfigured recorder is inert rather than broken,
// and so a team that wants the latency and error data without sending cost
// anywhere has something to configure.
type discardSink struct{}

func (discardSink) Send(context.Context, []*pb.Activity) error { return nil }
func (discardSink) Name() string                               { return "discard" }

// Discard returns a sink that drops everything it is given.
func Discard() Sink { return discardSink{} }
