package recorder

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

func (f *fakeMetering) carried() []*pb.RecorderLosses {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*pb.RecorderLosses(nil), f.losses...)
}

func (f *fakeMetering) refuseWith(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func closeWithin(t *testing.T, r *Recorder) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r.Close(ctx)
}

func TestADroppedRecordIsReportedOnTheNextAcceptedBatch(t *testing.T) {
	service := &fakeMetering{}
	r := New(Config{Sinks: []Sink{NewGRPCSink(serve(t, service), "organisations/techbridge")}, FlushEvery: time.Hour})
	r.counters.Dropped.Add(2)
	r.noteUnrecorded()
	r.NotePanicked()

	r.Record(activity("a"))
	closeWithin(t, r)

	carried := service.carried()
	if len(carried) != 1 {
		t.Fatalf("service saw %d calls, want 1", len(carried))
	}
	got := carried[0]
	if got.GetDropped() != 2 || got.GetUnrecognisedCalls() != 1 || got.GetPanicked() != 1 || got.GetUndelivered() != 0 {
		t.Errorf("losses %v, want 2 dropped, 1 unrecognised, 1 panicked", got)
	}
	if !strings.HasPrefix(got.GetRecorder(), "go") {
		t.Errorf("recorder %q, want it to name go", got.GetRecorder())
	}
	if s := r.Stats(); s.Dropped != 2 || s.Unrecorded != 1 {
		t.Errorf("stats %+v, want the cumulative counts left as they were", s)
	}
}

func TestLossesSurviveAFailedSendAndAreReportedOnce(t *testing.T) {
	service := &fakeMetering{err: status.Error(codes.InvalidArgument, "refused")}
	r := New(Config{Sinks: []Sink{NewGRPCSink(serve(t, service), "organisations/techbridge")}, FlushEvery: 10 * time.Millisecond})
	r.counters.Dropped.Add(1)

	r.Record(activity("a"))
	waitFor(t, "the refused batch", func() bool { return r.Stats().Failed == 1 })
	service.refuseWith(nil)

	r.Record(activity("b"))
	waitFor(t, "the accepted batch", func() bool { return r.Stats().Delivered == 1 })
	r.Record(activity("c"))
	closeWithin(t, r)

	carried := service.carried()
	if len(carried) != 3 {
		t.Fatalf("service saw %d calls, want 3", len(carried))
	}
	if carried[0].GetDropped() != 1 || carried[0].GetUndelivered() != 0 {
		t.Errorf("refused batch carried %v, want 1 dropped", carried[0])
	}
	if carried[1].GetDropped() != 1 || carried[1].GetUndelivered() != 1 {
		t.Errorf("accepted batch carried %v, want 1 dropped and the refused batch's record as undelivered", carried[1])
	}
	if carried[2] != nil {
		t.Errorf("batch after the report carried %v, want nothing", carried[2])
	}
}

func TestABatchWithNothingLostCarriesNoLosses(t *testing.T) {
	service := &fakeMetering{}
	r := New(Config{Sinks: []Sink{NewGRPCSink(serve(t, service), "organisations/techbridge")}, FlushEvery: time.Hour})
	r.Record(activity("a"))
	closeWithin(t, r)

	carried := service.carried()
	if len(carried) != 1 || carried[0] != nil {
		t.Errorf("service saw losses %v, want one call carrying none", carried)
	}
}

func TestEveryRetryOfABatchCarriesTheSameLosses(t *testing.T) {
	service := &fakeMetering{refusals: []error{
		status.Error(codes.Unavailable, "down"),
		status.Error(codes.Unavailable, "down"),
	}}
	r := New(Config{Sinks: []Sink{NewGRPCSink(serve(t, service), "organisations/techbridge")}, FlushEvery: time.Hour})
	r.counters.Dropped.Add(3)

	r.Record(activity("a"))
	closeWithin(t, r)

	carried := service.carried()
	if len(carried) != 3 {
		t.Fatalf("service saw %d attempts, want 3", len(carried))
	}
	if carried[0].GetDropped() != 3 {
		t.Errorf("first attempt carried %v, want 3 dropped", carried[0])
	}
	for i, losses := range carried[1:] {
		if !proto.Equal(losses, carried[0]) {
			t.Errorf("attempt %d carried %v, want %v", i+2, losses, carried[0])
		}
	}
	ids := service.requestIDs
	if ids[0] != ids[1] || ids[1] != ids[2] {
		t.Errorf("request ids %v, want one id on every attempt", ids)
	}
}

func TestBatchesSentAtOnceNeverReportTheSameLossTwice(t *testing.T) {
	service := &fakeMetering{}
	r := New(Config{Sinks: []Sink{NewGRPCSink(serve(t, service), "organisations/techbridge")}, FlushEvery: time.Hour})
	r.counters.Dropped.Add(1)

	for _, workspace := range []string{"north", "south", "east"} {
		r.RecordIn(WithWorkspace(context.Background(), workspace), activity("a"))
	}
	closeWithin(t, r)

	var dropped int64
	for _, losses := range service.carried() {
		dropped += losses.GetDropped()
	}
	if dropped != 1 {
		t.Errorf("batches reported %d dropped between them, want 1", dropped)
	}
}
