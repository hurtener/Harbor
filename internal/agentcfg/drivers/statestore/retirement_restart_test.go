package statestore_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
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

// TestRetirement_SQLiteRestartResumesFourSlotPersonalTombstones proves each
// discovered personal row is its own resumable cleanup unit. A restart after
// one logical tombstone preserves the frozen manifest, resumes the remaining
// row, and retains immutable config history.
func TestRetirement_SQLiteRestartResumesFourSlotPersonalTombstones(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "retirement-session-restart.db")
	open := func() (agentcfg.Registry, *sessionoverlay.DurableStore, state.StateStore, func()) {
		store, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
		if err != nil {
			t.Fatalf("open SQLite StateStore: %v", err)
		}
		bus := newBus(t)
		registry, err := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: store, Bus: bus})
		if err != nil {
			t.Fatalf("open registry: %v", err)
		}
		personal, err := sessionoverlay.NewDurableStore(store, nil)
		if err != nil {
			t.Fatalf("open personal store: %v", err)
		}
		return registry, personal, store, func() {
			_ = registry.Close(ctx)
			_ = bus.Close(ctx)
			_ = store.Close(ctx)
		}
	}

	admin := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-session-restart", UserID: "admin", SessionID: "control"}}
	one := identity.Quadruple{Identity: identity.Identity{TenantID: admin.TenantID, UserID: "alice", SessionID: "one"}}
	two := identity.Quadruple{Identity: identity.Identity{TenantID: admin.TenantID, UserID: "bob", SessionID: "two"}}
	const agent = "agent-session-restart"
	first, personal, _, closeFirst := open()
	revision, err := first.SetRevision(ctx, admin, agent, agentcfg.ConfigScopeAgent, skills("history"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i, session := range []identity.Quadruple{one, two} {
		if _, err := personal.SavePersonal(ctx, session, agent, retirementPersonalSkill(fmt.Sprintf("skill-%d", i)), "", ""); err != nil {
			t.Fatalf("seed personal %d: %v", i, err)
		}
	}
	retirer := first.(agentcfg.RetirementRegistry)
	status, err := retirer.Retire(ctx, admin, agent, agentcfg.RetirementRequest{OperationID: "restart-session-op", ExpectedContentHash: revision.ContentHash})
	if err != nil {
		t.Fatal(err)
	}
	var completedOne bool
	for _, step := range status.Cleanup {
		if step.Class == "session_personal" {
			status, err = retirer.CompleteRetirementStep(ctx, admin, agent, status.OperationID, step.Class, step.Resource)
			if err != nil {
				t.Fatal(err)
			}
			completedOne = true
			break
		}
	}
	if !completedOne || status.Completed {
		t.Fatalf("partial status=%+v", status)
	}
	closeFirst()

	second, _, store, closeSecond := open()
	defer closeSecond()
	retirer = second.(agentcfg.RetirementRegistry)
	status, err = retirer.Retire(ctx, admin, agent, agentcfg.RetirementRequest{OperationID: "restart-session-op", ExpectedContentHash: revision.ContentHash})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range status.Cleanup {
		if step.Completed {
			continue
		}
		status, err = retirer.CompleteRetirementStep(ctx, admin, agent, status.OperationID, step.Class, step.Resource)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !status.Completed {
		t.Fatalf("resumed status=%+v", status)
	}
	for i, session := range []identity.Quadruple{one, two} {
		kind, _ := sessionoverlay.PersonalSkillKind(agent, fmt.Sprintf("skill-%d", i))
		record, err := store.Load(ctx, session, kind)
		if err != nil || !strings.Contains(string(record.Bytes), `"deleted":true`) {
			t.Fatalf("session %d tombstone=(%s,%v)", i, record.Bytes, err)
		}
	}
	if got, err := second.Get(ctx, admin, agent, revision.RevisionID, agentcfg.ConfigScopeAgent); err != nil || got.ContentHash != revision.ContentHash {
		t.Fatalf("immutable history=(%+v,%v)", got, err)
	}
}
