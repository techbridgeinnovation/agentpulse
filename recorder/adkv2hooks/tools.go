package adkv2hooks

import (
	"encoding/json"
	"errors"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/techbridgeinnovation/agentpulse/recorder"
	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

// toolNotFound is the code a call to a tool the agent does not have is recorded under. The framework's own error for it is a sentence, so there is no code of its own to keep.
const toolNotFound = "TOOL_NOT_FOUND"

// runningTool is what is known about one tool call between the callback that starts it and the one that records it.
type runningTool struct {
	startedAt time.Time

	// raised is the error OnToolError saw, kept so that a call another error callback rescued is still recorded as the failure it was. AfterTool is handed the rescued result and no error.
	raised error
}

// runningTools holds what is known about each tool call, until the call ends and is recorded.
//
// Separate from the model store because the two are keyed differently: a model call is the one an agent has in flight, while several tool calls can be in flight at once for the same agent, each with its own id from the framework.
var runningTools = newInFlightStore[runningTool](maxInFlightCalls, inFlightCallTTL, time.Now)

// toolKey identifies one tool call.
//
// The framework's id for the function call is what distinguishes two calls to the same tool in the same turn, which is exactly what a parallel agent does. Where there is none, the invocation and the tool's name still tell one tool's call apart from another's in the same turn — one call of each at a time, which is the sequential case.
func toolKey(ctx agent.Context, name string) string {
	if id := ctx.FunctionCallID(); id != "" {
		return id
	}
	return ctx.InvocationID() + "\x00" + name
}

// BeforeTool notes when a tool call began.
//
// Registered so that a tool call reports how long it took. The framework hands AfterTool the result and the error but not the time, and a tool's duration is the figure that separates a slow tool from a broken one — a timeout is a tool that ran for its whole limit, and it reads as an ordinary failure without it.
//
// It records nothing and refuses nothing: a tool whose BeforeTool callback is not registered is still recorded by AfterTool, with no duration.
func BeforeTool(_ *recorder.Recorder, _ Options) llmagent.BeforeToolCallback {
	return func(ctx agent.Context, t tool.Tool, _ map[string]any) (map[string]any, error) {
		runningTools.put(toolKey(ctx, toolName(t)), runningTool{startedAt: time.Now()})
		return nil, nil
	}
}

// AfterTool records one activity per tool call.
//
// A tool call is a priced unit of work in its own right: a search or a lookup
// is charged per request, not per token, and those charges are invisible to
// anything that only counts tokens. Recorded on completion, so a tool that
// never ran is not counted.
//
// The error is read for its code, not its message, the same way a model call's is: a report that says a tool failed and not how it failed leaves a team to guess between a timeout, a bad argument and an outage, which are three different things to fix.
//
// A call waiting for a person to confirm it is not recorded. Its tool has not run, and the framework runs it again once the person answers, which is the call that is recorded.
func AfterTool(r *recorder.Recorder, opts Options) llmagent.AfterToolCallback {
	return func(ctx agent.Context, t tool.Tool, _, result map[string]any, callErr error) (map[string]any, error) {
		name := toolName(t)
		held, _ := runningTools.take(toolKey(ctx, name))
		if callErr == nil {
			callErr = held.raised
		}
		if awaitingConfirmation(callErr) {
			return nil, nil
		}

		status, code := toolOutcome(r, opts, name, result, callErr)
		// A tool that raised and was not rescued handed nothing back, which is not the same claim as handing back something empty.
		var bytes int64
		var empty bool
		if callErr == nil || result != nil {
			bytes, empty = resultSize(result)
		}
		recordTool(r, ctx, opts, name, held.startedAt, status, code, bytes, empty)
		return nil, nil
	}
}

// OnToolError records a tool call that never reaches AfterTool, and marks one that will.
//
// The framework runs only the error callbacks for a tool the model named that the agent does not have, which is what a model inventing a tool's name looks like, so that call is recorded here or not at all. A tool that ran and raised goes on to AfterTool, which records it; the error is kept for it, because an error callback that rescues the call hands AfterTool the substitute result and no error.
//
// It returns nothing, so the error reaches the model exactly as the tool raised it. The framework stops at the first error callback that returns a result, so this one belongs first in the list.
func OnToolError(r *recorder.Recorder, opts Options) llmagent.OnToolErrorCallback {
	return func(ctx agent.Context, t tool.Tool, _ map[string]any, callErr error) (map[string]any, error) {
		if callErr == nil {
			return nil, nil
		}
		name := toolName(t)
		key := toolKey(ctx, name)
		if reachesAfterTool(t) {
			runningTools.update(key, func(held runningTool) runningTool {
				held.raised = callErr
				return held
			})
			return nil, nil
		}
		held, _ := runningTools.take(key)
		recordTool(r, ctx, opts, name, held.startedAt, pb.Activity_FAILED, toolNotFound, 0, false)
		return nil, nil
	}
}

// reachesAfterTool reports whether the framework goes on to run AfterTool for a tool that raised.
//
// It does for a function tool, and only for one. A tool the agent does not have, or one it cannot call as a function, is handed to the error callbacks as a stand-in carrying only the name the model used, which has no declaration.
func reachesAfterTool(t tool.Tool) bool {
	_, ok := t.(interface {
		Declaration() *genai.FunctionDeclaration
	})
	return ok
}

// awaitingConfirmation reports whether a tool stopped to ask a person before running.
func awaitingConfirmation(err error) bool {
	return errors.Is(err, tool.ErrConfirmationRequired)
}

// recordTool files one tool call.
func recordTool(r *recorder.Recorder, ctx agent.Context, opts Options, name string, started time.Time, status pb.Activity_Status, code string, bytes int64, empty bool) {
	r.RecordIn(ctx, &pb.Activity{
		Agent:            opts.Agent,
		Request:          requestOf(ctx),
		Session:          ctx.SessionID(),
		User:             userOf(r, ctx),
		CallerService:    opts.Service,
		ObservedAs:       pb.Agent_AGENT,
		SubAgent:         ctx.AgentName(),
		CallerComponent:  "tool:" + name,
		Tool:             name,
		Skill:            skillOf(ctx, opts),
		Project:          recorder.ProjectFrom(ctx),
		BilledBy:         opts.billedBy(),
		Region:           opts.region(),
		Framework:        framework,
		FrameworkVersion: frameworkVersion(),
		DurationMs:       millisSince(started, time.Now),
		Status:           status,
		ErrorCode:        code,
		ResultBytes:      bytes,
		EmptyResult:      empty,
		OccurredAt:       timestamppb.Now(),
	})
}

// resultSize measures what a tool handed back.
//
// A size, never the content: the bytes are what a later turn carries, and the
// content is the adopter's to keep. Measured from the JSON the framework will
// serialise the result into, which is what actually reaches the model — a map
// of three large strings costs what they sum to, whatever the map looks like
// in Go.
//
// A result that cannot be marshalled is reported as unmeasured rather than as
// empty: zero bytes and empty are the same claim, and claiming a tool returned
// nothing when it may have returned plenty is the wrong error to make.
func resultSize(result map[string]any) (bytes int64, empty bool) {
	if len(result) == 0 {
		return 0, true
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return 0, false
	}
	return int64(len(encoded)), false
}

// toolName is the name of the tool that ran, where the framework named one.
func toolName(t tool.Tool) string {
	if t == nil {
		return ""
	}
	return t.Name()
}

// toolOutcome reads how a tool call ended.
//
// A raised error is a failure whatever else is true, so it is read first and the
// adopter's own judgement is not consulted: an error already says what went
// wrong, in a code, and a result returned alongside one says nothing. Where
// nothing was raised, the adopter decides, because only the product knows the
// shape of its own tool's answer.
func toolOutcome(r *recorder.Recorder, opts Options, tool string, result map[string]any, callErr error) (pb.Activity_Status, string) {
	if callErr != nil {
		return pb.Activity_FAILED, recorder.ErrorCode(callErr)
	}
	if opts.ToolFailed == nil {
		return pb.Activity_OK, ""
	}
	if failed, code := judgeTool(r, opts, tool, result); failed {
		// A failure the adopter named but gave no code for is still a failure. It is
		// recorded under one the server can classify rather than under nothing, which
		// would read as a success that happened to be marked.
		if code == "" {
			code = "TOOL_ERROR"
		}
		return pb.Activity_FAILED, code
	}
	return pb.Activity_OK, ""
}

// judgeTool runs the adopter's own judgement of a tool's result, and survives it
// panicking.
//
// It is the adopter's code running inside the framework's callback, so a panic in
// it would otherwise fail the tool call it was only meant to describe. A judgement
// that panics is counted and the call is recorded as the framework saw it.
func judgeTool(r *recorder.Recorder, opts Options, tool string, result map[string]any) (failed bool, code string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			r.NotePanicked()
			failed, code = false, ""
		}
	}()
	return opts.ToolFailed(tool, result)
}
