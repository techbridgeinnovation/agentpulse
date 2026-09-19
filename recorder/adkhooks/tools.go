package adkhooks

import (
	"time"

	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/tool"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/techbridgeinnovation/agentpulse/recorder"
	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
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
	return func(ctx tool.Context, t tool.Tool, _, _ map[string]any, callErr error) (map[string]any, error) {
		name := toolName(t)
		started, _ := runningTools.take(toolKey(ctx, name))

		status := pb.Activity_OK
		if callErr != nil {
			status = pb.Activity_FAILED
		}

		r.RecordIn(ctx, &pb.Activity{
			Agent:           opts.Agent,
			Request:         requestOf(ctx),
			Session:         ctx.SessionID(),
			User:            userOf(r, ctx),
			CallerService:   opts.Service,
			CallerComponent: "tool:" + name,
			Skill:           opts.Skill,
			Project:         recorder.ProjectFrom(ctx),
			Provider:        opts.provider(),
			DurationMs:      millisSince(started, time.Now),
			Status:          status,
			ErrorCode:       recorder.ErrorCode(callErr),
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
