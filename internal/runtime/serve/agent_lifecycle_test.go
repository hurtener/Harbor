package serve

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// lifecycleLoadBarrierStore returns a stale absent answer for precisely the
// first N lifecycle reads, after holding each reader until the test installs a
// competing record. It forces the dangerous absent-read / publication window
// across independent Registry instances without relying on scheduler timing.
type lifecycleLoadBarrierStore struct {
	state.StateStore
	identity    identity.Quadruple
	kind        string
	target      int
	seen        chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
	mu          sync.Mutex
	count       int
}

func (s *lifecycleLoadBarrierStore) Release() { s.releaseOnce.Do(func() { close(s.release) }) }

func (s *lifecycleLoadBarrierStore) Load(ctx context.Context, q identity.Quadruple, kind string) (state.StateRecord, error) {
	wait := false
	if q == s.identity && kind == s.kind {
		s.mu.Lock()
		if s.count < s.target {
			s.count++
			wait = true
		}
		s.mu.Unlock()
	}
	if wait {
		s.seen <- struct{}{}
		select {
		case <-s.release:
		case <-ctx.Done():
			return state.StateRecord{}, ctx.Err()
		}
		return state.StateRecord{}, fmt.Errorf("stale lifecycle absence: %w", state.ErrNotFound)
	}
	return s.StateStore.Load(ctx, q, kind)
}

func lifecycleTestRegistry(t *testing.T, st state.StateStore) agentcfg.Registry {
	t.Helper()
	reg, err := agentcfg.Open(t.Context(), agentcfg.Config{}, agentcfg.Deps{
		State: st,
		Bus:   mkDriverTestBus(t, auditpatterns.New()),
	})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })
	return reg
}

func TestEnsureBootAgentLifecycle_MaterializesOnlyAbsentSlot(t *testing.T) {
	const agentID = "boot-lifecycle-agent"
	id := identity.Identity{TenantID: "lifecycle-tenant", UserID: "lifecycle-user", SessionID: "lifecycle-session"}
	q := identity.Quadruple{Identity: id}

	t.Run("absent becomes active", func(t *testing.T) {
		st := runSnapshotState(t)
		reg := lifecycleTestRegistry(t, st)
		if err := EnsureBootAgentLifecycle(t.Context(), st, reg, id, agentID); err != nil {
			t.Fatalf("EnsureBootAgentLifecycle: %v", err)
		}
		rev, active, err := reg.Active(t.Context(), q, agentID, agentcfg.ConfigScopeAgent)
		if err != nil || !active {
			t.Fatalf("active boot lifecycle: active=%t err=%v", active, err)
		}
		if rev.ParentRevisionID != "" {
			t.Fatalf("first boot revision parent = %q, want empty", rev.ParentRevisionID)
		}
	})

	t.Run("terminal remains terminal", func(t *testing.T) {
		st := runSnapshotState(t)
		reg := lifecycleTestRegistry(t, st)
		slot, kind, err := agentcfg.LifecycleSlot(id.TenantID, agentID)
		if err != nil {
			t.Fatal(err)
		}
		terminal := []byte(`{"schema":1,"revision_id":"","updated_at":"2026-08-02T00:00:00Z"}`)
		if err := st.Save(t.Context(), state.StateRecord{ID: state.NewEventID(), Identity: slot, Kind: kind, Bytes: terminal}); err != nil {
			t.Fatalf("seed terminal lifecycle: %v", err)
		}
		if err := EnsureBootAgentLifecycle(t.Context(), st, reg, id, agentID); !errors.Is(err, agentcfg.ErrAgentRetired) {
			t.Fatalf("EnsureBootAgentLifecycle terminal = %v, want ErrAgentRetired", err)
		}
		record, err := st.Load(t.Context(), slot, kind)
		if err != nil {
			t.Fatalf("load terminal lifecycle: %v", err)
		}
		if string(record.Bytes) != string(terminal) {
			t.Fatalf("terminal lifecycle was changed: got %s want %s", record.Bytes, terminal)
		}
		if _, active, err := reg.Active(t.Context(), q, agentID, agentcfg.ConfigScopeAgent); !errors.Is(err, agentcfg.ErrAgentRetired) || active {
			t.Fatalf("terminal lifecycle Active = active=%t err=%v, want ErrAgentRetired", active, err)
		}
	})

	t.Run("corrupt remains corrupt", func(t *testing.T) {
		st := runSnapshotState(t)
		reg := lifecycleTestRegistry(t, st)
		slot, kind, err := agentcfg.LifecycleSlot(id.TenantID, agentID)
		if err != nil {
			t.Fatal(err)
		}
		corrupt := []byte(`{"schema":1,"revision_id":"active"}`)
		if err := st.Save(t.Context(), state.StateRecord{ID: state.NewEventID(), Identity: slot, Kind: kind, Bytes: corrupt}); err != nil {
			t.Fatalf("seed corrupt lifecycle: %v", err)
		}
		if err := EnsureBootAgentLifecycle(t.Context(), st, reg, id, agentID); !errors.Is(err, agentcfg.ErrStateUnavailable) {
			t.Fatalf("EnsureBootAgentLifecycle corrupt = %v, want ErrStateUnavailable", err)
		}
		record, err := st.Load(t.Context(), slot, kind)
		if err != nil {
			t.Fatalf("load corrupt lifecycle: %v", err)
		}
		if string(record.Bytes) != string(corrupt) {
			t.Fatalf("corrupt lifecycle was changed: got %s want %s", record.Bytes, corrupt)
		}
	})
}

func TestEnsureBootAgentLifecycle_ConcurrentFirstWritersPreserveRealConfig(t *testing.T) {
	const agentID = "boot-cas-agent"
	id := identity.Identity{TenantID: "cas-tenant", UserID: "cas-user", SessionID: "cas-session"}
	q := identity.Quadruple{Identity: id}
	base := runSnapshotState(t)
	slot, kind, err := agentcfg.LifecycleSlot(id.TenantID, agentID)
	if err != nil {
		t.Fatal(err)
	}
	st := &lifecycleLoadBarrierStore{
		StateStore: base, identity: slot, kind: kind, target: 1,
		seen: make(chan struct{}, 1), release: make(chan struct{}),
	}
	t.Cleanup(st.Release)
	bootReg := lifecycleTestRegistry(t, st)
	realReg := lifecycleTestRegistry(t, st)

	bootResult := make(chan error, 1)
	go func() { bootResult <- EnsureBootAgentLifecycle(context.Background(), st, bootReg, id, agentID) }()
	<-st.seen // boot observed a genuinely absent lifecycle slot.
	if _, err := realReg.SetRevision(t.Context(), q, agentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: []string{"real-first-write"}},
	}, agentcfg.SetOptions{ExpectedContentHash: agentcfg.ExpectNoActiveRevision}); err != nil {
		t.Fatalf("real first writer: %v", err)
	}
	st.Release()
	if err := <-bootResult; err != nil {
		t.Fatalf("boot initializer after real winner: %v", err)
	}
	active, ok, err := realReg.Active(t.Context(), q, agentID, agentcfg.ConfigScopeAgent)
	if err != nil || !ok {
		t.Fatalf("active real winner: ok=%t err=%v", ok, err)
	}
	if active.Payload.Skills == nil || len(active.Payload.Skills.Names) != 1 || active.Payload.Skills.Names[0] != "real-first-write" {
		t.Fatalf("boot initializer overwrote concurrent real config: %+v", active.Payload)
	}

	// Two booters that both observe absent are also a normal cold-start
	// shape. The loser must converge on the winner instead of failing boot.
	secondBase := runSnapshotState(t)
	secondSlot, secondKind, err := agentcfg.LifecycleSlot("two-booters", agentID)
	if err != nil {
		t.Fatal(err)
	}
	second := &lifecycleLoadBarrierStore{
		StateStore: secondBase, identity: secondSlot, kind: secondKind, target: 2,
		seen: make(chan struct{}, 2), release: make(chan struct{}),
	}
	t.Cleanup(second.Release)
	firstBootReg := lifecycleTestRegistry(t, second)
	secondBootReg := lifecycleTestRegistry(t, second)
	secondID := identity.Identity{TenantID: "two-booters", UserID: "user", SessionID: "session"}
	results := make(chan error, 2)
	go func() {
		results <- EnsureBootAgentLifecycle(context.Background(), second, firstBootReg, secondID, agentID)
	}()
	go func() {
		results <- EnsureBootAgentLifecycle(context.Background(), second, secondBootReg, secondID, agentID)
	}()
	<-second.seen
	<-second.seen
	second.Release()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent booter: %v", err)
		}
	}
	if _, ok, err := firstBootReg.Active(t.Context(), identity.Quadruple{Identity: secondID}, agentID, agentcfg.ConfigScopeAgent); err != nil || !ok {
		t.Fatalf("concurrent boot lifecycle active: ok=%t err=%v", ok, err)
	}
}

func TestEnsureBootAgentLifecycle_ConcurrentTerminalAndCorruptSlotsStayUntouched(t *testing.T) {
	const agentID = "boot-lifecycle-race-agent"
	for _, tc := range []struct {
		name  string
		bytes []byte
		want  error
	}{
		{name: "terminal", bytes: []byte(`{"schema":1,"revision_id":"","updated_at":"2026-08-02T00:00:00Z"}`), want: agentcfg.ErrAgentRetired},
		{name: "corrupt", bytes: []byte(`{"schema":1,"revision_id":"active"}`), want: agentcfg.ErrStateUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := identity.Identity{TenantID: "between-" + tc.name, UserID: "user", SessionID: "session"}
			base := runSnapshotState(t)
			slot, kind, err := agentcfg.LifecycleSlot(id.TenantID, agentID)
			if err != nil {
				t.Fatal(err)
			}
			st := &lifecycleLoadBarrierStore{
				StateStore: base, identity: slot, kind: kind, target: 1,
				seen: make(chan struct{}, 1), release: make(chan struct{}),
			}
			t.Cleanup(st.Release)
			bootReg := lifecycleTestRegistry(t, st)
			// A separately constructed observer Registry shares the durable
			// store. Phase 234 will replace the direct save below with its CAS
			// retirement verb; the interleaving remains the same authority test.
			observerReg := lifecycleTestRegistry(t, base)
			result := make(chan error, 1)
			go func() { result <- EnsureBootAgentLifecycle(context.Background(), st, bootReg, id, agentID) }()
			<-st.seen // boot's direct preflight observed the stale absence.
			if err := base.Save(t.Context(), state.StateRecord{ID: state.NewEventID(), Identity: slot, Kind: kind, Bytes: tc.bytes}); err != nil {
				t.Fatalf("concurrent lifecycle write: %v", err)
			}
			st.Release()
			if err := <-result; !errors.Is(err, tc.want) {
				t.Fatalf("boot after %s = %v, want %v", tc.name, err, tc.want)
			}
			record, err := base.Load(t.Context(), slot, kind)
			if err != nil {
				t.Fatalf("load %s lifecycle: %v", tc.name, err)
			}
			if string(record.Bytes) != string(tc.bytes) {
				t.Fatalf("%s lifecycle was overwritten: got %s want %s", tc.name, record.Bytes, tc.bytes)
			}
			if _, _, err := observerReg.Active(t.Context(), identity.Quadruple{Identity: id}, agentID, agentcfg.ConfigScopeAgent); tc.name == "terminal" && !errors.Is(err, agentcfg.ErrAgentRetired) {
				t.Fatalf("terminal observer Active = %v, want ErrAgentRetired", err)
			} else if tc.name == "corrupt" && !errors.Is(err, agentcfg.ErrStateUnavailable) {
				t.Fatalf("corrupt observer Active = %v, want ErrStateUnavailable", err)
			}
		})
	}
}
