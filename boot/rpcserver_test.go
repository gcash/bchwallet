package boot

import (
	"context"
	"google.golang.org/grpc/metadata"
	"testing"
)

func TestValidateAuthenticationToken(t *testing.T) {
	ctx := context.Background()
	cfg = &config{}
	cfg.AuthToken = "LetMeIn"

	if err := validateAuthenticationToken(ctx); err == nil {
		t.Error("Failed to error with empty context")
	}

	md := metadata.Pairs(AuthenticationTokenKey, cfg.AuthToken)
	ctx = metadata.NewIncomingContext(ctx, md)
	if err := validateAuthenticationToken(ctx); err != nil {
		t.Error("Failed to correctly authenticate context")
	}
}
