package recorder

import "strings"

// OrganisationOfKey returns the organisation an api key was issued for, as its resource name, or an empty string when the key does not say.
//
// A key issued by Agent Pulse carries its organisation in the clear, `ap.<organisation>[.<workspace>].<key>.<random>`, so an agent that has its key has its organisation too and need not be told it a second time. A key with no such shape was issued before the shape existed, and the organisation is configured beside it as it always was.
//
// Nothing about scope rests on this. The gateway holds every request to the organisation it resolves the whole key to, so a key whose prefix has been edited to name another tenant is refused, not believed. What this saves is one setting, not one check.
func OrganisationOfKey(apiKey string) string {
	parts := strings.Split(strings.TrimSpace(apiKey), ".")
	if len(parts) < 4 || parts[0] != "ap" || parts[1] == "" {
		return ""
	}
	return "organisations/" + parts[1]
}

// WorkspaceOfKey returns the workspace an api key is confined to, as its resource name, or an empty string when the key reaches every workspace of its organisation.
//
// A confined key carries the workspace as its third segment. A record sent with such a key must name that workspace, and this is how an agent holding one learns which.
func WorkspaceOfKey(apiKey string) string {
	parts := strings.Split(strings.TrimSpace(apiKey), ".")
	if len(parts) != 5 || parts[0] != "ap" || parts[1] == "" || parts[2] == "" {
		return ""
	}
	return "organisations/" + parts[1] + "/workspaces/" + parts[2]
}
