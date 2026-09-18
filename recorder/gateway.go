package recorder

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// apiKeyHeader is where the gateway reads the key. Presented on its own it carries a secret, which is how a key issued before keys were presented as a pair is sent; presented beside apiSecretHeader it carries the key's name.
const apiKeyHeader = "x-api-key"

// apiSecretHeader is where the gateway reads the secret when a key and its secret are presented as a pair.
//
// Two headers rather than HTTP basic authentication, because the gateway keeps the Authorization header for platform identity and a customer secret must never be mistaken for a verified token.
const apiSecretHeader = "x-api-secret"

// Dial opens the connection an agent uses to reach Agent Pulse through its
// gateway: TLS to the gateway's address, with the api key on every call.
//
// One connection serves every client the recorder builds, so the sink, the
// rate source and the decider are all given this. The connection is lazy: no
// network is touched until the first record is sent, so a gateway that is
// unreachable at startup delays nothing the agent does.
//
// Both arguments are required. An empty one is refused here rather than
// discovered later as every record being rejected, because a wrong setting
// should fail where a person is watching, at startup, and not silently for
// the life of the process.
func Dial(gateway, apiKey string) (*grpc.ClientConn, error) {
	if strings.TrimSpace(gateway) == "" {
		return nil, errors.New("recorder: gateway address is required")
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("recorder: api key is required")
	}
	return grpc.NewClient(gatewayTarget(gateway),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})),
		grpc.WithPerRPCCredentials(apiKeyCredentials{key: apiKey}),
	)
}

// DialWithKey opens the same connection as Dial, presenting a key and its secret as a pair.
//
// The key is the key's resource name, `organisations/<id>/apiKeys/<id>`, which is public and carries the organisation the spend is filed under. The secret is the value shown once when the key was issued. The two travel as separate headers, and the gateway resolves them only together: the right secret against another key's name is refused exactly as a wrong secret is.
//
// All three arguments are required, for the same reason Dial's are: a missing one should fail at startup where a person is watching.
func DialWithKey(gateway, key, secret string) (*grpc.ClientConn, error) {
	if strings.TrimSpace(gateway) == "" {
		return nil, errors.New("recorder: gateway address is required")
	}
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("recorder: api key is required")
	}
	if strings.TrimSpace(secret) == "" {
		return nil, errors.New("recorder: api secret is required")
	}
	return grpc.NewClient(gatewayTarget(gateway),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})),
		grpc.WithPerRPCCredentials(pairCredentials{key: strings.TrimSpace(key), secret: strings.TrimSpace(secret)}),
	)
}

// OrganisationOfKey returns the organisation a key was issued for, as its resource name, or an empty string when the value is not a key's name.
//
// A key is `organisations/<id>/apiKeys/<id>`, so an agent that has its key has its organisation too and need not be told it a second time. Nothing about scope rests on this: the gateway holds every request to the organisation the key resolves to, so a name edited to claim another tenant is refused, not believed.
func OrganisationOfKey(key string) string {
	organisation, _, ok := strings.Cut(strings.TrimSpace(key), "/apiKeys/")
	if !ok || !strings.HasPrefix(organisation, "organisations/") || strings.Count(organisation, "/") != 1 {
		return ""
	}
	return organisation
}

// gatewayTarget turns whatever a person pasted into a gRPC target: a scheme
// is dropped, a trailing slash is dropped, and a port is added when none was
// given, because the gateway serves on 443 and a bare host is what its URL
// shows.
func gatewayTarget(gateway string) string {
	target := strings.TrimSpace(gateway)
	for _, scheme := range []string{"https://", "http://"} {
		target = strings.TrimPrefix(target, scheme)
	}
	target = strings.TrimSuffix(target, "/")
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "443")
	}
	return target
}

// apiKeyCredentials presents the key on every call. It refuses to run over
// an insecure transport, so a key can never travel in clear.
type apiKeyCredentials struct {
	key string
}

func (c apiKeyCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{apiKeyHeader: c.key}, nil
}

func (apiKeyCredentials) RequireTransportSecurity() bool { return true }

// pairCredentials presents a key and its secret as a pair on every call. It refuses to run over an insecure transport for the same reason apiKeyCredentials does.
type pairCredentials struct {
	key, secret string
}

func (c pairCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{apiKeyHeader: c.key, apiSecretHeader: c.secret}, nil
}

func (pairCredentials) RequireTransportSecurity() bool { return true }
