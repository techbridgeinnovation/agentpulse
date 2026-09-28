package recorder

import (
	"net/http"
	"testing"
)

// The region comes from the Vertex location in the path, or from OpenAI's data-residency host.
func TestTheRegionIsReadFromTheCallsAddress(t *testing.T) {
	for url, want := range map[string]string{
		"https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/us-central1/publishers/google/models/gemini:generateContent": "us-central1",
		"https://aiplatform.googleapis.com/v1/projects/p/locations/global/publishers/google/models/gemini:generateContent":                  "global",
		"https://eu.api.openai.com/v1/responses":           "eu",
		"https://api.openai.com/v1/responses":              "",
		"https://api.anthropic.com/v1/messages":            "",
		"https://generativelanguage.googleapis.com/v1beta": "",
	} {
		req, err := http.NewRequest(http.MethodPost, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := regionOf(req); got != want {
			t.Errorf("%s: region %q, want %q", url, got, want)
		}
	}
}

// Off Vertex, or with no location set, a framework's calls carry no region.
func TestTheDefaultRegionIsTheVertexLocation(t *testing.T) {
	t.Setenv("GOOGLE_GENAI_USE_VERTEXAI", "true")
	t.Setenv("GOOGLE_CLOUD_LOCATION", "europe-west4")
	if got := DefaultRegion(ProviderVertexAI); got != "europe-west4" {
		t.Errorf("vertex: %q, want europe-west4", got)
	}
	if got := DefaultRegion("ANTHROPIC"); got != "" {
		t.Errorf("anthropic: %q, want none", got)
	}
	t.Setenv("GOOGLE_GENAI_USE_VERTEXAI", "false")
	if got := DefaultRegion(ProviderVertexAI); got != "" {
		t.Errorf("gemini api: %q, want none", got)
	}
}
