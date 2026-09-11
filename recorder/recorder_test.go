package recorder

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

// captureSink keeps what it was given, so a test can assert delivery.
type captureSink struct {
	mu    sync.Mutex
	seen  []*pb.Activity
	err   error
	panic bool
	block time.Duration
}

func (c *captureSink) Send(ctx context.Context, activities []*pb.Activity) error {
	if c.panic {
		panic("this sink is broken")
	}
	if c.block > 0 {
		select {
		case <-time.After(c.block):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, activities...)
	return c.err
}

func (c *captureSink) Name() string { return "capture" }

func (c *captureSink) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.seen)
}

func activity(name string) *pb.Activity {
	return &pb.Activity{Name: name, Request: "req-1"}
}

func TestRecordDeliversToTheSink(t *testing.T) {
	sink := &captureSink{}
	r := New(Config{Sinks: []Sink{sink}, FlushEvery: 10 * time.Millisecond})

	r.Record(activity("a"))
	r.Record(activity("b"))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r.Close(ctx)

	if got := sink.count(); got != 2 {
		t.Fatalf("sink saw %d records, want 2", got)
	}
	if s := r.Stats(); s.Delivered != 2 {
		t.Fatalf("delivered = %d, want 2", s.Delivered)
	}
}

// The guarantee the whole design rests on: recording must not slow the agent
// down, even when nothing is draining the queue.
func TestRecordNeverBlocksWhenTheQueueIsFull(t *testing.T) {
	// A sink that blocks past the test's patience, and a queue of one.
	sink := &captureSink{block: time.Hour}
	r := New(Config{Sinks: []Sink{sink}, QueueSize: 1, FlushEvery: time.Hour})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		r.Close(ctx)
	})

	done := make(chan struct{})
	go func() {
		for i := 0; i < 10000; i++ {
			r.Record(activity("a"))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked — the agent's turn would have stalled")
	}
}

func TestDroppedRecordsAreCounted(t *testing.T) {
	sink := &captureSink{block: time.Hour}
	r := New(Config{Sinks: []Sink{sink}, QueueSize: 1, FlushEvery: time.Hour})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		r.Close(ctx)
	})

	for i := 0; i < 500; i++ {
		r.Record(activity("a"))
	}

	// Losing records is acceptable; losing them silently is not.
	if s := r.Stats(); s.Dropped == 0 {
		t.Fatal("records were dropped but the counter stayed at zero")
	}
}

func TestASinkThatFailsDoesNotStopRecording(t *testing.T) {
	failing := &captureSink{err: errors.New("the collector is down")}
	r := New(Config{Sinks: []Sink{failing}, FlushEvery: 10 * time.Millisecond})

	r.Record(activity("a"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r.Close(ctx)

	s := r.Stats()
	if s.Failed != 1 {
		t.Fatalf("failed = %d, want 1", s.Failed)
	}
	if s.Delivered != 0 {
		t.Fatalf("delivered = %d, want 0", s.Delivered)
	}
}

// A sink is third-party code running inside someone else's agent. If it panics,
// the agent must survive.
func TestASinkThatPanicsDoesNotBringDownTheHost(t *testing.T) {
	r := New(Config{Sinks: []Sink{&captureSink{panic: true}}, FlushEvery: 10 * time.Millisecond})

	r.Record(activity("a"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r.Close(ctx)

	if s := r.Stats(); s.Panicked != 1 {
		t.Fatalf("panicked = %d, want 1", s.Panicked)
	}
}

// One team's broken destination must not cost another team its data.
func TestOneFailingSinkDoesNotAffectAnother(t *testing.T) {
	good := &captureSink{}
	bad := &captureSink{panic: true}
	r := New(Config{Sinks: []Sink{bad, good}, FlushEvery: 10 * time.Millisecond})

	r.Record(activity("a"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r.Close(ctx)

	if got := good.count(); got != 1 {
		t.Fatalf("the working sink saw %d records, want 1", got)
	}
}

func TestCloseDeliversWhatIsStillQueued(t *testing.T) {
	sink := &captureSink{}
	// Long flush interval, so only Close can move these.
	r := New(Config{Sinks: []Sink{sink}, FlushEvery: time.Hour})

	for i := 0; i < 5; i++ {
		r.Record(activity("a"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r.Close(ctx)

	if got := sink.count(); got != 5 {
		t.Fatalf("sink saw %d records after Close, want 5", got)
	}
}

func TestCloseReturnsEvenWhenASinkHangs(t *testing.T) {
	r := New(Config{Sinks: []Sink{&captureSink{block: time.Hour}}, FlushEvery: time.Millisecond})
	r.Record(activity("a"))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	r.Close(ctx)

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Close took %s — shutting down must not hang on a sink", elapsed)
	}
}

func TestARecorderWithNoConfigurationIsInertRatherThanBroken(t *testing.T) {
	r := New(Config{})
	r.Record(activity("a"))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r.Close(ctx)

	if s := r.Stats(); s.Recorded != 1 {
		t.Fatalf("recorded = %d, want 1", s.Recorded)
	}
}

// Wiring a recorder is two lines, and a team that gets one of them wrong should
// not crash. A nil recorder accepts records and does nothing.
func TestANilRecorderIsSafeToUse(t *testing.T) {
	var r *Recorder
	r.Record(activity("a"))
	r.Close(context.Background())
	if s := r.Stats(); s.Recorded != 0 {
		t.Fatalf("recorded = %d, want 0", s.Recorded)
	}
}

func TestRecordingFromManyGoroutinesAtOnceIsSafe(t *testing.T) {
	sink := &captureSink{}
	r := New(Config{Sinks: []Sink{sink}, FlushEvery: 5 * time.Millisecond, QueueSize: 4096})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				r.Record(activity("a"))
			}
		}()
	}
	wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r.Close(ctx)

	s := r.Stats()
	if s.Recorded+s.Dropped != 1000 {
		t.Fatalf("recorded %d plus dropped %d, want 1000 accounted for", s.Recorded, s.Dropped)
	}
}
