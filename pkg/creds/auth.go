package creds

import (
	"context"
	"crypto/subtle"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	authHeader   = "authorization"
	bearerPrefix = "Bearer "

	// TokenEnvVar carries the bearer token into the tunnel server process.
	// An environment variable rather than an argument: arguments are
	// visible in `kubectl get pod -o yaml` to anyone with `get pods`, and
	// when a Secret exists this variable is a secretKeyRef, which is not.
	//
	// Both halves of the delivery -- the pod spec that writes it and the
	// server that reads it -- use this constant, so they cannot drift.
	TokenEnvVar = "KTUNNEL_TOKEN"
)

// StreamAuthInterceptor rejects a stream whose caller does not present the
// expected bearer token.
//
// It runs before the handler, which matters more here than in an ordinary
// service: the handler is InitTunnel, and InitTunnel opens a listener on a
// tunnelled port. Checking inside it would let an unauthenticated caller
// cause a port to bind before being turned away.
//
// An empty expected token disables the check. That is `--insecure` and
// standalone `ktunnel server`, which predate authentication and keep their
// existing behaviour.
func StreamAuthInterceptor(expected string) grpc.StreamServerInterceptor {
	return func(srv interface{}, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if expected == "" {
			return handler(srv, stream)
		}
		md, ok := metadata.FromIncomingContext(stream.Context())
		if !ok {
			return status.Error(codes.Unauthenticated, "no credentials presented")
		}
		values := md.Get(authHeader)
		if len(values) == 0 {
			return status.Error(codes.Unauthenticated, "no credentials presented")
		}
		presented := strings.TrimPrefix(values[0], bearerPrefix)
		// Constant time: the comparison is against a secret, and a length
		// or prefix leak is the whole point of a timing attack.
		if subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) != 1 {
			return status.Error(codes.Unauthenticated, "invalid tunnel token")
		}
		return handler(srv, stream)
	}
}

// TokenCredentials is the client half: it attaches the token to every call.
type TokenCredentials string

// GetRequestMetadata implements credentials.PerRPCCredentials.
func (t TokenCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{authHeader: bearerPrefix + string(t)}, nil
}

// RequireTransportSecurity is false by design. When ktunnel cannot create a
// Secret it falls back to an authenticated but unencrypted tunnel, and that
// is the path where the token is the only control there is -- refusing to
// send it without TLS would disarm the fallback entirely.
func (t TokenCredentials) RequireTransportSecurity() bool { return false }

// TokenFromEnv returns the token the pod spec passed in, or empty when there
// is none -- an unauthenticated server, which is standalone use and
// `--insecure`.
func TokenFromEnv() string {
	return os.Getenv(TokenEnvVar)
}
