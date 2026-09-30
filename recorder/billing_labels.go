package recorder

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// labeller is a protocol that can carry billing labels on a model call, so the bill line the call produces names the agent and component that spent it.
type labeller interface {
	// label returns the request with labels added, or req itself when this call cannot carry them.
	label(req *http.Request, labels map[string]string) *http.Request
}

// billingLabels are the labels a call made under component carries. The agent and the component are labelled and the person is not, because Google keeps at most 1,000 values of a label key per billing account and drops the rest without notice.
func (rp *Reporter) billingLabels(component string) map[string]string {
	labels := map[string]string{}
	agent := rp.attribution.Agent
	if name := labelValue(agent[strings.LastIndex(agent, "/")+1:]); name != "" {
		labels["ap_agent"] = name
	}
	if c := labelValue(component); c != "" {
		labels["ap_component"] = c
	}
	return labels
}

// labelled adds labels to req, and on any failure returns req exactly as it was, since a label is never worth a call.
func labelled(p labeller, req *http.Request, labels map[string]string) (out *http.Request) {
	if len(labels) == 0 {
		return req
	}
	defer func() {
		if recover() != nil {
			out = req
		}
	}()
	return p.label(req, labels)
}

// withLabels returns a copy of req whose JSON body carries labels under "labels", keeping any label already there under the same key. A body that is not a JSON object leaves req untouched.
func withLabels(req *http.Request, labels map[string]string) *http.Request {
	if req.Body == nil || req.GetBody == nil {
		return req
	}
	original, err := req.GetBody()
	if err != nil {
		return req
	}
	body, err := io.ReadAll(original)
	_ = original.Close()
	if err != nil {
		return req
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return req
	}
	merged := map[string]string{}
	if existing, ok := fields["labels"]; ok && json.Unmarshal(existing, &merged) != nil {
		return req
	}
	for key, value := range labels {
		if _, set := merged[key]; !set {
			merged[key] = value
		}
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return req
	}
	fields["labels"] = encoded
	rewritten, err := json.Marshal(fields)
	if err != nil {
		return req
	}

	out := req.Clone(req.Context())
	out.Body = io.NopCloser(bytes.NewReader(rewritten))
	out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(rewritten)), nil }
	out.ContentLength = int64(len(rewritten))
	out.Header.Del("Content-Length")
	return out
}

// labelValue coerces a value into what the billing export accepts: lowercase letters, digits, dashes and underscores, up to 63 characters. It matches the ADK hooks' own, so a direct call and an agent's call label the same name the same way.
func labelValue(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if len(out) > 63 {
		out = out[:63]
	}
	return out
}
