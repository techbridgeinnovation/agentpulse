package recorder

import (
	"context"
	"strings"
	"sync"
)

// A name reaches the directory once per person per process, not once per
// call. The path is the same shape as the one records take — a bounded queue,
// a batch, a sink on the worker — and it drops on saturation for the same
// reason: naming someone is never allowed to slow down the work done for them.
//
// What is remembered is what was last sent. A person seen again with the same
// name and email costs a map lookup and nothing else; a person whose name has
// changed is sent again. A send that fails forgets the person, so the next
// call they make tries again rather than waiting for a restart.

// UserSink is a sink that also takes names. A sink that does not implement it
// receives records and no names, which is right for a log or a collector
// that has no directory to put a name in.
type UserSink interface {
	// SendUsers delivers a batch of people to name. The slice must not be
	// retained after returning.
	SendUsers(ctx context.Context, users []User) error
}

// maxRememberedNames bounds what the recorder keeps about who it has named.
// Past it, everything is forgotten and sent again as people reappear; a
// second upsert of the same name is harmless.
const maxRememberedNames = 10_000

// seenNames is what was last sent for each identifier.
type seenNames struct {
	mu   sync.Mutex
	last map[string]User
}

func newSeenNames() *seenNames {
	return &seenNames{last: map[string]User{}}
}

// unchanged reports whether user is exactly what was last sent for its ID.
func (s *seenNames) unchanged(user User) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last[user.ID] == user
}

func (s *seenNames) mark(user User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.last) >= maxRememberedNames {
		s.last = map[string]User{}
	}
	s.last[user.ID] = user
}

func (s *seenNames) forget(users []User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, user := range users {
		delete(s.last, user.ID)
	}
}

// NoteUser hands over a person to name and returns immediately.
//
// It never blocks, never returns an error and never panics. A user with no
// name and no email is nothing to say and is ignored; one whose identifier
// holds a slash cannot be named, because the identifier becomes a path
// segment, and is counted as failed so the mistake is visible. The reporter
// and the framework hooks call this on every record; the cost of that is one
// map lookup once the person has been sent.
func (r *Recorder) NoteUser(user User) {
	if r == nil || user.ID == "" || !user.named() {
		return
	}
	if strings.Contains(user.ID, "/") {
		r.counters.NamesFailed.Add(1)
		return
	}
	if r.seen.unchanged(user) {
		return
	}

	select {
	case r.names <- user:
		r.seen.mark(user)
		r.counters.Named.Add(1)
	default:
		r.counters.NamesDropped.Add(1)
	}
}

// deliverNames sends a batch to every sink that takes names, concurrently
// and each isolated, the same way records are delivered.
func (r *Recorder) deliverNames(batch []User) {
	sent := make([]User, len(batch))
	copy(sent, batch)

	var wg sync.WaitGroup
	for _, sink := range r.config.Sinks {
		userSink, ok := sink.(UserSink)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(s UserSink) {
			defer wg.Done()
			r.sendNamesTo(s, sent)
		}(userSink)
	}
	wg.Wait()
}

func (r *Recorder) sendNamesTo(sink UserSink, batch []User) {
	defer func() {
		if recovered := recover(); recovered != nil {
			r.counters.Panicked.Add(1)
			r.counters.NamesFailed.Add(int64(len(batch)))
			r.seen.forget(batch)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), r.config.SendTimeout)
	defer cancel()

	if err := sink.SendUsers(ctx, batch); err != nil {
		r.counters.NamesFailed.Add(int64(len(batch)))
		r.seen.forget(batch)
		return
	}
}
