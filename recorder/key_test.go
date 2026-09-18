package recorder

import "testing"

func TestAKeyKnowsItsOrganisationAndWorkspace(t *testing.T) {
	cases := []struct{ key, organisation, workspace string }{
		{"ap.acme.0f3b2b4e-1111-2222-3333-444444444444.RANDOM", "organisations/acme", ""},
		{"ap.acme.globex.0f3b2b4e-1111-2222-3333-444444444444.RANDOM", "organisations/acme", "organisations/acme/workspaces/globex"},
		{"RANDOMONLYKEYFROMBEFORE", "", ""},
		{"", "", ""},
		{"ap..k.RANDOM", "", ""},
	}
	for _, c := range cases {
		if got := OrganisationOfKey(c.key); got != c.organisation {
			t.Fatalf("OrganisationOfKey(%q) = %q, want %q", c.key, got, c.organisation)
		}
		if got := WorkspaceOfKey(c.key); got != c.workspace {
			t.Fatalf("WorkspaceOfKey(%q) = %q, want %q", c.key, got, c.workspace)
		}
	}
}
