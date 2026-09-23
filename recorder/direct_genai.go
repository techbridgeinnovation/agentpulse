package recorder

import (
	"encoding/json"
	"net/http"
	"strings"

	"google.golang.org/genai"
)

// InstrumentGenAI records every generate call made through a genai client, Gemini api and Vertex alike:
//
//	client, err := genai.NewClient(ctx, config)
//	reporter.InstrumentGenAI(client)
//
// Called on the client once it is built rather than on its config, because the client builds its own authenticated transport for Vertex and this has to sit in front of that one rather than replace it. Every call the client makes afterwards passes through it, including the ones written later.
//
// Each call is recorded under the reporter's attribution, with the user, request, session, workspace and project read from the call's context, and the part of the product that made it from WithComponent or, where none is named, the function that made the call. Token counting, embeddings and file uploads pass through unrecorded, and so does the Live api, which does not travel over the client's transport at all.
//
// A client built on an http.Client the product shares with other code instruments that client for all of it. Only generate calls are ever recorded, so the other code is passed through untouched.
func (rp *Reporter) InstrumentGenAI(client *genai.Client) {
	if rp == nil || client == nil {
		return
	}
	httpClient := client.ClientConfig().HTTPClient
	if httpClient == nil {
		return
	}
	if existing, ok := httpClient.Transport.(*genaiTransport); ok && existing.rp == rp {
		return
	}
	base := httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	httpClient.Transport = &genaiTransport{rp: rp, base: base}
}

// genaiTransport records a genai client's generate calls on their way through.
type genaiTransport struct {
	rp   *Reporter
	base http.RoundTripper
}

func (t *genaiTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.rp.observe(genaiProtocol{}, req, t.base.RoundTrip)
}

type genaiProtocol struct{}

func (genaiProtocol) call(req *http.Request) (bool, string) {
	if req.Method != http.MethodPost || req.URL == nil {
		return false, ""
	}
	path := req.URL.Path
	if !strings.HasSuffix(path, ":generateContent") && !strings.HasSuffix(path, ":streamGenerateContent") {
		return false, ""
	}
	model := path[:strings.LastIndex(path, ":")]
	if i := strings.LastIndex(model, "models/"); i >= 0 {
		model = model[i+len("models/"):]
	}
	return true, model
}

func (genaiProtocol) format(*http.Request, string) string { return FormatVertex }

func (genaiProtocol) billedBy(*http.Request) string { return ProviderVertexAI }

// reply reads a whole generate reply. Its counts, model and finish sit at the top level, as they do on each piece of a streamed one.
func (genaiProtocol) reply(field func(string) json.RawMessage, o *observed) {
	readGenerate(field("usageMetadata"), field("modelVersion"), field("candidates"), field("promptFeedback"), o)
}

// event reads one piece of a streamed generate reply. Each carries the counts so far, so the last to carry them is what the call cost.
func (genaiProtocol) event(data []byte, o *observed) {
	var e struct {
		UsageMetadata  json.RawMessage `json:"usageMetadata"`
		ModelVersion   json.RawMessage `json:"modelVersion"`
		Candidates     json.RawMessage `json:"candidates"`
		PromptFeedback json.RawMessage `json:"promptFeedback"`
		Error          *genai.APIError `json:"error"`
	}
	if json.Unmarshal(data, &e) != nil {
		return
	}
	if e.Error != nil {
		o.failed = true
		o.failure = reportedGenAI(*e.Error)
		o.code = firstOf(codeOf(o.failure), "error")
		return
	}
	readGenerate(e.UsageMetadata, e.ModelVersion, e.Candidates, e.PromptFeedback, o)
}

func readGenerate(usage, version, candidates, feedback json.RawMessage, o *observed) {
	var model string
	if json.Unmarshal(version, &model) == nil && model != "" {
		o.model = model
	}
	var u genai.GenerateContentResponseUsageMetadata
	if len(usage) > 0 && json.Unmarshal(usage, &u) == nil {
		if o.reported == nil {
			o.reported = map[string]int64{}
		}
		for _, q := range ReportedFromGenAI(&u) {
			o.reported[q.GetUnit()] = q.GetQuantity()
		}
		if u.TrafficType != "" {
			o.tier = string(u.TrafficType)
		}
	}
	var cs []struct {
		FinishReason string `json:"finishReason"`
	}
	if json.Unmarshal(candidates, &cs) == nil && len(cs) > 0 && cs[0].FinishReason != "" {
		o.finish = cs[0].FinishReason
	}
	// A prompt refused before the model saw it says so on the feedback, and the reply carries no candidate to name a finish of its own.
	var pf struct {
		BlockReason string `json:"blockReason"`
	}
	if json.Unmarshal(feedback, &pf) == nil && pf.BlockReason != "" {
		o.finish = pf.BlockReason
	}
}

func (genaiProtocol) failure(body []byte, status int, _ http.Header, _ string) ReportedFailure {
	var reply struct {
		Error genai.APIError `json:"error"`
	}
	if json.Unmarshal(body, &reply) != nil || (reply.Error.Code == 0 && reply.Error.Status == "") {
		reply.Error = genai.APIError{Code: status}
	}
	if reply.Error.Code == 0 {
		reply.Error.Code = status
	}
	return reportedGenAI(reply.Error)
}

func (genaiProtocol) denied(*http.Request) (*http.Response, error) {
	return nil, ErrDenied
}
