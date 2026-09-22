package adkhooks

import (
	"time"

	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/tool"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
	"github.com/techbridgeinnovation/agentpulse/recorder"
)

// runningTools holds when each tool call began, until the call ends and is recorded.
//
// Separate from the model store because the two are keyed differently: a model call is the one an agent has in flight, while several tool calls can be in flight at once for the same agent, each with its own id from the framework.
var runningTools = newInFlightStore[time.Time](maxInFlightCalls, inFlightCallTTL, time.Now)

// toolKey identifies one tool call.
//
// The framework's id for the function call is what distinguishes two calls to the same tool in the same turn, which is exactly what a parallel agent does. Where there is none, the invocation and the tool's name still tell one tool's call apart from another's in the same turn — one call of each at a time, which is the sequential case.
func toolKey(ctx tool.Context, name string) string {
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
	return func(ctx tool.Context, t tool.Tool, _ map[string]any) (map[string]any, error) {
		runningTools.put(toolKey(ctx, toolName(t)), time.Now())
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
func AfterTool(r *recorder.Recorder, opts Options) llmagent.AfterToolCallback {
	return func(ctx tool.Context, t tool.Tool, _, result map[string]any, callErr error) (map[string]any, error) {
		name := toolName(t)
		started, _ := runningTools.take(toolKey(ctx, name))

		status, code := toolOutcome(opts, name, result, callErr)

		r.RecordIn(ctx, &pb.Activity{
			Agent:           opts.Agent,
			Request:         requestOf(ctx),
			Session:         ctx.SessionID(),
			User:            userOf(r, ctx),
			CallerService:   opts.Service,
			CallerComponent: "tool:" + name,
			Tool:            name,
			Skill:           opts.Skill,
			Project:         recorder.ProjectFrom(ctx),
			BilledBy:        opts.billedBy(),
			DurationMs:      millisSince(started, time.Now),
			Status:          status,
			ErrorCode:       code,
			OccurredAt:      timestamppb.Now(),
		})
		return nil, nil
	}
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
func toolOutcome(opts Options, tool string, result map[string]any, callErr error) (pb.Activity_Status, string) {
	if callErr != nil {
		return pb.Activity_FAILED, recorder.ErrorCode(callErr)
	}
	if opts.ToolFailed == nil {
		return pb.Activity_OK, ""
	}
	if failed, code := opts.ToolFailed(tool, result); failed {
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
