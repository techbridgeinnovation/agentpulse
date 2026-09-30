package recorder

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/genai"
)

// labelsSeen answers every generate call and keeps the labels each request body carried.
func labelsSeen(t *testing.T) (*httptest.Server, *[]map[string]string) {
	t.Helper()
	var seen []map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var fields struct {
			Labels map[string]string `json:"labels"`
		}
		_ = json.Unmarshal(body, &fields)
		seen = append(seen, fields.Labels)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2,"totalTokenCount":12},"modelVersion":"gemini-2.5-flash-001"}`)
	}))
	t.Cleanup(server.Close)
	return server, &seen
}

func post(t *testing.T, rp *Reporter, url, body string) {
	t.Helper()
	client := &http.Client{Transport: &genaiTransport{rp: rp, base: http.DefaultTransport}}
	resp, err := client.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

const vertexPath = "/v1/projects/p/locations/global/publishers/google/models/gemini-2.5-flash:generateContent"

// A direct call to Vertex names its agent and component on the bill, the way an agent's own calls do.
func TestADirectVertexCallCarriesTheAgentAndComponentLabels(t *testing.T) {
	server, seen := labelsSeen(t)
	direct(t, service, func(rp *Reporter) {
		ctx := WithComponent(context.Background(), "Report Writer")
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+vertexPath, strings.NewReader(`{"contents":[]}`))
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{Transport: &genaiTransport{rp: rp, base: http.DefaultTransport}}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	})
	if len(*seen) != 1 {
		t.Fatalf("%d calls", len(*seen))
	}
	got := (*seen)[0]
	if got["ap_agent"] != "documents" || got["ap_component"] != "report_writer" {
		t.Errorf("labels = %v, want ap_agent documents and ap_component report_writer", got)
	}
	if _, ok := got["ap_user"]; ok {
		t.Errorf("labels = %v, want no person", got)
	}
}

// A label the caller set is theirs, and one of ours never replaces it.
func TestADirectCallKeepsTheLabelsItAlreadyCarries(t *testing.T) {
	server, seen := labelsSeen(t)
	direct(t, service, func(rp *Reporter) {
		post(t, rp, server.URL+vertexPath, `{"contents":[],"labels":{"ap_agent":"chosen","team":"search"}}`)
	})
	got := (*seen)[0]
	if got["ap_agent"] != "chosen" || got["team"] != "search" {
		t.Errorf("labels = %v, want the caller's kept", got)
	}
}

// The Gemini API refuses a request that carries labels, so labelling one would fail every call.
func TestAGeminiAPICallCarriesNoLabels(t *testing.T) {
	server, seen := labelsSeen(t)
	direct(t, service, func(rp *Reporter) {
		client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
			Backend: genai.BackendGeminiAPI, APIKey: "test", HTTPOptions: genai.HTTPOptions{BaseURL: server.URL},
		})
		if err != nil {
			t.Fatal(err)
		}
		rp.InstrumentGenAI(client)
		if _, err := client.Models.GenerateContent(context.Background(), "gemini-2.5-flash", genai.Text("the prompt"), nil); err != nil {
			t.Fatal(err)
		}
	})
	if len(*seen) != 1 || len((*seen)[0]) != 0 {
		t.Errorf("labels = %v, want none on the Gemini API", *seen)
	}
}

// A body that is not a JSON object is sent exactly as it was, and the call is still recorded.
func TestABodyThatIsNotAnObjectIsSentUnchanged(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"modelVersion":"gemini-2.5-flash-001"}`)
	}))
	defer server.Close()
	got := direct(t, service, func(rp *Reporter) {
		post(t, rp, server.URL+vertexPath, `not json`)
	})
	if body != "not json" {
		t.Errorf("body = %q, want it untouched", body)
	}
	if len(got) != 1 {
		t.Errorf("%d records, want the call recorded", len(got))
	}
}
