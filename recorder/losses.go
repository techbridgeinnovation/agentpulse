package recorder

import (
	"context"
	"reflect"
	"strings"
	"sync"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

// LossSink is a sink that also carries what the recorder lost on the batches it delivers, so a loss shows where spend is read and not only in this process.
//
// Losses is nil when there is nothing to report. A send that returns an error leaves those losses to the next batch, so the sink must not count them as delivered itself.
type LossSink interface {
	Sink
	SendWithLosses(ctx context.Context, activities []*pb.Activity, losses *pb.RecorderLosses) error
}

// recorderName is this recorder's language and version as metering reads it, such as go/0.1.1.
var recorderName = func() string {
	// The package path is the module path in both copies of this code, so the version is read the same way wherever it was installed from.
	version := strings.TrimPrefix(ModuleVersion(reflect.TypeOf(Recorder{}).PkgPath()), "v")
	if version == "" || version == "(devel)" {
		return "go"
	}
	return "go/" + version
}()

// carriedLosses is what one sink has told its destination about the recorder's losses.
//
// The recorder's counters stay cumulative, and a batch carries the difference between them and what was already reported. A claim is held apart while its batch is in flight, so two batches sent at once never report the same loss twice, and it counts as reported only once its batch is accepted.
type carriedLosses struct {
	sink LossSink

	mu sync.Mutex
	// undelivered is this sink's own, because a record one destination refused may have reached another.
	undelivered int64
	reported    lossCounts
	inFlight    lossCounts
}

type lossCounts struct {
	dropped, undelivered, panicked, unrecognised int64
}

func (a lossCounts) minus(b lossCounts) lossCounts {
	return lossCounts{a.dropped - b.dropped, a.undelivered - b.undelivered, a.panicked - b.panicked, a.unrecognised - b.unrecognised}
}

func (a lossCounts) plus(b lossCounts) lossCounts {
	return lossCounts{a.dropped + b.dropped, a.undelivered + b.undelivered, a.panicked + b.panicked, a.unrecognised + b.unrecognised}
}

// carriedFor keeps a ledger for each sink that carries losses, in the order of sinks, with nil for one that does not.
func carriedFor(sinks []Sink) []*carriedLosses {
	out := make([]*carriedLosses, len(sinks))
	for i, sink := range sinks {
		if carrier, ok := sink.(LossSink); ok {
			out[i] = &carriedLosses{sink: carrier}
		}
	}
	return out
}

// claim takes what has not been reported or claimed yet, and returns it with nil meaning nothing.
func (c *carriedLosses) claim(counters *Counters) (lossCounts, *pb.RecorderLosses) {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := lossCounts{
		dropped:      counters.Dropped.Load(),
		undelivered:  c.undelivered,
		panicked:     counters.Panicked.Load(),
		unrecognised: counters.Unrecorded.Load(),
	}
	claimed := total.minus(c.reported).minus(c.inFlight)
	if claimed == (lossCounts{}) {
		return claimed, nil
	}
	c.inFlight = c.inFlight.plus(claimed)
	return claimed, &pb.RecorderLosses{
		Dropped:           claimed.dropped,
		Undelivered:       claimed.undelivered,
		Panicked:          claimed.panicked,
		UnrecognisedCalls: claimed.unrecognised,
		Recorder:          recorderName,
	}
}

// settle closes a claim. Accepted, it is reported; refused, it is free for the next batch to claim, and the batch's own records join what was not delivered.
func (c *carriedLosses) settle(claimed lossCounts, accepted bool, records int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inFlight = c.inFlight.minus(claimed)
	if accepted {
		c.reported = c.reported.plus(claimed)
		return
	}
	c.undelivered += int64(records)
}
