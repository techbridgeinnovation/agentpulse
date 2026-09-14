package recorder

import (
	"context"
	"strings"
	"testing"
)

func TestDialRefusesAMissingSetting(t *testing.T) {
	for name, tc := range map[string]struct{ gateway, key string }{
		"no gateway": {"", "k"},
		"no key":     {"gateway.example:443", ""},
		"blank key":  {"gateway.example:443", "   "},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Dial(tc.gateway, tc.key); err == nil {
				t.Fatalf("Dial(%q, %q) succeeded, want a refusal", tc.gateway, tc.key)
			}
		})
	}
}

func TestDialAcceptsWhateverShapeTheAddressWasPastedIn(t *testing.T) {
	for in, want := range map[string]string{
		"gateway-v1-abc-ew.a.run.app":          "gateway-v1-abc-ew.a.run.app:443",
		"https://gateway-v1-abc-ew.a.run.app":  "gateway-v1-abc-ew.a.run.app:443",
		"https://gateway-v1-abc-ew.a.run.app/": "gateway-v1-abc-ew.a.run.app:443",
		"gateway-v1-abc-ew.a.run.app:8443":     "gateway-v1-abc-ew.a.run.app:8443",
		"  api.example.com  ":                  "api.example.com:443",
	} {
		if got := gatewayTarget(in); got != want {
			t.Errorf("gatewayTarget(%q) = %q, want %q", in, got, want)
		}
	}
	conn, err := Dial("https://gateway-v1-abc-ew.a.run.app/", "secret")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
}

func TestTheKeyTravelsOnEveryCallAndOnlyOverTLS(t *testing.T) {
	creds := apiKeyCredentials{key: "secret"}
	md, err := creds.GetRequestMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if md[apiKeyHeader] != "secret" {
		t.Fatalf("metadata = %v, want the key under %s", md, apiKeyHeader)
	}
	if !creds.RequireTransportSecurity() {
		t.Fatal("the key may travel in clear")
	}
}

func TestAnEmptyOrganisationIsRefusedAtConstruction(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NewGRPCSink accepted an empty organisation")
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, "organisation") {
			t.Fatalf("panic = %v, want one naming the organisation", r)
		}
	}()
	NewGRPCSink(nil, "  ")
}
