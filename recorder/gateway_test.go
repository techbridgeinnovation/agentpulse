package recorder

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
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

// A connection left idle until the first call does its handshake inside that call's deadline, and the first spend decision's is short enough to lose to it.
func TestDialStartsConnectingBeforeTheFirstCall(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	for name, dial := range map[string]func() (*grpc.ClientConn, error){
		"Dial":        func() (*grpc.ClientConn, error) { return Dial(listener.Addr().String(), "k") },
		"DialWithKey": func() (*grpc.ClientConn, error) { return DialWithKey(listener.Addr().String(), "k", "s") },
	} {
		t.Run(name, func(t *testing.T) {
			conn, err := dial()
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			for state := conn.GetState(); state == connectivity.Idle; state = conn.GetState() {
				if !conn.WaitForStateChange(ctx, state) {
					t.Fatal("the connection stayed idle with no call made")
				}
			}
		})
	}
}
