package creds

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// stubStream is the smallest thing satisfying grpc.ServerStream: the
// interceptor reads nothing from a stream but its context.
type stubStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s stubStream) Context() context.Context { return s.ctx }

// handlerRan records whether the interceptor passed the call through. That is
// the property that matters: InitTunnel binds a listener on a tunnelled port,
// so a rejected caller must never reach it.
func runInterceptor(t *testing.T, expected string, md metadata.MD) (bool, error) {
	t.Helper()
	ran := false
	handler := func(srv interface{}, stream grpc.ServerStream) error {
		ran = true
		return nil
	}
	ctx := context.Background()
	if md != nil {
		ctx = metadata.NewIncomingContext(ctx, md)
	}
	err := StreamAuthInterceptor(expected)(nil, stubStream{ctx: ctx}, nil, handler)
	return ran, err
}

func TestInterceptorAcceptsTheMatchingToken(t *testing.T) {
	md := metadata.Pairs(authHeader, bearerPrefix+"s3cret")
	ran, err := runInterceptor(t, "s3cret", md)
	if err != nil {
		t.Fatalf("matching token was rejected: %v", err)
	}
	if !ran {
		t.Error("handler did not run for a matching token")
	}
}

func TestInterceptorRejectsAWrongToken(t *testing.T) {
	md := metadata.Pairs(authHeader, bearerPrefix+"guess")
	ran, err := runInterceptor(t, "s3cret", md)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("wrong token gave %v, want Unauthenticated", status.Code(err))
	}
	if ran {
		t.Error("handler ran despite a wrong token; a rejected caller reached InitTunnel")
	}
}

func TestInterceptorRejectsAMissingToken(t *testing.T) {
	ran, err := runInterceptor(t, "s3cret", nil)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("absent metadata gave %v, want Unauthenticated", status.Code(err))
	}
	if ran {
		t.Error("handler ran with no credentials at all")
	}
}

// A server with no token configured is --insecure and standalone
// `ktunnel server`, which have to keep working exactly as they do today.
func TestInterceptorWithNoTokenConfiguredAcceptsEveryone(t *testing.T) {
	ran, err := runInterceptor(t, "", nil)
	if err != nil {
		t.Fatalf("unconfigured server rejected a caller: %v", err)
	}
	if !ran {
		t.Error("handler did not run on an unauthenticated server")
	}
}

func TestClientCredentialsSendTheHeaderTheServerReads(t *testing.T) {
	md, err := TokenCredentials("s3cret").GetRequestMetadata(context.Background())
	if err != nil {
		t.Fatalf("GetRequestMetadata: %v", err)
	}
	if got := md[authHeader]; got != bearerPrefix+"s3cret" {
		t.Errorf("client sent %q, server expects %q", got, bearerPrefix+"s3cret")
	}
	// The fallback path is authenticated but plaintext. Requiring transport
	// security here would make the token unusable on exactly the path that
	// has nothing else protecting it.
	if TokenCredentials("s3cret").RequireTransportSecurity() {
		t.Error("credentials require TLS, so the plaintext fallback cannot authenticate")
	}
}

// The pod spec writes this variable and the server process reads it. They are
// the same string only because they come from the same constant; a test that
// hardcoded it on both sides would pass while the two drifted apart.
func TestTokenFromEnvReadsWhatThePodSpecWrites(t *testing.T) {
	t.Setenv(TokenEnvVar, "from-the-secret")
	if got := TokenFromEnv(); got != "from-the-secret" {
		t.Errorf("TokenFromEnv returned %q, want %q", got, "from-the-secret")
	}
}

func TestTokenFromEnvIsEmptyWhenUnset(t *testing.T) {
	t.Setenv(TokenEnvVar, "")
	if got := TokenFromEnv(); got != "" {
		t.Errorf("TokenFromEnv returned %q with the variable unset, want empty", got)
	}
}
