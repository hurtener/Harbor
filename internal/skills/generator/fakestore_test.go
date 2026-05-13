package generator_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/generator"
)

// failingUpsertStore wraps an inner SkillStore but returns a fixed
// error from `Upsert` (after the first probe succeeds). Used to
// exercise the post-probe Upsert error path in Propose / Promote.
type failingUpsertStore struct {
	inner             skills.SkillStore
	upsertErr         error
	deleteErr         error
	upsertCalls       int
	deleteCalls       int
	failOnSecondOnly  bool
	returnPackRefused bool
}

func (f *failingUpsertStore) Upsert(ctx context.Context, id identity.Quadruple, sk skills.Skill) error {
	f.upsertCalls++
	if f.failOnSecondOnly && f.upsertCalls < 2 {
		return f.inner.Upsert(ctx, id, sk)
	}
	if f.returnPackRefused {
		return skills.ErrPackOverwriteRefused
	}
	if f.upsertErr != nil {
		return f.upsertErr
	}
	return f.inner.Upsert(ctx, id, sk)
}

func (f *failingUpsertStore) Get(ctx context.Context, id identity.Quadruple, name string) (skills.Skill, error) {
	return f.inner.Get(ctx, id, name)
}

func (f *failingUpsertStore) List(ctx context.Context, id identity.Quadruple, filter skills.ListFilter) ([]skills.Skill, error) {
	return f.inner.List(ctx, id, filter)
}

func (f *failingUpsertStore) Search(ctx context.Context, id identity.Quadruple, q string, limit int) ([]skills.RankedSkill, error) {
	return f.inner.Search(ctx, id, q, limit)
}

func (f *failingUpsertStore) Delete(ctx context.Context, id identity.Quadruple, name string) error {
	f.deleteCalls++
	if f.deleteErr != nil {
		return f.deleteErr
	}
	return f.inner.Delete(ctx, id, name)
}

func (f *failingUpsertStore) Close(ctx context.Context) error {
	return f.inner.Close(ctx)
}

// TestPropose_StoreUpsertReturnsPackOverwriteRefused exercises the
// race-defence branch: probe says no existing row → caller proceeds →
// store.Upsert returns ErrPackOverwriteRefused (race: a pack row
// arrived between probe and upsert). The generator surfaces a typed
// *ErrSkillConflict + emits the rejection event.
func TestPropose_StoreUpsertReturnsPackOverwriteRefused(t *testing.T) {
	t.Parallel()
	bus := newTestBus(t)
	inner := newTestStore(t, bus)
	wrapped := &failingUpsertStore{inner: inner, returnPackRefused: true}
	deps := newTestDeps(t, bus)
	ctx := ctxWithIdentity(t)

	_, err := generator.Propose(ctx, wrapped, deps,
		generator.ProposeArgs{Skill: validDraft("race"), Persist: true})
	var conflict *generator.ErrSkillConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("got %v, want *ErrSkillConflict", err)
	}
	if conflict.Reason != "pack_import_protected" {
		t.Fatalf("conflict.Reason=%q want pack_import_protected", conflict.Reason)
	}
}

// TestPropose_StoreUpsertReturnsGenericError surfaces a wrapped error
// from a non-ErrPackOverwriteRefused upsert failure.
func TestPropose_StoreUpsertReturnsGenericError(t *testing.T) {
	t.Parallel()
	bus := newTestBus(t)
	inner := newTestStore(t, bus)
	wrapped := &failingUpsertStore{inner: inner, upsertErr: errors.New("disk full")}
	deps := newTestDeps(t, bus)
	ctx := ctxWithIdentity(t)

	_, err := generator.Propose(ctx, wrapped, deps,
		generator.ProposeArgs{Skill: validDraft("disk"), Persist: true})
	if err == nil || !contains(err.Error(), "upsert") {
		t.Fatalf("got %v, want wrapped 'upsert' error", err)
	}
}

// TestPromote_UpsertFailsForTarget exercises Promote's per-target
// upsert error surfacing.
func TestPromote_UpsertFailsForTarget(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	inner := newTestStore(t, bus)
	deps := newTestDeps(t, bus)
	idA := testIdentity()
	ctxA, _ := identity.WithRun(context.Background(), idA.Identity, idA.RunID)

	// Seed via the inner store (no Upsert wrapper yet).
	if _, err := generator.Propose(ctxA, inner, deps,
		generator.ProposeArgs{Skill: validDraft("pseed"), Persist: true}); err != nil {
		t.Fatal(err)
	}

	// Wrap so the Promote-time Upsert fails.
	wrapped := &failingUpsertStore{inner: inner, upsertErr: errors.New("simulated upsert failure")}
	target := identity.Quadruple{Identity: identity.Identity{
		TenantID: idA.TenantID, UserID: idA.UserID, SessionID: "s-fail",
	}}
	err := generator.Promote(ctxA, wrapped, deps, idA, "pseed",
		[]identity.Quadruple{target}, skills.ScopeProject)
	if err == nil || !contains(err.Error(), "Promote upsert") {
		t.Fatalf("got %v, want wrapped 'Promote upsert' error", err)
	}
}

// TestPropose_EmitFailureAndDeleteFailure exercises the doubly-bad
// rollback path: the audit emit fails AND the cleanup Delete also
// fails. The error message names both.
func TestPropose_EmitFailureAndDeleteFailure(t *testing.T) {
	t.Parallel()

	innerBus := newTestBus(t)
	bus := newErrBus(innerBus, skills.EventTypeSkillProposed)
	inner := newTestStore(t, bus)
	wrapped := &failingUpsertStore{inner: inner, deleteErr: errors.New("delete simulated failure")}
	deps := generator.Deps{Bus: bus, Redactor: newTestDeps(t, innerBus).Redactor}
	ctx := ctxWithIdentity(t)

	_, err := generator.Propose(ctx, wrapped, deps,
		generator.ProposeArgs{Skill: validDraft("dbl-fail"), Persist: true})
	if err == nil {
		t.Fatal("got nil err, want wrapped emit+delete failure")
	}
	if !contains(err.Error(), "audit emit failed AND rollback delete failed") {
		t.Fatalf("err=%q, want substring naming both failures", err.Error())
	}
}

// TestPromote_EmitFailureAndDeleteFailure for Promote's
// dbl-rollback branch.
func TestPromote_EmitFailureAndDeleteFailure(t *testing.T) {
	t.Parallel()

	innerBus := newTestBus(t)
	bus := newErrBus(innerBus, skills.EventTypeSkillProposed)
	inner := newTestStore(t, bus)
	deps := generator.Deps{Bus: bus, Redactor: newTestDeps(t, innerBus).Redactor}
	idA := testIdentity()
	ctxA, _ := identity.WithRun(context.Background(), idA.Identity, idA.RunID)

	// Seed using inner directly to avoid the propose audit emit (which would also fail).
	seed := skills.Skill{
		Name: "promote-dbl", Title: "t", Trigger: "tr", Steps: []string{"s"},
		Origin: skills.OriginGenerated, Scope: skills.ScopeSession,
	}
	seed.ContentHash = skills.CanonicalContentHash(seed)
	if err := inner.Upsert(ctxA, idA, seed); err != nil {
		t.Fatal(err)
	}

	// Now wrap so the per-target Upsert succeeds (inner pass-through)
	// but the Delete fails. Combined with the bus failure, both
	// failures surface.
	wrapped := &failingUpsertStore{inner: inner, deleteErr: errors.New("simulated delete fail")}
	target := identity.Quadruple{Identity: identity.Identity{
		TenantID: idA.TenantID, UserID: idA.UserID, SessionID: "s-dbl",
	}}
	err := generator.Promote(ctxA, wrapped, deps, idA, "promote-dbl",
		[]identity.Quadruple{target}, skills.ScopeProject)
	if err == nil {
		t.Fatal("got nil err")
	}
	if !contains(err.Error(), "audit emit failed AND rollback delete failed") {
		t.Fatalf("err=%q, want substring naming both failures", err.Error())
	}
}
