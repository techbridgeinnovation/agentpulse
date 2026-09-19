package adkhooks

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/memory"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool/toolconfirmation"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/techbridgeinnovation/agentpulse/recorder"
	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

// fakeToolContext stands in for what the framework hands a tool callback: a
// callback context, plus the id of the function call that triggered the tool.
type fakeToolContext struct {
	fakeContext
	functionCallID string
}

func (f fakeToolContext) FunctionCallID() string { return f.functionCallID }
func (f fakeToolContext) Actions() *session.EventActions {
	return nil
}
func (f fakeToolContext) SearchMemory(context.Context, string) (*memory.SearchResponse, error) {
	return nil, nil
}
func (f fakeToolContext) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }
func (f fakeToolContext) RequestConfirmation(string, any) error                { return nil }

func newToolContext(functionCallID string) fakeToolContext {
	return fakeToolContext{fakeContext: newContext(), functionCallID: functionCallID}
}

// fakeTool is a tool the framework named, and nothing more: the callbacks only
// ever read its name.
type fakeTool struct{ name string }

func (f fakeTool) Name() string        { return f.name }
func (f fakeTool) Description() string { return "" }
func (f fakeTool) IsLongRunning() bool { return false }

var _ agent.ToolContext = fakeToolContext{}

// A tool that failed is the whole reason someone opens the failures panel, and
// a report that says only that it failed sends them to the logs. The code is
// the one part of an error that can be kept, and it was being discarded.
func TestAFailedToolRecordsWhyItFailed(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		ctx := newToolContext("call-1")
		AfterTool(r, options())(ctx, fakeTool{name: "web_search"}, nil, nil,
			status.Error(codes.DeadlineExceeded, "the search took too long"))
	})

	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
	if got[0].GetStatus() != pb.Activity_FAILED {
		t.Errorf("status = %v, want FAILED", got[0].GetStatus())
	}
	if got[0].GetErrorCode() != "DeadlineExceeded" {
		t.Errorf("error code = %q, want DeadlineExceeded", got[0].GetErrorCode())
	}
	if got[0].GetCallerComponent() != "tool:web_search" {
		t.Errorf("component = %q, want tool:web_search", got[0].GetCallerComponent())
	}
}

// A timeout is a tool that ran for its whole limit, and it reads as an ordinary
// failure unless the duration is there beside it.
func TestAToolCallReportsHowLongItRan(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		ctx := newToolContext("call-1")
		BeforeTool(r, options())(ctx, fakeTool{name: "web_search"}, nil)
		time.Sleep(2 * time.Millisecond)
		AfterTool(r, options())(ctx, fakeTool{name: "web_search"}, nil, nil, nil)
	})

	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
	if got[0].GetDurationMs() < 1 {
		t.Errorf("duration = %dms, want the time between the two callbacks", got[0].GetDurationMs())
	}
	if got[0].GetStatus() != pb.Activity_OK {
		t.Errorf("status = %v, want OK", got[0].GetStatus())
	}
}

// Registering the second callback and not the first is a mistake that must cost
// the duration and nothing else, because an adopter upgrading the library gets
// exactly that until they change their wiring.
func TestAToolCallIsStillRecordedWithoutTheCallbackThatTimesIt(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		AfterTool(r, options())(newToolContext("call-2"), fakeTool{name: "lookup"}, nil, nil, nil)
	})

	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
	if got[0].GetDurationMs() != 0 {
		t.Errorf("duration = %dms, want 0 for a call nothing timed", got[0].GetDurationMs())
	}
}

// A parallel agent runs several tools at once, and the framework tells them
// apart by the id of the function call that started each one.
func TestTwoToolsRunningAtOnceAreTimedSeparately(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		slow := newToolContext("call-slow")
		quick := newToolContext("call-quick")

		BeforeTool(r, options())(slow, fakeTool{name: "deep_research"}, nil)
		time.Sleep(4 * time.Millisecond)
		BeforeTool(r, options())(quick, fakeTool{name: "lookup"}, nil)
		AfterTool(r, options())(quick, fakeTool{name: "lookup"}, nil, nil, nil)
		AfterTool(r, options())(slow, fakeTool{name: "deep_research"}, nil, nil, nil)
	})

	if len(got) != 2 {
		t.Fatalf("recorded %d activities, want 2", len(got))
	}
	quick, slow := got[0], got[1]
	if quick.GetCallerComponent() != "tool:lookup" || slow.GetCallerComponent() != "tool:deep_research" {
		t.Fatalf("recorded %q then %q, want the quick tool first", quick.GetCallerComponent(), slow.GetCallerComponent())
	}
	if slow.GetDurationMs() < quick.GetDurationMs() {
		t.Errorf("the tool that ran longer reported %dms against the other's %dms", slow.GetDurationMs(), quick.GetDurationMs())
	}
}

// The store must not grow with every tool call that never came back.
func TestAToolCallIsForgottenOnceItIsRecorded(t *testing.T) {
	before := runningTools.size()
	record(t, func(r *recorder.Recorder) {
		ctx := newToolContext("call-3")
		BeforeTool(r, options())(ctx, fakeTool{name: "lookup"}, nil)
		AfterTool(r, options())(ctx, fakeTool{name: "lookup"}, nil, nil, nil)
	})
	if got := runningTools.size(); got != before {
		t.Errorf("the store holds %d calls, want %d", got, before)
	}
}

// A tool that fails with something the transport reported rather than a grpc
// status used to reach the report as Unknown, which is the same word every
// other kind of failure arrived as.
func TestAToolFailureThatIsNotAGrpcStatusStillNamesItself(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		AfterTool(r, options())(newToolContext("call-4"), fakeTool{name: "fetch"}, nil, nil,
			errors.Join(errors.New("fetching the page"), context.DeadlineExceeded))
	})

	if got[0].GetErrorCode() != "DeadlineExceeded" {
		t.Errorf("error code = %q, want DeadlineExceeded", got[0].GetErrorCode())
	}
}
