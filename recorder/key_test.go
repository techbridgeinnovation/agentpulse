package recorder

import (
	"context"
	"testing"
)

func TestAKeyAndSecretTravelAsAPair(t *testing.T) {
	md, err := pairCredentials{key: "organisations/acme/apiKeys/k1", secret: "S3CR3T"}.GetRequestMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if md[apiKeyHeader] != "organisations/acme/apiKeys/k1" || md[apiSecretHeader] != "S3CR3T" {
		t.Fatalf("metadata %v, want the key and the secret on their own headers", md)
	}
}

func TestAKeyNamesItsOrganisation(t *testing.T) {
	cases := map[string]string{
		"organisations/acme/apiKeys/0f3b2b4e-1111-2222-3333-444444444444": "organisations/acme",
		" organisations/acme/apiKeys/k ":                                  "organisations/acme",
		"S3CR3TONLY":                                                      "",
		"organisations/acme":                                              "",
		"organisations/acme/workspaces/w/apiKeys/k":                       "",
		"": "",
	}
	for key, want := range cases {
		if got := OrganisationOfKey(key); got != want {
			t.Fatalf("OrganisationOfKey(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestDialWithKeyRefusesAMissingValue(t *testing.T) {
	for _, c := range [][3]string{{"", "k", "s"}, {"gw", "", "s"}, {"gw", "k", ""}} {
		if _, err := DialWithKey(c[0], c[1], c[2]); err == nil {
			t.Fatalf("DialWithKey(%q, %q, %q) accepted a missing value", c[0], c[1], c[2])
		}
	}
}
