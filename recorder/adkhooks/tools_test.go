package adkhooks

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/memory"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/toolconfirmation"
	"google.golang.org/genai"
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
	if got[0].GetObservedAs() != pb.Agent_AGENT {
		t.Errorf("observed_as = %v, want AGENT: the call came through an agent's callbacks", got[0].GetObservedAs())
	}
	if got[0].GetSubAgent() != "atlas" {
		t.Errorf("sub_agent = %q, want atlas: a tool call is filed under the agent that ran it", got[0].GetSubAgent())
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
	if got[0].GetEmptyResult() {
		t.Error("a tool that raised was recorded as handing back an empty result")
	}
}

// The failure nothing reports: a tool catches what went wrong, hands back an
// answer saying so, and the framework — which only sees that a value was
// returned — calls it a success.
func TestAToolThatReportsItsOwnFailureIsRecordedAsOne(t *testing.T) {
	opts := options()
	opts.ToolFailed = func(_ string, result map[string]any) (bool, string) {
		if ok, _ := result["success"].(bool); !ok {
			return true, "UPSTREAM_UNAVAILABLE"
		}
		return false, ""
	}

	got := record(t, func(r *recorder.Recorder) {
		AfterTool(r, opts)(newToolContext("call-1"), fakeTool{name: "projects_get_context"},
			nil, map[string]any{"success": false}, nil)
	})

	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
	if got[0].GetStatus() != pb.Activity_FAILED {
		t.Errorf("status = %v, want FAILED", got[0].GetStatus())
	}
	if got[0].GetErrorCode() != "UPSTREAM_UNAVAILABLE" {
		t.Errorf("error code = %q, want the code the adopter named", got[0].GetErrorCode())
	}
}

// A tool that worked stays a success, and an adopter who wired nothing up sees
// exactly what they saw before.
func TestAToolThatWorkedStaysASuccess(t *testing.T) {
	judged := options()
	judged.ToolFailed = func(_ string, result map[string]any) (bool, string) {
		ok, _ := result["success"].(bool)
		return !ok, "UPSTREAM_UNAVAILABLE"
	}

	got := record(t, func(r *recorder.Recorder) {
		AfterTool(r, judged)(newToolContext("call-1"), fakeTool{name: "web_search"},
			nil, map[string]any{"success": true}, nil)
	})
	if len(got) != 1 || got[0].GetStatus() != pb.Activity_OK {
		t.Fatalf("recorded %v, want one OK activity", got)
	}

	unwired := record(t, func(r *recorder.Recorder) {
		AfterTool(r, options())(newToolContext("call-2"), fakeTool{name: "web_search"},
			nil, map[string]any{"success": false}, nil)
	})
	if len(unwired) != 1 || unwired[0].GetStatus() != pb.Activity_OK {
		t.Fatalf("unwired: recorded %v, want one OK activity — nothing changes for a team that set nothing", unwired)
	}
}

// A raised error already says what went wrong, in a code, so the adopter's
// judgement is not consulted and cannot talk a failed call into a success.
func TestARaisedErrorBeatsTheAdoptersJudgement(t *testing.T) {
	opts := options()
	opts.ToolFailed = func(string, map[string]any) (bool, string) { return false, "" }

	got := record(t, func(r *recorder.Recorder) {
		AfterTool(r, opts)(newToolContext("call-1"), fakeTool{name: "web_search"},
			nil, map[string]any{"success": true}, status.Error(codes.DeadlineExceeded, "too long"))
	})

	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
	if got[0].GetStatus() != pb.Activity_FAILED || got[0].GetErrorCode() != "DeadlineExceeded" {
		t.Errorf("got %v/%q, want FAILED and the raised error's own code", got[0].GetStatus(), got[0].GetErrorCode())
	}
}

// A failure the adopter named but gave no code for is recorded under one a
// report can group by, rather than under nothing.
func TestANamedFailureWithNoCodeStillCarriesOne(t *testing.T) {
	opts := options()
	opts.ToolFailed = func(string, map[string]any) (bool, string) { return true, "" }

	got := record(t, func(r *recorder.Recorder) {
		AfterTool(r, opts)(newToolContext("call-1"), fakeTool{name: "web_search"}, nil, nil, nil)
	})

	if len(got) != 1 || got[0].GetStatus() != pb.Activity_FAILED {
		t.Fatalf("recorded %v, want one FAILED activity", got)
	}
	if got[0].GetErrorCode() != "TOOL_ERROR" {
		t.Errorf("error code = %q, want TOOL_ERROR", got[0].GetErrorCode())
	}
}

// A tool is named on its own field as well as inside the component, so which
// tools fail is a question asked of the tool rather than of a prefixed string.
func TestAToolCallNamesTheToolThatRan(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		AfterTool(r, options())(newToolContext("call-1"), fakeTool{name: "web_search"}, nil, nil, nil)
	})

	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
	if got[0].GetTool() != "web_search" {
		t.Errorf("tool = %q, want web_search", got[0].GetTool())
	}
	if got[0].GetCallerComponent() != "tool:web_search" {
		t.Errorf("component = %q, want the prefixed name kept as it was", got[0].GetCallerComponent())
	}
}

// The adopter's judgement is their code running inside the framework's
// callback. A panic in it must cost the tool call nothing: the call is recorded
// as the framework saw it and the panic is counted.
func TestAJudgementThatPanicsCostsTheToolCallNothing(t *testing.T) {
	opts := options()
	opts.ToolFailed = func(string, map[string]any) (bool, string) { panic("adopter bug") }

	var panicked int64
	got := record(t, func(r *recorder.Recorder) {
		AfterTool(r, opts)(newToolContext("call-1"), fakeTool{name: "web_search"},
			nil, map[string]any{"success": false}, nil)
		panicked = r.Stats().Panicked
	})

	if len(got) != 1 || got[0].GetStatus() != pb.Activity_OK {
		t.Fatalf("recorded %v, want the call recorded as the framework saw it", got)
	}
	if panicked != 1 {
		t.Errorf("panicked = %d, want the panic counted", panicked)
	}
}

// functionTool is a tool the framework can call as a function, which is the kind it goes on to run AfterTool for once the tool has raised.
type functionTool struct{ fakeTool }

func (functionTool) Declaration() *genai.FunctionDeclaration { return nil }

// A model that invents a tool's name reaches only the error callbacks, so without OnToolError the call is never recorded.
func TestAToolTheAgentDoesNotHaveIsRecordedAsFailed(t *testing.T) {
	var result map[string]any
	var err error
	got := record(t, func(r *recorder.Recorder) {
		result, err = OnToolError(r, options())(newToolContext(t.Name()), fakeTool{name: "serach_web"}, nil,
			errors.New("tool 'serach_web' not found"))
	})

	if result != nil || err != nil {
		t.Fatalf("OnToolError = (%v, %v), want (nil, nil) so the error reaches the model unchanged", result, err)
	}
	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
	if got[0].GetStatus() != pb.Activity_FAILED || got[0].GetErrorCode() != toolNotFound {
		t.Errorf("recorded %v %q, want FAILED %s", got[0].GetStatus(), got[0].GetErrorCode(), toolNotFound)
	}
	if got[0].GetTool() != "serach_web" {
		t.Errorf("tool = %q, want the name the model used", got[0].GetTool())
	}
}

// A tool that ran and raised reaches both callbacks, and is one call.
func TestAToolThatRaisedIsRecordedOnce(t *testing.T) {
	raised := status.Error(codes.Unavailable, "backend down")
	got := record(t, func(r *recorder.Recorder) {
		ctx := newToolContext(t.Name())
		lookup := functionTool{fakeTool{name: "lookup"}}
		BeforeTool(r, options())(ctx, lookup, nil)
		OnToolError(r, options())(ctx, lookup, nil, raised)
		AfterTool(r, options())(ctx, lookup, nil, nil, raised)
	})

	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
	if got[0].GetStatus() != pb.Activity_FAILED || got[0].GetErrorCode() != "Unavailable" {
		t.Errorf("recorded %v %q, want FAILED Unavailable", got[0].GetStatus(), got[0].GetErrorCode())
	}
}

// An error callback that rescues a tool hands AfterTool the substitute and no error, which without the kept error is recorded as a success.
func TestAToolRescuedAfterItRaisedIsStillRecordedAsFailed(t *testing.T) {
	before, held := runningTools.size(), 0
	got := record(t, func(r *recorder.Recorder) {
		ctx := newToolContext(t.Name())
		lookup := functionTool{fakeTool{name: "lookup"}}
		OnToolError(r, options())(ctx, lookup, nil, status.Error(codes.DeadlineExceeded, "slow"))
		AfterTool(r, options())(ctx, lookup, nil, map[string]any{"fallback": true}, nil)
		held = runningTools.size()
	})

	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
	if got[0].GetStatus() != pb.Activity_FAILED || got[0].GetErrorCode() != "DeadlineExceeded" {
		t.Errorf("recorded %v %q, want FAILED DeadlineExceeded", got[0].GetStatus(), got[0].GetErrorCode())
	}
	if got[0].GetResultBytes() == 0 {
		t.Error("the substitute result was not measured")
	}
	if held != before {
		t.Errorf("the store holds %d calls after the call was recorded, want %d", held, before)
	}
}

// A tool waiting on a person has not run. The framework runs it again once they answer, and that run is the call.
func TestAToolAwaitingConfirmationIsNotRecorded(t *testing.T) {
	pending := fmt.Errorf("error tool %q %w", "send_email", tool.ErrConfirmationRequired)
	got := record(t, func(r *recorder.Recorder) {
		ctx := newToolContext(t.Name())
		send := functionTool{fakeTool{name: "send_email"}}
		BeforeTool(r, options())(ctx, send, nil)
		OnToolError(r, options())(ctx, send, nil, pending)
		AfterTool(r, options())(ctx, send, nil, nil, pending)
	})

	if len(got) != 0 {
		t.Fatalf("recorded %d activities, want none for a call that has not run", len(got))
	}
}

// A tool's record carries what a model call's does about where it ran, and how much it handed back.
func TestAToolCallRecordsItsFrameworkAndResultSize(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		AfterTool(r, options())(newToolContext(t.Name()), fakeTool{name: "read_file"}, nil, map[string]any{"content": "package main"}, nil)
		AfterTool(r, options())(newToolContext(t.Name()+"-empty"), fakeTool{name: "read_file"}, nil, nil, nil)
	})

	if len(got) != 2 {
		t.Fatalf("recorded %d activities, want 2", len(got))
	}
	a := got[0]
	if a.GetFramework() != framework || a.GetFrameworkVersion() != frameworkVersion() || a.GetRegion() != options().region() {
		t.Errorf("framework, version, region = %q %q %q, want %q %q %q", a.GetFramework(), a.GetFrameworkVersion(), a.GetRegion(), framework, frameworkVersion(), options().region())
	}
	if a.GetResultBytes() <= 0 || a.GetEmptyResult() {
		t.Errorf("result bytes = %d, empty = %v, want the size of a result with content", a.GetResultBytes(), a.GetEmptyResult())
	}
	if !got[1].GetEmptyResult() {
		t.Error("a tool that handed back nothing was not recorded as empty")
	}
}

// A component on the context names the model calls; a tool call is still filed under the tool, and both keep the agent that made them.
func TestAToolCallKeepsItsOwnComponentWhenTheContextNamesOne(t *testing.T) {
	ctx := newToolContext("call-1")
	ctx.Context = recorder.WithSkill(recorder.WithComponent(context.Background(), "report_generation"), "drafting")

	got := record(t, func(r *recorder.Recorder) {
		AfterTool(r, options())(ctx, fakeTool{name: "web_search"}, nil, nil, nil)
	})

	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
	if got[0].GetCallerComponent() != "tool:web_search" || got[0].GetSubAgent() != "atlas" {
		t.Errorf("component = %q, sub_agent = %q, want tool:web_search under atlas", got[0].GetCallerComponent(), got[0].GetSubAgent())
	}
	if got[0].GetSkill() != "drafting" {
		t.Errorf("skill = %q, want the one the context names", got[0].GetSkill())
	}
}
