package assemble_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/runtime/sessionadmission"
	"github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

func TestSessionAdmission_DowngradeStopsBeforeRuntimeRecovery(t *testing.T) {
	cfg := minimalCfg(t)
	cfg.State = config.StateConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "enrolled.sqlite")}
	store, err := sqlite.New(cfg.State)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := sessionadmission.New(store)
	if err != nil {
		t.Fatal(err)
	}
	id := identity.Identity{TenantID: "tenant", UserID: "owner", SessionID: "session"}
	ctx, err := identity.WithVerified(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx = auth.WithScopes(ctx, []auth.Scope{auth.ScopeAdmin})
	ctx = auth.WithTokenAuthority(ctx, auth.TokenAuthority{Issuer: "https://issuer.example", Subject: "coordinator"})
	if _, err := gate.Enroll(ctx, id, 0, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	stack, err := assemble.Assemble(t.Context(), cfg, assemble.Options{})
	if stack != nil {
		defer func() { _ = stack.Close(context.Background()) }()
	}
	if !errors.Is(err, sessionadmission.ErrUnavailable) {
		t.Fatalf("downgrade boot result: %v", err)
	}
	if stack == nil || stack.Bus != nil || stack.Memory != nil || stack.LLM != nil {
		t.Fatalf("runtime components opened before admission downgrade was refused")
	}
}
