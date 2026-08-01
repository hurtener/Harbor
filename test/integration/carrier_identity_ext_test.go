package integration_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
)

// mustVerifiedCtx seats id as the request's established identity — the
// shape a transport hands a surface once the request's identity has been
// resolved.
func mustVerifiedCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatalf("seat verified identity: %v", err)
	}
	return ctx
}
