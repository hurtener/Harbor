package statestore_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	statesqlite "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

type retirementDiscoveryBoundaryStore struct {
	state.StateStore
	failAt uint64
	failed atomic.Bool
}

func (s *retirementDiscoveryBoundaryStore) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	if next.Kind == agentcfg.ActiveSlotKind && !s.failed.Load() {
		var envelope struct {
			Retirement *struct {
				ManifestCount uint64 `json:"manifest_count"`
			} `json:"retirement"`
		}
		if json.Unmarshal(next.Bytes, &envelope) == nil && envelope.Retirement != nil && envelope.Retirement.ManifestCount == s.failAt && s.failed.CompareAndSwap(false, true) {
			return errors.New("injected discovery checkpoint stop")
		}
	}
	return s.StateStore.SaveIf(ctx, expectations, next)
}

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
	if len(status.Cleanup) == 1 && status.Cleanup[0].Class == "session_personal" {
		step := status.Cleanup[0]
		status, err = retirer.CompleteRetirementStep(ctx, admin, agent, status.OperationID, step.Class, step.Resource)
		if err != nil {
			t.Fatal(err)
		}
		completedOne = true
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
	for !status.Completed {
		if len(status.Cleanup) != 1 {
			t.Fatalf("bounded resumed status=%+v", status)
		}
		step := status.Cleanup[0]
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

func TestRetirement_SQLiteMultipageLongTargetsRestartMakesMonotonicProgress(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "retirement-multipage.db")
	openStore := func() state.StateStore {
		store, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
		if err != nil {
			t.Fatalf("open SQLite: %v", err)
		}
		return store
	}
	base := openStore()
	fault := &retirementDiscoveryBoundaryStore{StateStore: base, failAt: 257}
	registry := newRegistryOnStore(t, fault)
	personal, err := sessionoverlay.NewDurableStore(fault, nil)
	if err != nil {
		t.Fatal(err)
	}
	admin := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-multipage", UserID: "admin", SessionID: "control"}}
	otherAdmin := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-other", UserID: "admin", SessionID: "control"}}
	const (
		agent        = "a"
		siblingAgent = "ab"
		operation    = "multipage-long-target-op"
		targetCount  = 270
	)
	revision, err := registry.SetRevision(ctx, admin, agent, agentcfg.ConfigScopeAgent, skills("history"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.SetRevision(ctx, admin, siblingAgent, agentcfg.ConfigScopeAgent, skills("sibling"), agentcfg.SetOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.SetRevision(ctx, otherAdmin, agent, agentcfg.ConfigScopeAgent, skills("other"), agentcfg.SetOptions{}); err != nil {
		t.Fatal(err)
	}
	for i := range targetCount {
		id := identity.Quadruple{Identity: identity.Identity{TenantID: admin.TenantID, UserID: fmt.Sprintf("user-%03d-%s", i, strings.Repeat("u", 140)), SessionID: fmt.Sprintf("session-%03d-%s", i, strings.Repeat("s", 140))}}
		if _, err := personal.SavePersonal(ctx, id, agent, retirementPersonalSkill(fmt.Sprintf("skill-%03d", i)), "", ""); err != nil {
			t.Fatalf("seed target %d: %v", i, err)
		}
	}
	siblingID := identity.Quadruple{Identity: identity.Identity{TenantID: admin.TenantID, UserID: "sibling", SessionID: "session"}}
	otherID := identity.Quadruple{Identity: identity.Identity{TenantID: otherAdmin.TenantID, UserID: "other", SessionID: "session"}}
	if _, err := personal.SavePersonal(ctx, siblingID, siblingAgent, retirementPersonalSkill("sibling"), "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := personal.SavePersonal(ctx, otherID, agent, retirementPersonalSkill("other"), "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.(agentcfg.RetirementRegistry).Retire(ctx, admin, agent, agentcfg.RetirementRequest{OperationID: operation, ExpectedContentHash: revision.ContentHash}); err == nil {
		t.Fatal("injected discovery stop unexpectedly succeeded")
	}
	lifecycleQ, lifecycleKind, _ := agentcfg.LifecycleSlot(admin.TenantID, agent)
	checkpoint, err := base.Load(ctx, lifecycleQ, lifecycleKind)
	if err != nil {
		t.Fatal(err)
	}
	var durable struct {
		Retirement struct {
			ManifestCount uint64 `json:"manifest_count"`
			Discovery     any    `json:"discovery"`
		} `json:"retirement"`
	}
	if err := json.Unmarshal(checkpoint.Bytes, &durable); err != nil || durable.Retirement.ManifestCount != 256 || len(checkpoint.Bytes) >= agentcfg.MaxLifecycleRecordBytes {
		t.Fatalf("durable checkpoint count=%d bytes=%d err=%v", durable.Retirement.ManifestCount, len(checkpoint.Bytes), err)
	}
	pendingKind := fmt.Sprintf("agentcfg.retirement.manifest.%s.%020d", agentcfg.RetirementOperationHash(operation), durable.Retirement.ManifestCount)
	if _, err := base.Load(ctx, lifecycleQ, pendingKind); err != nil {
		t.Fatalf("failed retry exposed neither cursor advance nor exact pending item: %v", err)
	}
	_ = registry.Close(ctx)
	_ = base.Close(ctx)

	restartedStore := openStore()
	defer restartedStore.Close(ctx)
	restarted := newRegistryOnStore(t, restartedStore)
	status, err := restarted.(agentcfg.RetirementRegistry).Retire(ctx, admin, agent, agentcfg.RetirementRequest{OperationID: operation, ExpectedContentHash: revision.ContentHash})
	if err != nil {
		t.Fatalf("restart replay: %v", err)
	}
	manifestPrefix := "agentcfg.retirement.manifest." + agentcfg.RetirementOperationHash(operation) + "."
	continuation := ""
	manifestCount := 0
	manifestBytes := 0
	for {
		page, err := restartedStore.ScanKindForTenant(ctx, state.ListScope{MaintenanceScoped: true}, admin.TenantID, manifestPrefix, state.MaxStateScanLimit, continuation)
		if err != nil {
			t.Fatal(err)
		}
		manifestCount += len(page.Records)
		for _, record := range page.Records {
			manifestBytes += len(record.Bytes)
		}
		if page.Continuation == "" {
			break
		}
		continuation = page.Continuation
	}
	if manifestCount != targetCount || manifestBytes <= agentcfg.MaxLifecycleRecordBytes {
		t.Fatalf("bounded manifest records=%d bytes=%d", manifestCount, manifestBytes)
	}
	completed := 0
	for !status.Completed {
		if len(status.Cleanup) != 1 {
			t.Fatalf("pending status=%+v", status)
		}
		step := status.Cleanup[0]
		status, err = restarted.(agentcfg.RetirementRegistry).CompleteRetirementStep(ctx, admin, agent, operation, step.Class, step.Resource)
		if err != nil {
			t.Fatalf("cleanup %d: %v", completed, err)
		}
		completed++
	}
	if completed != targetCount {
		t.Fatalf("completed=%d want=%d", completed, targetCount)
	}
	personalPrefix, _ := sessionoverlay.PersonalSkillPrefix(agent)
	continuation = ""
	tombstones := 0
	for {
		page, err := restartedStore.ScanKindForTenant(ctx, state.ListScope{MaintenanceScoped: true}, admin.TenantID, personalPrefix, state.MaxStateScanLimit, continuation)
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range page.Records {
			if !strings.Contains(string(record.Bytes), `"deleted":true`) {
				t.Fatalf("cleanup retained live target kind=%q", record.Kind)
			}
			tombstones++
		}
		if page.Continuation == "" {
			break
		}
		continuation = page.Continuation
	}
	if tombstones != targetCount {
		t.Fatalf("logical tombstones=%d want=%d", tombstones, targetCount)
	}
	restartedPersonal, _ := sessionoverlay.NewDurableStore(restartedStore, nil)
	for _, survivor := range []struct {
		id    identity.Quadruple
		agent string
		name  string
	}{{siblingID, siblingAgent, "sibling"}, {otherID, agent, "other"}} {
		if got, found, err := restartedPersonal.LoadPersonal(ctx, survivor.id, survivor.agent, survivor.name); err != nil || !found || got.Deleted {
			t.Fatalf("survivor %s/%s=(%+v,%v,%v)", survivor.id.TenantID, survivor.agent, got, found, err)
		}
	}
	if got, err := restarted.Get(ctx, admin, agent, revision.RevisionID, agentcfg.ConfigScopeAgent); err != nil || got.ContentHash != revision.ContentHash {
		t.Fatalf("history=(%+v,%v)", got, err)
	}
}
