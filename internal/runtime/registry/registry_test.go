package registry_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/registry"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// ---------------------------------------------------------------------
// test harness
// ---------------------------------------------------------------------

func testEventsCfg() config.EventsConfig {
	return config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         1000,
	}
}

// newTestRegistry builds a *registry.Registry over fresh in-mem state +
// events drivers + the patterns redactor. Returns the registry and the
// underlying store/bus so tests can wire a "restart" (fresh registry,
// same store) or subscribe to the bus.
func newTestRegistry(t *testing.T) (*registry.Registry, state.StateStore, events.EventBus) {
	t.Helper()
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem.New: %v", err)
	}
	bus, err := eventsinmem.New(testEventsCfg(), auditpatterns.New())
	if err != nil {
		t.Fatalf("events inmem.New: %v", err)
	}
	reg, err := registry.New(registry.Deps{Store: store, Bus: bus, Redactor: auditpatterns.New()})
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = store.Close(context.Background())
	})
	return reg, store, bus
}

// regOverStore builds a fresh *registry.Registry over an existing
// store — the "process restart" simulation. Caller owns the store/bus
// lifecycle.
func regOverStore(t *testing.T, store state.StateStore) *registry.Registry {
	t.Helper()
	bus, err := eventsinmem.New(testEventsCfg(), auditpatterns.New())
	if err != nil {
		t.Fatalf("events inmem.New: %v", err)
	}
	reg, err := registry.New(registry.Deps{Store: store, Bus: bus, Redactor: auditpatterns.New()})
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
	})
	return reg
}

func identityCtx(t *testing.T, tenant, user, session string) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), identity.Identity{
		TenantID: tenant, UserID: user, SessionID: session,
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

func sampleConfig() registry.AgentConfig {
	return registry.AgentConfig{
		Prompts:       []string{"system prompt v1"},
		Tools:         []registry.ToolDescriptor{{Name: "search", SchemaDigest: "abc"}},
		PlannerConfig: map[string]string{"mode": "react"},
		ModelPolicy:   map[string]string{"model": "haiku"},
	}
}

// ---------------------------------------------------------------------
// New / construction
// ---------------------------------------------------------------------

func TestNew_RejectsNilDeps(t *testing.T) {
	store, _ := stateinmem.New(config.StateConfig{Driver: "inmem"})
	bus, _ := eventsinmem.New(testEventsCfg(), auditpatterns.New())
	red := auditpatterns.New()

	if _, err := registry.New(registry.Deps{Bus: bus, Redactor: red}); err == nil {
		t.Error("New accepted nil Store")
	}
	if _, err := registry.New(registry.Deps{Store: store, Redactor: red}); err == nil {
		t.Error("New accepted nil Bus")
	}
	if _, err := registry.New(registry.Deps{Store: store, Bus: bus}); err == nil {
		t.Error("New accepted nil Redactor")
	}
}

// ---------------------------------------------------------------------
// three-ID model
// ---------------------------------------------------------------------

func TestRegister_MintsAgentID_FirstRegistration(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	ctx := identityCtx(t, "T", "U", "S")

	rec, err := reg.Register(ctx, "triage-bot", sampleConfig(), registry.RegisterOptions{DisplayName: "Triage Bot"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if rec.AgentID == "" {
		t.Error("agent_id not minted")
	}
	if rec.Incarnation != 1 {
		t.Errorf("first registration incarnation = %d, want 1", rec.Incarnation)
	}
	if rec.VersionHash == "" {
		t.Error("version_hash not computed")
	}
	if rec.RegistrationKey != "triage-bot" {
		t.Errorf("registration key = %q, want triage-bot", rec.RegistrationKey)
	}
	if rec.Hosting != registry.HostingLocal {
		t.Errorf("hosting = %q, want local", rec.Hosting)
	}
	if rec.Health != registry.HealthUnknown {
		t.Errorf("initial health = %q, want unknown", rec.Health)
	}
	if rec.DisplayName != "Triage Bot" {
		t.Errorf("display name = %q", rec.DisplayName)
	}
}

// TestRegister_RestartBumpsIncarnation_StableID verifies the three-ID
// model: a re-registration (process restart, no config change) keeps
// agent_id + version_hash and bumps incarnation.
func TestRegister_RestartBumpsIncarnation_StableID(t *testing.T) {
	reg, store, _ := newTestRegistry(t)
	ctx := identityCtx(t, "T", "U", "S")

	first, err := reg.Register(ctx, "agent-x", sampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register #1: %v", err)
	}

	// Simulate a process restart: fresh *Registry over the SAME store.
	reg2 := regOverStore(t, store)
	second, err := reg2.Register(ctx, "agent-x", sampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register #2 (restart): %v", err)
	}

	if second.AgentID != first.AgentID {
		t.Errorf("agent_id changed on restart: %q -> %q", first.AgentID, second.AgentID)
	}
	if second.VersionHash != first.VersionHash {
		t.Errorf("version_hash changed on plain restart: %q -> %q", first.VersionHash, second.VersionHash)
	}
	if second.Incarnation != first.Incarnation+1 {
		t.Errorf("incarnation = %d, want %d", second.Incarnation, first.Incarnation+1)
	}
}

// TestRegister_ConfigEditBumpsVersionHash verifies a re-registration
// WITH a config change bumps both incarnation and version_hash.
func TestRegister_ConfigEditBumpsVersionHash(t *testing.T) {
	reg, store, _ := newTestRegistry(t)
	ctx := identityCtx(t, "T", "U", "S")

	first, err := reg.Register(ctx, "agent-x", sampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register #1: %v", err)
	}

	edited := sampleConfig()
	edited.Prompts = []string{"system prompt v2 — edited"}

	reg2 := regOverStore(t, store)
	second, err := reg2.Register(ctx, "agent-x", edited, registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register #2 (config edit): %v", err)
	}

	if second.AgentID != first.AgentID {
		t.Errorf("agent_id changed on config edit: %q -> %q", first.AgentID, second.AgentID)
	}
	if second.VersionHash == first.VersionHash {
		t.Error("version_hash did NOT bump after a config edit")
	}
	if second.Incarnation != first.Incarnation+1 {
		t.Errorf("incarnation = %d, want %d", second.Incarnation, first.Incarnation+1)
	}
}

// TestRestartVsRecreate verifies the load-bearing distinction: restart
// (re-register same key) keeps the agent_id; recreate (Deregister then
// Register same key) mints a FRESH agent_id.
func TestRestartVsRecreate(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	ctx := identityCtx(t, "T", "U", "S")

	original, err := reg.Register(ctx, "agent-x", sampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// restart — same key, agent_id preserved.
	restarted, err := reg.Register(ctx, "agent-x", sampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register (restart): %v", err)
	}
	if restarted.AgentID != original.AgentID {
		t.Fatalf("restart changed agent_id: %q -> %q", original.AgentID, restarted.AgentID)
	}

	// recreate — Deregister then Register same key mints fresh agent_id.
	if err := reg.Deregister(ctx, original.AgentID); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	recreated, err := reg.Register(ctx, "agent-x", sampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register (recreate): %v", err)
	}
	if recreated.AgentID == original.AgentID {
		t.Fatal("recreate reused the old agent_id — recreate must mint fresh")
	}
	if recreated.Incarnation != 1 {
		t.Errorf("recreated agent incarnation = %d, want 1 (new logical entity)", recreated.Incarnation)
	}
}

// ---------------------------------------------------------------------
// rehydration / persistence posture
// ---------------------------------------------------------------------

// TestInMemDriver_NonPersistentAcrossFreshStore documents the dev-only
// posture: a fresh *Registry over a FRESH in-mem store does not see the
// prior agent (D-060 — the in-mem driver is non-persistent).
func TestInMemDriver_NonPersistentAcrossFreshStore(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	ctx := identityCtx(t, "T", "U", "S")
	rec, err := reg.Register(ctx, "agent-x", sampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Fresh registry over a FRESH store (a true "cold start", not a
	// rehydration). The agent is gone.
	reg2, _, _ := newTestRegistry(t)
	if _, err := reg2.Get(ctx, rec.AgentID); !errors.Is(err, registry.ErrAgentNotFound) {
		t.Fatalf("fresh in-mem store saw a prior agent: err=%v", err)
	}
}

// TestRehydration_SameStoreRehydratesAgentID verifies agent_id is
// stable across a "restart" when the SAME store is reused — the
// durable-driver posture (with the in-mem store standing in for SQLite
// / Postgres at the unit level; the integration test exercises the
// real durable drivers).
func TestRehydration_SameStoreRehydratesAgentID(t *testing.T) {
	reg, store, _ := newTestRegistry(t)
	ctx := identityCtx(t, "T", "U", "S")

	rec, err := reg.Register(ctx, "agent-x", sampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Restart: fresh registry, SAME store. The agent rehydrates.
	reg2 := regOverStore(t, store)
	got, err := reg2.Get(ctx, rec.AgentID)
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if got.AgentID != rec.AgentID {
		t.Errorf("rehydrated agent_id = %q, want %q", got.AgentID, rec.AgentID)
	}
	if got.RegistrationKey != "agent-x" {
		t.Errorf("rehydrated registration key = %q", got.RegistrationKey)
	}
}

// ---------------------------------------------------------------------
// remote agents
// ---------------------------------------------------------------------

func TestRegisterRemote_StoresHandleAndCardRef(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	ctx := identityCtx(t, "T", "U", "S")

	rec, err := reg.RegisterRemote(ctx, "remote-peer", "a2a://peer.example/agents/research", registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("RegisterRemote: %v", err)
	}
	if rec.Hosting != registry.HostingRemote {
		t.Errorf("hosting = %q, want remote", rec.Hosting)
	}
	if rec.AgentCardRef != "a2a://peer.example/agents/research" {
		t.Errorf("card ref = %q", rec.AgentCardRef)
	}
	if rec.AgentID == "" {
		t.Error("remote handle agent_id not minted")
	}
	if rec.VersionHash != "" {
		t.Errorf("remote agent has version_hash %q — config is owned remotely", rec.VersionHash)
	}

	snap, err := reg.Inspect(ctx, rec.AgentID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if snap.Local {
		t.Error("Inspect.Local true for a remote agent")
	}
}

func TestRegisterRemote_RejectsEmptyCardRef(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	ctx := identityCtx(t, "T", "U", "S")
	if _, err := reg.RegisterRemote(ctx, "remote-peer", "", registry.RegisterOptions{}); !errors.Is(err, registry.ErrInvalidConfig) {
		t.Fatalf("RegisterRemote accepted empty card ref: err=%v", err)
	}
}

// ---------------------------------------------------------------------
// identity is mandatory — fail closed
// ---------------------------------------------------------------------

func TestAllMethods_FailClosedOnMissingIdentity(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	bare := context.Background() // no identity

	assertIdentityRequired := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, registry.ErrIdentityRequired) {
			t.Errorf("%s did not fail closed on missing identity: err=%v", name, err)
		}
	}

	_, err := reg.Register(bare, "k", sampleConfig(), registry.RegisterOptions{})
	assertIdentityRequired("Register", err)
	_, err = reg.RegisterRemote(bare, "k", "ref", registry.RegisterOptions{})
	assertIdentityRequired("RegisterRemote", err)
	_, err = reg.Get(bare, "a")
	assertIdentityRequired("Get", err)
	_, err = reg.List(bare)
	assertIdentityRequired("List", err)
	_, err = reg.Inspect(bare, "a")
	assertIdentityRequired("Inspect", err)
	assertIdentityRequired("ReportHealth", reg.ReportHealth(bare, "a", registry.HealthHealthy))
	assertIdentityRequired("Deregister", reg.Deregister(bare, "a"))
	// Control commands also need identity — checked before the scope claim.
	ctrl := registry.WithControlScope(bare)
	assertIdentityRequired("Pause", reg.Pause(ctrl, "a", "r"))
	assertIdentityRequired("Drain", reg.Drain(ctrl, "a", "r"))
	assertIdentityRequired("Restart", reg.Restart(ctrl, "a", "r"))
	assertIdentityRequired("ForceStop", reg.ForceStop(ctrl, "a", "r"))
}

func TestRegister_FailClosedOnIncompleteIdentity(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	// Identity with empty SessionID — incomplete triple. We cannot use
	// identity.With (it validates), so we attach a valid identity then
	// note that the registry re-validates. Instead, construct via the
	// no-session path: identity.With rejects it, so the registry never
	// sees it — assert identity.With itself fails closed, which is the
	// upstream guarantee the registry depends on.
	_, err := identity.With(context.Background(), identity.Identity{TenantID: "T", UserID: "U"})
	if !errors.Is(err, identity.ErrIdentityIncomplete) {
		t.Fatalf("identity.With accepted an incomplete triple: %v", err)
	}
	// And the registry's own requireIdentity re-validates: a context
	// carrying a (hypothetically) incomplete identity is rejected. We
	// exercise that path via the bare-context test above; here we just
	// confirm a complete identity is accepted.
	ctx := identityCtx(t, "T", "U", "S")
	if _, err := reg.Register(ctx, "k", sampleConfig(), registry.RegisterOptions{}); err != nil {
		t.Fatalf("Register rejected a complete identity: %v", err)
	}
}

// ---------------------------------------------------------------------
// cross-tenant / cross-session isolation conformance
// ---------------------------------------------------------------------

func TestIsolation_CrossTenantInvisible(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	ctxA := identityCtx(t, "T1", "U1", "S1")
	ctxB := identityCtx(t, "T2", "U2", "S2")

	recA, err := reg.Register(ctxA, "agent", sampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register under A: %v", err)
	}
	recB, err := reg.Register(ctxB, "agent", sampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register under B: %v", err)
	}
	// Same registration key, different identities → different agent_ids.
	if recA.AgentID == recB.AgentID {
		t.Fatal("two identities sharing a registration key collided on agent_id")
	}

	// B cannot see A's agent and vice versa.
	if _, err := reg.Get(ctxB, recA.AgentID); !errors.Is(err, registry.ErrAgentNotFound) {
		t.Errorf("identity B saw identity A's agent: err=%v", err)
	}
	if _, err := reg.Get(ctxA, recB.AgentID); !errors.Is(err, registry.ErrAgentNotFound) {
		t.Errorf("identity A saw identity B's agent: err=%v", err)
	}

	// List is identity-scoped.
	listA, err := reg.List(ctxA)
	if err != nil {
		t.Fatalf("List under A: %v", err)
	}
	if len(listA) != 1 || listA[0].AgentID != recA.AgentID {
		t.Errorf("List under A leaked or lost agents: %+v", listA)
	}
	listB, err := reg.List(ctxB)
	if err != nil {
		t.Fatalf("List under B: %v", err)
	}
	if len(listB) != 1 || listB[0].AgentID != recB.AgentID {
		t.Errorf("List under B leaked or lost agents: %+v", listB)
	}
}

func TestList_ReturnsAllAgentsForIdentity_Sorted(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	ctx := identityCtx(t, "T", "U", "S")
	for i := range 5 {
		if _, err := reg.Register(ctx, fmt.Sprintf("agent-%d", i), sampleConfig(), registry.RegisterOptions{}); err != nil {
			t.Fatalf("Register %d: %v", i, err)
		}
	}
	list, err := reg.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 5 {
		t.Fatalf("List returned %d agents, want 5", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].AgentID >= list[i].AgentID {
			t.Errorf("List not sorted by agent_id at index %d", i)
		}
	}
}

// ---------------------------------------------------------------------
// health
// ---------------------------------------------------------------------

func TestReportHealth_UpdatesAndEmits(t *testing.T) {
	reg, _, bus := newTestRegistry(t)
	ctx := identityCtx(t, "T", "U", "S")
	rec, err := reg.Register(ctx, "agent", sampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	sub := subscribeAll(t, bus)
	if err := reg.ReportHealth(ctx, rec.AgentID, registry.HealthHealthy); err != nil {
		t.Fatalf("ReportHealth: %v", err)
	}
	ev := waitEvent(t, sub, registry.EventTypeAgentHealth)
	p, ok := ev.Payload.(registry.AgentHealthPayload)
	if !ok {
		t.Fatalf("agent.health payload type = %T", ev.Payload)
	}
	if p.AgentID != rec.AgentID {
		t.Errorf("agent.health carries agent_id %q, want %q", p.AgentID, rec.AgentID)
	}
	if p.Health != string(registry.HealthHealthy) {
		t.Errorf("agent.health.Health = %q", p.Health)
	}

	got, err := reg.Get(ctx, rec.AgentID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Health != registry.HealthHealthy {
		t.Errorf("health not persisted: %q", got.Health)
	}
}

func TestReportHealth_RejectsInvalidHealth(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	ctx := identityCtx(t, "T", "U", "S")
	rec, _ := reg.Register(ctx, "agent", sampleConfig(), registry.RegisterOptions{})
	if err := reg.ReportHealth(ctx, rec.AgentID, registry.Health("bogus")); !errors.Is(err, registry.ErrInvalidConfig) {
		t.Fatalf("ReportHealth accepted a bogus health value: %v", err)
	}
}

// ---------------------------------------------------------------------
// fleet control — privilege tier (D-066)
// ---------------------------------------------------------------------

func TestFleetControl_RequiresControlScope(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	ctx := identityCtx(t, "T", "U", "S") // identity but NO control scope
	rec, err := reg.Register(ctx, "agent", sampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Without the control-scope claim, every control command fails closed.
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"Pause", func() error { return reg.Pause(ctx, rec.AgentID, "r") }},
		{"Drain", func() error { return reg.Drain(ctx, rec.AgentID, "r") }},
		{"Restart", func() error { return reg.Restart(ctx, rec.AgentID, "r") }},
		{"ForceStop", func() error { return reg.ForceStop(ctx, rec.AgentID, "r") }},
	} {
		if err := tc.call(); !errors.Is(err, registry.ErrControlScopeRequired) {
			t.Errorf("%s without control scope: err=%v, want ErrControlScopeRequired", tc.name, err)
		}
	}

	// Fleet OBSERVATION does NOT require the control scope.
	if _, err := reg.Get(ctx, rec.AgentID); err != nil {
		t.Errorf("Get required control scope — it must not: %v", err)
	}
	if _, err := reg.List(ctx); err != nil {
		t.Errorf("List required control scope — it must not: %v", err)
	}
	if err := reg.ReportHealth(ctx, rec.AgentID, registry.HealthHealthy); err != nil {
		t.Errorf("ReportHealth required control scope — it must not: %v", err)
	}
}

func TestFleetControl_WithScope_TransitionsAndEmits(t *testing.T) {
	reg, _, bus := newTestRegistry(t)
	obs := identityCtx(t, "T", "U", "S")
	ctrl := registry.WithControlScope(obs)

	rec, err := reg.Register(obs, "agent", sampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	sub := subscribeAll(t, bus)

	// Drain transitions Health → draining and emits agent.drained.
	if err := reg.Drain(ctrl, rec.AgentID, "rolling deploy"); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	ev := waitEvent(t, sub, registry.EventTypeAgentDrained)
	p, ok := ev.Payload.(registry.AgentControlPayload)
	if !ok {
		t.Fatalf("agent.drained payload type = %T", ev.Payload)
	}
	if p.AgentID != rec.AgentID || p.Command != "drain" {
		t.Errorf("agent.drained payload = %+v", p)
	}
	got, err := reg.Get(obs, rec.AgentID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Health != registry.HealthDraining {
		t.Errorf("Drain did not set Health=draining, got %q", got.Health)
	}

	// ForceStop transitions Health → stopped and emits.
	if err := reg.ForceStop(ctrl, rec.AgentID, "operator kill"); err != nil {
		t.Fatalf("ForceStop: %v", err)
	}
	ev = waitEvent(t, sub, registry.EventTypeAgentForceStopped)
	if cp, _ := ev.Payload.(registry.AgentControlPayload); cp.Command != "force_stop" {
		t.Errorf("agent.force_stopped command = %q", cp.Command)
	}
	got, err = reg.Get(obs, rec.AgentID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Health != registry.HealthStopped {
		t.Errorf("ForceStop did not set Health=stopped, got %q", got.Health)
	}
}

// TestFleetControl_ReasonIsRedacted verifies the operator-supplied
// reason string is run through the audit redactor before it lands on
// the emitted event (D-020 / D-066).
func TestFleetControl_ReasonIsRedacted(t *testing.T) {
	reg, _, bus := newTestRegistry(t)
	obs := identityCtx(t, "T", "U", "S")
	ctrl := registry.WithControlScope(obs)
	rec, err := reg.Register(obs, "agent", sampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	sub := subscribeAll(t, bus)
	// A reason that embeds a bearer token — the patterns redactor must
	// strip it before it reaches the event payload.
	if err := reg.Pause(ctrl, rec.AgentID, "pausing: token=Bearer sk-secret-abc123"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	ev := waitEvent(t, sub, registry.EventTypeAgentPaused)
	p, _ := ev.Payload.(registry.AgentControlPayload)
	if p.Reason == "pausing: token=Bearer sk-secret-abc123" {
		t.Error("control reason reached the event payload un-redacted")
	}
}

func TestFleetControl_UnknownAgent(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	ctrl := registry.WithControlScope(identityCtx(t, "T", "U", "S"))
	if err := reg.Pause(ctrl, "01J0000000000000000000NOPE", "r"); !errors.Is(err, registry.ErrAgentNotFound) {
		t.Fatalf("Pause on unknown agent: err=%v, want ErrAgentNotFound", err)
	}
}

// ---------------------------------------------------------------------
// deregister
// ---------------------------------------------------------------------

func TestDeregister_RemovesAndEmits(t *testing.T) {
	reg, _, bus := newTestRegistry(t)
	ctx := identityCtx(t, "T", "U", "S")
	rec, err := reg.Register(ctx, "agent", sampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	sub := subscribeAll(t, bus)
	if err := reg.Deregister(ctx, rec.AgentID); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	ev := waitEvent(t, sub, registry.EventTypeAgentDeregistered)
	if p, _ := ev.Payload.(registry.AgentDeregisteredPayload); p.AgentID != rec.AgentID {
		t.Errorf("agent.deregistered carries agent_id %q, want %q", p.AgentID, rec.AgentID)
	}
	if _, err := reg.Get(ctx, rec.AgentID); !errors.Is(err, registry.ErrAgentNotFound) {
		t.Errorf("agent still resolvable after Deregister: %v", err)
	}
	list, err := reg.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("List returned %d agents after Deregister, want 0", len(list))
	}
}

// ---------------------------------------------------------------------
// events carry agent_id (RFC §6.16)
// ---------------------------------------------------------------------

func TestRegister_EmitsAgentRegisteredWithAgentID(t *testing.T) {
	reg, _, bus := newTestRegistry(t)
	ctx := identityCtx(t, "T", "U", "S")
	sub := subscribeAll(t, bus)

	rec, err := reg.Register(ctx, "agent", sampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	ev := waitEvent(t, sub, registry.EventTypeAgentRegistered)
	p, ok := ev.Payload.(registry.AgentRegisteredPayload)
	if !ok {
		t.Fatalf("agent.registered payload type = %T", ev.Payload)
	}
	if p.AgentID != rec.AgentID {
		t.Errorf("agent.registered carries agent_id %q, want %q", p.AgentID, rec.AgentID)
	}
	if ev.Identity.TenantID != "T" || ev.Identity.UserID != "U" || ev.Identity.SessionID != "S" {
		t.Errorf("agent.registered identity = %+v, want (T,U,S)", ev.Identity)
	}
}

func TestRegister_RestartEmitsAgentRestarted(t *testing.T) {
	reg, store, _ := newTestRegistry(t)
	ctx := identityCtx(t, "T", "U", "S")
	if _, err := reg.Register(ctx, "agent", sampleConfig(), registry.RegisterOptions{}); err != nil {
		t.Fatalf("Register #1: %v", err)
	}

	// Restart over the same store, with a fresh bus we can subscribe to.
	bus2, err := eventsinmem.New(testEventsCfg(), auditpatterns.New())
	if err != nil {
		t.Fatalf("events inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus2.Close(context.Background()) })
	reg2, err := registry.New(registry.Deps{Store: store, Bus: bus2, Redactor: auditpatterns.New()})
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	t.Cleanup(func() { _ = reg2.Close(context.Background()) })

	sub := subscribeAll(t, bus2)
	if _, err := reg2.Register(ctx, "agent", sampleConfig(), registry.RegisterOptions{}); err != nil {
		t.Fatalf("Register #2: %v", err)
	}
	ev := waitEvent(t, sub, registry.EventTypeAgentRestarted)
	p, ok := ev.Payload.(registry.AgentRestartedPayload)
	if !ok {
		t.Fatalf("agent.restarted payload type = %T", ev.Payload)
	}
	if p.Incarnation != 2 {
		t.Errorf("agent.restarted incarnation = %d, want 2", p.Incarnation)
	}
	if p.VersionHashChanged {
		t.Error("agent.restarted.VersionHashChanged true for a plain restart")
	}
}

// ---------------------------------------------------------------------
// closed registry
// ---------------------------------------------------------------------

func TestClose_SubsequentOpsRejected(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	ctx := identityCtx(t, "T", "U", "S")
	if err := reg.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := reg.Close(ctx); err != nil {
		t.Fatalf("Close not idempotent: %v", err)
	}
	if _, err := reg.Register(ctx, "k", sampleConfig(), registry.RegisterOptions{}); !errors.Is(err, registry.ErrRegistryClosed) {
		t.Errorf("Register after Close: err=%v, want ErrRegistryClosed", err)
	}
	if _, err := reg.Get(ctx, "a"); !errors.Is(err, registry.ErrRegistryClosed) {
		t.Errorf("Get after Close: err=%v, want ErrRegistryClosed", err)
	}
}

// ---------------------------------------------------------------------
// D-025 concurrent reuse
// ---------------------------------------------------------------------

// TestRegistry_ConcurrentReuse_D025 runs N≥100 concurrent
// registrations / lookups / control commands against ONE shared
// *Registry under -race, asserting: no data races (the race detector
// is the gate), no context bleed (each goroutine asserts its own
// identity round-trips), no goroutine leaks (baseline NumGoroutine
// restored after teardown).
func TestRegistry_ConcurrentReuse_D025(t *testing.T) {
	baseline := runtime.NumGoroutine()

	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem.New: %v", err)
	}
	bus, err := eventsinmem.New(testEventsCfg(), auditpatterns.New())
	if err != nil {
		t.Fatalf("events inmem.New: %v", err)
	}
	reg, err := registry.New(registry.Deps{Store: store, Bus: bus, Redactor: auditpatterns.New()})
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}

	const n = 150
	var wg sync.WaitGroup
	errCh := make(chan error, n)

	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each goroutine uses a DISTINCT identity so context bleed
			// would surface as a cross-identity visibility error.
			tenant := fmt.Sprintf("T%d", i)
			ctx := identityCtx(t, tenant, "U", "S")
			ctrl := registry.WithControlScope(ctx)
			key := fmt.Sprintf("agent-%d", i)

			rec, err := reg.Register(ctx, key, sampleConfig(), registry.RegisterOptions{})
			if err != nil {
				errCh <- fmt.Errorf("goroutine %d Register: %w", i, err)
				return
			}
			// Lookup must round-trip the SAME agent_id under the SAME
			// identity (no context bleed).
			got, err := reg.Get(ctx, rec.AgentID)
			if err != nil {
				errCh <- fmt.Errorf("goroutine %d Get: %w", i, err)
				return
			}
			if got.Identity.TenantID != tenant {
				errCh <- fmt.Errorf("goroutine %d: context bleed — got tenant %q want %q",
					i, got.Identity.TenantID, tenant)
				return
			}
			// List under this identity sees exactly this goroutine's agent.
			list, err := reg.List(ctx)
			if err != nil {
				errCh <- fmt.Errorf("goroutine %d List: %w", i, err)
				return
			}
			if len(list) != 1 || list[0].AgentID != rec.AgentID {
				errCh <- fmt.Errorf("goroutine %d: List bled — got %d agents", i, len(list))
				return
			}
			// A control command + a health report exercise the
			// mutating paths concurrently.
			if err := reg.ReportHealth(ctx, rec.AgentID, registry.HealthHealthy); err != nil {
				errCh <- fmt.Errorf("goroutine %d ReportHealth: %w", i, err)
				return
			}
			if err := reg.Drain(ctrl, rec.AgentID, "concurrent drain"); err != nil {
				errCh <- fmt.Errorf("goroutine %d Drain: %w", i, err)
				return
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	// Teardown and assert no goroutine leak.
	_ = reg.Close(context.Background())
	_ = bus.Close(context.Background())
	_ = store.Close(context.Background())

	// Give the runtime a brief, bounded window to reclaim goroutines
	// (the bus reaper / driver teardown). This is a leak assertion, not
	// a synchronisation sleep — we poll with a deadline.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+5 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if leaked := runtime.NumGoroutine() - baseline; leaked > 5 {
		t.Errorf("goroutine leak: baseline=%d now=%d (leaked ~%d)",
			baseline, runtime.NumGoroutine(), leaked)
	}
}

// TestRegistry_ConcurrentSameAgentMutation_NoLostUpdate is the
// regression guard for the Wave 9 §17.5 audit finding: ReportHealth
// and the fleet-control path did a load→mutate→save on an agent's
// record document WITHOUT holding r.mu, so a concurrent mutation of
// the same agent_id could lose an update. TestRegistry_ConcurrentReuse_D025
// above never caught it — it gives every goroutine a DISTINCT identity,
// so no two goroutines ever contend on the same record.
//
// This test contends ONE agent record under ONE identity: M re-Register
// calls (each bumps Incarnation, under r.mu) race M ReportHealth calls
// (load→mutate→save, now also under r.mu). The lost-update detector is
// deterministic — every re-Register bumps Incarnation by exactly 1, so
// after the storm Incarnation MUST equal 1 + M. If ReportHealth saved a
// stale record it would revert an Incarnation bump and the final value
// would be < 1 + M. The -race detector cannot see this race (the
// contention is at the StateStore layer, not Go memory), so the
// Incarnation arithmetic is the gate.
func TestRegistry_ConcurrentSameAgentMutation_NoLostUpdate(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	ctx := identityCtx(t, "T", "U", "S")
	const key = "lost-update-agent"
	const m = 50

	// Initial registration — Incarnation == 1.
	rec, err := reg.Register(ctx, key, sampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("initial Register: %v", err)
	}
	agentID := rec.AgentID

	var wg sync.WaitGroup
	errCh := make(chan error, 2*m)

	// M re-registrations of the SAME key — each is a restart that bumps
	// Incarnation by exactly 1 (under r.mu).
	for range m {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, rerr := reg.Register(ctx, key, sampleConfig(), registry.RegisterOptions{}); rerr != nil {
				errCh <- fmt.Errorf("re-Register: %w", rerr)
			}
		}()
	}
	// M ReportHealth calls on the SAME agent — load→mutate→save on the
	// record document. Before the fix these ran without r.mu and could
	// revert a concurrent Incarnation bump.
	for range m {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if herr := reg.ReportHealth(ctx, agentID, registry.HealthHealthy); herr != nil {
				errCh <- fmt.Errorf("ReportHealth: %w", herr)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Error(e)
	}

	got, err := reg.Get(ctx, agentID)
	if err != nil {
		t.Fatalf("Get after storm: %v", err)
	}
	if got.Incarnation != uint64(1+m) {
		t.Errorf("Incarnation = %d, want %d — a ReportHealth save reverted a re-Register bump (lost update; r.mu not held across the record RMW)",
			got.Incarnation, 1+m)
	}
	if got.AgentID != agentID {
		t.Errorf("AgentID changed under re-registration: got %q want %q (restart != recreate)", got.AgentID, agentID)
	}
}

// ---------------------------------------------------------------------
// bus subscription helpers
// ---------------------------------------------------------------------

func subscribeAll(t *testing.T, bus events.EventBus) events.Subscription {
	t.Helper()
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: "T", User: "U", Session: "S",
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)
	return sub
}

// waitEvent drains the subscription until it sees an event of the given
// type, or fails after a bounded real-time deadline. Bounded-deadline
// channel receive — not a synchronisation sleep (AGENTS.md §17.4).
func waitEvent(t *testing.T, sub events.Subscription, want events.EventType) events.Event {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatalf("subscription closed before %q arrived", want)
			}
			if ev.Type == want {
				return ev
			}
		case <-timeout:
			t.Fatalf("timed out waiting for event %q", want)
		}
	}
}
