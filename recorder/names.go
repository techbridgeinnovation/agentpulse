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
//
// One batch is one workspace, on ctx and read with WorkspaceFrom, exactly as a
// batch of records carries it. The directory a person belongs in is the one
// their records are filed in, or the join the name exists for does not happen.
type UserSink interface {
	// SendUsers delivers a batch of people to name. The slice must not be
	// retained after returning.
	SendUsers(ctx context.Context, users []User) error
}

// person is one name to write and the workspace it belongs in.
type person struct {
	user      User
	workspace string
}

// maxRememberedNames bounds what the recorder keeps about who it has named.
// Past it, everything is forgotten and sent again as people reappear; a
// second upsert of the same name is harmless.
const maxRememberedNames = 10_000

// seenKey is an identifier in one workspace.
//
// The workspace is part of it because a directory is per workspace: the same identifier seen in two of them is two rows to write, and remembering it by identifier alone would name the person in the first workspace and leave them an identifier in the second for as long as the process ran.
type seenKey struct {
	workspace string
	id        string
}

// seenNames is what was last sent for each identifier.
type seenNames struct {
	mu   sync.Mutex
	last map[seenKey]User
}

func newSeenNames() *seenNames {
	return &seenNames{last: map[seenKey]User{}}
}

// unchanged reports whether p is exactly what was last sent for its identifier in its workspace.
func (s *seenNames) unchanged(p person) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last[keyOf(p)] == p.user
}

func (s *seenNames) mark(p person) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.last) >= maxRememberedNames {
		s.last = map[seenKey]User{}
	}
	s.last[keyOf(p)] = p.user
}

func (s *seenNames) forget(people []person) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range people {
		delete(s.last, keyOf(p))
	}
}

func keyOf(p person) seenKey {
	return seenKey{workspace: p.workspace, id: p.user.ID}
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
	r.notePerson(person{user: user})
}

// NoteUserIn hands over a person seen in ctx's workspace, and returns immediately.
//
// The same contract as NoteUser. The workspace comes from ctx for the same reason a record's does, and it decides which directory the name is written to: the one holding the records that carry this identifier.
func (r *Recorder) NoteUserIn(ctx context.Context, user User) {
	r.notePerson(person{user: user, workspace: WorkspaceFrom(ctx)})
}

func (r *Recorder) notePerson(p person) {
	if r == nil || p.user.ID == "" || !p.user.named() {
		return
	}
	if strings.Contains(p.user.ID, "/") {
		r.counters.NamesFailed.Add(1)
		return
	}
	if r.seen.unchanged(p) {
		return
	}

	select {
	case r.names <- p:
		r.seen.mark(p)
		r.counters.Named.Add(1)
	default:
		r.counters.NamesDropped.Add(1)
	}
}

// workspacePeople is the part of one batch of names that belongs in one workspace.
type workspacePeople struct {
	workspace string
	people    []person
	users     []User
}

// groupPeopleByWorkspace splits a batch of names into one batch per workspace, in the order the workspaces were first seen, for the same reason records are split that way: one directory is written under one parent.
//
// Each group carries both forms of the same people, because the sink is given the names and a failure gives back the people to forget, workspace and all.
func groupPeopleByWorkspace(batch []person) []workspacePeople {
	groups := make([]workspacePeople, 0, 1)
	at := make(map[string]int, 1)

	for _, p := range batch {
		position, held := at[p.workspace]
		if !held {
			position = len(groups)
			at[p.workspace] = position
			groups = append(groups, workspacePeople{workspace: p.workspace})
		}
		groups[position].people = append(groups[position].people, p)
		groups[position].users = append(groups[position].users, p.user)
	}
	return groups
}

// deliverNames sends a batch per workspace to every sink that takes names,
// concurrently and each isolated, the same way records are delivered.
func (r *Recorder) deliverNames(batch []person) {
	groups := groupPeopleByWorkspace(batch)

	var wg sync.WaitGroup
	for _, sink := range r.config.Sinks {
		userSink, ok := sink.(UserSink)
		if !ok {
			continue
		}
		for _, group := range groups {
			wg.Add(1)
			go func(s UserSink, g workspacePeople) {
				defer wg.Done()
				r.sendNamesTo(s, g)
			}(userSink, group)
		}
	}
	wg.Wait()
}

func (r *Recorder) sendNamesTo(sink UserSink, group workspacePeople) {
	defer func() {
		if recovered := recover(); recovered != nil {
			r.counters.Panicked.Add(1)
			r.counters.NamesFailed.Add(int64(len(group.users)))
			r.seen.forget(group.people)
		}
	}()

	ctx, cancel := context.WithTimeout(WithWorkspace(context.Background(), group.workspace), r.config.SendTimeout)
	defer cancel()

	if err := sink.SendUsers(ctx, group.users); err != nil {
		r.counters.NamesFailed.Add(int64(len(group.users)))
		r.seen.forget(group.people)
		return
	}
}
