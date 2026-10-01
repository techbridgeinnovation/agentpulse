package adkv2hooks

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/techbridgeinnovation/agentpulse/recorder"
	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

// promptQuote is what a provider's error message looks like: it quotes the prompt back, which is why no field on a record may carry it.
const promptQuote = "request rejected while processing: <the user's prompt>"

// The framework skips AfterModel for a call that returned an error, so a rejected call was never recorded at all.
func TestARejectedModelCallIsRecordedAsFailed(t *testing.T) {
	ctx := contextFor(t.Name())
	var resp *adkmodel.LLMResponse
	var err error
	got := record(t, func(r *recorder.Recorder) {
		BeforeModel(r, options())(ctx, &adkmodel.LLMRequest{Model: "gemini-3.5-pro"})
		time.Sleep(2 * time.Millisecond)
		resp, err = OnModelError(r, options())(ctx, &adkmodel.LLMRequest{Model: "gemini-3.5-pro"},
			genai.APIError{Code: 400, Status: "INVALID_ARGUMENT", Message: promptQuote})
	})

	if resp != nil || err != nil {
		t.Fatalf("OnModelError = (%v, %v), want (nil, nil) so the error reaches the agent unchanged", resp, err)
	}
	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
	a := got[0]
	if a.GetStatus() != pb.Activity_FAILED || a.GetErrorCode() != "INVALID_ARGUMENT" {
		t.Errorf("recorded %v %q, want FAILED INVALID_ARGUMENT", a.GetStatus(), a.GetErrorCode())
	}
	if a.GetErrorFormat() == "" || len(a.GetReportedError()) == 0 {
		t.Errorf("reported error = %q %v, want the provider's own fields", a.GetErrorFormat(), a.GetReportedError())
	}
	for _, field := range a.GetReportedError() {
		if field.GetValue() == promptQuote {
			t.Fatal("the provider's message reached the record")
		}
	}
	if a.GetModel() != "gemini-3.5-pro" {
		t.Errorf("model = %q, want the model requested over the configured one", a.GetModel())
	}
	if a.GetDurationMs() < 1 {
		t.Errorf("duration = %dms, want the time since BeforeModel", a.GetDurationMs())
	}
}

// A stream that breaks partway has already been billed for the chunks it delivered, and those chunks are the only place the usage was reported.
func TestAStreamThatBreaksKeepsTheUsageItsChunksReported(t *testing.T) {
	ctx := contextFor(t.Name())
	got := record(t, func(r *recorder.Recorder) {
		after := AfterModel(r, options())
		BeforeModel(r, options())(ctx, &adkmodel.LLMRequest{Model: "gemini-3.5-pro"})
		after(ctx, &adkmodel.LLMResponse{ModelVersion: "gemini-3.5-pro-001", Partial: true,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 800, CandidatesTokenCount: 10, TotalTokenCount: 810}}, nil)
		after(ctx, &adkmodel.LLMResponse{Partial: true,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 800, CandidatesTokenCount: 40, TotalTokenCount: 840}}, nil)
		OnModelError(r, options())(ctx, &adkmodel.LLMRequest{Model: "gemini-3.5-pro"}, context.DeadlineExceeded)
	})

	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
	a := got[0]
	if a.GetModel() != "gemini-3.5-pro-001" {
		t.Errorf("model = %q, want the model the stream reported", a.GetModel())
	}
	if a.GetPromptTokens() != 800 || a.GetCandidateTokens() != 40 || a.GetTotalTokens() != 840 {
		t.Errorf("tokens = %d/%d/%d, want the latest chunk's 800/40/840", a.GetPromptTokens(), a.GetCandidateTokens(), a.GetTotalTokens())
	}
	if a.GetErrorCode() != "DeadlineExceeded" {
		t.Errorf("error code = %q, want DeadlineExceeded", a.GetErrorCode())
	}
}

// A streamed call names the model its chunks reported, which is what was served and billed, ahead of the one configured.
func TestAStreamedCallNamesTheModelItsChunksReported(t *testing.T) {
	ctx := contextFor(t.Name())
	got := record(t, func(r *recorder.Recorder) {
		after := AfterModel(r, options())
		after(ctx, &adkmodel.LLMResponse{ModelVersion: "gemini-3.5-flash-lite-001", Partial: true}, nil)
		after(ctx, &adkmodel.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{TotalTokenCount: 10}}, nil)
	})

	if len(got) != 1 || got[0].GetModel() != "gemini-3.5-flash-lite-001" {
		t.Fatalf("recorded %v, want one call against the model the stream reported", got)
	}
}

// An error callback may hand the framework a response in place of the error, which then reaches AfterModel as an ordinary success. That is the same call, already recorded.
func TestACallRescuedAfterItFailedIsRecordedOnce(t *testing.T) {
	ctx := contextFor(t.Name())
	got := record(t, func(r *recorder.Recorder) {
		BeforeModel(r, options())(ctx, &adkmodel.LLMRequest{Model: "gemini-3.5-pro"})
		OnModelError(r, options())(ctx, &adkmodel.LLMRequest{Model: "gemini-3.5-pro"}, genai.APIError{Code: 429, Status: "RESOURCE_EXHAUSTED"})
		AfterModel(r, options())(ctx, &adkmodel.LLMResponse{Content: genai.NewContentFromText("try again later", genai.RoleModel)}, nil)
	})

	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
	if got[0].GetStatus() != pb.Activity_FAILED {
		t.Errorf("status = %v, want the failure, not the substitute's success", got[0].GetStatus())
	}
}

func TestTheCallAfterAFailedOneIsRecorded(t *testing.T) {
	ctx := contextFor(t.Name())
	got := record(t, func(r *recorder.Recorder) {
		BeforeModel(r, options())(ctx, &adkmodel.LLMRequest{Model: "gemini-3.5-pro"})
		OnModelError(r, options())(ctx, &adkmodel.LLMRequest{Model: "gemini-3.5-pro"}, context.Canceled)
		BeforeModel(r, options())(ctx, &adkmodel.LLMRequest{Model: "gemini-3.5-flash"})
		AfterModel(r, options())(ctx, &adkmodel.LLMResponse{}, nil)
	})

	if len(got) != 2 {
		t.Fatalf("recorded %d activities, want 2", len(got))
	}
	if got[1].GetStatus() != pb.Activity_OK || got[1].GetModel() != "gemini-3.5-flash" {
		t.Errorf("second call = %v %q, want OK against its own model", got[1].GetStatus(), got[1].GetModel())
	}
}

// An agent that registers the error callback and not BeforeModel still records a model, and never the error's message.
func TestAFailedCallWithoutBeforeModelStillNamesAModel(t *testing.T) {
	got := record(t, func(r *recorder.Recorder) {
		OnModelError(r, options())(contextFor(t.Name()), nil, errors.New(promptQuote))
	})

	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
	if got[0].GetModel() != options().Model {
		t.Errorf("model = %q, want the configured %q", got[0].GetModel(), options().Model)
	}
	if got[0].GetErrorCode() == promptQuote {
		t.Fatal("the error's message reached the record")
	}
}

// functionTool is a tool the framework can call as a function, which is the kind it goes on to run AfterTool for once the tool has raised.
type functionTool struct{ name string }

func (f functionTool) Name() string                          { return f.name }
func (functionTool) Description() string                     { return "" }
func (functionTool) IsLongRunning() bool                     { return false }
func (functionTool) Declaration() *genai.FunctionDeclaration { return nil }

// notFoundTool is the stand-in the framework hands the error callbacks for a tool the agent does not have: a name and nothing more.
type notFoundTool struct{ name string }

func (f notFoundTool) Name() string      { return f.name }
func (notFoundTool) Description() string { return "Tool not found" }
func (notFoundTool) IsLongRunning() bool { return false }

// A model that invents a tool's name reaches only the error callbacks, so without OnToolError the call is never recorded.
func TestAToolTheAgentDoesNotHaveIsRecordedAsFailed(t *testing.T) {
	var result map[string]any
	var err error
	got := record(t, func(r *recorder.Recorder) {
		result, err = OnToolError(r, options())(contextFor(t.Name()), notFoundTool{name: "serach_web"}, nil,
			errors.New("tool 'serach_web' not found"))
	})

	if result != nil || err != nil {
		t.Fatalf("OnToolError = (%v, %v), want (nil, nil) so the error reaches the model unchanged", result, err)
	}
	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
	if got[0].GetStatus() != pb.Activity_FAILED || got[0].GetErrorCode() != toolNotFound || got[0].GetTool() != "serach_web" {
		t.Errorf("recorded %v %q %q, want FAILED %s for the name the model used", got[0].GetStatus(), got[0].GetErrorCode(), got[0].GetTool(), toolNotFound)
	}
}

// A tool that ran and raised reaches both callbacks, and is one call, timed from BeforeTool.
func TestAToolThatRaisedIsRecordedOnce(t *testing.T) {
	raised := genai.APIError{Code: 503, Status: "UNAVAILABLE", Message: "backend down"}
	got := record(t, func(r *recorder.Recorder) {
		ctx := contextFor(t.Name())
		lookup := functionTool{name: "lookup"}
		BeforeTool(r, options())(ctx, lookup, nil)
		time.Sleep(2 * time.Millisecond)
		OnToolError(r, options())(ctx, lookup, nil, raised)
		AfterTool(r, options())(ctx, lookup, nil, nil, raised)
	})

	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
	if got[0].GetStatus() != pb.Activity_FAILED || got[0].GetErrorCode() != "UNAVAILABLE" {
		t.Errorf("recorded %v %q, want FAILED UNAVAILABLE", got[0].GetStatus(), got[0].GetErrorCode())
	}
	if got[0].GetDurationMs() < 1 {
		t.Errorf("duration = %dms, want the time since BeforeTool", got[0].GetDurationMs())
	}
	if got[0].GetEmptyResult() {
		t.Error("a tool that raised was recorded as handing back an empty result")
	}
}

// An error callback that rescues a tool hands AfterTool the substitute and no error, which without the kept error is recorded as a success.
func TestAToolRescuedAfterItRaisedIsStillRecordedAsFailed(t *testing.T) {
	before, held := runningTools.size(), 0
	got := record(t, func(r *recorder.Recorder) {
		ctx := contextFor(t.Name())
		lookup := functionTool{name: "lookup"}
		OnToolError(r, options())(ctx, lookup, nil, context.DeadlineExceeded)
		AfterTool(r, options())(ctx, lookup, nil, map[string]any{"fallback": true}, nil)
		held = runningTools.size()
	})

	if len(got) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(got))
	}
	if got[0].GetStatus() != pb.Activity_FAILED || got[0].GetErrorCode() != "DeadlineExceeded" {
		t.Errorf("recorded %v %q, want FAILED DeadlineExceeded", got[0].GetStatus(), got[0].GetErrorCode())
	}
	if held != before {
		t.Errorf("the store holds %d calls after the call was recorded, want %d", held, before)
	}
}

// A tool waiting on a person has not run. The framework runs it again once they answer, and that run is the call.
func TestAToolAwaitingConfirmationIsNotRecorded(t *testing.T) {
	pending := fmt.Errorf("error tool %q %w", "send_email", tool.ErrConfirmationRequired)
	got := record(t, func(r *recorder.Recorder) {
		ctx := contextFor(t.Name())
		send := functionTool{name: "send_email"}
		BeforeTool(r, options())(ctx, send, nil)
		OnToolError(r, options())(ctx, send, nil, pending)
		AfterTool(r, options())(ctx, send, nil, nil, pending)
	})

	if len(got) != 0 {
		t.Fatalf("recorded %d activities, want none for a call that has not run", len(got))
	}
}
