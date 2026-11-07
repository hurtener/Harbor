package statestore_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// hintBus records every published event and fails the test on an invalid
// identity, so the tools.catalog.changed contract "never publish an invalid
// identity" is asserted at the bus boundary.
type hintBus struct {
	events.EventBus
	t      *testing.T
	mu     sync.Mutex
	hints  []events.Event
	others int
}

func (b *hintBus) Publish(ctx context.Context, ev events.Event) error {
	if err := identity.Validate(ev.Identity.Identity); err != nil {
		b.t.Errorf("event %s published with invalid identity %+v: %v", ev.Type, ev.Identity, err)
	}
	b.mu.Lock()
	if ev.Type == agentcfg.EventTypeToolsCatalogChanged {
		b.hints = append(b.hints, ev)
	} else {
		b.others++
	}
	b.mu.Unlock()
	return b.EventBus.Publish(ctx, ev)
}

func (b *hintBus) take() []agentcfg.ToolsCatalogChangedPayload {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]agentcfg.ToolsCatalogChangedPayload, 0, len(b.hints))
	for _, ev := range b.hints {
		out = append(out, ev.Payload.(agentcfg.ToolsCatalogChangedPayload))
	}
	b.hints = nil
	return out
}

func newHintRegistry(t *testing.T) (agentcfg.Registry, state.StateStore, *hintBus) {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	inner, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64,
		IdleTimeout: time.Minute, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatal(err)
	}
	bus := &hintBus{EventBus: inner, t: t}
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = inner.Close(context.Background())
		_ = st.Close(context.Background())
	})
	return reg, st, bus
}

func expectHint(t *testing.T, bus *hintBus, what, scope, reason string) {
	const agentID = "agent"
	t.Helper()
	hints := bus.take()
	if len(hints) != 1 {
		t.Fatalf("%s: hints=%+v, want exactly one", what, hints)
	}
	if got := hints[0]; got.AgentID != agentID || got.Scope != scope || got.Reason != reason {
		t.Fatalf("%s: hint=%+v, want agent=%s scope=%s reason=%s", what, got, agentID, scope, reason)
	}
}

// TestCatalogInputs_ReportsWriteIdentities pins the stamp's durable input:
// absent slots are empty, and every pointer / fence write produces a new
// write identity.
func TestCatalogInputs_ReportsWriteIdentities(t *testing.T) {
	ctx := context.Background()
	reg, st, _ := newHintRegistry(t)
	reader, ok := reg.(agentcfg.CatalogInputReader)
	if !ok {
		t.Fatal("statestore registry does not implement CatalogInputReader")
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	empty, err := reader.CatalogInputs(ctx, q, "agent")
	if err != nil || empty.AgentActive != "" || empty.UserActive != "" || len(empty.Fences) != 0 {
		t.Fatalf("empty inputs=%+v err=%v", empty, err)
	}
	if _, err := reg.SetRevision(ctx, q, "agent", agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.SetRevision(ctx, q, "agent", agentcfg.ConfigScopeUser, agentcfg.ConfigPayload{ToolExposure: &agentcfg.ToolExposure{DisabledTools: []string{"x"}}}, agentcfg.SetOptions{}); err != nil {
		t.Fatal(err)
	}
	fences, err := agentcfg.NewSignedOAuthMCPActivationFenceStore(st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fences.Begin(ctx, "t", "agent", "op", "fp", "hash", ""); err != nil {
		t.Fatal(err)
	}
	full, err := reader.CatalogInputs(ctx, q, "agent")
	if err != nil || full.AgentActive == "" || full.UserActive == "" || len(full.Fences) != 1 {
		t.Fatalf("full inputs=%+v err=%v", full, err)
	}
	if _, err := reader.CatalogInputs(ctx, identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: agentcfg.ReservedAgentConfigUser, SessionID: "s"}}, "agent"); err == nil {
		t.Fatal("reserved user must be refused like the user-tier read")
	}
}

// TestToolsCatalogChanged_RegistryPointerWrites is the H3 AC9 (a) half: every
// committed pointer / lifecycle write publishes exactly one hint with a valid
// identity; an idempotent re-set publishes none.
func TestToolsCatalogChanged_RegistryPointerWrites(t *testing.T) {
	ctx := context.Background()
	reg, st, bus := newHintRegistry(t)
	operator := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "operator", SessionID: "op"}}
	user := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}

	first, err := reg.SetRevision(ctx, operator, "agent", agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	expectHint(t, bus, "agent revision", agentcfg.CatalogChangeScopeAgent, agentcfg.CatalogChangeReasonConfigRevision)
	if _, err := reg.SetRevision(ctx, operator, "agent", agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{}); err != nil {
		t.Fatal(err)
	}
	if hints := bus.take(); len(hints) != 0 {
		t.Fatalf("idempotent re-set published %+v", hints)
	}
	second, err := reg.SetRevision(ctx, operator, "agent", agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{ToolExposure: &agentcfg.ToolExposure{DisabledTools: []string{"x"}}}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	expectHint(t, bus, "agent second revision", agentcfg.CatalogChangeScopeAgent, agentcfg.CatalogChangeReasonConfigRevision)
	if _, err := reg.Rollback(ctx, operator, "agent", first.RevisionID, agentcfg.ConfigScopeAgent, agentcfg.SetOptions{}); err != nil {
		t.Fatal(err)
	}
	expectHint(t, bus, "rollback", agentcfg.CatalogChangeScopeAgent, agentcfg.CatalogChangeReasonConfigRevision)
	urev, err := reg.SetRevision(ctx, user, "agent", agentcfg.ConfigScopeUser, agentcfg.ConfigPayload{ToolExposure: &agentcfg.ToolExposure{DisabledTools: []string{"y"}}}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	expectHint(t, bus, "user revision", agentcfg.CatalogChangeScopeUser, agentcfg.CatalogChangeReasonConfigRevision)
	if changed, err := reg.DeactivateIfActive(ctx, user, "agent", urev.RevisionID, agentcfg.ConfigScopeUser); err != nil || !changed {
		t.Fatalf("deactivate changed=%v err=%v", changed, err)
	}
	expectHint(t, bus, "deactivate", agentcfg.CatalogChangeScopeUser, agentcfg.CatalogChangeReasonConfigRevision)
	if changed, _ := reg.DeactivateIfActive(ctx, user, "agent", urev.RevisionID, agentcfg.ConfigScopeUser); changed {
		t.Fatal("second deactivate changed")
	}
	if hints := bus.take(); len(hints) != 0 {
		t.Fatalf("no-op deactivate published %+v", hints)
	}
	_ = second

	// (b) activation-fence transitions through the production observer.
	fences, err := agentcfg.NewSignedOAuthMCPActivationFenceStore(st)
	if err != nil {
		t.Fatal(err)
	}
	fences = fences.WithChangeObserver(agentcfg.CatalogChangeObserverFor(reg))
	fence, err := fences.Begin(ctx, "t", "agent", "op", "fp", "hash", first.RevisionID)
	if err != nil {
		t.Fatal(err)
	}
	expectHint(t, bus, "fence begin", agentcfg.CatalogChangeScopeAgent, agentcfg.CatalogChangeReasonOAuthFence)
	if _, err := fences.Advance(ctx, fence, agentcfg.SignedOAuthMCPFenceAborted, ""); err != nil {
		t.Fatal(err)
	}
	expectHint(t, bus, "fence advance", agentcfg.CatalogChangeScopeAgent, agentcfg.CatalogChangeReasonOAuthFence)
	pending, err := fences.BeginForOperation(ctx, "t", "agent", "op-2", "fp-2", "hash-2", first.RevisionID)
	if err != nil {
		t.Fatal(err)
	}
	bus.take()
	if err := fences.ReleasePending(ctx, pending); err != nil {
		t.Fatal(err)
	}
	expectHint(t, bus, "fence release", agentcfg.CatalogChangeScopeAgent, agentcfg.CatalogChangeReasonOAuthFence)

	// Lifecycle: retirement publishes the lifecycle hint.
	active, _, err := reg.Active(ctx, operator, "agent", agentcfg.ConfigScopeAgent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.(agentcfg.RetirementRegistry).Retire(ctx, operator, "agent", agentcfg.RetirementRequest{OperationID: "retire", ExpectedContentHash: active.ContentHash}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, hint := range bus.take() {
		found = found || hint.Reason == agentcfg.CatalogChangeReasonLifecycle
	}
	if !found {
		t.Fatal("retirement published no lifecycle hint")
	}
}

// TestCatalogChangedEvent_IdentityResolution pins the identity rule: the
// writer's quadruple when complete and same-tenant, else the agent's
// control-plane scope; unresolvable (tenant, agent) is skipped.
func TestCatalogChangedEvent_IdentityResolution(t *testing.T) {
	writer := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	ev, ok := agentcfg.CatalogChangedEvent(writer, "t", "a", agentcfg.CatalogChangeScopeAgent, agentcfg.CatalogChangeReasonConnection, time.Now())
	if !ok || ev.Identity != writer {
		t.Fatalf("same-tenant writer: ok=%v identity=%+v", ok, ev.Identity)
	}
	ev, ok = agentcfg.CatalogChangedEvent(writer, "other", "a", agentcfg.CatalogChangeScopeAgent, agentcfg.CatalogChangeReasonConnection, time.Now())
	want := identity.Identity{TenantID: "other", UserID: agentcfg.ReservedAgentConfigUser, SessionID: "a"}
	if !ok || ev.Identity.Identity != want {
		t.Fatalf("cross-tenant writer: ok=%v identity=%+v", ok, ev.Identity)
	}
	ev, ok = agentcfg.CatalogChangedEvent(identity.Quadruple{}, "t", "a", agentcfg.CatalogChangeScopeAgent, agentcfg.CatalogChangeReasonConnection, time.Now())
	if !ok || ev.Identity.Identity != (identity.Identity{TenantID: "t", UserID: agentcfg.ReservedAgentConfigUser, SessionID: "a"}) {
		t.Fatalf("absent writer: ok=%v identity=%+v", ok, ev.Identity)
	}
	if _, ok := agentcfg.CatalogChangedEvent(writer, "t", "", agentcfg.CatalogChangeScopeAgent, agentcfg.CatalogChangeReasonConnection, time.Now()); ok {
		t.Fatal("empty agent must be skipped")
	}
	if _, ok := agentcfg.CatalogChangedEvent(writer, "", "a", agentcfg.CatalogChangeScopeAgent, agentcfg.CatalogChangeReasonConnection, time.Now()); ok {
		t.Fatal("empty tenant must be skipped")
	}
}
