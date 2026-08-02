package statestore_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	statesqlite "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

// TestRetirement_SQLiteRestartRetainsTerminalLifecycle closes the first
// durable StateStore and reconstructs the registry over a new handle. The
// post-restart reader must retain the terminal fence and immutable history;
// neither a fresh process nor a missing in-memory cache may resurrect it.
func TestRetirement_SQLiteRestartRetainsTerminalLifecycle(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "retirement-restart.db")
	open := func() (agentcfg.Registry, func()) {
		store, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
		if err != nil {
			t.Fatalf("open SQLite StateStore: %v", err)
		}
		bus := newBus(t)
		registry, err := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: store, Bus: bus})
		if err != nil {
			_ = store.Close(ctx)
			t.Fatalf("open agentcfg registry: %v", err)
		}
		return registry, func() {
			_ = registry.Close(ctx)
			_ = store.Close(ctx)
		}
	}

	id := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-restart", UserID: "admin", SessionID: "control"}}
	const agent = "agent-restart"
	first, closeFirst := open()
	revision, err := first.SetRevision(ctx, id, agent, agentcfg.ConfigScopeAgent, skills("before-restart"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("seed revision: %v", err)
	}
	if _, err := first.(agentcfg.RetirementRegistry).Retire(ctx, id, agent, agentcfg.RetirementRequest{OperationID: "restart-op", ExpectedContentHash: revision.ContentHash}); err != nil {
		t.Fatalf("retire before restart: %v", err)
	}
	closeFirst()

	second, closeSecond := open()
	defer closeSecond()
	retirer := second.(agentcfg.RetirementRegistry)
	status, found, err := retirer.RetirementStatus(ctx, id, agent)
	if err != nil || !found || status.OperationID != "restart-op" || !status.Completed {
		t.Fatalf("post-restart retirement status=(%+v,%v,%v)", status, found, err)
	}
	if _, _, err := second.Active(ctx, id, agent, agentcfg.ConfigScopeAgent); !errors.Is(err, agentcfg.ErrAgentRetired) {
		t.Fatalf("post-restart active config = %v, want ErrAgentRetired", err)
	}
	if got, err := second.Get(ctx, id, agent, revision.RevisionID, agentcfg.ConfigScopeAgent); err != nil || got.ContentHash != revision.ContentHash {
		t.Fatalf("post-restart immutable history = (%+v,%v), want retained revision", got, err)
	}
	if _, err := second.SetRevision(ctx, id, agent, agentcfg.ConfigScopeUser, skills("must-not-resurrect"), agentcfg.SetOptions{}); !errors.Is(err, agentcfg.ErrAgentRetired) {
		t.Fatalf("post-restart user mutation = %v, want ErrAgentRetired", err)
	}
}
