package harbortest_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/harbortest"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// payloadWithIdentityField is a struct payload with an Identity
// field of type identity.Quadruple — exercising the reflection
// fallback in assertions.go.
type payloadWithIdentityField struct {
	events.Sealed
	Identity identity.Quadruple
	Data     string
}

// payloadWithIdentityTriple is the same shape but the Identity
// field is the bare identity.Identity (not the Quadruple). The
// reflect helper widens it to a zero-RunID Quadruple.
type payloadWithIdentityTriple struct {
	events.Sealed
	Identity identity.Identity
}

// payloadWithIdentityHolder satisfies the explicit
// IdentityQuadruple() identity.Quadruple interface — exercises the
// type-assertion fast path.
type payloadWithIdentityHolder struct {
	events.Sealed
	q identity.Quadruple
}

func (p payloadWithIdentityHolder) IdentityQuadruple() identity.Quadruple {
	return p.q
}

// TestAssertNoLeaks_PayloadCrossTalk_QuadrupleField — payload
// embeds an identity.Quadruple that disagrees with the outer
// event identity; AssertNoLeaks must flag it.
func TestAssertNoLeaks_PayloadCrossTalk_QuadrupleField(t *testing.T) {
	bus := openInmemBus(t)

	idA := identity.Identity{TenantID: "ta", UserID: "u", SessionID: "sa"}
	idB := identity.Identity{TenantID: "tb", UserID: "u", SessionID: "sb"}

	agent := harbortest.AgentFunc(func(ctx context.Context, _ any) (any, error) {
		bus := events.MustFrom(ctx)
		return nil, bus.Publish(ctx, events.Event{
			Type:     events.EventTypeRuntimeError,
			Identity: identity.Quadruple{Identity: idA, RunID: "run-a"},
			Payload: payloadWithIdentityField{
				Identity: identity.Quadruple{Identity: idB, RunID: "run-b"},
				Data:     "leak",
			},
		})
	})
	_, log, err := harbortest.RunOnce(t.Context(), agent, nil, harbortest.Deps{
		Bus:      bus,
		Identity: &idA,
		RunID:    "run-a",
	})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	rt := &recordingT{}
	if harbortest.AssertNoLeaks(rt, log) {
		t.Fatalf("AssertNoLeaks: expected payload-cross-talk failure, log=%+v", log.All())
	}
	if len(rt.failures) == 0 {
		t.Fatal("AssertNoLeaks: did not call t.Errorf")
	}
}

// TestAssertNoLeaks_PayloadIdentityHolder_TypeAssertionPath —
// payloads that implement IdentityQuadruple() exercise the type
// switch in payloadQuadruple before the reflection fallback.
func TestAssertNoLeaks_PayloadIdentityHolder_TypeAssertionPath(t *testing.T) {
	bus := openInmemBus(t)

	idA := identity.Identity{TenantID: "ta", UserID: "u", SessionID: "sa"}
	idB := identity.Identity{TenantID: "tb", UserID: "u", SessionID: "sb"}

	agent := harbortest.AgentFunc(func(ctx context.Context, _ any) (any, error) {
		bus := events.MustFrom(ctx)
		return nil, bus.Publish(ctx, events.Event{
			Type:     events.EventTypeRuntimeError,
			Identity: identity.Quadruple{Identity: idA, RunID: "run-a"},
			Payload: payloadWithIdentityHolder{
				q: identity.Quadruple{Identity: idB, RunID: "run-b"},
			},
		})
	})
	_, log, err := harbortest.RunOnce(t.Context(), agent, nil, harbortest.Deps{
		Bus:      bus,
		Identity: &idA,
		RunID:    "run-a",
	})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	rt := &recordingT{}
	if harbortest.AssertNoLeaks(rt, log) {
		t.Fatalf("AssertNoLeaks: expected type-asserted holder cross-talk to fire; log=%+v", log.All())
	}
}

// TestAssertNoLeaks_PayloadIdentityTripleField — Identity field of
// the bare identity.Identity type triggers the reflect widen path.
func TestAssertNoLeaks_PayloadIdentityTripleField(t *testing.T) {
	bus := openInmemBus(t)

	idA := identity.Identity{TenantID: "ta", UserID: "u", SessionID: "sa"}
	idB := identity.Identity{TenantID: "tb", UserID: "u", SessionID: "sb"}

	agent := harbortest.AgentFunc(func(ctx context.Context, _ any) (any, error) {
		bus := events.MustFrom(ctx)
		return nil, bus.Publish(ctx, events.Event{
			Type:     events.EventTypeRuntimeError,
			Identity: identity.Quadruple{Identity: idA, RunID: "run-a"},
			Payload:  payloadWithIdentityTriple{Identity: idB},
		})
	})
	_, log, err := harbortest.RunOnce(t.Context(), agent, nil, harbortest.Deps{
		Bus:      bus,
		Identity: &idA,
		RunID:    "run-a",
	})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	rt := &recordingT{}
	if harbortest.AssertNoLeaks(rt, log) {
		t.Fatalf("AssertNoLeaks: expected widen-path cross-talk to fire; log=%+v", log.All())
	}
}

// TestRunOnce_RedactorOverride_Honoured — a non-nil Deps.Redactor
// is used when RunOnce opens its own bus. We can't easily inspect
// "is this the same redactor instance?" without internals, but we
// can confirm the path doesn't error.
func TestRunOnce_RedactorOverride_Honoured(t *testing.T) {
	red := stubRedactor{}
	agent := harbortest.AgentFunc(func(_ context.Context, _ any) (any, error) { return nil, nil })
	if _, _, err := harbortest.RunOnce(t.Context(), agent, nil, harbortest.Deps{Redactor: red}); err != nil {
		t.Fatalf("RunOnce w/ explicit redactor: %v", err)
	}
}
