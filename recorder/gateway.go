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

// apiKeyHeader is where the gateway reads a caller's key from. It is the
// only credential an agent outside Agent Pulse's own project presents.
const apiKeyHeader = "x-api-key"

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
