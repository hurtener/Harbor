package statestore_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionfence"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	statesqlite "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

type retirementDiscoveryBoundaryStore struct {
	state.StateStore
	failAt uint64
	failed atomic.Bool
}

type retirementAfterItemSaveFaultStore struct {
	state.StateStore
	failed atomic.Bool
}

type retirementScrubBoundaryStore struct {
	state.StateStore
	boundary string
	failed   atomic.Bool
}

func (s *retirementScrubBoundaryStore) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	if s.failed.Load() {
		return s.StateStore.SaveIf(ctx, expectations, next)
	}
	if s.boundary == "compact" && strings.HasPrefix(next.Kind, "agentcfg.retirement.manifest.") && strings.Contains(string(next.Bytes), `"scrubbed":true`) && s.failed.CompareAndSwap(false, true) {
		return errors.New("injected manifest compaction failure")
	}
	if next.Kind == agentcfg.ActiveSlotKind {
		var envelope struct {
			Retirement *struct {
				CleanupCompleted uint64 `json:"cleanup_completed"`
				ScrubCompleted   uint64 `json:"scrub_completed"`
			} `json:"retirement"`
		}
		if json.Unmarshal(next.Bytes, &envelope) == nil && envelope.Retirement != nil {
			matches := s.boundary == "cleanup" && envelope.Retirement.CleanupCompleted == 1 && envelope.Retirement.ScrubCompleted == 0
			matches = matches || s.boundary == "scrub-cursor" && envelope.Retirement.CleanupCompleted == 1 && envelope.Retirement.ScrubCompleted == 1
			if matches && s.failed.CompareAndSwap(false, true) {
				return errors.New("injected retirement scrub lifecycle failure")
			}
		}
	}
	return s.StateStore.SaveIf(ctx, expectations, next)
}

func (s *retirementAfterItemSaveFaultStore) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	if next.Kind == agentcfg.ActiveSlotKind && !s.failed.Load() {
		var envelope struct {
			Retirement *struct {
				ManifestCount uint64 `json:"manifest_count"`
			} `json:"retirement"`
		}
		if json.Unmarshal(next.Bytes, &envelope) == nil && envelope.Retirement != nil && envelope.Retirement.ManifestCount == 1 && s.failed.CompareAndSwap(false, true) {
			return errors.New("injected lifecycle failure after manifest item save")
		}
	}
	return s.StateStore.SaveIf(ctx, expectations, next)
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
	continuation = ""
	scrubbed := 0
	for {
		page, err := restartedStore.ScanKindForTenant(ctx, state.ListScope{MaintenanceScoped: true}, admin.TenantID, manifestPrefix, state.MaxStateScanLimit, continuation)
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range page.Records {
			body := string(record.Bytes)
			if !strings.Contains(body, `"scrubbed":true`) || strings.Contains(body, `"resource"`) || strings.Contains(body, `"source"`) || strings.Contains(body, "user-") || strings.Contains(body, "session-") || strings.Contains(body, "skill-") {
				t.Fatalf("completed manifest retained target content kind=%q body=%s", record.Kind, body)
			}
			scrubbed++
		}
		if page.Continuation == "" {
			break
		}
		continuation = page.Continuation
	}
	if scrubbed != targetCount {
		t.Fatalf("scrubbed=%d want=%d", scrubbed, targetCount)
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

func TestRetirement_OccupiedOrdinalReplaysStoredSuccessorAfterSourceDeletion(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		for _, following := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/following=%t", driver, following), func(t *testing.T) {
				ctx := context.Background()
				var base state.StateStore
				var err error
				dsn := filepath.Join(t.TempDir(), "successor.db")
				if driver == "sqlite" {
					base, err = statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
				} else {
					base, err = stateinmem.New(config.StateConfig{Driver: "inmem"})
				}
				if err != nil {
					t.Fatal(err)
				}
				admin := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-successor", UserID: "admin", SessionID: "control"}}
				const agent = "successor-agent"
				operation := fmt.Sprintf("successor-%s-%t", driver, following)
				first := newRegistryOnStore(t, &retirementAfterItemSaveFaultStore{StateStore: base})
				revision, err := first.SetRevision(ctx, admin, agent, agentcfg.ConfigScopeAgent, skills("history"), agentcfg.SetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				personal, _ := sessionoverlay.NewDurableStore(base, nil)
				seedCount := 1 + map[bool]int{true: 1}[following]
				for i := range seedCount {
					target := identity.Quadruple{Identity: identity.Identity{TenantID: admin.TenantID, UserID: fmt.Sprintf("erased-user-%d", i), SessionID: fmt.Sprintf("erased-session-%d", i)}}
					if _, err := personal.SavePersonal(ctx, target, agent, retirementPersonalSkill(fmt.Sprintf("erased-name-%d", i)), "", ""); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := first.(agentcfg.RetirementRegistry).Retire(ctx, admin, agent, agentcfg.RetirementRequest{OperationID: operation, ExpectedContentHash: revision.ContentHash}); err == nil {
					t.Fatal("injected lifecycle failure unexpectedly succeeded")
				}
				lifecycleQ, _, _ := agentcfg.LifecycleSlot(admin.TenantID, agent)
				manifestKind := fmt.Sprintf("agentcfg.retirement.manifest.%s.%020d", agentcfg.RetirementOperationHash(operation), 0)
				pending, err := base.Load(ctx, lifecycleQ, manifestKind)
				if err != nil {
					t.Fatal(err)
				}
				var persisted struct {
					Resource string `json:"resource"`
				}
				if err := json.Unmarshal(pending.Bytes, &persisted); err != nil {
					t.Fatal(err)
				}
				targetBytes, err := base64.RawURLEncoding.DecodeString(persisted.Resource)
				if err != nil {
					t.Fatal(err)
				}
				var target struct {
					TenantID  string `json:"tenant_id"`
					UserID    string `json:"user_id"`
					SessionID string `json:"session_id"`
					Kind      string `json:"kind"`
				}
				if err := json.Unmarshal(targetBytes, &target); err != nil {
					t.Fatal(err)
				}
				deletedQ := identity.Quadruple{Identity: identity.Identity{TenantID: target.TenantID, UserID: target.UserID, SessionID: target.SessionID}}
				fenceQ, fenceKind, err := sessionfence.PendingSlot(deletedQ)
				if err != nil {
					t.Fatal(err)
				}
				if err := base.Save(ctx, state.StateRecord{ID: state.NewEventID(), Identity: fenceQ, Kind: fenceKind, Bytes: []byte(`{"schema":1,"stage":"pending"}`)}); err != nil {
					t.Fatal(err)
				}
				if _, err := base.DeleteScope(ctx, deletedQ.Identity); err != nil {
					t.Fatal(err)
				}
				_ = first.Close(ctx)
				if driver == "sqlite" {
					if err := base.Close(ctx); err != nil {
						t.Fatal(err)
					}
					base, err = statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
					if err != nil {
						t.Fatal(err)
					}
				}
				defer base.Close(ctx)
				restarted := newRegistryOnStore(t, base)
				status, err := restarted.(agentcfg.RetirementRegistry).Retire(ctx, admin, agent, agentcfg.RetirementRequest{OperationID: operation, ExpectedContentHash: revision.ContentHash})
				if err != nil {
					t.Fatalf("resume from occupied ordinal: %v", err)
				}
				completed := 0
				for !status.Completed {
					if len(status.Cleanup) != 1 {
						t.Fatalf("pending status=%+v", status)
					}
					step := status.Cleanup[0]
					status, err = restarted.(agentcfg.RetirementRegistry).CompleteRetirementStep(ctx, admin, agent, operation, step.Class, step.Resource)
					if err != nil {
						t.Fatalf("complete item %d: %v", completed, err)
					}
					completed++
				}
				want := 1
				if following {
					want = 2
				}
				if completed != want || len(status.Cleanup) != 0 {
					t.Fatalf("completed=%d cleanup=%+v want=%d", completed, status.Cleanup, want)
				}
				for ordinal := range want {
					record, err := base.Load(ctx, lifecycleQ, fmt.Sprintf("agentcfg.retirement.manifest.%s.%020d", agentcfg.RetirementOperationHash(operation), ordinal))
					if err != nil || !strings.Contains(string(record.Bytes), `"scrubbed":true`) || strings.Contains(string(record.Bytes), "erased-") {
						t.Fatalf("final manifest ordinal %d retained target content: %s err=%v", ordinal, record.Bytes, err)
					}
				}
				if got, err := restarted.Get(ctx, admin, agent, revision.RevisionID, agentcfg.ConfigScopeAgent); err != nil || got.ContentHash != revision.ContentHash {
					t.Fatalf("immutable history=(%+v,%v)", got, err)
				}
			})
		}
	}
}

func TestRetirement_UnfencedAbsentFrozenPersonalTargetFailsClosedAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "unfenced-absent.db")
	base, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	first := newRegistryOnStore(t, base)
	admin := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-unfenced", UserID: "admin", SessionID: "control"}}
	missing := identity.Quadruple{Identity: identity.Identity{TenantID: admin.TenantID, UserID: "a-missing-user", SessionID: "a-missing-session"}}
	sibling := missing
	other := identity.Quadruple{Identity: identity.Identity{TenantID: admin.TenantID, UserID: "z-survivor-user", SessionID: "z-survivor-session"}}
	const agent = "a"
	const siblingAgent = "ab"
	const operation = "unfenced-absent-operation"

	revision, err := first.SetRevision(ctx, admin, agent, agentcfg.ConfigScopeAgent, skills("history"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.SetRevision(ctx, admin, siblingAgent, agentcfg.ConfigScopeAgent, skills("sibling-history"), agentcfg.SetOptions{}); err != nil {
		t.Fatal(err)
	}
	personal, err := sessionoverlay.NewDurableStore(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := personal.SavePersonal(ctx, missing, agent, retirementPersonalSkill("missing"), "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := personal.SavePersonal(ctx, sibling, siblingAgent, retirementPersonalSkill("sibling"), "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := personal.SavePersonal(ctx, other, agent, retirementPersonalSkill("other"), "", ""); err != nil {
		t.Fatal(err)
	}
	missingKind, _ := sessionoverlay.PersonalSkillKind(agent, "missing")
	siblingKind, _ := sessionoverlay.PersonalSkillKind(siblingAgent, "sibling")
	otherKind, _ := sessionoverlay.PersonalSkillKind(agent, "other")
	siblingBefore, err := base.Load(ctx, sibling, siblingKind)
	if err != nil {
		t.Fatal(err)
	}
	otherBefore, err := base.Load(ctx, other, otherKind)
	if err != nil {
		t.Fatal(err)
	}

	status, err := first.(agentcfg.RetirementRegistry).Retire(ctx, admin, agent, agentcfg.RetirementRequest{OperationID: operation, ExpectedContentHash: revision.ContentHash})
	if err != nil || len(status.Cleanup) != 1 || status.Cleanup[0].Class != "session_personal" {
		t.Fatalf("frozen status=(%+v,%v)", status, err)
	}
	lifecycleQ, lifecycleKind, _ := agentcfg.LifecycleSlot(admin.TenantID, agent)
	lifecycleBefore, err := base.Load(ctx, lifecycleQ, lifecycleKind)
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint struct {
		Retirement struct {
			CleanupCompleted uint64 `json:"cleanup_completed"`
			ScrubCompleted   uint64 `json:"scrub_completed"`
		} `json:"retirement"`
	}
	if err := json.Unmarshal(lifecycleBefore.Bytes, &checkpoint); err != nil {
		t.Fatal(err)
	}
	if checkpoint.Retirement.CleanupCompleted != 0 || checkpoint.Retirement.ScrubCompleted != 0 {
		t.Fatalf("initial cursors cleanup=%d scrub=%d", checkpoint.Retirement.CleanupCompleted, checkpoint.Retirement.ScrubCompleted)
	}
	if err := base.Delete(ctx, missing, missingKind); err != nil {
		t.Fatal(err)
	}
	for _, slot := range []func(identity.Quadruple) (identity.Quadruple, string, error){sessionfence.PendingSlot, sessionfence.TombstoneSlot} {
		q, kind, err := slot(missing)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := base.Load(ctx, q, kind); !errors.Is(err, state.ErrNotFound) {
			t.Fatalf("unexpected erasure fence %q: %v", kind, err)
		}
	}

	step := status.Cleanup[0]
	if _, err := first.(agentcfg.RetirementRegistry).CompleteRetirementStep(ctx, admin, agent, operation, step.Class, step.Resource); !errors.Is(err, sessionoverlay.ErrStateUnavailable) {
		t.Fatalf("unfenced cleanup error=%v, want ErrStateUnavailable", err)
	}
	_, found, err := first.(agentcfg.RetirementRegistry).RetirementStatus(ctx, admin, agent)
	if !errors.Is(err, sessionoverlay.ErrStateUnavailable) || !found {
		t.Fatalf("status after refusal=(found=%v,error=%v), want found ErrStateUnavailable", found, err)
	}
	assertUnchanged := func(store state.StateStore) {
		t.Helper()
		lifecycleAfter, err := store.Load(ctx, lifecycleQ, lifecycleKind)
		if err != nil || lifecycleAfter.ID != lifecycleBefore.ID || !bytes.Equal(lifecycleAfter.Bytes, lifecycleBefore.Bytes) {
			t.Fatalf("lifecycle changed after refusal: before=%s after=%s err=%v", lifecycleBefore.Bytes, lifecycleAfter.Bytes, err)
		}
		for _, survivor := range []state.StateRecord{siblingBefore, otherBefore} {
			got, err := store.Load(ctx, survivor.Identity, survivor.Kind)
			if err != nil || got.ID != survivor.ID || !bytes.Equal(got.Bytes, survivor.Bytes) {
				t.Fatalf("survivor changed: got=(%+v,%v) want=%+v", got, err, survivor)
			}
		}
		if _, err := store.Load(ctx, missing, missingKind); !errors.Is(err, state.ErrNotFound) {
			t.Fatalf("missing target changed: %v", err)
		}
	}
	assertUnchanged(base)

	_ = first.Close(ctx)
	if err := base.Close(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(ctx)
	restarted := newRegistryOnStore(t, reopened)
	_, found, err = restarted.(agentcfg.RetirementRegistry).RetirementStatus(ctx, admin, agent)
	if !errors.Is(err, sessionoverlay.ErrStateUnavailable) || !found {
		t.Fatalf("restarted status=(found=%v,error=%v), want found ErrStateUnavailable", found, err)
	}
	if _, err := restarted.(agentcfg.RetirementRegistry).CompleteRetirementStep(ctx, admin, agent, operation, step.Class, step.Resource); !errors.Is(err, sessionoverlay.ErrStateUnavailable) {
		t.Fatalf("restarted unfenced cleanup error=%v, want ErrStateUnavailable", err)
	}
	assertUnchanged(reopened)
}

func TestRetirement_SQLiteScrubOrderingRestartsAtEveryBoundary(t *testing.T) {
	for _, boundary := range []string{"cleanup", "compact", "scrub-cursor"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			dsn := filepath.Join(t.TempDir(), "scrub-order.db")
			base, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
			if err != nil {
				t.Fatal(err)
			}
			fault := &retirementScrubBoundaryStore{StateStore: base, boundary: boundary}
			first := newRegistryOnStore(t, fault)
			admin := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-scrub", UserID: "admin", SessionID: "control"}}
			agent := "scrub-" + boundary
			operation := "operation-" + boundary
			payload := agentcfg.ConfigPayload{Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{{Name: "private-resource-" + boundary}}}}
			revision, err := first.SetRevision(ctx, admin, agent, agentcfg.ConfigScopeAgent, payload, agentcfg.SetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			status, err := first.(agentcfg.RetirementRegistry).Retire(ctx, admin, agent, agentcfg.RetirementRequest{OperationID: operation, ExpectedContentHash: revision.ContentHash})
			if err != nil || len(status.Cleanup) != 1 {
				t.Fatalf("retire=(%+v,%v)", status, err)
			}
			step := status.Cleanup[0]
			if _, err := first.(agentcfg.RetirementRegistry).CompleteRetirementStep(ctx, admin, agent, operation, step.Class, step.Resource); err == nil {
				t.Fatal("injected scrub boundary unexpectedly completed")
			}
			lifecycleQ, lifecycleKind, _ := agentcfg.LifecycleSlot(admin.TenantID, agent)
			manifestKind := fmt.Sprintf("agentcfg.retirement.manifest.%s.%020d", agentcfg.RetirementOperationHash(operation), 0)
			manifest, err := base.Load(ctx, lifecycleQ, manifestKind)
			if err != nil {
				t.Fatal(err)
			}
			isCompact := strings.Contains(string(manifest.Bytes), `"scrubbed":true`)
			if (boundary == "scrub-cursor") != isCompact {
				t.Fatalf("boundary %s compact=%t body=%s", boundary, isCompact, manifest.Bytes)
			}
			lifecycle, err := base.Load(ctx, lifecycleQ, lifecycleKind)
			if err != nil {
				t.Fatal(err)
			}
			var durable struct {
				Retirement struct {
					CleanupCompleted uint64 `json:"cleanup_completed"`
					ScrubCompleted   uint64 `json:"scrub_completed"`
				} `json:"retirement"`
			}
			if err := json.Unmarshal(lifecycle.Bytes, &durable); err != nil {
				t.Fatal(err)
			}
			wantCleanup := uint64(1)
			if boundary == "cleanup" {
				wantCleanup = 0
			}
			if durable.Retirement.CleanupCompleted != wantCleanup || durable.Retirement.ScrubCompleted != 0 {
				t.Fatalf("boundary %s lifecycle cleanup=%d scrub=%d", boundary, durable.Retirement.CleanupCompleted, durable.Retirement.ScrubCompleted)
			}
			_ = first.Close(ctx)
			_ = base.Close(ctx)
			reopened, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close(ctx)
			restarted := newRegistryOnStore(t, reopened)
			status, err = restarted.(agentcfg.RetirementRegistry).Retire(ctx, admin, agent, agentcfg.RetirementRequest{OperationID: operation, ExpectedContentHash: revision.ContentHash})
			if err != nil {
				t.Fatalf("restart replay: %v", err)
			}
			if boundary == "cleanup" {
				if len(status.Cleanup) != 1 {
					t.Fatalf("cleanup-CAS restart lost full item: %+v", status)
				}
				step = status.Cleanup[0]
				status, err = restarted.(agentcfg.RetirementRegistry).CompleteRetirementStep(ctx, admin, agent, operation, step.Class, step.Resource)
				if err != nil {
					t.Fatalf("retry cleanup: %v", err)
				}
			}
			if !status.Completed || len(status.Cleanup) != 0 {
				t.Fatalf("restart status=%+v", status)
			}
			finalManifest, err := reopened.Load(ctx, lifecycleQ, manifestKind)
			if err != nil || !strings.Contains(string(finalManifest.Bytes), `"scrubbed":true`) || strings.Contains(string(finalManifest.Bytes), "private-resource") {
				t.Fatalf("final compact manifest=%s err=%v", finalManifest.Bytes, err)
			}
			replay, err := restarted.(agentcfg.RetirementRegistry).Retire(ctx, admin, agent, agentcfg.RetirementRequest{OperationID: operation, ExpectedContentHash: revision.ContentHash})
			if err != nil || !replay.Completed || len(replay.Cleanup) != 0 {
				t.Fatalf("completed replay=(%+v,%v)", replay, err)
			}
			if got, err := restarted.Get(ctx, admin, agent, revision.RevisionID, agentcfg.ConfigScopeAgent); err != nil || got.ContentHash != revision.ContentHash {
				t.Fatalf("immutable history=(%+v,%v)", got, err)
			}
		})
	}
}
