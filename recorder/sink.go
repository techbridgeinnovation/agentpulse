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
//
// One batch is one workspace. A tenant is not a field on a record, so it arrives on ctx instead and WorkspaceFrom reads it: a destination that files records under a parent needs it, and one that does not — a log, a collector — is free to ignore it and see only the records. An empty workspace is the ordinary case for a product with one tenant, never a value that went missing on the way.
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
