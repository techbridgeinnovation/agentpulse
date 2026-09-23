package recorder

import (
	"encoding/json"
	"net/http"
	"strings"
)

// AnthropicMiddleware records every Messages call made through an Anthropic Go sdk client, for `option.WithMiddleware`:
//
//	client := anthropic.NewClient(option.WithMiddleware(reporter.AnthropicMiddleware()))
//
// Each call is recorded under the reporter's attribution, with the user, request, session, workspace and project read from the call's context, and the part of the product that made it from WithComponent or, where none is named, the function that made the call. Token counting and every other endpoint pass through unrecorded.
//
// Claude served through Vertex or Bedrock is billed by Google or by Amazon, not by Anthropic, so set Attribution.BilledBy for such a client. Register this before the sdk's `vertex` or `bedrock` option: those rewrite the request into their own shape, and on Bedrock into a binary stream this does not read.
func (rp *Reporter) AnthropicMiddleware() Middleware {
	return func(req *http.Request, next func(*http.Request) (*http.Response, error)) (*http.Response, error) {
		return rp.observe(anthropicProtocol{}, req, next)
	}
}

type anthropicProtocol struct{}

func (anthropicProtocol) call(req *http.Request) (bool, string) {
	if req.Method != http.MethodPost || req.URL == nil {
		return false, ""
	}
	path := req.URL.Path
	switch {
	case strings.HasSuffix(path, "/v1/messages"):
		return true, ""
	// The same call after the sdk's vertex option has rewritten it, for a client whose options were registered the other way round.
	case strings.Contains(path, "/publishers/anthropic/models/") &&
		(strings.HasSuffix(path, ":rawPredict") || strings.HasSuffix(path, ":streamRawPredict")) &&
		!strings.Contains(path, "count-tokens"):
		model := path[strings.Index(path, "/publishers/anthropic/models/")+len("/publishers/anthropic/models/"):]
		return true, model[:strings.Index(model, ":")]
	}
	return false, ""
}

func (anthropicProtocol) format(*http.Request, string) string { return FormatAnthropic }

func (anthropicProtocol) billedBy(req *http.Request) string {
	if strings.Contains(req.URL.Path, "/publishers/anthropic/models/") {
		return ProviderVertexAI
	}
	return ProviderAnthropic
}

func (anthropicProtocol) reply(field func(string) json.RawMessage, o *observed) {
	var model, stop, tier string
	_ = json.Unmarshal(field("model"), &model)
	_ = json.Unmarshal(field("stop_reason"), &stop)
	usage := field("usage")
	var u struct {
		ServiceTier string `json:"service_tier"`
	}
	_ = json.Unmarshal(usage, &u)
	tier = u.ServiceTier
	if model != "" {
		o.model = model
	}
	o.finish = stop
	o.tier = tier
	o.setReported(usage)
}

// event reads one event of a streamed Messages reply. The input counts arrive on `message_start` and the output counts on `message_delta`, each a running total, so what the last event stated is what the call cost.
func (anthropicProtocol) event(data []byte, o *observed) {
	var e struct {
		Type    string `json:"type"`
		Message struct {
			Model string          `json:"model"`
			Usage json.RawMessage `json:"usage"`
		} `json:"message"`
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		Usage json.RawMessage `json:"usage"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) != nil {
		return
	}
	switch e.Type {
	case "message_start":
		if e.Message.Model != "" {
			o.model = e.Message.Model
		}
		o.setReported(e.Message.Usage)
		var u struct {
			ServiceTier string `json:"service_tier"`
		}
		if json.Unmarshal(e.Message.Usage, &u) == nil && u.ServiceTier != "" {
			o.tier = u.ServiceTier
		}
	case "message_delta":
		if e.Delta.StopReason != "" {
			o.finish = e.Delta.StopReason
		}
		o.setReported(e.Usage)
	case "error":
		// A failure after the reply began, such as an overload part way through, arrives as an event rather than a status.
		o.failed = true
		o.code = e.Error.Type
		var s fieldSet
		s.add("type", e.Error.Type)
		o.failure = s.failure(ErrorFormatAnthropic)
	}
}

func (anthropicProtocol) failure(body []byte, status int, header http.Header, billedBy string) ReportedFailure {
	return reportedBody(body, status, header, ProviderAnthropic)
}

func (anthropicProtocol) denied(req *http.Request) (*http.Response, error) {
	return deniedReply(req, `{"type":"error","error":{"type":"`+deniedType+`","message":"`+ErrDenied.Error()+`"}}`), nil
}
