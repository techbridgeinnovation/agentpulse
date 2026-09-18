package recorder

import (
	"context"
	"errors"
	"slices"
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
	r.RecordIn(WithWorkspace(context.Background(), "acme"), activity("b"))
	r.NoteUserIn(WithWorkspace(context.Background(), "acme"), ada)
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

// groupingSink keeps each call whole, with the workspace the batch was
// delivered under, so a test can see how a flush was split rather than only
// what survived it.
type groupingSink struct {
	mu     sync.Mutex
	calls  []deliveredBatch
	refuse string
}

// deliveredBatch is one call to a sink: the workspace it was made under and
// the records it carried.
type deliveredBatch struct {
	workspace string
	names     []string
}

func (g *groupingSink) Send(ctx context.Context, activities []*pb.Activity) error {
	workspace := WorkspaceFrom(ctx)

	names := make([]string, 0, len(activities))
	for _, a := range activities {
		names = append(names, a.GetName())
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, deliveredBatch{workspace: workspace, names: names})
	if g.refuse != "" && workspace == g.refuse {
		return errors.New("this workspace is refused")
	}
	return nil
}

func (g *groupingSink) Name() string { return "grouping" }

func (g *groupingSink) delivered() []deliveredBatch {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]deliveredBatch(nil), g.calls...)
}

// find returns the call made under workspace, and whether one was made at all.
func (g *groupingSink) find(workspace string) (deliveredBatch, bool) {
	for _, call := range g.delivered() {
		if call.workspace == workspace {
			return call, true
		}
	}
	return deliveredBatch{}, false
}

// The compatibility case, and the one that matters most: a recorder that knows
// nothing about workspaces hands over records that name none, and they are
// delivered as one batch naming none. Nothing about them is refused on the way.
func TestRecordsNamingNoWorkspaceAreDeliveredAsOneBatchNamingNone(t *testing.T) {
	sink := &groupingSink{}
	r := New(Config{Sinks: []Sink{sink}, FlushEvery: time.Hour})

	r.Record(activity("a"))
	// A context with nothing on it says the same thing as no context at all.
	r.RecordIn(context.Background(), activity("b"))
	closeSoon(t, r)

	calls := sink.delivered()
	if len(calls) != 1 {
		t.Fatalf("sink was called %d times, want once for one workspace: %+v", len(calls), calls)
	}
	if calls[0].workspace != "" {
		t.Fatalf("batch delivered under workspace %q, want none", calls[0].workspace)
	}
	if len(calls[0].names) != 2 {
		t.Fatalf("batch carried %v, want both records", calls[0].names)
	}
	if s := r.Stats(); s.Delivered != 2 || s.Failed != 0 {
		t.Fatalf("stats = %+v, want both delivered and none failed", s)
	}
}

func TestAFlushIsSplitIntoOneBatchPerWorkspace(t *testing.T) {
	sink := &groupingSink{}
	r := New(Config{Sinks: []Sink{sink}, FlushEvery: time.Hour})

	r.RecordIn(WithWorkspace(context.Background(), "acme"), activity("a"))
	r.RecordIn(WithWorkspace(context.Background(), "globex"), activity("b"))
	r.RecordIn(WithWorkspace(context.Background(), "acme"), activity("c"))
	r.Record(activity("d"))
	closeSoon(t, r)

	calls := sink.delivered()
	if len(calls) != 3 {
		t.Fatalf("sink was called %d times, want one per workspace: %+v", len(calls), calls)
	}

	for workspace, want := range map[string][]string{
		"acme":   {"a", "c"},
		"globex": {"b"},
		"":       {"d"},
	} {
		got, ok := sink.find(workspace)
		if !ok {
			t.Fatalf("no batch was delivered under workspace %q", workspace)
		}
		if !slices.Equal(got.names, want) {
			t.Errorf("workspace %q was sent %v, want %v", workspace, got.names, want)
		}
	}
}

// One tenant's records being refused says nothing about another's, so the
// others are still sent and the counters still count records rather than
// batches.
func TestAWorkspaceThatFailsDoesNotLoseAnother(t *testing.T) {
	sink := &groupingSink{refuse: "acme"}
	r := New(Config{Sinks: []Sink{sink}, FlushEvery: time.Hour})

	r.RecordIn(WithWorkspace(context.Background(), "acme"), activity("a"))
	r.RecordIn(WithWorkspace(context.Background(), "globex"), activity("b"))
	r.RecordIn(WithWorkspace(context.Background(), "globex"), activity("c"))
	closeSoon(t, r)

	got, ok := sink.find("globex")
	if !ok {
		t.Fatal("globex was never sent, although only acme was refused")
	}
	if !slices.Equal(got.names, []string{"b", "c"}) {
		t.Fatalf("globex was sent %v, want both of its records", got.names)
	}
	if s := r.Stats(); s.Delivered != 2 || s.Failed != 1 {
		t.Fatalf("stats = %+v, want globex's two delivered and acme's one failed", s)
	}
}
