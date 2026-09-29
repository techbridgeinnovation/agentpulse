package recorder

import "testing"

// A module the binary was not built with reports no version rather than failing.
func TestAnUnknownModuleHasNoVersion(t *testing.T) {
	if got := ModuleVersion("example.com/not-built-in"); got != "" {
		t.Errorf("unknown module reported %q, want nothing", got)
	}
}
