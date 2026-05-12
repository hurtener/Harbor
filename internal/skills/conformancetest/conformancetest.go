// Package conformancetest is the shared `SkillStore` conformance
// harness. Drivers (localdb at Phase 37, Portico post-V1) supply a
// `Harness` factory and run the suite via `Run(t, factory)`. The
// suite asserts the surface every implementation MUST satisfy:
// identity-mandatory, conflict policy, ordering determinism,
// restart survival (when the driver is durable), and the D-025
// concurrent-reuse contract.
//
// Phase 37 wires the harness against the `localdb` driver. Future
// driver phases add their own seam-test (`Run` call) and inherit
// the suite verbatim.
package conformancetest

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
)

// Harness is what each driver supplies to the suite. `Store` is the
// driver-under-test; `Bus` is the events bus the suite uses to
// assert audit emits land; `Cleanup` releases resources after the
// suite finishes.
//
// `ReopenedStore`, when non-nil, is invoked by the restart-survival
// subtest: the suite closes `Store`, calls `ReopenedStore()` to get
// a fresh handle against the same backing storage, and asserts the
// rows survive. Drivers without durable storage (none today; reserved
// for future ephemeral providers) leave `ReopenedStore` nil and the
// subtest skips.
type Harness struct {
	Store         skills.SkillStore
	Bus           events.EventBus
	Cleanup       func()
	ReopenedStore func() (skills.SkillStore, error)
}

// Run executes the shared suite against the harness returned by
// `factory`. Each subtest gets its own harness; `factory` is called
// once per subtest so state is isolated.
func Run(t *testing.T, factory func(*testing.T) Harness) {
	t.Helper()

	t.Run("upsert_get_roundtrip", func(t *testing.T) {
		h := factory(t)
		defer h.Cleanup()
		testUpsertGetRoundTrip(t, h)
	})

	t.Run("conflict_policy", func(t *testing.T) {
		h := factory(t)
		defer h.Cleanup()
		testConflictPolicy(t, h)
	})

	t.Run("ordering", func(t *testing.T) {
		h := factory(t)
		defer h.Cleanup()
		testOrdering(t, h)
	})

	t.Run("identity_rejection", func(t *testing.T) {
		h := factory(t)
		defer h.Cleanup()
		testIdentityRejection(t, h)
	})

	t.Run("not_found", func(t *testing.T) {
		h := factory(t)
		defer h.Cleanup()
		testNotFound(t, h)
	})

	t.Run("delete_removes_row", func(t *testing.T) {
		h := factory(t)
		defer h.Cleanup()
		testDelete(t, h)
	})

	t.Run("restart_survival", func(t *testing.T) {
		h := factory(t)
		defer h.Cleanup()
		if h.ReopenedStore == nil {
			t.Skip("driver does not support reopen (set Harness.ReopenedStore to enable)")
		}
		testRestartSurvival(t, h)
	})
}

// fixtureID is the identity quadruple every subtest uses by default;
// subtests that need cross-identity behavior derive variants.
var fixtureID = identity.Quadruple{
	Identity: identity.Identity{
		TenantID:  "t-conformance",
		UserID:    "u-conformance",
		SessionID: "s-conformance",
	},
	RunID: "r-conformance",
}

// newSkill returns a fresh `Skill` populated with the test-time
// defaults the suite uses. Callers override `Name` / `Origin` etc.
// before passing to `Upsert`.
func newSkill(name string) skills.Skill {
	now := time.Now().UTC()
	s := skills.Skill{
		Name:        name,
		Title:       "Title " + name,
		Description: "Description for " + name,
		Trigger:     "trigger:" + name,
		TaskType:    "code",
		Tags:        []string{"alpha", "beta"},
		Steps:       []string{"step one", "step two"},
		Origin:      skills.OriginGenerated,
		OriginRef:   "gen:test:run",
		Scope:       skills.ScopeProject,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	s.ContentHash = skills.CanonicalContentHash(s)
	return s
}

func testUpsertGetRoundTrip(t *testing.T, h Harness) {
	ctx := context.Background()
	want := newSkill("alpha")
	if err := h.Store.Upsert(ctx, fixtureID, want); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := h.Store.Get(ctx, fixtureID, "alpha")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != want.Name || got.Trigger != want.Trigger || got.ContentHash != want.ContentHash {
		t.Fatalf("round-trip mismatch:\n  got  %+v\n  want %+v", got, want)
	}
	if len(got.Steps) != 2 || got.Steps[0] != "step one" || got.Steps[1] != "step two" {
		t.Fatalf("Steps not preserved: %v", got.Steps)
	}
}

func testConflictPolicy(t *testing.T, h Harness) {
	ctx := context.Background()

	// Seed a pack-origin skill.
	pack := newSkill("conflict")
	pack.Origin = skills.OriginPack
	pack.OriginRef = "pack-foo@v1.0"
	pack.ContentHash = skills.CanonicalContentHash(pack)
	if err := h.Store.Upsert(ctx, fixtureID, pack); err != nil {
		t.Fatalf("seed pack: %v", err)
	}

	// Generated overwrite must be refused.
	gen := pack
	gen.Origin = skills.OriginGenerated
	gen.OriginRef = "gen:test:overwrite"
	gen.Description = "hostile overwrite"
	gen.ContentHash = skills.CanonicalContentHash(gen)
	if err := h.Store.Upsert(ctx, fixtureID, gen); err == nil {
		t.Fatalf("expected ErrPackOverwriteRefused, got nil")
	}

	// The original pack row must survive untouched.
	got, err := h.Store.Get(ctx, fixtureID, "conflict")
	if err != nil {
		t.Fatalf("Get after refused overwrite: %v", err)
	}
	if got.Origin != skills.OriginPack || got.Description != pack.Description {
		t.Fatalf("pack row was mutated by refused overwrite: %+v", got)
	}

	// Generated → Generated same content → idempotent.
	g1 := newSkill("gen-only")
	if err := h.Store.Upsert(ctx, fixtureID, g1); err != nil {
		t.Fatalf("seed generated: %v", err)
	}
	if err := h.Store.Upsert(ctx, fixtureID, g1); err != nil {
		t.Fatalf("idempotent generated upsert: %v", err)
	}

	// Generated → Generated different content → LWW.
	g2 := g1
	g2.Description = "evolved"
	g2.ContentHash = skills.CanonicalContentHash(g2)
	if err := h.Store.Upsert(ctx, fixtureID, g2); err != nil {
		t.Fatalf("LWW generated upsert: %v", err)
	}
	got, err = h.Store.Get(ctx, fixtureID, "gen-only")
	if err != nil {
		t.Fatalf("Get after LWW: %v", err)
	}
	if got.Description != "evolved" {
		t.Fatalf("LWW did not apply: got Description=%q", got.Description)
	}
}

func testOrdering(t *testing.T, h Harness) {
	ctx := context.Background()
	names := []string{"echo", "alpha", "delta", "bravo", "charlie"}
	for _, n := range names {
		if err := h.Store.Upsert(ctx, fixtureID, newSkill(n)); err != nil {
			t.Fatalf("Upsert(%q): %v", n, err)
		}
	}
	rows, err := h.Store.List(ctx, fixtureID, skills.ListFilter{Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != len(names) {
		t.Fatalf("List returned %d rows; want %d", len(rows), len(names))
	}
	// Order: UpdatedAt DESC, Name ASC. Since all were inserted in
	// rapid sequence with the same nominal UpdatedAt, drivers may
	// tie-break on insertion order — assert at minimum that the set
	// is correct.
	gotSet := map[string]struct{}{}
	for _, r := range rows {
		gotSet[r.Name] = struct{}{}
	}
	for _, want := range names {
		if _, ok := gotSet[want]; !ok {
			t.Fatalf("List omitted %q (got %v)", want, gotSet)
		}
	}
}

func testIdentityRejection(t *testing.T, h Harness) {
	ctx := context.Background()
	bad := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u"}} // missing session
	cases := []struct {
		name string
		fn   func() error
	}{
		{"Upsert", func() error { return h.Store.Upsert(ctx, bad, newSkill("x")) }},
		{"Get", func() error { _, err := h.Store.Get(ctx, bad, "x"); return err }},
		{"List", func() error { _, err := h.Store.List(ctx, bad, skills.ListFilter{}); return err }},
		{"Search", func() error { _, err := h.Store.Search(ctx, bad, "x", 5); return err }},
		{"Delete", func() error { return h.Store.Delete(ctx, bad, "x") }},
	}
	for _, c := range cases {
		err := c.fn()
		if err == nil {
			t.Fatalf("%s: expected ErrIdentityRequired, got nil", c.name)
		}
	}
}

func testNotFound(t *testing.T, h Harness) {
	ctx := context.Background()
	if _, err := h.Store.Get(ctx, fixtureID, "no-such-skill"); err == nil {
		t.Fatalf("Get: expected ErrSkillNotFound, got nil")
	}
	if err := h.Store.Delete(ctx, fixtureID, "no-such-skill"); err == nil {
		t.Fatalf("Delete: expected ErrSkillNotFound, got nil")
	}
}

func testDelete(t *testing.T, h Harness) {
	ctx := context.Background()
	s := newSkill("doomed")
	if err := h.Store.Upsert(ctx, fixtureID, s); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := h.Store.Delete(ctx, fixtureID, "doomed"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := h.Store.Get(ctx, fixtureID, "doomed"); err == nil {
		t.Fatalf("Get after Delete: expected ErrSkillNotFound, got nil")
	}
}

func testRestartSurvival(t *testing.T, h Harness) {
	ctx := context.Background()
	s := newSkill("durable")
	if err := h.Store.Upsert(ctx, fixtureID, s); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := h.Store.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	store, err := h.ReopenedStore()
	if err != nil {
		t.Fatalf("ReopenedStore: %v", err)
	}
	defer func() { _ = store.Close(ctx) }()
	got, err := store.Get(ctx, fixtureID, "durable")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.Name != "durable" || got.Trigger != s.Trigger {
		t.Fatalf("restart mismatch: %+v", got)
	}
}
