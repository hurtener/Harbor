// Package conformance is the shared agentcfg.Registry driver conformance
// suite: a single set of behaviour assertions every driver must pass, so a
// future driver inherits the contract verbatim (the §11 conformance-suite
// pattern). The StateStore-backed driver is the first consumer.
//
// The full revision/diff/rollback matrix runs under BOTH ConfigScope arms
// (ConfigScopeAgent and ConfigScopeUser), asserting parity AND cross-scope
// invisibility: a revision id minted under one scope is not resolvable under
// the other (the distinct record-kind prefix guarantees this). The user
// variant is keyed by the caller's REAL (tenant, user), so the conformance
// identity (a complete, non-sentinel triple) keys both arms correctly.
package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
)

// Factory builds a fresh, empty Registry for one sub-test. The returned
// cleanup is called via t.Cleanup by Run.
type Factory func(t *testing.T) agentcfg.Registry

// admin returns a complete admin caller identity.
func admin() identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{
		TenantID: "tenant-a", UserID: "admin-1", SessionID: "sess-admin",
	}}
}

const agentID = "agent-x"

func skillsPayload(names ...string) agentcfg.ConfigPayload {
	return agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: names}}
}

// Run executes the full conformance suite against a driver built by mk,
// under both ConfigScope arms, plus the cross-scope invisibility assertion.
func Run(t *testing.T, mk Factory) {
	t.Helper()
	for _, sc := range []struct {
		name  string
		scope agentcfg.ConfigScope
	}{
		{"ScopeAgent", agentcfg.ConfigScopeAgent},
		{"ScopeUser", agentcfg.ConfigScopeUser},
	} {
		scope := sc.scope
		t.Run(sc.name, func(t *testing.T) {
			t.Run("SetThenActiveThenGet", func(t *testing.T) { testSetActiveGet(t, mk, scope) })
			t.Run("RevisionImmutability", func(t *testing.T) { testImmutability(t, mk, scope) })
			t.Run("IdempotentReset", func(t *testing.T) { testIdempotentReset(t, mk, scope) })
			t.Run("ParentChain", func(t *testing.T) { testParentChain(t, mk, scope) })
			t.Run("ListNewestFirst", func(t *testing.T) { testListNewestFirst(t, mk, scope) })
			t.Run("RollbackRepoints", func(t *testing.T) { testRollback(t, mk, scope) })
			t.Run("RollbackMissingFailsLoud", func(t *testing.T) { testRollbackMissing(t, mk, scope) })
			t.Run("DiffDeterministic", func(t *testing.T) { testDiff(t, mk, scope) })
			t.Run("GetMissingFailsLoud", func(t *testing.T) { testGetMissing(t, mk, scope) })
			t.Run("IdentityRequired", func(t *testing.T) { testIdentityRequired(t, mk, scope) })
			t.Run("LLMParamsRoundTrip", func(t *testing.T) { testLLMParamsRoundTrip(t, mk, scope) })
		})
	}
	t.Run("CrossScopeInvisibility", func(t *testing.T) { testCrossScopeInvisibility(t, mk) })
}

// testCrossScopeInvisibility proves the two key spaces never alias: a
// revision id minted under one scope is not resolvable under the other, and
// each scope's active pointer is independent.
func testCrossScopeInvisibility(t *testing.T, mk Factory) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	agentRev, err := r.SetRevision(ctx, id, agentID, agentcfg.ConfigScopeAgent, skillsPayload("agent-only"))
	if err != nil {
		t.Fatalf("set agent: %v", err)
	}
	userRev, err := r.SetRevision(ctx, id, agentID, agentcfg.ConfigScopeUser, skillsPayload("user-only"))
	if err != nil {
		t.Fatalf("set user: %v", err)
	}
	// The agent revision id is not resolvable under the user scope.
	if _, err := r.Get(ctx, id, agentID, agentRev.RevisionID, agentcfg.ConfigScopeUser); !errors.Is(err, agentcfg.ErrRevisionNotFound) {
		t.Fatalf("agent revision visible under user scope: %v", err)
	}
	// The user revision id is not resolvable under the agent scope.
	if _, err := r.Get(ctx, id, agentID, userRev.RevisionID, agentcfg.ConfigScopeAgent); !errors.Is(err, agentcfg.ErrRevisionNotFound) {
		t.Fatalf("user revision visible under agent scope: %v", err)
	}
	// Each scope's active pointer is independent and carries its own payload.
	agentActive, ok, err := r.Active(ctx, id, agentID, agentcfg.ConfigScopeAgent)
	if err != nil || !ok || len(agentActive.Payload.SkillNames()) != 1 || agentActive.Payload.SkillNames()[0] != "agent-only" {
		t.Fatalf("agent active wrong: ok=%v err=%v payload=%v", ok, err, agentActive.Payload.SkillNames())
	}
	userActive, ok, err := r.Active(ctx, id, agentID, agentcfg.ConfigScopeUser)
	if err != nil || !ok || len(userActive.Payload.SkillNames()) != 1 || userActive.Payload.SkillNames()[0] != "user-only" {
		t.Fatalf("user active wrong: ok=%v err=%v payload=%v", ok, err, userActive.Payload.SkillNames())
	}
}

// testLLMParamsRoundTrip proves every driver round-trips the per-agent
// LLM-parameters section through Set → Active → Get and surfaces its delta
// through Diff (driver parity for the new section).
func testLLMParamsRoundTrip(t *testing.T, mk Factory, scope agentcfg.ConfigScope) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	model := "model-x"
	temp := 0.4
	rev1 := mustSet(t, r, id, scope, agentcfg.ConfigPayload{
		LLMParams: &agentcfg.LLMParams{Model: &model, Temperature: &temp},
	})
	got, ok, err := mustActive(t, r, id, scope)
	if err != nil || !ok {
		t.Fatalf("Active: ok=%v err=%v", ok, err)
	}
	if got.Payload.LLMParams == nil || got.Payload.LLMParams.Model == nil || *got.Payload.LLMParams.Model != "model-x" {
		t.Fatalf("LLM-params not round-tripped: %+v", got.Payload.LLMParams)
	}
	if got.Payload.LLMParams.Temperature == nil || *got.Payload.LLMParams.Temperature != 0.4 {
		t.Fatalf("temperature not round-tripped: %+v", got.Payload.LLMParams)
	}
	// A second revision changing the model surfaces the delta via Diff.
	model2 := "model-y"
	rev2 := mustSet(t, r, id, scope, agentcfg.ConfigPayload{
		LLMParams: &agentcfg.LLMParams{Model: &model2, Temperature: &temp},
	})
	d, err := r.Diff(ctx, id, agentID, rev1.RevisionID, rev2.RevisionID, scope)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !d.LLMParams.ModelChanged || d.LLMParams.ModelFrom != "model-x" || d.LLMParams.ModelTo != "model-y" {
		t.Fatalf("LLM-params diff = %+v", d.LLMParams)
	}
}

func testSetActiveGet(t *testing.T, mk Factory, scope agentcfg.ConfigScope) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	if _, ok, err := mustActive(t, r, id, scope); err != nil || ok {
		t.Fatalf("Active on empty: ok=%v err=%v", ok, err)
	}
	rev, err := r.SetRevision(ctx, id, agentID, scope, skillsPayload("a", "b"))
	if err != nil {
		t.Fatalf("SetRevision: %v", err)
	}
	if rev.RevisionID == "" || rev.ContentHash == "" {
		t.Fatalf("revision missing id/hash: %+v", rev)
	}
	if rev.ParentRevisionID != "" {
		t.Fatalf("first revision parent should be empty, got %q", rev.ParentRevisionID)
	}
	got, ok, err := r.Active(ctx, id, agentID, scope)
	if err != nil || !ok {
		t.Fatalf("Active after set: ok=%v err=%v", ok, err)
	}
	if got.RevisionID != rev.RevisionID {
		t.Fatalf("Active id=%q want %q", got.RevisionID, rev.RevisionID)
	}
	byID, err := r.Get(ctx, id, agentID, rev.RevisionID, scope)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if byID.ContentHash != rev.ContentHash {
		t.Fatalf("Get hash=%q want %q", byID.ContentHash, rev.ContentHash)
	}
}

func testImmutability(t *testing.T, mk Factory, scope agentcfg.ConfigScope) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	rev1, err := r.SetRevision(ctx, id, agentID, scope, skillsPayload("a"))
	if err != nil {
		t.Fatalf("set rev1: %v", err)
	}
	rev2, err := r.SetRevision(ctx, id, agentID, scope, skillsPayload("a", "b"))
	if err != nil {
		t.Fatalf("set rev2: %v", err)
	}
	if rev1.RevisionID == rev2.RevisionID {
		t.Fatalf("second set must mint a new revision id")
	}
	// rev1 is unchanged after rev2.
	got1, err := r.Get(ctx, id, agentID, rev1.RevisionID, scope)
	if err != nil {
		t.Fatalf("get rev1 after rev2: %v", err)
	}
	if len(got1.Payload.SkillNames()) != 1 || got1.Payload.SkillNames()[0] != "a" {
		t.Fatalf("rev1 mutated: %+v", got1.Payload.SkillNames())
	}
	if got1.ContentHash != rev1.ContentHash {
		t.Fatalf("rev1 hash changed after rev2")
	}
}

func testIdempotentReset(t *testing.T, mk Factory, scope agentcfg.ConfigScope) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	// Different name order must canonicalise to the same content → no-op.
	rev1, err := r.SetRevision(ctx, id, agentID, scope, skillsPayload("b", "a"))
	if err != nil {
		t.Fatalf("set rev1: %v", err)
	}
	rev2, err := r.SetRevision(ctx, id, agentID, scope, skillsPayload("a", "b"))
	if err != nil {
		t.Fatalf("set rev2: %v", err)
	}
	if rev1.RevisionID != rev2.RevisionID {
		t.Fatalf("idempotent re-set should return existing id: %q != %q", rev1.RevisionID, rev2.RevisionID)
	}
	revs, err := r.ListRevisions(ctx, id, agentID, scope, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(revs) != 1 {
		t.Fatalf("idempotent re-set must not add a revision: got %d", len(revs))
	}
}

func testParentChain(t *testing.T, mk Factory, scope agentcfg.ConfigScope) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	rev1 := mustSet(t, r, id, scope, skillsPayload("a"))
	rev2, err := r.SetRevision(ctx, id, agentID, scope, skillsPayload("a", "b"))
	if err != nil {
		t.Fatalf("set rev2: %v", err)
	}
	if rev2.ParentRevisionID != rev1.RevisionID {
		t.Fatalf("rev2 parent=%q want %q", rev2.ParentRevisionID, rev1.RevisionID)
	}
}

func testListNewestFirst(t *testing.T, mk Factory, scope agentcfg.ConfigScope) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	rev1 := mustSet(t, r, id, scope, skillsPayload("a"))
	rev2 := mustSet(t, r, id, scope, skillsPayload("a", "b"))
	rev3 := mustSet(t, r, id, scope, skillsPayload("a", "b", "c"))
	revs, err := r.ListRevisions(ctx, id, agentID, scope, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(revs) != 3 {
		t.Fatalf("want 3 revisions, got %d", len(revs))
	}
	if revs[0].RevisionID != rev3.RevisionID || revs[2].RevisionID != rev1.RevisionID {
		t.Fatalf("not newest-first: %q .. %q (want %q .. %q)", revs[0].RevisionID, revs[2].RevisionID, rev3.RevisionID, rev1.RevisionID)
	}
	_ = rev2
	// Limit caps.
	capped, err := r.ListRevisions(ctx, id, agentID, scope, 2)
	if err != nil {
		t.Fatalf("list limit: %v", err)
	}
	if len(capped) != 2 {
		t.Fatalf("limit 2 returned %d", len(capped))
	}
}

func testRollback(t *testing.T, mk Factory, scope agentcfg.ConfigScope) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	rev1 := mustSet(t, r, id, scope, skillsPayload("a"))
	rev2 := mustSet(t, r, id, scope, skillsPayload("a", "b"))
	back, err := r.Rollback(ctx, id, agentID, rev1.RevisionID, scope)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if back.RevisionID != rev1.RevisionID {
		t.Fatalf("rollback returned %q want %q", back.RevisionID, rev1.RevisionID)
	}
	active, ok, err := r.Active(ctx, id, agentID, scope)
	if err != nil || !ok {
		t.Fatalf("active after rollback: ok=%v err=%v", ok, err)
	}
	if active.RevisionID != rev1.RevisionID {
		t.Fatalf("active=%q want %q after rollback", active.RevisionID, rev1.RevisionID)
	}
	// rev2 is still present and unmutated (rollback never deletes).
	got2, err := r.Get(ctx, id, agentID, rev2.RevisionID, scope)
	if err != nil {
		t.Fatalf("rev2 deleted by rollback: %v", err)
	}
	if len(got2.Payload.SkillNames()) != 2 {
		t.Fatalf("rev2 mutated by rollback: %+v", got2.Payload.SkillNames())
	}
	revs, err := r.ListRevisions(ctx, id, agentID, scope, 0)
	if err != nil {
		t.Fatalf("list after rollback: %v", err)
	}
	if len(revs) != 2 {
		t.Fatalf("rollback must not add/remove revisions: got %d", len(revs))
	}
}

func testRollbackMissing(t *testing.T, mk Factory, scope agentcfg.ConfigScope) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	mustSet(t, r, id, scope, skillsPayload("a"))
	_, err := r.Rollback(ctx, id, agentID, "01ZZZNONEXISTENT", scope)
	if !errors.Is(err, agentcfg.ErrRevisionNotFound) {
		t.Fatalf("rollback to missing should be ErrRevisionNotFound, got %v", err)
	}
}

func testDiff(t *testing.T, mk Factory, scope agentcfg.ConfigScope) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	rev1 := mustSet(t, r, id, scope, skillsPayload("a", "b"))
	rev2 := mustSet(t, r, id, scope, skillsPayload("b", "c"))
	d, err := r.Diff(ctx, id, agentID, rev1.RevisionID, rev2.RevisionID, scope)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(d.Skills.Added) != 1 || d.Skills.Added[0] != "c" {
		t.Fatalf("added=%v want [c]", d.Skills.Added)
	}
	if len(d.Skills.Removed) != 1 || d.Skills.Removed[0] != "a" {
		t.Fatalf("removed=%v want [a]", d.Skills.Removed)
	}
	// Deterministic: a second diff returns the same shape.
	d2, err := r.Diff(ctx, id, agentID, rev1.RevisionID, rev2.RevisionID, scope)
	if err != nil {
		t.Fatalf("diff2: %v", err)
	}
	if len(d2.Skills.Added) != 1 || d2.Skills.Added[0] != "c" {
		t.Fatalf("diff not deterministic: %v", d2.Skills.Added)
	}
}

func testGetMissing(t *testing.T, mk Factory, scope agentcfg.ConfigScope) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	_, err := r.Get(ctx, id, agentID, "01ZZZNONEXISTENT", scope)
	if !errors.Is(err, agentcfg.ErrRevisionNotFound) {
		t.Fatalf("get missing should be ErrRevisionNotFound, got %v", err)
	}
}

func testIdentityRequired(t *testing.T, mk Factory, scope agentcfg.ConfigScope) {
	ctx := context.Background()
	r := mk(t)
	incomplete := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a"}}
	if _, err := r.SetRevision(ctx, incomplete, agentID, scope, skillsPayload("a")); !errors.Is(err, agentcfg.ErrIdentityRequired) {
		t.Fatalf("incomplete identity should be ErrIdentityRequired, got %v", err)
	}
	// Empty agent id also rejected.
	if _, err := r.SetRevision(ctx, admin(), "", scope, skillsPayload("a")); !errors.Is(err, agentcfg.ErrIdentityRequired) {
		t.Fatalf("empty agent id should be ErrIdentityRequired, got %v", err)
	}
}

// mustSet sets a revision and fails the test on error.
func mustSet(t *testing.T, r agentcfg.Registry, id identity.Quadruple, scope agentcfg.ConfigScope, payload agentcfg.ConfigPayload) agentcfg.Revision {
	t.Helper()
	rev, err := r.SetRevision(context.Background(), id, agentID, scope, payload)
	if err != nil {
		t.Fatalf("SetRevision: %v", err)
	}
	return rev
}

func mustActive(t *testing.T, r agentcfg.Registry, id identity.Quadruple, scope agentcfg.ConfigScope) (agentcfg.Revision, bool, error) {
	t.Helper()
	return r.Active(context.Background(), id, agentID, scope)
}
