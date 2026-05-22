package generator_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/generator"
)

// TestPromote_HappyPath asserts Promote writes a sibling row under
// each target identity, restamps Scope to the supplied value, and
// emits a `skill.proposed` event (Result="persisted", Promotion=true)
// per target.
func TestPromote_HappyPath(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)

	idA := testIdentity()
	idB := identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  idA.TenantID,
			UserID:    idA.UserID,
			SessionID: "s-target-B",
		},
		RunID: "r-B",
	}

	// Identity A persists a Scope=session skill.
	ctxA, err := identity.WithRun(context.Background(), idA.Identity, idA.RunID)
	if err != nil {
		t.Fatal(err)
	}
	draft := validDraft("promote-target")
	draft.Scope = skills.ScopeSession
	if _, err = generator.Propose(ctxA, store, deps, generator.ProposeArgs{Skill: draft, Persist: true}); err != nil {
		t.Fatalf("Propose under A: %v", err)
	}

	// Identity B initially sees nothing.
	ctxB, err := identity.WithRun(context.Background(), idB.Identity, idB.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Get(ctxB, idB, "promote-target"); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("pre-promote Get under B: got %v, want ErrSkillNotFound (cross-session no-leak invariant)", err)
	}

	// Promote to B's session at Scope=project.
	bDrain := collectProposedEvents(t, bus, idB)
	if err = generator.Promote(ctxA, store, deps, idA, "promote-target",
		[]identity.Quadruple{idB}, skills.ScopeProject); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	// Identity B now sees the row.
	got, err := store.Get(ctxB, idB, "promote-target")
	if err != nil {
		t.Fatalf("post-promote Get under B: %v", err)
	}
	if got.Scope != skills.ScopeProject {
		t.Fatalf("post-promote Scope=%q want %q", got.Scope, skills.ScopeProject)
	}
	wantOriginRef := "gen:" + idA.SessionID + ":" + idA.RunID
	if got.OriginRef != wantOriginRef {
		t.Fatalf("post-promote OriginRef=%q want %q (preserved from source)", got.OriginRef, wantOriginRef)
	}

	// One skill.proposed event landed under B's identity with
	// Promotion=true.
	evs := bDrain()
	if len(evs) != 1 {
		t.Fatalf("got %d skill.proposed events under B, want 1", len(evs))
	}
	payload, ok := evs[0].Payload.(generator.SkillProposedPayload)
	if !ok {
		t.Fatalf("payload type=%T", evs[0].Payload)
	}
	if !payload.Promotion {
		t.Fatalf("payload.Promotion=false, want true")
	}
	if payload.Result != string(generator.ResultPersisted) {
		t.Fatalf("payload.Result=%q want %q", payload.Result, generator.ResultPersisted)
	}
}

// TestPromote_MultipleTargets asserts a per-target row is written for
// each target.
func TestPromote_MultipleTargets(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)

	idA := testIdentity()
	ctxA, _ := identity.WithRun(context.Background(), idA.Identity, idA.RunID)

	draft := validDraft("multi-target")
	if _, err := generator.Propose(ctxA, store, deps, generator.ProposeArgs{Skill: draft, Persist: true}); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	targets := []identity.Quadruple{
		{Identity: identity.Identity{TenantID: idA.TenantID, UserID: idA.UserID, SessionID: "s-1"}},
		{Identity: identity.Identity{TenantID: idA.TenantID, UserID: idA.UserID, SessionID: "s-2"}},
		{Identity: identity.Identity{TenantID: idA.TenantID, UserID: idA.UserID, SessionID: "s-3"}},
	}
	if err := generator.Promote(ctxA, store, deps, idA, "multi-target", targets, skills.ScopeProject); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	for _, target := range targets {
		if _, err := store.Get(context.Background(), target, "multi-target"); err != nil {
			t.Fatalf("Get for target %s: %v", target.SessionID, err)
		}
	}
}

// TestPromote_RejectsScopeSession asserts Promote refuses
// Scope=session (a contradiction).
func TestPromote_RejectsScopeSession(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)
	idA := testIdentity()
	ctxA, _ := identity.WithRun(context.Background(), idA.Identity, idA.RunID)

	if _, err := generator.Propose(ctxA, store, deps,
		generator.ProposeArgs{Skill: validDraft("scope-session"), Persist: true}); err != nil {
		t.Fatal(err)
	}

	target := identity.Quadruple{Identity: identity.Identity{
		TenantID: idA.TenantID, UserID: idA.UserID, SessionID: "s-target",
	}}
	err := generator.Promote(ctxA, store, deps, idA, "scope-session",
		[]identity.Quadruple{target}, skills.ScopeSession)
	if err == nil {
		t.Fatal("got nil err, want refusal of Scope=session")
	}
}

// TestPromote_DefaultScope asserts an empty scope defaults to project.
func TestPromote_DefaultScope(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)
	idA := testIdentity()
	ctxA, _ := identity.WithRun(context.Background(), idA.Identity, idA.RunID)
	if _, err := generator.Propose(ctxA, store, deps,
		generator.ProposeArgs{Skill: validDraft("default-scope"), Persist: true}); err != nil {
		t.Fatal(err)
	}
	target := identity.Quadruple{Identity: identity.Identity{
		TenantID: idA.TenantID, UserID: idA.UserID, SessionID: "s-default",
	}}
	if err := generator.Promote(ctxA, store, deps, idA, "default-scope",
		[]identity.Quadruple{target}, ""); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	got, err := store.Get(context.Background(), target, "default-scope")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Scope != skills.ScopeProject {
		t.Fatalf("Scope=%q want %q (default)", got.Scope, skills.ScopeProject)
	}
}

// TestPromote_SourceMissing asserts Promote bubbles a wrapped
// ErrSkillNotFound when the source row doesn't exist.
func TestPromote_SourceMissing(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)
	idA := testIdentity()
	target := identity.Quadruple{Identity: identity.Identity{
		TenantID: idA.TenantID, UserID: idA.UserID, SessionID: "s-target",
	}}
	err := generator.Promote(context.Background(), store, deps, idA, "missing",
		[]identity.Quadruple{target}, skills.ScopeProject)
	if !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("got %v, want wrapped ErrSkillNotFound", err)
	}
}

// TestPromote_EmptyTargets asserts an empty target slice errors out.
func TestPromote_EmptyTargets(t *testing.T) {
	t.Parallel()
	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)
	idA := testIdentity()
	err := generator.Promote(context.Background(), store, deps, idA, "x",
		nil, skills.ScopeProject)
	if err == nil {
		t.Fatal("got nil err for empty targets")
	}
}

// TestPromote_MissingIdentityOnSource — Promote with a partial src
// identity rejects.
func TestPromote_MissingIdentityOnSource(t *testing.T) {
	t.Parallel()
	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)

	target := identity.Quadruple{Identity: identity.Identity{
		TenantID: "t", UserID: "u", SessionID: "s",
	}}
	err := generator.Promote(context.Background(), store, deps,
		identity.Quadruple{}, "x",
		[]identity.Quadruple{target}, skills.ScopeProject)
	if !errors.Is(err, skills.ErrIdentityRequired) {
		t.Fatalf("got %v, want wrapped ErrIdentityRequired", err)
	}
}
