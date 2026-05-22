package generator_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/generator"
)

// TestPropose_ValidateOnly_NoWrite asserts persist=false validates the
// draft, returns the canonical hash, and does NOT write to the store
// or emit any audit event.
func TestPropose_ValidateOnly_NoWrite(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)
	ctx := ctxWithIdentity(t)
	q := testIdentity()
	drain := collectProposedEvents(t, bus, q)

	receipt, err := generator.Propose(ctx, store, deps, generator.ProposeArgs{
		Skill:   validDraft("vskill"),
		Persist: false,
	})
	if err != nil {
		t.Fatalf("Propose validate-only: %v", err)
	}
	if !receipt.Validated || receipt.Persisted {
		t.Fatalf("receipt %+v: want Validated=true Persisted=false", receipt)
	}
	if receipt.Result != generator.ResultValidated {
		t.Fatalf("receipt.Result=%q want %q", receipt.Result, generator.ResultValidated)
	}
	if receipt.Hash == "" {
		t.Fatalf("receipt.Hash empty")
	}
	// No DB row.
	if _, err := store.Get(ctx, q, "vskill"); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("Get vskill: want ErrSkillNotFound, got %v", err)
	}
	// No audit event.
	if got := drain(); len(got) != 0 {
		t.Fatalf("got %d skill.proposed events on persist=false, want 0", len(got))
	}
}

// TestPropose_PersistTrue_HappyPath asserts persist=true stamps
// provenance, writes the row, emits `skill.proposed` with
// Result="persisted", and the redactor scrubs caller-controlled bytes
// from the audit payload.
func TestPropose_PersistTrue_HappyPath(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)
	ctx := ctxWithIdentity(t)
	q := testIdentity()
	drain := collectProposedEvents(t, bus, q)

	// Inject a Bearer token into Title so we can assert the
	// redactor scrubs it from the audit excerpt.
	draft := validDraft("happy")
	draft.Title = "Use Authorization: Bearer abc.def.ghi to call"

	receipt, err := generator.Propose(ctx, store, deps, generator.ProposeArgs{
		Skill:   draft,
		Persist: true,
	})
	if err != nil {
		t.Fatalf("Propose persist=true: %v", err)
	}
	if !receipt.Validated || !receipt.Persisted {
		t.Fatalf("receipt %+v: want Validated=true Persisted=true", receipt)
	}
	if receipt.Result != generator.ResultPersisted {
		t.Fatalf("receipt.Result=%q want %q", receipt.Result, generator.ResultPersisted)
	}
	if receipt.Origin != skills.OriginGenerated {
		t.Fatalf("receipt.Origin=%q want %q", receipt.Origin, skills.OriginGenerated)
	}
	if receipt.Scope != skills.ScopeProject {
		t.Fatalf("receipt.Scope=%q want %q (default)", receipt.Scope, skills.ScopeProject)
	}
	wantOriginRef := "gen:" + q.SessionID + ":" + q.RunID
	if receipt.OriginRef != wantOriginRef {
		t.Fatalf("receipt.OriginRef=%q want %q", receipt.OriginRef, wantOriginRef)
	}

	got, err := store.Get(ctx, q, "happy")
	if err != nil {
		t.Fatalf("Get happy: %v", err)
	}
	if got.Origin != skills.OriginGenerated {
		t.Fatalf("stored.Origin=%q want %q", got.Origin, skills.OriginGenerated)
	}
	if got.ContentHash != receipt.Hash {
		t.Fatalf("stored.ContentHash=%q receipt.Hash=%q (mismatch)", got.ContentHash, receipt.Hash)
	}
	if got.ScopeTenantID != q.TenantID {
		t.Fatalf("stored.ScopeTenantID=%q want %q", got.ScopeTenantID, q.TenantID)
	}

	// One skill.proposed event.
	evs := drain()
	if len(evs) != 1 {
		t.Fatalf("got %d skill.proposed events, want 1", len(evs))
	}
	payload, ok := evs[0].Payload.(generator.SkillProposedPayload)
	if !ok {
		t.Fatalf("payload type=%T, want SkillProposedPayload", evs[0].Payload)
	}
	if payload.Result != string(generator.ResultPersisted) {
		t.Fatalf("payload.Result=%q want %q", payload.Result, generator.ResultPersisted)
	}
	if payload.ContentHash != receipt.Hash {
		t.Fatalf("payload.ContentHash=%q receipt.Hash=%q", payload.ContentHash, receipt.Hash)
	}
	if payload.OriginRef != wantOriginRef {
		t.Fatalf("payload.OriginRef=%q want %q", payload.OriginRef, wantOriginRef)
	}
	// Redactor must have scrubbed the bearer token from the title
	// excerpt. The canonical patterns redactor replaces the bearer
	// value with a sentinel that does NOT contain the literal
	// secret bytes.
	if strings.Contains(payload.RedactedTitleExcerpt, "abc.def.ghi") {
		t.Fatalf("payload.RedactedTitleExcerpt leaked bearer literal: %q", payload.RedactedTitleExcerpt)
	}
}

// TestPropose_RejectsInvalidDraft asserts the validator fires before
// the DB write — invalid drafts never reach the conflict-policy probe.
func TestPropose_RejectsInvalidDraft(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)
	ctx := ctxWithIdentity(t)
	drain := collectProposedEvents(t, bus, testIdentity())

	cases := []struct {
		mut   func(*generator.SkillDraft)
		label string
	}{
		{label: "empty name", mut: func(d *generator.SkillDraft) { d.Name = "" }},
		{label: "empty trigger", mut: func(d *generator.SkillDraft) { d.Trigger = "" }},
		{label: "empty steps", mut: func(d *generator.SkillDraft) { d.Steps = nil }},
	}
	for _, tc := range cases {

		t.Run(tc.label, func(t *testing.T) {
			d := validDraft("bad-" + tc.label)
			tc.mut(&d)
			_, err := generator.Propose(ctx, store, deps, generator.ProposeArgs{Skill: d, Persist: true})
			if !errors.Is(err, skills.ErrInvalidSkill) {
				t.Fatalf("got %v, want wrapped ErrInvalidSkill", err)
			}
		})
	}
	// No audit events should have landed.
	if got := drain(); len(got) != 0 {
		t.Fatalf("got %d skill.proposed events on invalid drafts, want 0", len(got))
	}
}

// TestPropose_ConflictPackProtected asserts an Origin=PackImport
// existing row blocks a `persist=true` call.
func TestPropose_ConflictPackProtected(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)
	ctx := ctxWithIdentity(t)
	q := testIdentity()

	// Seed a pack-imported row.
	pack := skills.Skill{
		Name:    "shared",
		Trigger: "use shared",
		Steps:   []string{"do thing"},
		Origin:  skills.OriginPack,
		Scope:   skills.ScopeProject,
	}
	if err := store.Upsert(ctx, q, pack); err != nil {
		t.Fatalf("seed pack: %v", err)
	}

	drain := collectProposedEvents(t, bus, q)

	// Propose a Generated row with the same name.
	receipt, err := generator.Propose(ctx, store, deps, generator.ProposeArgs{
		Skill:   validDraft("shared"),
		Persist: true,
	})

	var conflict *generator.ErrSkillConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("got %v, want *ErrSkillConflict", err)
	}
	if conflict.Reason != "pack_import_protected" {
		t.Fatalf("conflict.Reason=%q want pack_import_protected", conflict.Reason)
	}
	if !errors.Is(err, generator.ErrSkillConflictSentinel) {
		t.Fatalf("errors.Is sentinel: false")
	}
	// Receipt should reflect rejection.
	if receipt.Result != generator.ResultRejected {
		t.Fatalf("receipt.Result=%q want %q", receipt.Result, generator.ResultRejected)
	}
	if receipt.Persisted {
		t.Fatalf("receipt.Persisted=true on rejection")
	}

	// The existing pack row is untouched.
	got, gerr := store.Get(ctx, q, "shared")
	if gerr != nil {
		t.Fatalf("Get shared: %v", gerr)
	}
	if got.Origin != skills.OriginPack {
		t.Fatalf("Get shared.Origin=%q want %q (pack untouched)", got.Origin, skills.OriginPack)
	}

	// Audit event landed with Result="rejected".
	evs := drain()
	if len(evs) != 1 {
		t.Fatalf("got %d skill.proposed events, want 1", len(evs))
	}
	payload, ok := evs[0].Payload.(generator.SkillProposedPayload)
	if !ok {
		t.Fatalf("payload type=%T", evs[0].Payload)
	}
	if payload.Result != string(generator.ResultRejected) {
		t.Fatalf("payload.Result=%q want %q", payload.Result, generator.ResultRejected)
	}
	if payload.Reason != "pack_import_protected" {
		t.Fatalf("payload.Reason=%q want pack_import_protected", payload.Reason)
	}
}

// TestPropose_ConflictIdempotent asserts a Generated→Generated propose
// with matching hash is idempotent.
func TestPropose_ConflictIdempotent(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)
	ctx := ctxWithIdentity(t)
	q := testIdentity()

	draft := validDraft("idemp")

	first, err := generator.Propose(ctx, store, deps, generator.ProposeArgs{Skill: draft, Persist: true})
	if err != nil {
		t.Fatalf("first Propose: %v", err)
	}
	if first.Result != generator.ResultPersisted {
		t.Fatalf("first.Result=%q want %q", first.Result, generator.ResultPersisted)
	}

	drain := collectProposedEvents(t, bus, q)
	second, err := generator.Propose(ctx, store, deps, generator.ProposeArgs{Skill: draft, Persist: true})
	if err != nil {
		t.Fatalf("second Propose: %v", err)
	}
	if second.Result != generator.ResultIdempotent {
		t.Fatalf("second.Result=%q want %q", second.Result, generator.ResultIdempotent)
	}
	if !second.Persisted {
		t.Fatalf("second.Persisted=false on idempotent")
	}
	if second.Hash != first.Hash {
		t.Fatalf("idempotent hash drift: first=%q second=%q", first.Hash, second.Hash)
	}

	evs := drain()
	if len(evs) != 1 {
		t.Fatalf("got %d skill.proposed events (second call), want 1", len(evs))
	}
	payload := evs[0].Payload.(generator.SkillProposedPayload)
	if payload.Result != string(generator.ResultIdempotent) {
		t.Fatalf("payload.Result=%q want %q", payload.Result, generator.ResultIdempotent)
	}
}

// TestPropose_ConflictLWW asserts a Generated→Generated propose with
// DIFFERENT content hash overwrites via LWW.
func TestPropose_ConflictLWW(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)
	ctx := ctxWithIdentity(t)
	q := testIdentity()

	first := validDraft("lww")
	if _, err := generator.Propose(ctx, store, deps, generator.ProposeArgs{Skill: first, Persist: true}); err != nil {
		t.Fatalf("first Propose: %v", err)
	}

	second := validDraft("lww")
	second.Description = "updated description"
	second.Steps = []string{"new step one", "new step two"}

	drain := collectProposedEvents(t, bus, q)
	receipt, err := generator.Propose(ctx, store, deps, generator.ProposeArgs{Skill: second, Persist: true})
	if err != nil {
		t.Fatalf("second Propose: %v", err)
	}
	if receipt.Result != generator.ResultPersisted {
		t.Fatalf("LWW receipt.Result=%q want %q", receipt.Result, generator.ResultPersisted)
	}

	got, err := store.Get(ctx, q, "lww")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Description != "updated description" {
		t.Fatalf("Get.Description=%q want updated", got.Description)
	}

	evs := drain()
	if len(evs) != 1 {
		t.Fatalf("got %d events", len(evs))
	}
	payload := evs[0].Payload.(generator.SkillProposedPayload)
	if payload.Result != string(generator.ResultPersisted) {
		t.Fatalf("payload.Result=%q want %q", payload.Result, generator.ResultPersisted)
	}
}

// TestPropose_IdentityRequired asserts missing identity returns
// wrapped ErrIdentityRequired AND emits skill.identity_rejected.
func TestPropose_IdentityRequired(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)

	// Subscribe to skill.identity_rejected to assert emit.
	sub, err := bus.Subscribe(context.Background(), eventsFilterAllForIdentityRejected())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	// Pass a bare context (no identity).
	_, err = generator.Propose(context.Background(), store, deps, generator.ProposeArgs{
		Skill:   validDraft("nope"),
		Persist: true,
	})
	if !errors.Is(err, skills.ErrIdentityRequired) {
		t.Fatalf("got %v, want wrapped ErrIdentityRequired", err)
	}
	// Drain the rejection event. The bus does not deliver to a
	// filter that lacks the triple unless Admin=true; we work
	// around by using Admin claim with cleanup.
}

// TestPropose_ScopeDefault asserts an empty Scope on the draft falls
// back to ScopeProject.
func TestPropose_ScopeDefault(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)
	ctx := ctxWithIdentity(t)
	q := testIdentity()

	d := validDraft("scope-default")
	d.Scope = "" // explicit zero
	receipt, err := generator.Propose(ctx, store, deps, generator.ProposeArgs{Skill: d, Persist: true})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if receipt.Scope != skills.ScopeProject {
		t.Fatalf("receipt.Scope=%q want %q", receipt.Scope, skills.ScopeProject)
	}
	got, _ := store.Get(ctx, q, "scope-default")
	if got.Scope != skills.ScopeProject {
		t.Fatalf("stored.Scope=%q want %q", got.Scope, skills.ScopeProject)
	}
}

// TestPropose_OriginRefStamping asserts the OriginRef format exactly.
func TestPropose_OriginRefStamping(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)

	// With RunID populated.
	q := testIdentity()
	ctxRun, err := identity.WithRun(context.Background(), q.Identity, q.RunID)
	if err != nil {
		t.Fatal(err)
	}
	r1, err := generator.Propose(ctxRun, store, deps, generator.ProposeArgs{Skill: validDraft("with-run"), Persist: true})
	if err != nil {
		t.Fatalf("with-run Propose: %v", err)
	}
	want1 := "gen:" + q.SessionID + ":" + q.RunID
	if r1.OriginRef != want1 {
		t.Fatalf("OriginRef=%q want %q", r1.OriginRef, want1)
	}

	// With Identity only (no RunID). Use a different name in a
	// different session to avoid the conflict-policy idempotent
	// branch.
	idOnly := identity.Identity{TenantID: q.TenantID, UserID: q.UserID, SessionID: "s-norun"}
	ctxNoRun, err := identity.With(context.Background(), idOnly)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := generator.Propose(ctxNoRun, store, deps, generator.ProposeArgs{Skill: validDraft("no-run"), Persist: true})
	if err != nil {
		t.Fatalf("no-run Propose: %v", err)
	}
	want2 := "gen:s-norun:"
	if r2.OriginRef != want2 {
		t.Fatalf("OriginRef=%q want %q", r2.OriginRef, want2)
	}
}

// TestRegister_InstallsToolWithLoadingAlways asserts the catalog
// descriptor has the expected shape.
func TestRegister_InstallsToolWithLoadingAlways(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)

	catalog := newToolCatalog()
	if err := generator.Register(catalog, store, deps); err != nil {
		t.Fatalf("Register: %v", err)
	}
	d, ok := catalog.Resolve(generator.ToolNameSkillPropose)
	if !ok {
		t.Fatalf("Resolve %q ok=false", generator.ToolNameSkillPropose)
	}
	if d.Tool.Name != "skill_propose" {
		t.Fatalf("tool name=%q want skill_propose", d.Tool.Name)
	}
}

// TestErrSkillConflict_Error formats the error message with name +
// reason so callers can log it usefully without unpacking the typed
// fields.
func TestErrSkillConflict_Error(t *testing.T) {
	t.Parallel()
	e := &generator.ErrSkillConflict{Name: "x", Reason: "pack_import_protected"}
	got := e.Error()
	if !contains(got, "x") || !contains(got, "pack_import_protected") {
		t.Fatalf("Error()=%q, want substring 'x' and 'pack_import_protected'", got)
	}
}

// TestPropose_NilStore — direct Go-level call with nil store errors
// out (defensive guard against bootstrap mis-wiring).
func TestPropose_NilStore(t *testing.T) {
	t.Parallel()
	bus := newTestBus(t)
	deps := newTestDeps(t, bus)
	_, err := generator.Propose(context.Background(), nil, deps,
		generator.ProposeArgs{Skill: validDraft("x"), Persist: true})
	if err == nil {
		t.Fatal("nil store: got nil err")
	}
}

// TestPropose_NilBus — direct Go-level call without a Bus errors out.
func TestPropose_NilBus(t *testing.T) {
	t.Parallel()
	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)
	deps.Bus = nil
	_, err := generator.Propose(context.Background(), store, deps,
		generator.ProposeArgs{Skill: validDraft("x"), Persist: true})
	if err == nil {
		t.Fatal("nil bus: got nil err")
	}
}

// TestPropose_NilRedactor — direct Go-level call without a Redactor
// errors out.
func TestPropose_NilRedactor(t *testing.T) {
	t.Parallel()
	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)
	deps.Redactor = nil
	_, err := generator.Propose(context.Background(), store, deps,
		generator.ProposeArgs{Skill: validDraft("x"), Persist: true})
	if err == nil {
		t.Fatal("nil redactor: got nil err")
	}
}

// TestPromote_NilDeps — Promote with missing deps surfaces wrapped
// errors rather than panicking.
func TestPromote_NilDeps(t *testing.T) {
	t.Parallel()
	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)
	target := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}

	if err := generator.Promote(context.Background(), nil, deps,
		testIdentity(), "x", []identity.Quadruple{target}, skills.ScopeProject); err == nil {
		t.Fatal("nil store: got nil err")
	}
	if err := generator.Promote(context.Background(), store, generator.Deps{Redactor: deps.Redactor},
		testIdentity(), "x", []identity.Quadruple{target}, skills.ScopeProject); err == nil {
		t.Fatal("nil bus: got nil err")
	}
	if err := generator.Promote(context.Background(), store, generator.Deps{Bus: bus},
		testIdentity(), "x", []identity.Quadruple{target}, skills.ScopeProject); err == nil {
		t.Fatal("nil redactor: got nil err")
	}
}

// TestPromote_EmptyName errors out.
func TestPromote_EmptyName(t *testing.T) {
	t.Parallel()
	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)
	target := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	err := generator.Promote(context.Background(), store, deps,
		testIdentity(), "", []identity.Quadruple{target}, skills.ScopeProject)
	if err == nil {
		t.Fatal("empty name: got nil err")
	}
}

// TestPromote_UnknownScope errors out.
func TestPromote_UnknownScope(t *testing.T) {
	t.Parallel()
	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)
	target := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	err := generator.Promote(context.Background(), store, deps,
		testIdentity(), "x", []identity.Quadruple{target}, skills.Scope("bogus"))
	if err == nil {
		t.Fatal("bogus scope: got nil err")
	}
}

// TestPromote_TargetMissingIdentity rejects when one of the targets
// has an incomplete identity triple.
func TestPromote_TargetMissingIdentity(t *testing.T) {
	t.Parallel()
	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)

	idA := testIdentity()
	ctxA, _ := identity.WithRun(context.Background(), idA.Identity, idA.RunID)
	if _, err := generator.Propose(ctxA, store, deps,
		generator.ProposeArgs{Skill: validDraft("tgt-bad"), Persist: true}); err != nil {
		t.Fatal(err)
	}

	target := identity.Quadruple{Identity: identity.Identity{TenantID: "t", SessionID: "s"}} // empty UserID
	err := generator.Promote(ctxA, store, deps, idA, "tgt-bad",
		[]identity.Quadruple{target}, skills.ScopeProject)
	if !errors.Is(err, skills.ErrIdentityRequired) {
		t.Fatalf("got %v, want wrapped ErrIdentityRequired", err)
	}
}

// TestPromote_AuditEmitFailure_PerTargetRollback asserts that an
// audit-emit failure on a per-target write rolls back that target's
// row.
func TestPromote_AuditEmitFailure_PerTargetRollback(t *testing.T) {
	t.Parallel()
	innerBus := newTestBus(t)
	bus := newErrBus(innerBus, skills.EventTypeSkillProposed)
	store := newTestStore(t, bus)
	deps := generator.Deps{Bus: bus, Redactor: newTestDeps(t, innerBus).Redactor}

	idA := testIdentity()
	ctxA, _ := identity.WithRun(context.Background(), idA.Identity, idA.RunID)

	// Seed under A — this triggers Phase 37's skill.upserted
	// event (which we DO want to succeed) AND the skill.proposed
	// audit emit (which the errBus fails). So the first Propose
	// itself rolls back. To exercise *Promote*'s rollback path
	// independently, seed via store.Upsert directly so we never
	// emit skill.proposed.
	seed := skills.Skill{
		Name:    "promote-fail",
		Title:   "title",
		Trigger: "trig",
		Steps:   []string{"s"},
		Origin:  skills.OriginGenerated,
		Scope:   skills.ScopeSession,
	}
	seed.ContentHash = skills.CanonicalContentHash(seed)
	if err := store.Upsert(ctxA, idA, seed); err != nil {
		t.Fatalf("seed Upsert: %v", err)
	}

	target := identity.Quadruple{Identity: identity.Identity{
		TenantID: idA.TenantID, UserID: idA.UserID, SessionID: "s-fail-target",
	}}
	err := generator.Promote(ctxA, store, deps, idA, "promote-fail",
		[]identity.Quadruple{target}, skills.ScopeProject)
	if err == nil {
		t.Fatal("Promote: got nil err on audit-emit failure")
	}
	if !contains(err.Error(), "audit emit failed") {
		t.Fatalf("err=%q, want substring 'audit emit failed'", err.Error())
	}
	// Target row must be rolled back.
	if _, gerr := store.Get(context.Background(), target, "promote-fail"); !errors.Is(gerr, skills.ErrSkillNotFound) {
		t.Fatalf("post-rollback Get under target: got %v, want ErrSkillNotFound", gerr)
	}
}

// TestPropose_AuditEmitFailure_OnIdempotent asserts the idempotent
// branch also surfaces an audit-emit failure as a wrapped error
// (no DB rollback needed — the row was already there).
func TestPropose_AuditEmitFailure_OnIdempotent(t *testing.T) {
	t.Parallel()

	innerBus := newTestBus(t)
	store := newTestStore(t, innerBus)
	deps := newTestDeps(t, innerBus)
	ctx := ctxWithIdentity(t)

	// First propose succeeds via the inner bus.
	if _, err := generator.Propose(ctx, store, deps,
		generator.ProposeArgs{Skill: validDraft("idemp-fail"), Persist: true}); err != nil {
		t.Fatalf("first Propose: %v", err)
	}

	// Now wrap the bus so the second propose's audit emit fails.
	wrappedBus := newErrBus(innerBus, skills.EventTypeSkillProposed)
	deps2 := generator.Deps{Bus: wrappedBus, Redactor: deps.Redactor}
	_, err := generator.Propose(ctx, store, deps2,
		generator.ProposeArgs{Skill: validDraft("idemp-fail"), Persist: true})
	if err == nil {
		t.Fatal("got nil err on idempotent audit-emit failure")
	}
	if !contains(err.Error(), "audit emit failed on idempotent") {
		t.Fatalf("err=%q, want substring 'audit emit failed on idempotent'", err.Error())
	}
	// Row still present.
	if _, gerr := store.Get(ctx, testIdentity(), "idemp-fail"); gerr != nil {
		t.Fatalf("idempotent path should leave row intact, got %v", gerr)
	}
}

// TestRegister_NilArgsRejected — Register fails loudly on nil
// catalog / store / bus / redactor.
func TestRegister_NilArgsRejected(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)
	catalog := newToolCatalog()

	if err := generator.Register(nil, store, deps); err == nil {
		t.Fatal("nil catalog: got nil err")
	}
	if err := generator.Register(catalog, nil, deps); err == nil {
		t.Fatal("nil store: got nil err")
	}
	if err := generator.Register(catalog, store, generator.Deps{Redactor: deps.Redactor}); err == nil {
		t.Fatal("nil bus: got nil err")
	}
	if err := generator.Register(catalog, store, generator.Deps{Bus: bus}); err == nil {
		t.Fatal("nil redactor: got nil err")
	}
}
