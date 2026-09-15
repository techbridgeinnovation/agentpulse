package recorder

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

// namingSink keeps the people it was asked to name, beside the records.
type namingSink struct {
	captureSink

	mu    sync.Mutex
	users []User
	fail  error
	block time.Duration
}

func (n *namingSink) SendUsers(ctx context.Context, users []User) error {
	if n.block > 0 {
		select {
		case <-time.After(n.block):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.users = append(n.users, users...)
	return n.fail
}

func (n *namingSink) named() []User {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]User(nil), n.users...)
}

func closeSoon(t *testing.T, r *Recorder) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r.Close(ctx)
}

var ada = User{ID: "8c21e0b4", Name: "Ada Lovelace", Email: "ada@example.com"}

func TestAPersonIsNamedOncePerProcess(t *testing.T) {
	sink := &namingSink{}
	r := New(Config{Sinks: []Sink{sink}, FlushEvery: 10 * time.Millisecond})

	for i := 0; i < 50; i++ {
		r.NoteUser(ada)
	}
	closeSoon(t, r)

	if got := sink.named(); len(got) != 1 || got[0] != ada {
		t.Fatalf("sink named %v, want exactly one %v", got, ada)
	}
	if s := r.Stats(); s.Named != 1 || s.NamesDropped != 0 || s.NamesFailed != 0 {
		t.Fatalf("stats = %+v, want one named and nothing else", s)
	}
}

func TestAChangedNameIsSentAgain(t *testing.T) {
	sink := &namingSink{}
	r := New(Config{Sinks: []Sink{sink}, FlushEvery: 10 * time.Millisecond})

	r.NoteUser(ada)
	renamed := ada
	renamed.Name = "Ada King"
	r.NoteUser(renamed)
	r.NoteUser(renamed)
	closeSoon(t, r)

	if got := sink.named(); len(got) != 2 || got[1] != renamed {
		t.Fatalf("sink named %v, want the original and then the renamed", got)
	}
}

func TestTheReporterNamesThePersonItRecordsFor(t *testing.T) {
	sink := &namingSink{}
	r := New(Config{Sinks: []Sink{sink}, FlushEvery: 10 * time.Millisecond})
	reporter := r.For(Attribution{Agent: "organisations/techbridge/agents/sources", Service: "sources"})

	ctx := WithUser(context.Background(), ada)
	reporter.ModelCall(ctx, ModelCall{Model: "gemini-2.5-pro"})
	reporter.ModelCall(ctx, ModelCall{Model: "gemini-2.5-pro"})
	closeSoon(t, r)

	if got := sink.named(); len(got) != 1 || got[0] != ada {
		t.Fatalf("sink named %v, want %v once", got, ada)
	}
	// The record itself carries the identifier only.
	if got := sink.count(); got != 2 {
		t.Fatalf("sink saw %d records, want 2", got)
	}
	for _, a := range sink.seen {
		if a.GetUser() != ada.ID {
			t.Fatalf("record carries user %q, want %q", a.GetUser(), ada.ID)
		}
	}
}

func TestAnIdentifierAloneIsNothingToName(t *testing.T) {
	sink := &namingSink{}
	r := New(Config{Sinks: []Sink{sink}, FlushEvery: 10 * time.Millisecond})

	r.NoteUser(User{ID: "8c21e0b4"})
	r.NoteUser(User{Name: "Nobody"})
	closeSoon(t, r)

	if got := sink.named(); len(got) != 0 {
		t.Fatalf("sink named %v, want nothing", got)
	}
	if s := r.Stats(); s.Named != 0 || s.NamesFailed != 0 {
		t.Fatalf("stats = %+v, want nothing counted", s)
	}
}

func TestAnIdentifierWithASlashCannotBeNamedAndIsCounted(t *testing.T) {
	sink := &namingSink{}
	r := New(Config{Sinks: []Sink{sink}, FlushEvery: 10 * time.Millisecond})

	r.NoteUser(User{ID: "users/8c21e0b4", Name: "Ada"})
	closeSoon(t, r)

	if got := sink.named(); len(got) != 0 {
		t.Fatalf("sink named %v, want nothing", got)
	}
	if s := r.Stats(); s.NamesFailed != 1 {
		t.Fatalf("NamesFailed = %d, want 1", s.NamesFailed)
	}
}

func TestNamingNeverBlocksWhenTheQueueIsFull(t *testing.T) {
	sink := &namingSink{block: time.Hour}
	r := New(Config{Sinks: []Sink{sink}, QueueSize: 1, FlushEvery: time.Hour})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		r.Close(ctx)
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			r.NoteUser(User{ID: string(rune('a'+i%26)) + string(rune('a'+i/26)), Name: "Someone"})
		}
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("NoteUser blocked on a full queue")
	}
	if s := r.Stats(); s.NamesDropped == 0 {
		t.Fatalf("nothing counted as dropped from a queue of one")
	}
}

func TestAFailedNamingIsCountedAndTriedAgain(t *testing.T) {
	sink := &namingSink{fail: errors.New("metering unavailable")}
	r := New(Config{Sinks: []Sink{sink}, FlushEvery: 10 * time.Millisecond})

	r.NoteUser(ada)
	time.Sleep(50 * time.Millisecond)
	if s := r.Stats(); s.NamesFailed != 1 {
		t.Fatalf("NamesFailed = %d after a refused send, want 1", s.NamesFailed)
	}

	// The next time the person acts, the name goes again rather than
	// waiting for a restart.
	sink.mu.Lock()
	sink.fail = nil
	sink.mu.Unlock()
	r.NoteUser(ada)
	closeSoon(t, r)

	if got := sink.named(); len(got) != 2 {
		t.Fatalf("sink was asked %d times, want 2 (one refused, one accepted)", len(got))
	}
}

func TestASinkWithoutADirectoryReceivesNoNames(t *testing.T) {
	sink := &captureSink{}
	r := New(Config{Sinks: []Sink{sink}, FlushEvery: 10 * time.Millisecond})

	r.NoteUser(ada)
	r.Record(&pb.Activity{Request: "req-1", User: ada.ID})
	closeSoon(t, r)

	if got := sink.count(); got != 1 {
		t.Fatalf("sink saw %d records, want 1", got)
	}
	if s := r.Stats(); s.Named != 1 || s.NamesFailed != 0 {
		t.Fatalf("stats = %+v, want the name queued and no failure", s)
	}
}

func TestCloseNamesWhoIsStillQueued(t *testing.T) {
	sink := &namingSink{}
	r := New(Config{Sinks: []Sink{sink}, FlushEvery: time.Hour})

	r.NoteUser(ada)
	closeSoon(t, r)

	if got := sink.named(); len(got) != 1 {
		t.Fatalf("sink named %v, want %v", got, ada)
	}
}
