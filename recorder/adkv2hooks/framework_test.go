package adkv2hooks

import "testing"

// The hooks name ADK Go as the framework.
func TestTheFrameworkIsADKGo(t *testing.T) {
	if framework != "google/adk-go" {
		t.Errorf("framework %q, want google/adk-go", framework)
	}
}
