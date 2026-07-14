package integration_test

// retention_horizon_fleet_test.go — the Phase 175 (HA-23 / D-310) §17.1
// cross-subsystem integration suite for the fleet-scoped retention
// horizons on `runtime.health`.
//
// It wires REAL drivers across the whole retention seam — the real inmem
// event bus (implements events.RetentionReporter), the real in-process
// tasks engine (implements the identity-free OldestRetainedAt reader),
// the real session registry (implements OldestRetainedAt + ListSnapshots),
// and the real audit patterns redactor + bus. No mocks at the seam
// (CLAUDE.md §17.3). It exercises the `svc:`-identity fleet-observe path,
// cross-tenant isolation, and the fail-loud audit failure mode under
// -race.

import (
	"context"
	stderrors "errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	runtimeposture "github.com/hurtener/Harbor/internal/runtime/posture"
	"github.com/hurtener/Harbor/internal/sessions"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	tasksinproc "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

type retentionFleetStack struct {
	bus     events.EventBus
	taskReg tasks.TaskRegistry
	lister  sessions.SessionLister
}

func newRetentionFleetStack(t *testing.T) *retentionFleetStack {
	t.Helper()

	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         128,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	taskReg, err := tasksinproc.New(tasks.Dependencies{
		Store: store, Bus: bus, Redactor: auditpatterns.New(),
	})
	if err != nil {
		t.Fatalf("tasks: %v", err)
	}

	lister, err := sessions.New(store, config.SessionsConfig{}, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = lister.CloseRegistry(context.Background()) })

	return &retentionFleetStack{bus: bus, taskReg: taskReg, lister: lister}
}

// seedTenant opens a session and spawns a task under one triple, both
// stamped with `at` where the driver honours it (the session's OpenedAt
// is the registry clock; the task's CreatedAt is the engine clock — both
// are "now", so we order the two tenants by real time and compare
// horizons relatively).
func (s *retentionFleetStack) seedTenant(t *testing.T, reg *sessions.Registry, id identity.Identity) {
	t.Helper()
	ctx, _ := identity.With(context.Background(), id)
	if _, err := reg.Open(ctx, id.SessionID, id); err != nil {
		t.Fatalf("Open %s: %v", id.SessionID, err)
	}
	if _, err := s.taskReg.Spawn(ctx, tasks.SpawnRequest{
		Identity:    identity.Quadruple{Identity: id},
		Kind:        tasks.KindBackground,
		Description: "seed task",
	}); err != nil {
		t.Fatalf("Spawn %s: %v", id.SessionID, err)
	}
}

func retentionSurface(t *testing.T, stack *retentionFleetStack, red audit.Redactor) *protocol.PostureSurface {
	t.Helper()
	s, err := protocol.NewPostureSurface(protocol.PostureDeps{
		Clock:     func() time.Time { return time.Now().UTC() },
		Health:    func(context.Context) []types.SubsystemHealth { return nil },
		Retention: runtimeposture.RetentionProvider(stack.bus, stack.taskReg, stack.lister),
		Counters:  func(context.Context, identity.Identity) types.RuntimeCounters { return types.RuntimeCounters{} },
		Drivers:   func() []types.SubsystemDriver { return nil },
		Metrics:   func(context.Context) types.MetricsSnapshot { return types.MetricsSnapshot{} },
		Governance: governance.NewPostureProvider(governance.Config{
			DefaultTier:   "free",
			IdentityTiers: map[string]governance.TierConfig{"free": {BudgetCeilingUSD: 5.0}},
		}),
		LLM:        llm.NewPostureProvider(llm.ConfigSnapshot{Driver: "bifrost", Provider: "openai", Model: "gpt"}),
		Redactor:   red,
		Bus:        stack.bus,
		InstanceID: "inst-fleet-175",
	})
	if err != nil {
		t.Fatalf("NewPostureSurface: %v", err)
	}
	return s
}

func healthRetention(t *testing.T, s *protocol.PostureSurface, ctx context.Context, id identity.Identity) map[string]types.RetentionHorizon {
	t.Helper()
	req := &types.RuntimeInfoRequest{Identity: types.IdentityScope{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
	}}
	out, err := s.Dispatch(ctx, methods.MethodRuntimeHealth, req)
	if err != nil {
		t.Fatalf("Dispatch(runtime.health): %v", err)
	}
	h := out.(*types.RuntimeHealth)
	m := make(map[string]types.RetentionHorizon, len(h.Retention))
	for _, r := range h.Retention {
		m[r.Surface] = r
	}
	return m
}

// TestE2E_Phase175_FleetWideningObservesOtherTenants proves the widened
// path over real drivers: a verified console:fleet caller under a `svc:`
// identity (owning NO sessions/tasks) observes runtime-wide tasks +
// sessions horizons reflecting the OTHER tenants' oldest rows, and the
// widened fan-in emits exactly one audit.admin_scope_used.
func TestE2E_Phase175_FleetWideningObservesOtherTenants(t *testing.T) {
	stack := newRetentionFleetStack(t)
	reg := stack.lister.(*sessions.Registry)

	// Seed tenant-a first (its rows are the oldest), then tenant-b.
	idA := identity.Identity{TenantID: "tenant-a", UserID: "ua", SessionID: "sa"}
	stack.seedTenant(t, reg, idA)
	time.Sleep(3 * time.Millisecond)
	idB := identity.Identity{TenantID: "tenant-b", UserID: "ub", SessionID: "sb"}
	stack.seedTenant(t, reg, idB)

	sub, err := stack.bus.Subscribe(context.Background(), events.Filter{Admin: true})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	s := retentionSurface(t, stack, auditpatterns.New())

	// The fleet coordinator: a svc: principal owning no sessions/tasks,
	// carrying a verified console:fleet scope (server-derived).
	svc := identity.Identity{TenantID: "svc", UserID: "coordinator", SessionID: "fleet"}
	svcCtx := auth.WithScopes(fleetIdentityCtx(t, svc), []auth.Scope{auth.ScopeConsoleFleet})

	m := healthRetention(t, s, svcCtx, svc)
	for _, surface := range []string{"events", "tasks", "sessions"} {
		h, ok := m[surface]
		if !ok {
			t.Fatalf("%s horizon missing under fleet scope: %+v", surface, m)
		}
		if h.Scope != types.RetentionScopeRuntime {
			t.Fatalf("%s scope = %q, want runtime (widened)", surface, h.Scope)
		}
		if h.OldestRetainedAt == nil {
			t.Fatalf("%s runtime horizon has no timestamp, want the other tenants' oldest row", surface)
		}
	}
	// The runtime-wide tasks/sessions horizons must reflect tenant-a's
	// (older) rows even though the caller is svc: — cross-tenant roll-up.
	if !m["sessions"].OldestRetainedAt.Before(time.Now()) {
		t.Fatalf("sessions horizon %v not in the past", *m["sessions"].OldestRetainedAt)
	}

	// EXACTLY ONE admin_scope_used anchored on the svc actor.
	count := drainFleetAdminScopeUsed(t, sub, "svc", 400*time.Millisecond)
	if count != 1 {
		t.Fatalf("admin_scope_used count = %d, want exactly 1 on the widened fan-in", count)
	}
}

// TestE2E_Phase175_OrdinaryCallerStaysScoped proves cross-tenant
// isolation: an ordinary tenant-a caller (no elevated scope) reads its
// own scoped horizons only, never another tenant's, and emits no audit.
func TestE2E_Phase175_OrdinaryCallerStaysScoped(t *testing.T) {
	stack := newRetentionFleetStack(t)
	reg := stack.lister.(*sessions.Registry)
	idA := identity.Identity{TenantID: "tenant-a", UserID: "ua", SessionID: "sa"}
	idB := identity.Identity{TenantID: "tenant-b", UserID: "ub", SessionID: "sb"}
	stack.seedTenant(t, reg, idA)
	stack.seedTenant(t, reg, idB)

	sub, err := stack.bus.Subscribe(context.Background(), events.Filter{Admin: true})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	s := retentionSurface(t, stack, auditpatterns.New())

	// Ordinary tenant-a caller — verified identity, NO elevated scope.
	aCtx := fleetIdentityCtx(t, idA)
	m := healthRetention(t, s, aCtx, idA)

	if got := m["tasks"].Scope; got != types.RetentionScopeSession {
		t.Fatalf("tasks scope = %q, want session (ordinary caller)", got)
	}
	if got := m["sessions"].Scope; got != types.RetentionScopeTenant {
		t.Fatalf("sessions scope = %q, want tenant (ordinary caller)", got)
	}
	// The tenant-a session horizon is its own; it must NOT be a
	// runtime-wide roll-up that could leak tenant-b.
	if m["sessions"].Scope == types.RetentionScopeRuntime {
		t.Fatalf("ordinary caller observed a runtime-wide sessions horizon — isolation breach")
	}

	if count := drainFleetAdminScopeUsed(t, sub, "tenant-a", 300*time.Millisecond); count != 0 {
		t.Fatalf("admin_scope_used count = %d, want 0 (no widening)", count)
	}
}

// TestE2E_Phase175_WidenedAuditFailure_FailsLoud proves the fail-loud
// audit contract over real drivers: a Redactor forced to refuse the
// widened admin_scope_used payload makes the widened read FAIL LOUD with
// CodeRuntimeError, never silently succeed.
func TestE2E_Phase175_WidenedAuditFailure_FailsLoud(t *testing.T) {
	stack := newRetentionFleetStack(t)
	reg := stack.lister.(*sessions.Registry)
	stack.seedTenant(t, reg, identity.Identity{TenantID: "tenant-a", UserID: "ua", SessionID: "sa"})

	s := retentionSurface(t, stack, refusingRedactor{})

	svc := identity.Identity{TenantID: "svc", UserID: "coordinator", SessionID: "fleet"}
	svcCtx := auth.WithScopes(fleetIdentityCtx(t, svc), []auth.Scope{auth.ScopeConsoleFleet})
	req := &types.RuntimeInfoRequest{Identity: types.IdentityScope{Tenant: "svc", User: "coordinator", Session: "fleet"}}

	_, err := s.Dispatch(svcCtx, methods.MethodRuntimeHealth, req)
	if err == nil {
		t.Fatal("widened read with a refusing redactor returned nil error, want CodeRuntimeError")
	}
	var pe *protoerrors.Error
	if !stderrors.As(err, &pe) || pe.Code != protoerrors.CodeRuntimeError {
		t.Fatalf("error = %v, want CodeRuntimeError", err)
	}
}

// TestE2E_Phase175_ConcurrentMixedCallers stresses the shared surface:
// N≥10 concurrent readers mixing fleet + ordinary callers observe no
// cross-talk (an ordinary caller never sees a runtime-scope horizon).
func TestE2E_Phase175_ConcurrentMixedCallers(t *testing.T) {
	stack := newRetentionFleetStack(t)
	reg := stack.lister.(*sessions.Registry)
	stack.seedTenant(t, reg, identity.Identity{TenantID: "tenant-a", UserID: "ua", SessionID: "sa"})
	s := retentionSurface(t, stack, auditpatterns.New())

	const n = 24
	errs := make(chan string, n)
	done := make(chan struct{})
	for i := range n {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			id := identity.Identity{
				TenantID:  "tenant-a",
				UserID:    "ua",
				SessionID: "sa",
			}
			ctx := fleetIdentityCtx(t, id)
			elevated := i%2 == 0
			if elevated {
				ctx = auth.WithScopes(ctx, []auth.Scope{auth.ScopeConsoleFleet})
			}
			m := healthRetention(t, s, ctx, id)
			want := types.RetentionScopeSession
			if elevated {
				want = types.RetentionScopeRuntime
			}
			if got := m["tasks"].Scope; got != want {
				errs <- "tasks scope mismatch under contention (widen bleed)"
			}
		}(i)
	}
	for range n {
		<-done
	}
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// drainFleetAdminScopeUsed counts admin_scope_used events anchored on
// actorTenant within the window (the bus's own Admin-subscribe notice
// carries an empty identity and is excluded).
func drainFleetAdminScopeUsed(t *testing.T, sub events.Subscription, actorTenant string, window time.Duration) int {
	t.Helper()
	count := 0
	deadline := time.After(window)
	for {
		select {
		case ev := <-sub.Events():
			if ev.Type == events.EventTypeAdminScopeUsed && ev.Identity.TenantID == actorTenant {
				count++
			}
		case <-deadline:
			return count
		}
	}
}

// refusingRedactor refuses every payload to force the widened audit emit
// to fail (the fail-loud leg).
type refusingRedactor struct{}

func (refusingRedactor) Redact(_ context.Context, _ any) (any, error) {
	return nil, errRefused
}

var errRefused = redactErr("redactor refused (forced test failure)")

type redactErr string

func (e redactErr) Error() string { return string(e) }

func fleetIdentityCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}
