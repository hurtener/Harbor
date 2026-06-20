// Package conformance is the shared agentcfg.Registry driver conformance
// suite: a single set of behaviour assertions every driver must pass, so a
// future driver inherits the contract verbatim (the §11 conformance-suite
// pattern). The StateStore-backed driver is the first consumer.
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

// Run executes the full conformance suite against a driver built by mk.
func Run(t *testing.T, mk Factory) {
	t.Helper()
	t.Run("SetThenActiveThenGet", func(t *testing.T) { testSetActiveGet(t, mk) })
	t.Run("RevisionImmutability", func(t *testing.T) { testImmutability(t, mk) })
	t.Run("IdempotentReset", func(t *testing.T) { testIdempotentReset(t, mk) })
	t.Run("ParentChain", func(t *testing.T) { testParentChain(t, mk) })
	t.Run("ListNewestFirst", func(t *testing.T) { testListNewestFirst(t, mk) })
	t.Run("RollbackRepoints", func(t *testing.T) { testRollback(t, mk) })
	t.Run("RollbackMissingFailsLoud", func(t *testing.T) { testRollbackMissing(t, mk) })
	t.Run("DiffDeterministic", func(t *testing.T) { testDiff(t, mk) })
	t.Run("GetMissingFailsLoud", func(t *testing.T) { testGetMissing(t, mk) })
	t.Run("IdentityRequired", func(t *testing.T) { testIdentityRequired(t, mk) })
}

func testSetActiveGet(t *testing.T, mk Factory) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	if _, ok, err := mustActive(t, r, id); err != nil || ok {
		t.Fatalf("Active on empty: ok=%v err=%v", ok, err)
	}
	rev, err := r.SetRevision(ctx, id, agentID, skillsPayload("a", "b"))
	if err != nil {
		t.Fatalf("SetRevision: %v", err)
	}
	if rev.RevisionID == "" || rev.ContentHash == "" {
		t.Fatalf("revision missing id/hash: %+v", rev)
	}
	if rev.ParentRevisionID != "" {
		t.Fatalf("first revision parent should be empty, got %q", rev.ParentRevisionID)
	}
	got, ok, err := r.Active(ctx, id, agentID)
	if err != nil || !ok {
		t.Fatalf("Active after set: ok=%v err=%v", ok, err)
	}
	if got.RevisionID != rev.RevisionID {
		t.Fatalf("Active id=%q want %q", got.RevisionID, rev.RevisionID)
	}
	byID, err := r.Get(ctx, id, agentID, rev.RevisionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if byID.ContentHash != rev.ContentHash {
		t.Fatalf("Get hash=%q want %q", byID.ContentHash, rev.ContentHash)
	}
}

func testImmutability(t *testing.T, mk Factory) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	rev1, err := r.SetRevision(ctx, id, agentID, skillsPayload("a"))
	if err != nil {
		t.Fatalf("set rev1: %v", err)
	}
	rev2, err := r.SetRevision(ctx, id, agentID, skillsPayload("a", "b"))
	if err != nil {
		t.Fatalf("set rev2: %v", err)
	}
	if rev1.RevisionID == rev2.RevisionID {
		t.Fatalf("second set must mint a new revision id")
	}
	// rev1 is unchanged after rev2.
	got1, err := r.Get(ctx, id, agentID, rev1.RevisionID)
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

func testIdempotentReset(t *testing.T, mk Factory) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	// Different name order must canonicalise to the same content → no-op.
	rev1, err := r.SetRevision(ctx, id, agentID, skillsPayload("b", "a"))
	if err != nil {
		t.Fatalf("set rev1: %v", err)
	}
	rev2, err := r.SetRevision(ctx, id, agentID, skillsPayload("a", "b"))
	if err != nil {
		t.Fatalf("set rev2: %v", err)
	}
	if rev1.RevisionID != rev2.RevisionID {
		t.Fatalf("idempotent re-set should return existing id: %q != %q", rev1.RevisionID, rev2.RevisionID)
	}
	revs, err := r.ListRevisions(ctx, id, agentID, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(revs) != 1 {
		t.Fatalf("idempotent re-set must not add a revision: got %d", len(revs))
	}
}

func testParentChain(t *testing.T, mk Factory) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	rev1 := mustSet(t, r, id, skillsPayload("a"))
	rev2, err := r.SetRevision(ctx, id, agentID, skillsPayload("a", "b"))
	if err != nil {
		t.Fatalf("set rev2: %v", err)
	}
	if rev2.ParentRevisionID != rev1.RevisionID {
		t.Fatalf("rev2 parent=%q want %q", rev2.ParentRevisionID, rev1.RevisionID)
	}
}

func testListNewestFirst(t *testing.T, mk Factory) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	rev1 := mustSet(t, r, id, skillsPayload("a"))
	rev2 := mustSet(t, r, id, skillsPayload("a", "b"))
	rev3 := mustSet(t, r, id, skillsPayload("a", "b", "c"))
	revs, err := r.ListRevisions(ctx, id, agentID, 0)
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
	capped, err := r.ListRevisions(ctx, id, agentID, 2)
	if err != nil {
		t.Fatalf("list limit: %v", err)
	}
	if len(capped) != 2 {
		t.Fatalf("limit 2 returned %d", len(capped))
	}
}

func testRollback(t *testing.T, mk Factory) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	rev1 := mustSet(t, r, id, skillsPayload("a"))
	rev2 := mustSet(t, r, id, skillsPayload("a", "b"))
	back, err := r.Rollback(ctx, id, agentID, rev1.RevisionID)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if back.RevisionID != rev1.RevisionID {
		t.Fatalf("rollback returned %q want %q", back.RevisionID, rev1.RevisionID)
	}
	active, ok, err := r.Active(ctx, id, agentID)
	if err != nil || !ok {
		t.Fatalf("active after rollback: ok=%v err=%v", ok, err)
	}
	if active.RevisionID != rev1.RevisionID {
		t.Fatalf("active=%q want %q after rollback", active.RevisionID, rev1.RevisionID)
	}
	// rev2 is still present and unmutated (rollback never deletes).
	got2, err := r.Get(ctx, id, agentID, rev2.RevisionID)
	if err != nil {
		t.Fatalf("rev2 deleted by rollback: %v", err)
	}
	if len(got2.Payload.SkillNames()) != 2 {
		t.Fatalf("rev2 mutated by rollback: %+v", got2.Payload.SkillNames())
	}
	revs, err := r.ListRevisions(ctx, id, agentID, 0)
	if err != nil {
		t.Fatalf("list after rollback: %v", err)
	}
	if len(revs) != 2 {
		t.Fatalf("rollback must not add/remove revisions: got %d", len(revs))
	}
}

func testRollbackMissing(t *testing.T, mk Factory) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	mustSet(t, r, id, skillsPayload("a"))
	_, err := r.Rollback(ctx, id, agentID, "01ZZZNONEXISTENT")
	if !errors.Is(err, agentcfg.ErrRevisionNotFound) {
		t.Fatalf("rollback to missing should be ErrRevisionNotFound, got %v", err)
	}
}

func testDiff(t *testing.T, mk Factory) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	rev1 := mustSet(t, r, id, skillsPayload("a", "b"))
	rev2 := mustSet(t, r, id, skillsPayload("b", "c"))
	d, err := r.Diff(ctx, id, agentID, rev1.RevisionID, rev2.RevisionID)
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
	d2, err := r.Diff(ctx, id, agentID, rev1.RevisionID, rev2.RevisionID)
	if err != nil {
		t.Fatalf("diff2: %v", err)
	}
	if len(d2.Skills.Added) != 1 || d2.Skills.Added[0] != "c" {
		t.Fatalf("diff not deterministic: %v", d2.Skills.Added)
	}
}

func testGetMissing(t *testing.T, mk Factory) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	_, err := r.Get(ctx, id, agentID, "01ZZZNONEXISTENT")
	if !errors.Is(err, agentcfg.ErrRevisionNotFound) {
		t.Fatalf("get missing should be ErrRevisionNotFound, got %v", err)
	}
}

func testIdentityRequired(t *testing.T, mk Factory) {
	ctx := context.Background()
	r := mk(t)
	incomplete := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a"}}
	if _, err := r.SetRevision(ctx, incomplete, agentID, skillsPayload("a")); !errors.Is(err, agentcfg.ErrIdentityRequired) {
		t.Fatalf("incomplete identity should be ErrIdentityRequired, got %v", err)
	}
	// Empty agent id also rejected.
	if _, err := r.SetRevision(ctx, admin(), "", skillsPayload("a")); !errors.Is(err, agentcfg.ErrIdentityRequired) {
		t.Fatalf("empty agent id should be ErrIdentityRequired, got %v", err)
	}
}

// mustSet sets a revision and fails the test on error.
func mustSet(t *testing.T, r agentcfg.Registry, id identity.Quadruple, payload agentcfg.ConfigPayload) agentcfg.Revision {
	t.Helper()
	rev, err := r.SetRevision(context.Background(), id, agentID, payload)
	if err != nil {
		t.Fatalf("SetRevision: %v", err)
	}
	return rev
}

func mustActive(t *testing.T, r agentcfg.Registry, id identity.Quadruple) (agentcfg.Revision, bool, error) {
	t.Helper()
	return r.Active(context.Background(), id, agentID)
}
