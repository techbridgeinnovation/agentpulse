package recorder

import (
	"encoding/json"
	"net/http"
	"strings"
)

// OpenAIMiddleware records every Chat Completions and Responses call made through an OpenAI Go sdk client, for `option.WithMiddleware`:
//
//	client := openai.NewClient(option.WithMiddleware(reporter.OpenAIMiddleware()))
//
// The same client pointed at another provider that speaks OpenAI's api, such as Perplexity through `option.WithBaseURL`, is recorded the same way. Who billed the call is read from the host where Attribution.BilledBy does not say, because Perplexity uses OpenAI's field names with its own meanings and is read by its own convention on the server.
//
// A streamed Chat Completions reply reports its token counts only when the request asks for them with `stream_options.include_usage`. This does not add that to a request, because it changes the stream the caller reads: one more chunk, with no choices. A streamed chat without it is recorded, with no counts.
func (rp *Reporter) OpenAIMiddleware() Middleware {
	return func(req *http.Request, next func(*http.Request) (*http.Response, error)) (*http.Response, error) {
		return rp.observe(openAIProtocol{}, req, next)
	}
}

type openAIProtocol struct{}

func (openAIProtocol) call(req *http.Request) (bool, string) {
	if req.Method != http.MethodPost || req.URL == nil {
		return false, ""
	}
	path := strings.TrimSuffix(req.URL.Path, "/")
	return strings.HasSuffix(path, "/chat/completions") || strings.HasSuffix(path, "/responses"), ""
}

func (openAIProtocol) format(req *http.Request, billedBy string) string {
	if strings.HasSuffix(strings.TrimSuffix(req.URL.Path, "/"), "/responses") {
		return FormatOpenAIResponses
	}
	if strings.EqualFold(billedBy, ProviderPerplexity) {
		return FormatPerplexity
	}
	return FormatOpenAIChat
}

// openAIHosts are the providers known by the host their OpenAI-compatible api is served from.
var openAIHosts = map[string]string{
	"api.openai.com":    ProviderOpenAI,
	"api.perplexity.ai": ProviderPerplexity,
}

func (openAIProtocol) billedBy(req *http.Request) string {
	if provider, ok := openAIHosts[req.URL.Hostname()]; ok {
		return provider
	}
	return ProviderOpenAI
}

// reply reads a whole Chat Completions or Responses reply. The two put the finish in different places: a chat names it on each choice, and a response states its own status, with the reason beside it where it stopped short.
func (openAIProtocol) reply(field func(string) json.RawMessage, o *observed) {
	var model, tier, status string
	_ = json.Unmarshal(field("model"), &model)
	_ = json.Unmarshal(field("service_tier"), &tier)
	if model != "" {
		o.model = model
	}
	o.tier = tier
	o.setReported(field("usage"))

	var choices []struct {
		FinishReason string `json:"finish_reason"`
	}
	if json.Unmarshal(field("choices"), &choices) == nil && len(choices) > 0 {
		o.finish = choices[0].FinishReason
	}
	if json.Unmarshal(field("status"), &status) == nil {
		responseFinish(status, field("incomplete_details"), field("error"), o)
	}
}

// responseFinish reads how a Responses reply ended from its status.
func responseFinish(status string, incomplete, failure json.RawMessage, o *observed) {
	switch status {
	case "incomplete":
		var d struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(incomplete, &d)
		o.finish = firstOf(d.Reason, "incomplete")
	case "failed":
		var e struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(failure, &e)
		o.failed = true
		o.code = firstOf(e.Code, "failed")
		var s fieldSet
		s.add("code", e.Code)
		o.failure = s.failure(ErrorFormatOpenAI)
	}
}

// event reads one event of a streamed reply. A chat streams chunks, the last of which carries the counts where they were asked for; a response streams typed events, and the one that ends it carries the whole response with its counts.
func (openAIProtocol) event(data []byte, o *observed) {
	var e struct {
		Type     string          `json:"type"`
		Model    string          `json:"model"`
		Usage    json.RawMessage `json:"usage"`
		Tier     string          `json:"service_tier"`
		Response struct {
			Model      string          `json:"model"`
			Usage      json.RawMessage `json:"usage"`
			Status     string          `json:"status"`
			Tier       string          `json:"service_tier"`
			Incomplete json.RawMessage `json:"incomplete_details"`
			Error      json.RawMessage `json:"error"`
		} `json:"response"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Code  string `json:"code"`
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) != nil {
		return
	}

	switch e.Type {
	case "response.created", "response.in_progress", "response.completed", "response.incomplete", "response.failed":
		if e.Response.Model != "" {
			o.model = e.Response.Model
		}
		if e.Response.Tier != "" {
			o.tier = e.Response.Tier
		}
		o.setReported(e.Response.Usage)
		responseFinish(e.Response.Status, e.Response.Incomplete, e.Response.Error, o)
		return
	case "error":
		o.failed = true
		o.code = firstOf(e.Code, "error")
		var s fieldSet
		s.add("code", e.Code)
		o.failure = s.failure(ErrorFormatOpenAI)
		return
	case "":
	default:
		return
	}

	// A chat chunk.
	if e.Model != "" {
		o.model = e.Model
	}
	if e.Tier != "" {
		o.tier = e.Tier
	}
	if len(e.Choices) > 0 && e.Choices[0].FinishReason != "" {
		o.finish = e.Choices[0].FinishReason
	}
	if e.Error.Type != "" || e.Error.Code != "" {
		o.failed = true
		o.code = firstOf(e.Error.Code, e.Error.Type)
		var s fieldSet
		s.add("type", e.Error.Type)
		s.add("code", e.Error.Code)
		o.failure = s.failure(ErrorFormatOpenAI)
	}
	o.setReported(e.Usage)
}

func (openAIProtocol) failure(body []byte, status int, header http.Header, billedBy string) ReportedFailure {
	return reportedBody(body, status, header, billedBy)
}

func (openAIProtocol) denied(req *http.Request) (*http.Response, error) {
	return deniedReply(req, `{"error":{"type":"`+deniedType+`","code":"`+deniedType+`","message":"`+ErrDenied.Error()+`"}}`), nil
}
