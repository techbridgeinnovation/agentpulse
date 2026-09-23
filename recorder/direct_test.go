package recorder

import (
	"context"
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
	sink := &captureSink{}
	rec := New(Config{Sinks: []Sink{sink}, FlushEvery: time.Millisecond})
	do(rec.For(attribution))
	closing, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rec.Close(closing)
	return sink.seen
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

// A stream the caller stops reading and closes is still recorded, once, with what it had said so far.
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
	decision governancepb.DecideResponse_Decision
	err      error
	panics   bool
	requests []*governancepb.DecideRequest
}

func (d *fixedDecider) Decide(_ context.Context, req *governancepb.DecideRequest) (*governancepb.DecideResponse, error) {
	if d.panics {
		panic("decider bug")
	}
	d.requests = append(d.requests, req)
	if d.err != nil {
		return nil, d.err
	}
	return &governancepb.DecideResponse{Decision: d.decision}, nil
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
