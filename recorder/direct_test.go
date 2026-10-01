package recorder

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	governancepb "github.com/techbridgeinnovation/agentpulse/recorder/pb/governance"
	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

// direct records what a service does through an instrumented client and returns every record that reached the sink.
func direct(t *testing.T, attribution Attribution, do func(*Reporter)) []*pb.Activity {
	t.Helper()
	got, _ := directWithRecorder(t, attribution, do)
	return got
}

// directWithRecorder is direct, but also returns the Recorder so a test can
// read Stats — the downgrade tests need this to distinguish a DOWNGRADE
// that was applied to the outbound call from one that fell open.
func directWithRecorder(t *testing.T, attribution Attribution, do func(*Reporter)) ([]*pb.Activity, *Recorder) {
	t.Helper()
	sink := &captureSink{}
	rec := New(Config{Sinks: []Sink{sink}, FlushEvery: time.Millisecond})
	do(rec.For(attribution))
	closing, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rec.Close(closing)
	return sink.seen, rec
}

var service = Attribution{Agent: "organisations/techbridge/agents/documents", Service: "documents-service"}

// provider stands in for a provider's api, answering every request with one reply.
func provider(t *testing.T, status int, contentType, body string, header ...string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

// through sends one request through a middleware the way an sdk does, reads the whole reply as the sdk would, and returns what the caller got.
func through(t *testing.T, ctx context.Context, mw Middleware, url string, header ...string) []byte {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{"model":"requested","messages":[{"role":"user","content":"the prompt"}]}`))
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := mw(req, http.DefaultTransport.RoundTrip)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return got
}

func counts(a *pb.Activity) map[string]int64 {
	out := map[string]int64{}
	for _, q := range a.GetReportedUsage() {
		out[q.GetUnit()] = q.GetQuantity()
	}
	return out
}

func only(t *testing.T, got []*pb.Activity) *pb.Activity {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("%d records, want 1", len(got))
	}
	return got[0]
}

const anthropicReply = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"the answer"}],"stop_reason":"end_turn","usage":{"input_tokens":1200,"output_tokens":300,"cache_read_input_tokens":800,"cache_creation_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":64},"server_tool_use":{"web_search_requests":2},"service_tier":"standard"}}`

// A Claude call through the sdk records who answered and what it counted, under Anthropic's own names, and the caller reads exactly the reply the provider sent.
func TestAnAnthropicCallIsRecordedFromTheReplyTheCallerReads(t *testing.T) {
	server := provider(t, 200, "application/json", anthropicReply)
	ctx := WithComponent(WithWorkspace(WithUser(context.Background(), User{ID: "u-1"}), "hp"), "report_generation")

	var body []byte
	a := only(t, direct(t, service, func(rp *Reporter) {
		body = through(t, ctx, rp.AnthropicMiddleware(), server.URL+"/v1/messages")
	}))

	if string(body) != anthropicReply {
		t.Fatalf("the caller read %q, want the reply untouched", body)
	}
	if a.GetModel() != "claude-sonnet-4-5" || a.GetBilledBy() != ProviderAnthropic || a.GetUsageFormat() != FormatAnthropic {
		t.Errorf("model %q billed by %q in %q", a.GetModel(), a.GetBilledBy(), a.GetUsageFormat())
	}
	c := counts(a)
	for name, want := range map[string]int64{
		"input_tokens":                             1200,
		"output_tokens":                            300,
		"cache_read_input_tokens":                  800,
		"cache_creation.ephemeral_1h_input_tokens": 64,
		"server_tool_use.web_search_requests":      2,
	} {
		if c[name] != want {
			t.Errorf("%s = %d, want %d", name, c[name], want)
		}
	}
	if a.GetCallerComponent() != "report_generation" || a.GetUser() != "u-1" || a.GetServiceTier() != "standard" {
		t.Errorf("component %q, user %q, tier %q", a.GetCallerComponent(), a.GetUser(), a.GetServiceTier())
	}
	if a.GetStatus() != pb.Activity_OK {
		t.Errorf("status = %v, want OK", a.GetStatus())
	}
}

// A streamed reply states its counts as running totals across events, so what the last one said is what the call cost.
func TestAStreamedAnthropicCallIsRecordedFromItsLastCounts(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":1000,"output_tokens":1,"cache_read_input_tokens":200}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"the answer"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":400}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	server := provider(t, 200, "text/event-stream", stream)

	var body []byte
	a := only(t, direct(t, service, func(rp *Reporter) {
		body = through(t, context.Background(), rp.AnthropicMiddleware(), server.URL+"/v1/messages")
	}))

	if string(body) != stream {
		t.Fatal("the stream reached the caller changed")
	}
	c := counts(a)
	if c["input_tokens"] != 1000 || c["output_tokens"] != 400 || c["cache_read_input_tokens"] != 200 {
		t.Errorf("counts = %v, want the input from the start and the output from the last delta", c)
	}
	if a.GetStatus() != pb.Activity_TRUNCATED || a.GetErrorCode() != "max_tokens" {
		t.Errorf("status %v code %q, want a call cut short by its limit", a.GetStatus(), a.GetErrorCode())
	}
}

// A refused call is recorded as the failure it was, in the provider's own words, and the sdk still reads the error body it needs to raise its own error.
func TestARefusedCallIsRecordedAsAFailure(t *testing.T) {
	refusal := `{"type":"error","error":{"type":"rate_limit_error","message":"the prompt said..."},"request_id":"req_1"}`
	server := provider(t, 429, "application/json", refusal, "Retry-After", "12")

	var body []byte
	a := only(t, direct(t, service, func(rp *Reporter) {
		body = through(t, context.Background(), rp.AnthropicMiddleware(), server.URL+"/v1/messages", "X-Stainless-Retry-Count", "1")
	}))

	if string(body) != refusal {
		t.Fatal("the sdk would not see the error it has to raise")
	}
	if a.GetStatus() != pb.Activity_FAILED || a.GetErrorCode() != "rate_limit_error" || a.GetErrorFormat() != ErrorFormatAnthropic {
		t.Errorf("status %v code %q format %q", a.GetStatus(), a.GetErrorCode(), a.GetErrorFormat())
	}
	fields := map[string]string{}
	for _, f := range a.GetReportedError() {
		fields[f.GetName()] = f.GetValue()
	}
	if fields["retry_after"] != "12" || fields["request_id"] != "req_1" {
		t.Errorf("fields = %v, want the wait and the request id", fields)
	}
	if _, ok := fields["message"]; ok {
		t.Error("the message reached the record")
	}
	// The second attempt of one call, numbered from one.
	if a.GetAttempt() != 2 {
		t.Errorf("attempt = %d, want 2", a.GetAttempt())
	}
}

// Counting tokens costs nothing and is not a model call, so it is passed through unrecorded.
func TestCountingTokensIsNotRecorded(t *testing.T) {
	server := provider(t, 200, "application/json", `{"input_tokens":12}`)
	got := direct(t, service, func(rp *Reporter) {
		through(t, context.Background(), rp.AnthropicMiddleware(), server.URL+"/v1/messages/count_tokens")
	})
	if len(got) != 0 {
		t.Errorf("recorded %d, want none", len(got))
	}
}

// An OpenAI chat names its counts in its own convention, and a client pointed at Perplexity is read in Perplexity's.
func TestAnOpenAIChatIsRecordedInTheConventionOfWhoBilledIt(t *testing.T) {
	reply := `{"id":"c1","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"the answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":900,"completion_tokens":120,"total_tokens":1020,"prompt_tokens_details":{"cached_tokens":300},"completion_tokens_details":{"reasoning_tokens":64}},"service_tier":"default"}`
	server := provider(t, 200, "application/json", reply)

	a := only(t, direct(t, service, func(rp *Reporter) {
		through(t, context.Background(), rp.OpenAIMiddleware(), server.URL+"/v1/chat/completions")
	}))
	if a.GetModel() != "gpt-5" || a.GetUsageFormat() != FormatOpenAIChat || a.GetBilledBy() != ProviderOpenAI {
		t.Errorf("model %q format %q billed by %q", a.GetModel(), a.GetUsageFormat(), a.GetBilledBy())
	}
	if c := counts(a); c["prompt_tokens_details.cached_tokens"] != 300 || c["completion_tokens_details.reasoning_tokens"] != 64 {
		t.Errorf("counts = %v, want the nested counts named by their path", c)
	}

	perplexity := Attribution{Agent: service.Agent, Service: service.Service, BilledBy: ProviderPerplexity}
	p := only(t, direct(t, perplexity, func(rp *Reporter) {
		through(t, context.Background(), rp.OpenAIMiddleware(), server.URL+"/chat/completions")
	}))
	if p.GetUsageFormat() != FormatPerplexity {
		t.Errorf("format = %q, want Perplexity's", p.GetUsageFormat())
	}
}

// A streamed response carries its counts on the event that ends it.
func TestAStreamedOpenAIResponseIsRecordedFromTheEventThatEndsIt(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"model":"gpt-5","status":"in_progress","usage":null}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"the answer"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"model":"gpt-5","status":"completed","usage":{"input_tokens":500,"output_tokens":80,"input_tokens_details":{"cached_tokens":100},"output_tokens_details":{"reasoning_tokens":20}}}}`,
		``,
	}, "\n")
	server := provider(t, 200, "text/event-stream", stream)

	a := only(t, direct(t, service, func(rp *Reporter) {
		through(t, context.Background(), rp.OpenAIMiddleware(), server.URL+"/v1/responses")
	}))
	if a.GetUsageFormat() != FormatOpenAIResponses {
		t.Errorf("format = %q", a.GetUsageFormat())
	}
	if c := counts(a); c["input_tokens"] != 500 || c["output_tokens_details.reasoning_tokens"] != 20 {
		t.Errorf("counts = %v", c)
	}
}

// A streamed chat that did not ask for its counts still records the call, with none, rather than the request being changed to ask.
func TestAStreamedChatWithoutCountsIsStillRecorded(t *testing.T) {
	stream := "data: {\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n" +
		"data: {\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":null}\n\ndata: [DONE]\n\n"
	server := provider(t, 200, "text/event-stream", stream)

	a := only(t, direct(t, service, func(rp *Reporter) {
		through(t, context.Background(), rp.OpenAIMiddleware(), server.URL+"/v1/chat/completions")
	}))
	if a.GetModel() != "gpt-5" || len(a.GetReportedUsage()) != 0 || a.GetUsageFormat() != "" {
		t.Errorf("model %q with %d counts in %q, want the call and no counts", a.GetModel(), len(a.GetReportedUsage()), a.GetUsageFormat())
	}
}

// A reply longer than is ever held is read from its ends, which is where a provider puts the model and the counts.
func TestAReplyLongerThanIsHeldIsStillCounted(t *testing.T) {
	long := `{"id":"c1","object":"chat.completion","model":"gpt-image-1","choices":[{"message":{"content":"` + strings.Repeat("a", 3<<20) + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":4000}}`
	server := provider(t, 200, "application/json", long)

	var body []byte
	a := only(t, direct(t, service, func(rp *Reporter) {
		body = through(t, context.Background(), rp.OpenAIMiddleware(), server.URL+"/v1/chat/completions")
	}))
	if len(body) != len(long) {
		t.Fatalf("caller read %d bytes, want %d", len(body), len(long))
	}
	if a.GetModel() != "gpt-image-1" || counts(a)["completion_tokens"] != 4000 {
		t.Errorf("model %q counts %v", a.GetModel(), counts(a))
	}
}

// A stream the caller stops reading and closes is still recorded, once, with what it had said so far, as cut short.
func TestAnAbandonedStreamIsRecordedOnce(t *testing.T) {
	stream := `data: {"type":"message_start","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":1000}}}` + "\n\n" + strings.Repeat(`data: {"type":"content_block_delta","delta":{"text":"x"}}`+"\n\n", 2000)
	server := provider(t, 200, "text/event-stream", stream)

	a := only(t, direct(t, service, func(rp *Reporter) {
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", nil)
		resp, err := rp.AnthropicMiddleware()(req, http.DefaultTransport.RoundTrip)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 256)
		_, _ = resp.Body.Read(buf)
		_ = resp.Body.Close()
		_ = resp.Body.Close()
	}))
	if counts(a)["input_tokens"] != 1000 {
		t.Errorf("counts = %v, want what the stream had said", counts(a))
	}
	if a.GetStatus() != pb.Activity_TRUNCATED || a.GetErrorCode() != "Canceled" {
		t.Errorf("status %v code %q, want a call the caller cut short", a.GetStatus(), a.GetErrorCode())
	}
}

// A call that never reached a provider is recorded as the failure it was, and the caller gets its error unchanged.
func TestACallThatNeverArrivedIsRecordedAsAFailure(t *testing.T) {
	server := provider(t, 200, "application/json", "{}")
	url := server.URL
	server.Close()

	a := only(t, direct(t, service, func(rp *Reporter) {
		req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", nil)
		if _, err := rp.OpenAIMiddleware()(req, http.DefaultTransport.RoundTrip); err == nil {
			t.Fatal("want the transport's error handed back")
		}
	}))
	if a.GetStatus() != pb.Activity_FAILED {
		t.Errorf("status = %v, want FAILED", a.GetStatus())
	}
}

// A Gemini call through a real genai client is recorded once the client is instrumented, streamed and whole, and the client still gets its answer.
func TestAGenAIClientRecordsEveryGenerateCall(t *testing.T) {
	whole := `{"candidates":[{"content":{"role":"model","parts":[{"text":"the answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":700,"candidatesTokenCount":90,"cachedContentTokenCount":200,"thoughtsTokenCount":30,"totalTokenCount":820},"modelVersion":"gemini-2.5-flash-001"}`
	stream := "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"the \"}]}}],\"usageMetadata\":{\"promptTokenCount\":700,\"totalTokenCount\":700},\"modelVersion\":\"gemini-2.5-flash-001\"}\r\n\r\n" +
		"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"answer\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":700,\"candidatesTokenCount\":90,\"totalTokenCount\":790},\"modelVersion\":\"gemini-2.5-flash-001\"}\r\n\r\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, stream)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, whole)
	}))
	defer server.Close()

	got := direct(t, service, func(rp *Reporter) {
		client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
			Backend: genai.BackendGeminiAPI, APIKey: "test", HTTPOptions: genai.HTTPOptions{BaseURL: server.URL},
		})
		if err != nil {
			t.Fatal(err)
		}
		rp.InstrumentGenAI(client)
		rp.InstrumentGenAI(client)

		resp, err := client.Models.GenerateContent(context.Background(), "gemini-2.5-flash", genai.Text("the prompt"), nil)
		if err != nil || resp.Text() != "the answer" {
			t.Fatalf("generate = %v, %v", resp, err)
		}
		var text string
		for chunk, err := range client.Models.GenerateContentStream(context.Background(), "gemini-2.5-flash", genai.Text("the prompt"), nil) {
			if err != nil {
				t.Fatal(err)
			}
			text += chunk.Text()
		}
		if text != "the answer" {
			t.Fatalf("stream read %q", text)
		}
	})

	if len(got) != 2 {
		t.Fatalf("%d records, want one per call, however many times the client was instrumented", len(got))
	}
	for _, a := range got {
		if a.GetModel() != "gemini-2.5-flash-001" || a.GetUsageFormat() != FormatVertex || a.GetBilledBy() != ProviderVertexAI {
			t.Errorf("model %q format %q billed by %q", a.GetModel(), a.GetUsageFormat(), a.GetBilledBy())
		}
	}
	if c := counts(got[0]); c["cachedContentTokenCount"] != 200 || c["thoughtsTokenCount"] != 30 {
		t.Errorf("whole counts = %v", c)
	}
	if c := counts(got[1]); c["candidatesTokenCount"] != 90 {
		t.Errorf("streamed counts = %v, want the last piece's", c)
	}
}

// A component nobody named is the function in the product's own code that made the call, without the path it was imported from.
func TestAComponentNobodyNamedIsTheFunctionThatMadeTheCall(t *testing.T) {
	for function, want := range map[string]string{
		"rezco.re.sources.service.v1/internal/summary.Generate":           "summary.Generate",
		"main.(*PromptsServiceServerType).GeneratePrompt":                 "PromptsServiceServerType.GeneratePrompt",
		"main.generateTitle.func1":                                        "generateTitle",
		"github.com/acme/reports/internal/writer.(*Writer).Draft.func2.1": "Writer.Draft",
	} {
		if got := shortName(function); got != want {
			t.Errorf("%s = %q, want %q", function, got, want)
		}
	}
	for _, function := range []string{
		"net/http.(*Client).do",
		"github.com/anthropics/anthropic-sdk-go/internal/requestconfig.(*RequestConfig).Execute",
		"google.golang.org/genai.(*Models).GenerateContent",
		ownPackage + ".(*Reporter).observe",
	} {
		if !infrastructure(function) {
			t.Errorf("%s read as the product's own code", function)
		}
	}
}

// fixedDecider answers every decision the same way.
type fixedDecider struct {
	decision            governancepb.DecideResponse_Decision
	replacementProvider string
	replacementModel    string
	err                 error
	panics              bool
	requests            []*governancepb.DecideRequest
}

func (d *fixedDecider) Decide(_ context.Context, req *governancepb.DecideRequest) (*governancepb.DecideResponse, error) {
	if d.panics {
		panic("decider bug")
	}
	d.requests = append(d.requests, req)
	if d.err != nil {
		return nil, d.err
	}
	return &governancepb.DecideResponse{
		Decision:            d.decision,
		ReplacementProvider: d.replacementProvider,
		ReplacementModel:    d.replacementModel,
	}, nil
}

// A budget that stops an agent stops a service spending against it too: the call is never sent, the refusal is recorded, and the sdk is told not to retry it.
func TestAGovernedClientSendsNothingABudgetRefuses(t *testing.T) {
	sent := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sent++
		_, _ = io.WriteString(w, anthropicReply)
	}))
	defer server.Close()
	deny := &fixedDecider{decision: governancepb.DecideResponse_DENY}
	ctx := WithWorkspace(WithUser(context.Background(), User{ID: "u-1"}), "hp")

	a := only(t, direct(t, service, func(rp *Reporter) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/messages", nil)
		resp, err := rp.Governed(Governance{Decider: deny}).AnthropicMiddleware()(req, http.DefaultTransport.RoundTrip)
		if err != nil {
			t.Fatalf("a refusal is a reply the sdk raises, not a transport error it retries: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusForbidden || resp.Header.Get("X-Should-Retry") != "false" {
			t.Errorf("status %d, retry %q", resp.StatusCode, resp.Header.Get("X-Should-Retry"))
		}
		if !Denied(&fakeSDKError{body: string(body)}) {
			t.Errorf("Denied does not recognise the refusal the sdk would raise: %s", body)
		}
	}))

	if sent != 0 {
		t.Errorf("the provider received %d calls, want none", sent)
	}
	if a.GetStatus() != pb.Activity_DENIED {
		t.Errorf("status = %v, want DENIED", a.GetStatus())
	}
	if got := deny.requests[0]; got.GetWorkspace() != "organisations/techbridge/workspaces/hp" || got.GetUser() != "u-1" || got.GetAgent() != service.Agent {
		t.Errorf("asked about %v, want the service's workspace, user and registration", got)
	}
}

// A decision that cannot be made lets the call go ahead: governance unreachable, or a decider that panics, must not be the reason the product stops working.
func TestAGovernedClientGoesAheadWithoutAnAnswer(t *testing.T) {
	server := provider(t, 200, "application/json", anthropicReply)
	for name, d := range map[string]*fixedDecider{
		"unreachable": {err: errors.New("governance down")},
		"panics":      {panics: true},
		"allows":      {decision: governancepb.DecideResponse_ALLOW},
	} {
		a := only(t, direct(t, service, func(rp *Reporter) {
			through(t, context.Background(), rp.Governed(Governance{Decider: d}).AnthropicMiddleware(), server.URL+"/v1/messages")
		}))
		if a.GetStatus() != pb.Activity_OK {
			t.Errorf("%s: status = %v, want the call made", name, a.GetStatus())
		}
	}
}

// A genai client refused by a budget returns ErrDenied, and Denied recognises it through whatever the client wraps it in.
func TestAGovernedGenAIClientReturnsErrDenied(t *testing.T) {
	server := provider(t, 200, "application/json", `{}`)
	got := direct(t, service, func(rp *Reporter) {
		client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
			Backend: genai.BackendGeminiAPI, APIKey: "test", HTTPOptions: genai.HTTPOptions{BaseURL: server.URL},
		})
		if err != nil {
			t.Fatal(err)
		}
		rp.Governed(Governance{Decider: &fixedDecider{decision: governancepb.DecideResponse_DENY}}).InstrumentGenAI(client)
		_, err = client.Models.GenerateContent(context.Background(), "gemini-2.5-flash", genai.Text("the prompt"), nil)
		if !Denied(err) {
			t.Errorf("err = %v, want one Denied recognises", err)
		}
	})
	if len(got) != 1 || got[0].GetStatus() != pb.Activity_DENIED || got[0].GetModel() != "gemini-2.5-flash" {
		t.Fatalf("recorded %v, want one denied call naming its model", got)
	}
}

// A DOWNGRADE governance returns is applied to a genai call by rewriting the
// model segment of the outbound URL — genai's call already parses the
// requested model out of that same URL before the request is sent, so this
// is the one direct-SDK shape this package can safely rewrite in this
// phase.
func TestAGovernedGenAIClientAppliesADowngrade(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"the answer"}]},"finishReason":"STOP"}],"modelVersion":"gemini-2.5-flash-lite"}`)
	}))
	defer server.Close()

	downgrade := &fixedDecider{
		decision:            governancepb.DecideResponse_DOWNGRADE,
		replacementProvider: ProviderVertexAI,
		replacementModel:    "gemini-2.5-flash-lite",
	}
	got, rec := directWithRecorder(t, service, func(rp *Reporter) {
		client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
			Backend: genai.BackendGeminiAPI, APIKey: "test", HTTPOptions: genai.HTTPOptions{BaseURL: server.URL},
		})
		if err != nil {
			t.Fatal(err)
		}
		rp.Governed(Governance{Decider: downgrade}).InstrumentGenAI(client)
		if _, err := client.Models.GenerateContent(context.Background(), "gemini-2.5-pro", genai.Text("the prompt"), nil); err != nil {
			t.Fatalf("generate: %v", err)
		}
	})

	if !strings.Contains(gotPath, "models/gemini-2.5-flash-lite:") {
		t.Fatalf("provider received path %q, want it naming the replacement model gemini-2.5-flash-lite, not the original gemini-2.5-pro", gotPath)
	}
	a := only(t, got)
	if a.GetModel() != "gemini-2.5-flash-lite" {
		t.Errorf("recorded model = %q, want the replacement gemini-2.5-flash-lite", a.GetModel())
	}
	if stats := rec.Stats(); stats.Downgraded != 1 || stats.DowngradeApplied != 1 || stats.DowngradeNotApplied != 0 {
		t.Errorf("stats = %+v, want Downgraded=1, DowngradeApplied=1, DowngradeNotApplied=0", stats)
	}
	if len(downgrade.requests) != 1 {
		t.Fatalf("Decide was called %d times, want exactly 1 — a downgrade must never call Decide again for the replacement", len(downgrade.requests))
	}
	if r := downgrade.requests[0]; r.GetRequestedProvider() != ProviderVertexAI || r.GetRequestedModel() != "gemini-2.5-pro" {
		t.Errorf("asked governance about %q/%q, want %q/gemini-2.5-pro", r.GetRequestedProvider(), r.GetRequestedModel(), ProviderVertexAI)
	}
}

// A DOWNGRADE whose replacement provider does not match the provider this
// client actually talks to is never applied: rewriting only the model name
// would send the call to the wrong endpoint entirely. The original request
// proceeds unchanged, and DowngradeNotApplied — not DowngradeApplied —
// records why.
func TestAGovernedGenAIClientFailsOpenWhenReplacementProviderDoesNotMatch(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"the answer"}]},"finishReason":"STOP"}],"modelVersion":"gemini-2.5-pro"}`)
	}))
	defer server.Close()

	// genai only ever talks to Vertex/the Gemini API — a replacement naming
	// ANTHROPIC can never be reached through this client.
	downgrade := &fixedDecider{
		decision:            governancepb.DecideResponse_DOWNGRADE,
		replacementProvider: ProviderAnthropic,
		replacementModel:    "claude-haiku-4-5",
	}
	_, rec := directWithRecorder(t, service, func(rp *Reporter) {
		client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
			Backend: genai.BackendGeminiAPI, APIKey: "test", HTTPOptions: genai.HTTPOptions{BaseURL: server.URL},
		})
		if err != nil {
			t.Fatal(err)
		}
		rp.Governed(Governance{Decider: downgrade}).InstrumentGenAI(client)
		if _, err := client.Models.GenerateContent(context.Background(), "gemini-2.5-pro", genai.Text("the prompt"), nil); err != nil {
			t.Fatalf("generate: %v", err)
		}
	})

	if !strings.Contains(gotPath, "models/gemini-2.5-pro:") {
		t.Fatalf("provider received path %q, want the original gemini-2.5-pro — a provider-mismatched replacement must never be attempted", gotPath)
	}
	if stats := rec.Stats(); stats.Downgraded != 1 || stats.DowngradeApplied != 0 || stats.DowngradeNotApplied != 1 {
		t.Errorf("stats = %+v, want Downgraded=1, DowngradeApplied=0, DowngradeNotApplied=1", stats)
	}
}

// A DOWNGRADE with no replacement model named — governance's contract
// guarantees this cannot happen for a real DOWNGRADE, but a decider stub or
// a future contract change should not crash this integration — is treated
// as nothing to apply: the call proceeds with the model it originally
// asked for, and neither DowngradeApplied nor DowngradeNotApplied is
// counted, since no application was ever attempted.
func TestAGovernedClientTreatsADowngradeWithNoReplacementModelAsNothingToApply(t *testing.T) {
	server := provider(t, 200, "application/json", anthropicReply)
	downgrade := &fixedDecider{decision: governancepb.DecideResponse_DOWNGRADE}
	_, rec := directWithRecorder(t, service, func(rp *Reporter) {
		through(t, context.Background(), rp.Governed(Governance{Decider: downgrade}).AnthropicMiddleware(), server.URL+"/v1/messages")
	})

	if stats := rec.Stats(); stats.Downgraded != 1 || stats.DowngradeApplied != 0 || stats.DowngradeNotApplied != 0 {
		t.Errorf("stats = %+v, want Downgraded=1, DowngradeApplied=0, DowngradeNotApplied=0 — nothing was ever attempted", stats)
	}
}

// A DOWNGRADE never applies to a native Anthropic call: /v1/messages names
// its model in the JSON request body, which this package does not read or
// rewrite before sending. The call proceeds with the model it originally
// asked for.
func TestAGovernedAnthropicClientNeverAppliesADowngradeOnTheNativePath(t *testing.T) {
	server := provider(t, 200, "application/json", anthropicReply)
	downgrade := &fixedDecider{
		decision:            governancepb.DecideResponse_DOWNGRADE,
		replacementProvider: ProviderAnthropic,
		replacementModel:    "claude-haiku-4-5",
	}
	got, rec := directWithRecorder(t, service, func(rp *Reporter) {
		through(t, context.Background(), rp.Governed(Governance{Decider: downgrade}).AnthropicMiddleware(), server.URL+"/v1/messages")
	})

	a := only(t, got)
	if a.GetModel() != "claude-sonnet-4-5" {
		t.Errorf("recorded model = %q, want the reply's own claude-sonnet-4-5 — never the unapplied replacement", a.GetModel())
	}
	if stats := rec.Stats(); stats.DowngradeApplied != 0 || stats.DowngradeNotApplied != 1 {
		t.Errorf("stats = %+v, want DowngradeApplied=0, DowngradeNotApplied=1", stats)
	}
}

// A DOWNGRADE is applied to an Anthropic call routed through Vertex by
// rewriting the model segment of that rewritten URL — the one Anthropic
// request shape whose model this package can read before sending, exactly
// as call already does for billedBy and recording.
func TestAGovernedAnthropicClientAppliesADowngradeOnTheVertexPath(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicReply)
	}))
	defer server.Close()

	downgrade := &fixedDecider{
		decision:            governancepb.DecideResponse_DOWNGRADE,
		replacementProvider: ProviderVertexAI,
		replacementModel:    "claude-haiku-4-5",
	}
	_, rec := directWithRecorder(t, service, func(rp *Reporter) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
			server.URL+"/v1/projects/acme/locations/us-east5/publishers/anthropic/models/claude-sonnet-4-5:rawPredict",
			strings.NewReader(`{"anthropic_version":"vertex-2023-10-16","messages":[{"role":"user","content":"the prompt"}]}`))
		resp, err := rp.Governed(Governance{Decider: downgrade}).AnthropicMiddleware()(req, http.DefaultTransport.RoundTrip)
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	})

	if !strings.Contains(gotPath, "/publishers/anthropic/models/claude-haiku-4-5:rawPredict") {
		t.Fatalf("provider received path %q, want it naming the replacement claude-haiku-4-5, not the original claude-sonnet-4-5", gotPath)
	}
	if stats := rec.Stats(); stats.DowngradeApplied != 1 || stats.DowngradeNotApplied != 0 {
		t.Errorf("stats = %+v, want DowngradeApplied=1, DowngradeNotApplied=0", stats)
	}
}

// A DOWNGRADE never applies to an OpenAI-protocol call: Chat Completions and
// Responses both name their model in the JSON request body, which this
// package does not read or rewrite before sending. The call proceeds with
// the model it originally asked for, exactly as the native Anthropic path
// does.
func TestAGovernedOpenAIClientNeverAppliesADowngrade(t *testing.T) {
	server := provider(t, 200, "application/json", `{"id":"x","model":"gpt-5","choices":[{"finish_reason":"stop"}]}`)
	downgrade := &fixedDecider{
		decision:            governancepb.DecideResponse_DOWNGRADE,
		replacementProvider: ProviderOpenAI,
		replacementModel:    "gpt-5-mini",
	}
	got, rec := directWithRecorder(t, service, func(rp *Reporter) {
		through(t, context.Background(), rp.Governed(Governance{Decider: downgrade}).OpenAIMiddleware(), server.URL+"/v1/chat/completions")
	})

	a := only(t, got)
	if a.GetModel() != "gpt-5" {
		t.Errorf("recorded model = %q, want the reply's own gpt-5 — never the unapplied replacement", a.GetModel())
	}
	if stats := rec.Stats(); stats.Downgraded != 1 || stats.DowngradeApplied != 0 || stats.DowngradeNotApplied != 1 {
		t.Errorf("stats = %+v, want Downgraded=1, DowngradeApplied=0, DowngradeNotApplied=1", stats)
	}
}

// TestARetriedCallAsksGovernanceOncePerAttemptNeverRecursively proves a
// downgrade cannot create a loop: decide is called exactly once per
// middleware invocation and never calls itself, so an sdk's own retry —
// which re-enters the middleware for each attempt, exactly as
// X-Stainless-Retry-Count already proves elsewhere — asks governance once
// per attempt, bounded by however many attempts the sdk itself makes, never
// unboundedly and never as a side effect of the downgrade that was applied
// to the previous attempt.
func TestARetriedCallAsksGovernanceOncePerAttemptNeverRecursively(t *testing.T) {
	server := provider(t, 200, "application/json", `{"id":"x","model":"gpt-5-mini","choices":[{"finish_reason":"stop"}]}`)
	downgrade := &fixedDecider{decision: governancepb.DecideResponse_DOWNGRADE, replacementProvider: ProviderOpenAI, replacementModel: "gpt-5-mini"}

	got := direct(t, service, func(rp *Reporter) {
		mw := rp.Governed(Governance{Decider: downgrade}).OpenAIMiddleware()
		// Two separate middleware invocations, the same shape an sdk's own
		// retry loop produces: it calls the whole middleware chain again
		// for each attempt, this library included, rather than looping
		// inside any one call to it.
		through(t, context.Background(), mw, server.URL+"/v1/chat/completions")
		through(t, context.Background(), mw, server.URL+"/v1/chat/completions", "X-Stainless-Retry-Count", "1")
	})

	if len(downgrade.requests) != 2 {
		t.Fatalf("Decide was called %d times for two attempts, want exactly 2 — one per attempt, never more", len(downgrade.requests))
	}
	if len(got) != 2 {
		t.Fatalf("recorded %d activities, want 2", len(got))
	}
	if got[0].GetAttempt() != 0 || got[1].GetAttempt() != 2 {
		t.Fatalf("attempts = %d, %d, want 0, 2 — attemptOf reports X-Stainless-Retry-Count + 1", got[0].GetAttempt(), got[1].GetAttempt())
	}
}

// A DOWNGRADE is never treated as a DENY: the call must reach the provider
// even where this integration cannot apply the replacement.
func TestADowngradeIsNeverTreatedAsADeny(t *testing.T) {
	sent := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sent = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"gpt-5","choices":[{"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	downgrade := &fixedDecider{decision: governancepb.DecideResponse_DOWNGRADE, replacementProvider: ProviderOpenAI, replacementModel: "gpt-5-mini"}
	got := only(t, direct(t, service, func(rp *Reporter) {
		through(t, context.Background(), rp.Governed(Governance{Decider: downgrade}).OpenAIMiddleware(), server.URL+"/v1/chat/completions")
	}))

	if !sent {
		t.Fatal("the provider never received the call — DOWNGRADE must proceed, never block like DENY")
	}
	if got.GetStatus() != pb.Activity_OK {
		t.Errorf("status = %v, want OK", got.GetStatus())
	}
}

// panickingProtocol is a protocol with a bug in how it reads a whole reply.
type panickingProtocol struct{ openAIProtocol }

func (panickingProtocol) reply(func(string) json.RawMessage, *observed) { panic("protocol bug") }

// A protocol that panics while a reply is read never leaves the call locked or reaches the caller, and the call is still recorded and the panic counted.
func TestAProtocolThatPanicsNeitherHangsNorLosesTheRecord(t *testing.T) {
	server := provider(t, 200, "application/json", `{"model":"gpt-5"}`)

	got, rec := directWithRecorder(t, service, func(rp *Reporter) {
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", nil)
		resp, err := rp.observe(panickingProtocol{}, req, http.DefaultTransport.RoundTrip)
		if err != nil {
			t.Fatal(err)
		}
		closed := make(chan struct{})
		go func() {
			_, _ = io.ReadAll(resp.Body)
			_, _ = resp.Body.Read(make([]byte, 8))
			_ = resp.Body.Close()
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(2 * time.Second):
			t.Fatal("closing the reply hung after the protocol panicked")
		}
	})
	only(t, got)
	if rec.Stats().Panicked != 1 {
		t.Errorf("panicked = %d, want 1", rec.Stats().Panicked)
	}
}

// A reply that breaks off part way is recorded as the failure it was, under the code its error reduces to, not as a call that succeeded and counted nothing.
func TestAReplyThatBreaksOffIsRecordedAsAFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "1000")
		_, _ = io.WriteString(w, `{"model":"gpt-5",`)
	}))
	defer server.Close()

	a := only(t, direct(t, service, func(rp *Reporter) {
		through(t, context.Background(), rp.OpenAIMiddleware(), server.URL+"/v1/chat/completions")
	}))
	if a.GetStatus() != pb.Activity_FAILED || a.GetErrorCode() != ErrorCode(io.ErrUnexpectedEOF) {
		t.Errorf("status %v code %q, want a failure", a.GetStatus(), a.GetErrorCode())
	}
}

// A refusal whose body is compressed is still recorded as a refusal, from its status.
func TestACompressedRefusalIsRecordedAsAFailure(t *testing.T) {
	server := provider(t, 503, "application/json", "compressed bytes", "Content-Encoding", "gzip")

	a := only(t, direct(t, service, func(rp *Reporter) {
		through(t, context.Background(), rp.OpenAIMiddleware(), server.URL+"/v1/chat/completions", "Accept-Encoding", "gzip")
	}))
	if a.GetStatus() != pb.Activity_FAILED || a.GetErrorCode() == "" {
		t.Errorf("status %v code %q, want a failure with a code", a.GetStatus(), a.GetErrorCode())
	}
}

// A genai stream that ends in an error line with no event prefix is recorded as that error, which is what the genai client raises.
func TestAGenAIStreamEndingInABareErrorIsRecordedAsAFailure(t *testing.T) {
	stream := "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"the \"}]}}],\"usageMetadata\":{\"promptTokenCount\":700},\"modelVersion\":\"gemini-2.5-flash-001\"}\n\n" +
		"{\"error\":{\"code\":429,\"message\":\"the prompt said\",\"status\":\"RESOURCE_EXHAUSTED\"}}\n"
	server := provider(t, 200, "text/event-stream", stream)

	a := only(t, direct(t, service, func(rp *Reporter) {
		client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
			Backend: genai.BackendGeminiAPI, APIKey: "test", HTTPOptions: genai.HTTPOptions{BaseURL: server.URL},
		})
		if err != nil {
			t.Fatal(err)
		}
		rp.InstrumentGenAI(client)
		var failed error
		for _, err := range client.Models.GenerateContentStream(context.Background(), "gemini-2.5-flash", genai.Text("the prompt"), nil) {
			if err != nil {
				failed = err
			}
		}
		if failed == nil {
			t.Fatal("the client raised no error for the stream")
		}
	}))
	if a.GetStatus() != pb.Activity_FAILED || a.GetErrorCode() != "RESOURCE_EXHAUSTED" {
		t.Errorf("status %v code %q, want the stream's error", a.GetStatus(), a.GetErrorCode())
	}
	if counts(a)["promptTokenCount"] != 700 {
		t.Errorf("counts = %v, want what the stream said before it failed", counts(a))
	}
}

// A whole reply the caller has read in full is complete, even where it is closed before the end of the body is reported.
func TestAWholeReplyClosedOnceReadIsComplete(t *testing.T) {
	reply := `{"model":"gpt-5","choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":10}}`
	a := only(t, direct(t, service, func(rp *Reporter) {
		req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions", nil)
		resp, err := rp.OpenAIMiddleware()(req, func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(reply))}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadFull(resp.Body, make([]byte, len(reply)))
		_ = resp.Body.Close()
	}))
	if a.GetStatus() != pb.Activity_OK || counts(a)["prompt_tokens"] != 10 {
		t.Errorf("status %v counts %v, want a complete call", a.GetStatus(), counts(a))
	}
}

// A stream whose caller's context ends part way is recorded as cut short, under the context's code, with what it had said.
func TestAStreamWhoseContextEndsIsRecordedAsCutShort(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"message_start","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":1000}}}`+"\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	a := only(t, direct(t, service, func(rp *Reporter) {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/messages", nil)
		resp, err := rp.AnthropicMiddleware()(req, http.DefaultTransport.RoundTrip)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = resp.Body.Read(make([]byte, 4096))
		cancel()
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}))
	if a.GetStatus() != pb.Activity_TRUNCATED || a.GetErrorCode() != "Canceled" || counts(a)["input_tokens"] != 1000 {
		t.Errorf("status %v code %q counts %v, want a call cut short", a.GetStatus(), a.GetErrorCode(), counts(a))
	}
}

// A transport that answers with neither a reply nor an error is recorded as a failure.
func TestACallWithNoReplyIsRecordedAsAFailure(t *testing.T) {
	a := only(t, direct(t, service, func(rp *Reporter) {
		req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions", nil)
		_, _ = rp.OpenAIMiddleware()(req, func(*http.Request) (*http.Response, error) { return nil, nil })
	}))
	if a.GetStatus() != pb.Activity_FAILED || a.GetErrorCode() != codeNoResponse {
		t.Errorf("status %v code %q, want a failure", a.GetStatus(), a.GetErrorCode())
	}
}

// slowDecider allows every call after a wait.
type slowDecider struct{ wait time.Duration }

func (d slowDecider) Decide(context.Context, *governancepb.DecideRequest) (*governancepb.DecideResponse, error) {
	time.Sleep(d.wait)
	return &governancepb.DecideResponse{Decision: governancepb.DecideResponse_ALLOW}, nil
}

// How long a call took is the model's time alone, not the wait for governance before it.
func TestACallsDurationLeavesOutTheSpendDecision(t *testing.T) {
	server := provider(t, 200, "application/json", anthropicReply)
	a := only(t, direct(t, service, func(rp *Reporter) {
		through(t, context.Background(), rp.Governed(Governance{Decider: slowDecider{wait: 300 * time.Millisecond}}).AnthropicMiddleware(), server.URL+"/v1/messages")
	}))
	if a.GetDurationMs() >= 250 {
		t.Errorf("duration = %dms, want the decision's wait left out", a.GetDurationMs())
	}
}

// A post through an instrumented client that is not recorded is counted, so the spend it may carry is not invisible. A read is not.
func TestARequestPassedThroughUnrecordedIsCounted(t *testing.T) {
	server := provider(t, 200, "application/json", `{"input_tokens":12}`)
	_, rec := directWithRecorder(t, service, func(rp *Reporter) {
		through(t, context.Background(), rp.AnthropicMiddleware(), server.URL+"/v1/messages/count_tokens")
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/models", nil)
		resp, err := rp.AnthropicMiddleware()(req, http.DefaultTransport.RoundTrip)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	})
	if got := rec.Stats().Unrecorded; got != 1 {
		t.Errorf("unrecorded = %d, want 1", got)
	}
}
