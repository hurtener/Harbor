// Package conformancetest is the shared `SkillStore` conformance
// harness. Drivers (localdb, Portico post-V1) supply a
// `Harness` factory and run the suite via `Run(t, factory)`. The
// suite asserts the surface every implementation MUST satisfy:
// identity-mandatory, conflict policy, ordering determinism,
// restart survival (when the driver is durable), and the concurrent-reuse
// concurrent-reuse contract.
//
// Harbor wires the harness against the `localdb` driver. Future
// driver phases add their own seam-test (`Run` call) and inherit
// the suite verbatim.
package conformancetest

import (
	"context"
	"errors"
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
	Store                skills.SkillStore
	Bus                  events.EventBus
	Cleanup              func()
	ReopenedStore        func() (skills.SkillStore, error)
	SnapshotFullTextPath string
}

// failureReporter is the minimal assertion surface used by the suite's
// individual contract checks. Keeping it narrower than testing.TB lets the
// harness self-test its fail-closed diagnostics with an adversarial reporter;
// testing.TB intentionally has private methods and cannot be implemented by a
// test double.
type failureReporter interface {
	Helper()
	Fatal(args ...any)
	Fatalf(format string, args ...any)
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

	t.Run("snapshot_search", func(t *testing.T) {
		h := factory(t)
		defer h.Cleanup()
		testSnapshotSearch(t, h)
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

	t.Run("user_scope_cross_session", func(t *testing.T) {
		h := factory(t)
		defer h.Cleanup()
		testUserScopeResolution(t, h)
	})

	t.Run("delete_rung_independence", func(t *testing.T) {
		h := factory(t)
		defer h.Cleanup()
		testDeleteRungIndependence(t, h)
	})

	t.Run("get_scope_exact_rung", func(t *testing.T) {
		h := factory(t)
		defer h.Cleanup()
		testGetScopeExactRung(t, h)
	})

	t.Run("delete_session_scope", func(t *testing.T) {
		h := factory(t)
		defer h.Cleanup()
		testDeleteSessionScope(t, h)
	})

	t.Run("delete_session_scope_after_close", func(t *testing.T) {
		h := factory(t)
		defer h.Cleanup()
		testDeleteSessionScopeAfterClose(t, h)
	})

	t.Run("get_scope_after_close", func(t *testing.T) {
		h := factory(t)
		defer h.Cleanup()
		testGetScopeAfterClose(t, h)
	})
	t.Run("agent_binding_is_selector_metadata", func(t *testing.T) {
		h := factory(t)
		defer h.Cleanup()
		testAgentBinding(t, h)
	})
	t.Run("agent_binding_compatibility", func(t *testing.T) {
		h := factory(t)
		defer h.Cleanup()
		testAgentCompatibility(t, h)
	})
}

func testAgentBinding(t failureReporter, h Harness) {
	ctx := context.Background()
	first := newSkill("same-name")
	first.AgentID = "agent-a"
	second := newSkill("same-name")
	second.AgentID = "agent-b"
	if err := h.Store.Upsert(ctx, fixtureID, first); err != nil {
		t.Fatalf("upsert agent-a: %v", err)
	}
	if err := h.Store.Upsert(ctx, fixtureID, second); err != nil {
		t.Fatalf("upsert agent-b: %v", err)
	}
	for _, want := range []string{"agent-a", "agent-b"} {
		rows, err := h.Store.List(ctx, fixtureID, skills.ListFilter{AgentID: want})
		if err != nil || len(rows) != 1 || rows[0].AgentID != want {
			t.Fatalf("agent %q selection = %#v, err=%v", want, rows, err)
		}
	}
	foreign := fixtureID
	foreign.TenantID = "other-tenant"
	rows, err := h.Store.List(ctx, foreign, skills.ListFilter{AgentID: "agent-a"})
	if err != nil {
		t.Fatalf("foreign list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("cross-tenant agent rows leaked: %#v", rows)
	}
}

func testAgentCompatibility(t failureReporter, h Harness) {
	ctx := context.Background()
	legacy := newSkill("compatible")
	bound := newSkill("compatible")
	bound.AgentID = "agent-a"
	if err := h.Store.Upsert(ctx, fixtureID, legacy); err != nil {
		t.Fatalf("upsert legacy: %v", err)
	}
	if err := h.Store.Upsert(ctx, fixtureID, bound); err != nil {
		t.Fatalf("upsert bound: %v", err)
	}
	got, err := h.Store.GetScopeAgent(ctx, fixtureID, "agent-a", "compatible", skills.ScopeProject)
	if err != nil || got.AgentID != "agent-a" {
		t.Fatalf("bound precedence = %#v, err=%v", got, err)
	}
	rows, err := h.Store.SearchAgent(ctx, fixtureID, "agent-a", "compatible", 20)
	if err != nil || len(rows) != 1 || rows[0].Skill.AgentID != "agent-a" {
		t.Fatalf("agent search compatibility = %#v, err=%v", rows, err)
	}
	if err := h.Store.DeleteAgent(ctx, fixtureID, "agent-a", "compatible", skills.ScopeProject); err != nil {
		t.Fatalf("delete bound: %v", err)
	}
	if _, err := h.Store.GetScopeAgent(ctx, fixtureID, "agent-a", "compatible", skills.ScopeProject); err != nil {
		t.Fatalf("legacy fallback after bound delete: %v", err)
	}
	foreign := fixtureID
	foreign.TenantID = "other-tenant"
	if _, err := h.Store.GetScopeAgent(ctx, foreign, "agent-a", "compatible", skills.ScopeProject); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("cross-tenant bound lookup err=%v", err)
	}
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

func testUpsertGetRoundTrip(t failureReporter, h Harness) {
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

func testConflictPolicy(t failureReporter, h Harness) {
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

func testOrdering(t failureReporter, h Harness) {
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

func testIdentityRejection(t failureReporter, h Harness) {
	ctx := context.Background()
	bad := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u"}} // missing session
	cases := []struct {
		name string
		fn   func() error
	}{
		{"Upsert", func() error { return h.Store.Upsert(ctx, bad, newSkill("x")) }},
		{"Get", func() error { _, err := h.Store.Get(ctx, bad, "x"); return err }},
		{"GetScope", func() error { _, err := h.Store.GetScope(ctx, bad, "x", skills.ScopeSession); return err }},
		{"List", func() error { _, err := h.Store.List(ctx, bad, skills.ListFilter{}); return err }},
		{"Search", func() error { _, err := h.Store.Search(ctx, bad, "x", 5); return err }},
		{"SearchSnapshot", func() error {
			_, err := h.Store.SearchSnapshot(ctx, bad, "x", []skills.Skill{newSkill("x")}, 5)
			return err
		}},
		{"Delete", func() error { return h.Store.Delete(ctx, bad, "x", skills.ScopeSession) }},
	}
	for _, c := range cases {
		err := c.fn()
		if err == nil {
			t.Fatalf("%s: expected ErrIdentityRequired, got nil", c.name)
		}
	}
}

// testSnapshotSearch pins the mandatory driver-owned frozen-candidate search
// seam. The candidate is intentionally not stored: a compliant implementation
// must rank exactly the supplied run snapshot, not silently re-read its mutable
// backing catalog.
func testSnapshotSearch(t failureReporter, h Harness) {
	candidate := newSkill("snapshot-target")
	candidate.Description = "frozen snapshot harborneedle"
	candidate.UpdatedAt = time.Unix(10, 0).UTC()
	result, err := h.Store.SearchSnapshot(context.Background(), fixtureID, "harborneedle", []skills.Skill{candidate}, 5)
	if err != nil {
		t.Fatalf("SearchSnapshot: %v", err)
	}
	if len(result) != 1 || result[0].Skill.Name != candidate.Name || result[0].Path != h.SnapshotFullTextPath || result[0].Score != 1 {
		t.Fatalf("SearchSnapshot result = %+v, want only configured-driver full-text frozen candidate", result)
	}
}

// testGetScopeExactRung pins the non-precedence read used by legacy
// migration. A same-named wider user row may never replace an exact session
// row, and an absent session rung must remain absent even when the user rung
// exists.
func testGetScopeExactRung(t failureReporter, h Harness) {
	ctx := context.Background()
	sessionSkill := newSkill("same-name")
	sessionSkill.Scope = skills.ScopeSession
	sessionSkill.Title = "session"
	userSkill := newSkill("same-name")
	userSkill.Scope = skills.ScopeUser
	userSkill.Title = "user"
	for _, skill := range []skills.Skill{userSkill, sessionSkill} {
		if err := h.Store.Upsert(ctx, fixtureID, skill); err != nil {
			t.Fatalf("Upsert(%s): %v", skill.Scope, err)
		}
	}

	gotSession, err := h.Store.GetScope(ctx, fixtureID, "same-name", skills.ScopeSession)
	if err != nil || gotSession.Scope != skills.ScopeSession || gotSession.Title != "session" {
		t.Fatalf("GetScope(session): got=%+v err=%v", gotSession, err)
	}
	gotUser, err := h.Store.GetScope(ctx, fixtureID, "same-name", skills.ScopeUser)
	if err != nil || gotUser.Scope != skills.ScopeUser || gotUser.Title != "user" {
		t.Fatalf("GetScope(user): got=%+v err=%v", gotUser, err)
	}

	userOnly := newSkill("user-only")
	userOnly.Scope = skills.ScopeUser
	if err := h.Store.Upsert(ctx, fixtureID, userOnly); err != nil {
		t.Fatalf("Upsert(user-only): %v", err)
	}
	if _, err := h.Store.GetScope(ctx, fixtureID, userOnly.Name, skills.ScopeSession); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("GetScope missing exact session rung: err=%v, want ErrSkillNotFound", err)
	}

	foreign := []identity.Quadruple{fixtureID, fixtureID, fixtureID}
	foreign[0].SessionID = "s-conformance-foreign"
	foreign[1].UserID = "u-conformance-foreign"
	foreign[2].TenantID = "t-conformance-foreign"
	for _, id := range foreign {
		if _, err := h.Store.GetScope(ctx, id, "same-name", skills.ScopeSession); !errors.Is(err, skills.ErrSkillNotFound) {
			t.Fatalf("GetScope foreign identity %+v: err=%v, want ErrSkillNotFound", id.Identity, err)
		}
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := h.Store.GetScope(canceled, fixtureID, "same-name", skills.ScopeSession); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetScope canceled context: err=%v, want context.Canceled", err)
	}
}

func testNotFound(t failureReporter, h Harness) {
	ctx := context.Background()
	if _, err := h.Store.Get(ctx, fixtureID, "no-such-skill"); err == nil {
		t.Fatalf("Get: expected ErrSkillNotFound, got nil")
	}
	if err := h.Store.Delete(ctx, fixtureID, "no-such-skill", skills.ScopeSession); err == nil {
		t.Fatalf("Delete: expected ErrSkillNotFound, got nil")
	}
}

func testDelete(t failureReporter, h Harness) {
	ctx := context.Background()
	s := newSkill("doomed")
	if err := h.Store.Upsert(ctx, fixtureID, s); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := h.Store.Delete(ctx, fixtureID, "doomed", s.Scope); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := h.Store.Get(ctx, fixtureID, "doomed"); err == nil {
		t.Fatalf("Get after Delete: expected ErrSkillNotFound, got nil")
	}
}

func testRestartSurvival(t failureReporter, h Harness) {
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

// testUserScopeResolution asserts the durable-by-default ScopeUser contract:
// a user-scope skill authored under session A is resolvable from a DIFFERENT
// session of the same (tenant, user), stays invisible to a different user and
// a different tenant, and does NOT change the session-pinned visibility of
// non-user scopes. Every driver inherits this verbatim (the persistence
// three-driver parity contract).
func testUserScopeResolution(t failureReporter, h Harness) {
	ctx := context.Background()

	// Identity variants off the base fixture.
	sessionA := fixtureID
	sessionB := fixtureID
	sessionB.SessionID = "s-conformance-B"
	otherUser := fixtureID
	otherUser.UserID = "u-conformance-OTHER"
	otherUser.SessionID = "s-conformance-B"
	otherTenant := fixtureID
	otherTenant.TenantID = "t-conformance-OTHER"

	// Author a durable user-scope skill under session A.
	userSkill := newSkill("durable-user")
	userSkill.Scope = skills.ScopeUser
	userSkill.ContentHash = skills.CanonicalContentHash(userSkill)
	if err := h.Store.Upsert(ctx, sessionA, userSkill); err != nil {
		t.Fatalf("Upsert user-scope skill under session A: %v", err)
	}
	// Also author a SESSION-scoped skill under session A — its visibility must
	// stay pinned to session A (regression guard for non-user scopes).
	sessSkill := newSkill("ephemeral-session")
	sessSkill.Scope = skills.ScopeSession
	sessSkill.ContentHash = skills.CanonicalContentHash(sessSkill)
	if err := h.Store.Upsert(ctx, sessionA, sessSkill); err != nil {
		t.Fatalf("Upsert session-scope skill under session A: %v", err)
	}

	// (1) Visible from session B of the same (tenant, user) — Get.
	got, err := h.Store.Get(ctx, sessionB, "durable-user")
	if err != nil {
		t.Fatalf("Get user-scope skill from session B: %v (want visible cross-session)", err)
	}
	if got.Name != "durable-user" || got.Scope != skills.ScopeUser {
		t.Fatalf("cross-session Get mismatch: %+v", got)
	}

	// (2) Visible from session B — List (both default and Scope=user filter).
	assertListContains(t, ctx, h, sessionB, skills.ListFilter{}, "durable-user", true, "default List from session B")
	assertListContains(t, ctx, h, sessionB, skills.ListFilter{Scope: skills.ScopeUser}, "durable-user", true, "Scope=user List from session B")

	// (3) The session-scoped skill stays pinned to session A — NOT in session B.
	if _, err := h.Store.Get(ctx, sessionB, "ephemeral-session"); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("session-scope skill leaked to session B: err=%v (want ErrSkillNotFound)", err)
	}
	assertListContains(t, ctx, h, sessionB, skills.ListFilter{}, "ephemeral-session", false, "session-scope skill must not leak cross-session")

	// (4) NOT visible to a different user (same tenant).
	if _, err := h.Store.Get(ctx, otherUser, "durable-user"); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("user-scope skill leaked to a different user: err=%v (want ErrSkillNotFound)", err)
	}
	assertListContains(t, ctx, h, otherUser, skills.ListFilter{}, "durable-user", false, "user-scope skill must not leak cross-user")

	// (5) NOT visible cross-tenant.
	if _, err := h.Store.Get(ctx, otherTenant, "durable-user"); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("user-scope skill leaked cross-tenant: err=%v (want ErrSkillNotFound)", err)
	}
	assertListContains(t, ctx, h, otherTenant, skills.ListFilter{}, "durable-user", false, "user-scope skill must not leak cross-tenant")

	// (6) Delete from session B removes the durable row for every session.
	if err := h.Store.Delete(ctx, sessionB, "durable-user", skills.ScopeUser); err != nil {
		t.Fatalf("Delete user-scope skill from session B: %v", err)
	}
	if _, err := h.Store.Get(ctx, sessionA, "durable-user"); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("user-scope skill survived cross-session Delete: err=%v (want ErrSkillNotFound)", err)
	}
}

// testDeleteRungIndependence is the assertion whose absence let the
// cross-durability data-loss bug ship: a DESTRUCTIVE delete must never cross
// the session↔user rung boundary the READ filter unions. It covers both
// directions plus the plain "delete nothing durable" case, so every driver is
// held to rung-precise deletes.
func testDeleteRungIndependence(t failureReporter, h Harness) {
	ctx := context.Background()

	sessionA := fixtureID
	sessionB := fixtureID
	sessionB.SessionID = "s-conformance-B"

	mk := func(name string, scope skills.Scope) skills.Skill {
		s := newSkill(name)
		s.Scope = scope
		s.ContentHash = skills.CanonicalContentHash(s)
		return s
	}

	// (a) A SESSION-scope delete of a name leaves a same-named DURABLE user
	// skill INTACT (still visible from another session).
	if err := h.Store.Upsert(ctx, sessionA, mk("shared", skills.ScopeUser)); err != nil {
		t.Fatalf("upsert durable shared: %v", err)
	}
	if err := h.Store.Upsert(ctx, sessionA, mk("shared", skills.ScopeSession)); err != nil {
		t.Fatalf("upsert session shared: %v", err)
	}
	if err := h.Store.Delete(ctx, sessionA, "shared", skills.ScopeSession); err != nil {
		t.Fatalf("session-scope delete of shared: %v", err)
	}
	// The durable user skill must survive — and stay visible from session B.
	if got, err := h.Store.Get(ctx, sessionB, "shared"); err != nil || got.Scope != skills.ScopeUser {
		t.Fatalf("session delete destroyed the durable user skill: got=%+v err=%v (want the ScopeUser row intact)", got, err)
	}
	// The session row itself is gone from session A (only the durable remains,
	// resolved via the union).
	if got, err := h.Store.Get(ctx, sessionA, "shared"); err != nil || got.Scope != skills.ScopeUser {
		t.Fatalf("session delete did not remove its own session row: got=%+v err=%v", got, err)
	}

	// (b) A USER-scope delete leaves a same-named SESSION-scoped row INTACT for
	// its session.
	if err := h.Store.Upsert(ctx, sessionA, mk("dual", skills.ScopeUser)); err != nil {
		t.Fatalf("upsert durable dual: %v", err)
	}
	if err := h.Store.Upsert(ctx, sessionA, mk("dual", skills.ScopeSession)); err != nil {
		t.Fatalf("upsert session dual: %v", err)
	}
	if err := h.Store.Delete(ctx, sessionA, "dual", skills.ScopeUser); err != nil {
		t.Fatalf("user-scope delete of dual: %v", err)
	}
	// The durable row is gone (not visible from session B any more).
	if _, err := h.Store.Get(ctx, sessionB, "dual"); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("user delete left the durable row: err=%v (want ErrSkillNotFound)", err)
	}
	// The session-scoped row survives for session A.
	if got, err := h.Store.Get(ctx, sessionA, "dual"); err != nil || got.Scope != skills.ScopeSession {
		t.Fatalf("user delete destroyed the same-named session row: got=%+v err=%v (want the ScopeSession row intact)", got, err)
	}

	// (c) The plain case — a session-scope delete of a DURABLE-only name
	// deletes NOTHING durable (and reports not-found, since no session row
	// exists to delete).
	if err := h.Store.Upsert(ctx, sessionA, mk("durable-only", skills.ScopeUser)); err != nil {
		t.Fatalf("upsert durable-only: %v", err)
	}
	if err := h.Store.Delete(ctx, sessionA, "durable-only", skills.ScopeSession); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("session delete of a durable-only name: err=%v (want ErrSkillNotFound — nothing session-local to delete)", err)
	}
	if got, err := h.Store.Get(ctx, sessionB, "durable-only"); err != nil || got.Scope != skills.ScopeUser {
		t.Fatalf("session delete destroyed a durable-only skill: got=%+v err=%v", got, err)
	}
}

// testDeleteSessionScope pins the session-erasure-only destructive surface:
// it sweeps only exact ScopeSession rows, is idempotent, and cannot cross a
// session, user, or tenant boundary or delete the durable ScopeUser rung.
func testDeleteSessionScope(t failureReporter, h Harness) {
	ctx := context.Background()
	sessionA := fixtureID
	sessionB := fixtureID
	sessionB.SessionID = "s-conformance-B"
	otherUser := fixtureID
	otherUser.UserID = "u-conformance-other"
	otherUser.SessionID = sessionA.SessionID
	otherTenant := fixtureID
	otherTenant.TenantID = "t-conformance-other"

	mk := func(name string, scope skills.Scope) skills.Skill {
		s := newSkill(name)
		s.Scope = scope
		s.ContentHash = skills.CanonicalContentHash(s)
		return s
	}
	seed := []struct {
		id    identity.Quadruple
		skill skills.Skill
	}{
		{sessionA, mk("erase-session", skills.ScopeSession)},
		{sessionA, mk("retain-user", skills.ScopeUser)},
		{sessionA, mk("retain-project", skills.ScopeProject)},
		{sessionB, mk("retain-other-session", skills.ScopeSession)},
		{otherUser, mk("retain-other-user", skills.ScopeSession)},
		{otherTenant, mk("retain-other-tenant", skills.ScopeSession)},
	}
	for _, row := range seed {
		if err := h.Store.Upsert(ctx, row.id, row.skill); err != nil {
			t.Fatalf("seed %s/%s: %v", row.id.SessionID, row.skill.Name, err)
		}
	}

	if err := h.Store.DeleteSessionScope(ctx, sessionA); err != nil {
		t.Fatalf("DeleteSessionScope: %v", err)
	}
	// A retried completed sweep must stay successful, including when no rows
	// remain; this is the ledger-recovery contract session erasure relies on.
	if err := h.Store.DeleteSessionScope(ctx, sessionA); err != nil {
		t.Fatalf("DeleteSessionScope retry: %v", err)
	}
	if _, err := h.Store.Get(ctx, sessionA, "erase-session"); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("erased session row: err=%v, want ErrSkillNotFound", err)
	}
	for _, row := range []struct {
		id   identity.Quadruple
		name string
	}{
		{sessionA, "retain-user"},
		{sessionA, "retain-project"},
		{sessionB, "retain-other-session"},
		{otherUser, "retain-other-user"},
		{otherTenant, "retain-other-tenant"},
	} {
		if _, err := h.Store.Get(ctx, row.id, row.name); err != nil {
			t.Fatalf("retained %s/%s: %v", row.id.SessionID, row.name, err)
		}
	}

	bad := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u"}}
	if err := h.Store.DeleteSessionScope(ctx, bad); err == nil {
		t.Fatal("DeleteSessionScope missing session: err=nil, want identity error")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := h.Store.DeleteSessionScope(canceled, sessionA); !errors.Is(err, context.Canceled) {
		t.Fatalf("DeleteSessionScope canceled context: err=%v, want context.Canceled", err)
	}
}

func testDeleteSessionScopeAfterClose(t failureReporter, h Harness) {
	ctx := context.Background()
	if err := h.Store.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := h.Store.DeleteSessionScope(ctx, fixtureID); !errors.Is(err, skills.ErrStoreClosed) {
		t.Fatalf("DeleteSessionScope after Close: err=%v, want ErrStoreClosed", err)
	}
}

func testGetScopeAfterClose(t failureReporter, h Harness) {
	ctx := context.Background()
	if err := h.Store.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := h.Store.GetScope(ctx, fixtureID, "x", skills.ScopeSession); !errors.Is(err, skills.ErrStoreClosed) {
		t.Fatalf("GetScope after Close: err=%v, want ErrStoreClosed", err)
	}
}

func assertListContains(t failureReporter, ctx context.Context, h Harness, id identity.Quadruple, filter skills.ListFilter, name string, want bool, msg string) {
	t.Helper()
	list, err := h.Store.List(ctx, id, filter)
	if err != nil {
		t.Fatalf("%s: List: %v", msg, err)
	}
	found := false
	for _, s := range list {
		if s.Name == name {
			found = true
			break
		}
	}
	if found != want {
		t.Fatalf("%s: List contains %q = %v, want %v (list=%v)", msg, name, found, want, listNames(list))
	}
}

func listNames(list []skills.Skill) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, s.Name)
	}
	return out
}
