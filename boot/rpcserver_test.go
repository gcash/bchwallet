package boot

import (
	"context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"testing"
)

func TestValidateAuthenticationToken(t *testing.T) {
	ctx := context.Background()
	cfg = &config{}
	cfg.AuthToken = "LetMeIn"

	if err := validateAuthenticationToken(ctx); status.Code(err) != codes.Unauthenticated {
		t.Errorf("Failed to refuse an empty context: %v", err)
	}

	md := metadata.Pairs(AuthenticationTokenKey, "LetMeOut")
	wrongCtx := metadata.NewIncomingContext(ctx, md)
	if err := validateAuthenticationToken(wrongCtx); status.Code(err) != codes.Unauthenticated {
		t.Errorf("Failed to refuse a wrong token: %v", err)
	}

	md = metadata.Pairs(AuthenticationTokenKey, cfg.AuthToken)
	ctx = metadata.NewIncomingContext(ctx, md)
	if err := validateAuthenticationToken(ctx); err != nil {
		t.Error("Failed to correctly authenticate context")
	}

	cfg.AuthToken = ""
	if err := validateAuthenticationToken(context.Background()); err != nil {
		t.Errorf("Failed to accept a call without a configured token: %v", err)
	}
}

// testServerStream is a grpc.ServerStream that only has a context.
type testServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *testServerStream) Context() context.Context {
	return s.ctx
}

// TestInterceptorsRecoverPanics ensures a handler that panics makes its call
// fail with Internal instead of crashing the process.
func TestInterceptorsRecoverPanics(t *testing.T) {
	cfg = &config{}
	ctx := context.Background()
	const method = "/walletrpc.VersionService/Version"

	resp, err := interceptUnary(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method},
		func(context.Context, interface{}) (interface{}, error) {
			panic("unary handler bug")
		})
	if resp != nil || status.Code(err) != codes.Internal {
		t.Errorf("unary: got %v, %v, want an Internal error", resp, err)
	}

	err = interceptStreaming(nil, &testServerStream{ctx: ctx},
		&grpc.StreamServerInfo{FullMethod: method},
		func(interface{}, grpc.ServerStream) error {
			panic("streaming handler bug")
		})
	if status.Code(err) != codes.Internal {
		t.Errorf("streaming: got %v, want an Internal error", err)
	}

	resp, err = interceptUnary(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method},
		func(context.Context, interface{}) (interface{}, error) {
			return "ok", nil
		})
	if resp != "ok" || err != nil {
		t.Errorf("unary without a panic: got %v, %v", resp, err)
	}
}
