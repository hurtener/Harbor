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

// FaultFactory builds a fresh, empty Registry whose backing store is ARMED
// to fail the write that publishes a revision as the ACTIVE one, while the
// write that persists the revision record itself still succeeds.
//
// It is a second, mandatory constructor rather than an optional capability
// (CLAUDE.md §4.4 — no `Supports*` ceremony) because the invariant it feeds
// is one every driver owes: a write that does not complete leaves NO
// revision behind. A driver whose two writes ARE one atomic operation arms
// the fault on that operation instead and passes the same row unchanged —
// the assertion is about the residue, not about how many writes there were.
//
// It is a separate parameter of Run rather than a method on Factory so a
// second driver cannot COMPILE without supplying one: the mechanism a driver
// uses to leave an orphan is driver-specific (record kinds, table names),
// so only the driver's own test can arm it.
type FaultFactory func(t *testing.T) agentcfg.Registry

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
// under both ConfigScope arms, plus the cross-scope invisibility assertion
// and the partial-write atomicity row (driven by mkFaulty).
func Run(t *testing.T, mk Factory, mkFaulty FaultFactory) {
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
			// The additive prompt blocks: ORDER is semantic, so every driver
			// must return them in the declared order and must report a pure
			// re-ordering as a real change. A driver that persisted them
			// through a map (or sorted them) passes a naive round-trip and
			// fails this.
			t.Run("ExtraSystemBlocksRoundTripInOrder", func(t *testing.T) { testExtraSystemBlocksRoundTrip(t, mk, scope) })
			// The expected-revision precondition. These four rows live in the
			// SHARED suite rather than the statestore driver's own tests
			// precisely so a second driver cannot ship the Registry interface
			// without the precondition (RFC §9 conformance parity).
			t.Run("ConditionalWrite_MatchingHashProceeds", func(t *testing.T) { testConditionalMatch(t, mk, scope) })
			t.Run("ConditionalWrite_MismatchRefusedAndPersistsNothing", func(t *testing.T) { testConditionalMismatch(t, mk, scope) })
			t.Run("ConditionalWrite_NoActiveRevisionRefused", func(t *testing.T) { testConditionalNoActive(t, mk, scope) })
			t.Run("ConditionalWrite_EmptyTokenIsUnconditional", func(t *testing.T) { testConditionalEmptyToken(t, mk, scope) })
			// The FIRST-WRITE token. Without it the read-modify-write
			// composition protocol has no expressible form at its base case
			// (a read that answers "no config" has no hash to echo), so two
			// first contributors silently revert each other — a lost update
			// on the one write the token exists to protect. Registered here,
			// beside the four rows above, so a second driver inherits it.
			t.Run("ConditionalWrite_FirstWriteSentinel", func(t *testing.T) { testConditionalFirstWrite(t, mk, scope) })
			t.Run("ConditionalWrite_FirstWriteSentinelRefusedOnceSet", func(t *testing.T) { testConditionalFirstWriteRefused(t, mk, scope) })
			t.Run("ConditionalWrite_FirstWriteSentinelPreventsLostUpdate", func(t *testing.T) { testConditionalFirstWriteLostUpdate(t, mk, scope) })
			// The PARTIAL-WRITE row. A write that does not complete must
			// leave no revision behind. It lives in the SHARED suite for the
			// same reason the precondition rows do: the invariant is owed by
			// the interface, not by one driver's implementation of it.
			t.Run("WriteAtomicity_FailedPointerWriteLeavesNoOrphanRevision", func(t *testing.T) {
				testWriteAtomicity(t, mkFaulty, scope)
			})
		})
	}
	t.Run("CrossScopeInvisibility", func(t *testing.T) { testCrossScopeInvisibility(t, mk) })
}

// testWriteAtomicity — a store error between the revision write and the
// active-pointer write leaves NO revision in history.
//
// The failure this pins is not a reader-correctness one: the pointer is the
// source of truth, so an orphaned revision is invisible to Active and to the
// run-start projection, and its content was never applied. It is an OPERATOR
// one. `list_revisions` enumerates by record kind rather than by walking the
// parent chain, so an orphan surfaces in history between two real revisions,
// belonging to no chain and never having been active — which reads exactly
// like a write that was lost. It also burns a revision id nothing references.
//
// The row asserts three things together, because any one of them alone stays
// green through the defect: the write FAILS (a driver that silently succeeded
// would be worse), the active pointer did NOT move, and history is EMPTY.
func testWriteAtomicity(t *testing.T, mk FaultFactory, scope agentcfg.ConfigScope) {
	ctx := context.Background()
	r := mk(t)
	id := admin()

	if _, err := r.SetRevision(ctx, id, agentID, scope, skillsPayload("orphan-a"), agentcfg.SetOptions{}); err == nil {
		t.Fatal("SetRevision reported success against a store armed to fail the active-pointer write")
	}

	rev, ok, err := r.Active(ctx, id, agentID, scope)
	if err != nil {
		t.Fatalf("Active after a failed write: %v", err)
	}
	if ok {
		t.Fatalf("the active pointer moved despite the failed write: revision %s", rev.RevisionID)
	}

	revs, err := r.ListRevisions(ctx, id, agentID, scope, 0)
	if err != nil {
		t.Fatalf("ListRevisions after a failed write: %v", err)
	}
	if len(revs) != 0 {
		t.Fatalf("a failed write left %d revision(s) in history: %+v — an unreferenced revision reads to an operator as a lost write", len(revs), revs)
	}
}

// testCrossScopeInvisibility proves the two key spaces never alias: a
// revision id minted under one scope is not resolvable under the other, and
// each scope's active pointer is independent.
func testCrossScopeInvisibility(t *testing.T, mk Factory) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	agentRev, err := r.SetRevision(ctx, id, agentID, agentcfg.ConfigScopeAgent, skillsPayload("agent-only"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("set agent: %v", err)
	}
	userRev, err := r.SetRevision(ctx, id, agentID, agentcfg.ConfigScopeUser, skillsPayload("user-only"), agentcfg.SetOptions{})
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

// testExtraSystemBlocksRoundTrip proves every driver round-trips the
// ORDERED additive prompt blocks through Set → Active in their DECLARED
// order, and that a pure re-ordering of the same blocks is a REAL new
// revision (a different content hash) whose Diff reports Reordered — the
// property that distinguishes this section from its sorted siblings.
func testExtraSystemBlocksRoundTrip(t *testing.T, mk Factory, scope agentcfg.ConfigScope) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	forward := agentcfg.ConfigPayload{ExtraSystemBlocks: &agentcfg.ExtraSystemBlocks{Blocks: []agentcfg.NamedBlock{
		{Name: "alpha", Body: "first"},
		{Name: "beta", Body: "second"},
	}}}
	rev1 := mustSet(t, r, id, scope, forward)

	got, ok, err := mustActive(t, r, id, scope)
	if err != nil || !ok {
		t.Fatalf("Active: ok=%v err=%v", ok, err)
	}
	blocks := got.Payload.ExtraSystemBlockList()
	if len(blocks) != 2 || blocks[0].Name != "alpha" || blocks[1].Name != "beta" {
		t.Fatalf("blocks not round-tripped in declared order: %+v", blocks)
	}
	if blocks[0].Body != "first" || blocks[1].Body != "second" {
		t.Fatalf("block bodies not round-tripped: %+v", blocks)
	}

	// A pure re-ordering must be a NEW revision, not an idempotent no-op.
	reversed := agentcfg.ConfigPayload{ExtraSystemBlocks: &agentcfg.ExtraSystemBlocks{Blocks: []agentcfg.NamedBlock{
		{Name: "beta", Body: "second"},
		{Name: "alpha", Body: "first"},
	}}}
	rev2 := mustSet(t, r, id, scope, reversed)
	if rev2.RevisionID == rev1.RevisionID {
		t.Fatalf("a re-ordering was treated as an idempotent re-set — block order is the RENDER order and must change the content hash")
	}
	if rev2.ContentHash == rev1.ContentHash {
		t.Fatalf("a re-ordering produced the same content hash %q — the canonical form is sorting the blocks", rev1.ContentHash)
	}
	d, err := r.Diff(ctx, id, agentID, rev1.RevisionID, rev2.RevisionID, scope)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !d.ExtraSystemBlocks.Reordered {
		t.Fatalf("Diff did not report the re-ordering: %+v", d.ExtraSystemBlocks)
	}
	if len(d.ExtraSystemBlocks.Added) != 0 || len(d.ExtraSystemBlocks.Removed) != 0 || len(d.ExtraSystemBlocks.Changed) != 0 {
		t.Fatalf("a pure re-ordering reported a membership change: %+v", d.ExtraSystemBlocks)
	}
}

func testSetActiveGet(t *testing.T, mk Factory, scope agentcfg.ConfigScope) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	if _, ok, err := mustActive(t, r, id, scope); err != nil || ok {
		t.Fatalf("Active on empty: ok=%v err=%v", ok, err)
	}
	rev, err := r.SetRevision(ctx, id, agentID, scope, skillsPayload("a", "b"), agentcfg.SetOptions{})
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
	rev1, err := r.SetRevision(ctx, id, agentID, scope, skillsPayload("a"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("set rev1: %v", err)
	}
	rev2, err := r.SetRevision(ctx, id, agentID, scope, skillsPayload("a", "b"), agentcfg.SetOptions{})
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
	rev1, err := r.SetRevision(ctx, id, agentID, scope, skillsPayload("b", "a"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("set rev1: %v", err)
	}
	rev2, err := r.SetRevision(ctx, id, agentID, scope, skillsPayload("a", "b"), agentcfg.SetOptions{})
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
	rev2, err := r.SetRevision(ctx, id, agentID, scope, skillsPayload("a", "b"), agentcfg.SetOptions{})
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
	back, err := r.Rollback(ctx, id, agentID, rev1.RevisionID, scope, agentcfg.SetOptions{})
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
	_, err := r.Rollback(ctx, id, agentID, "01ZZZNONEXISTENT", scope, agentcfg.SetOptions{})
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
	if _, err := r.SetRevision(ctx, incomplete, agentID, scope, skillsPayload("a"), agentcfg.SetOptions{}); !errors.Is(err, agentcfg.ErrIdentityRequired) {
		t.Fatalf("incomplete identity should be ErrIdentityRequired, got %v", err)
	}
	// Empty agent id also rejected.
	if _, err := r.SetRevision(ctx, admin(), "", scope, skillsPayload("a"), agentcfg.SetOptions{}); !errors.Is(err, agentcfg.ErrIdentityRequired) {
		t.Fatalf("empty agent id should be ErrIdentityRequired, got %v", err)
	}
}

// mustSet sets a revision and fails the test on error.
func mustSet(t *testing.T, r agentcfg.Registry, id identity.Quadruple, scope agentcfg.ConfigScope, payload agentcfg.ConfigPayload) agentcfg.Revision {
	t.Helper()
	rev, err := r.SetRevision(context.Background(), id, agentID, scope, payload, agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("SetRevision: %v", err)
	}
	return rev
}

func mustActive(t *testing.T, r agentcfg.Registry, id identity.Quadruple, scope agentcfg.ConfigScope) (agentcfg.Revision, bool, error) {
	t.Helper()
	return r.Active(context.Background(), id, agentID, scope)
}

// ---------------------------------------------------------------------------
// The expected-revision precondition (SetOptions.ExpectedContentHash).
//
// Four rows, run against every driver under both scope arms. They pin the
// CONTRACT, not one driver's implementation of it: the token gates on the
// ACTIVE revision's content hash; a failed precondition persists nothing; a
// token against an agent with no active revision is a conflict; and the empty
// token is byte-for-byte the unconditional write.
//
// The atomicity these rows describe is bounded to a single process — the
// Registry has no store-level compare-and-swap to lean on (see
// agentcfg.SetOptions). The rows below therefore assert the DECISION the
// driver makes from the read it already performs; the racing-writers property
// is asserted separately by the driver's own concurrency test.
// ---------------------------------------------------------------------------

// testConditionalMatch — a token equal to the active revision's content hash
// lets the write through exactly as an unconditional write would.
func testConditionalMatch(t *testing.T, mk Factory, scope agentcfg.ConfigScope) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	base := mustSet(t, r, id, scope, skillsPayload("a"))

	next, err := r.SetRevision(ctx, id, agentID, scope, skillsPayload("a", "b"),
		agentcfg.SetOptions{ExpectedContentHash: base.ContentHash})
	if err != nil {
		t.Fatalf("matching token must proceed, got %v", err)
	}
	if next.RevisionID == base.RevisionID {
		t.Fatalf("matching conditional write must mint a new revision")
	}
	if next.ParentRevisionID != base.RevisionID {
		t.Fatalf("parent = %q, want %q", next.ParentRevisionID, base.RevisionID)
	}
	active, ok, err := r.Active(ctx, id, agentID, scope)
	if err != nil || !ok {
		t.Fatalf("Active after conditional write: ok=%v err=%v", ok, err)
	}
	if active.RevisionID != next.RevisionID {
		t.Fatalf("active = %q, want the conditional write's revision %q", active.RevisionID, next.RevisionID)
	}
}

// testConditionalMismatch — a token whose base has moved is refused with
// ErrRevisionConflict, and NOTHING is persisted: the revision chain does not
// grow and the active pointer does not move.
func testConditionalMismatch(t *testing.T, mk Factory, scope agentcfg.ConfigScope) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	stale := mustSet(t, r, id, scope, skillsPayload("a"))
	// A second writer moves the base out from under the token.
	current := mustSet(t, r, id, scope, skillsPayload("a", "b"))
	if current.ContentHash == stale.ContentHash {
		t.Fatalf("fixture broken: the second write did not change the content hash")
	}
	before, err := r.ListRevisions(ctx, id, agentID, scope, 0)
	if err != nil {
		t.Fatalf("list before: %v", err)
	}

	_, err = r.SetRevision(ctx, id, agentID, scope, skillsPayload("a", "c"),
		agentcfg.SetOptions{ExpectedContentHash: stale.ContentHash})
	if !errors.Is(err, agentcfg.ErrRevisionConflict) {
		t.Fatalf("stale token must fail ErrRevisionConflict, got %v", err)
	}

	after, err := r.ListRevisions(ctx, id, agentID, scope, 0)
	if err != nil {
		t.Fatalf("list after: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("a refused write persisted a revision: %d -> %d", len(before), len(after))
	}
	active, ok, err := r.Active(ctx, id, agentID, scope)
	if err != nil || !ok {
		t.Fatalf("Active after refusal: ok=%v err=%v", ok, err)
	}
	if active.RevisionID != current.RevisionID {
		t.Fatalf("a refused write moved the active pointer: %q, want %q", active.RevisionID, current.RevisionID)
	}
	// The pointer-move door refuses on the same predicate.
	if _, rerr := r.Rollback(ctx, id, agentID, stale.RevisionID, scope,
		agentcfg.SetOptions{ExpectedContentHash: stale.ContentHash}); !errors.Is(rerr, agentcfg.ErrRevisionConflict) {
		t.Fatalf("stale token on Rollback must fail ErrRevisionConflict, got %v", rerr)
	}
	active, ok, err = r.Active(ctx, id, agentID, scope)
	if err != nil || !ok || active.RevisionID != current.RevisionID {
		t.Fatalf("a refused rollback moved the active pointer: %q ok=%v err=%v", active.RevisionID, ok, err)
	}
}

// testConditionalNoActive — a HASH token supplied when the agent has NO
// active revision is a conflict: the caller claims to have read content that
// does not exist. ("I expect none" is expressible, but only through the
// reserved agentcfg.ExpectNoActiveRevision sentinel — see
// testConditionalFirstWrite.)
func testConditionalNoActive(t *testing.T, mk Factory, scope agentcfg.ConfigScope) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	if _, ok, err := r.Active(ctx, id, agentID, scope); err != nil || ok {
		t.Fatalf("fixture broken: expected no active revision, ok=%v err=%v", ok, err)
	}

	_, err := r.SetRevision(ctx, id, agentID, scope, skillsPayload("a"),
		agentcfg.SetOptions{ExpectedContentHash: absentHash})
	if !errors.Is(err, agentcfg.ErrRevisionConflict) {
		t.Fatalf("token against no active revision must fail ErrRevisionConflict, got %v", err)
	}
	if _, ok, aerr := r.Active(ctx, id, agentID, scope); aerr != nil || ok {
		t.Fatalf("a refused first write created an active revision: ok=%v err=%v", ok, aerr)
	}
	revs, err := r.ListRevisions(ctx, id, agentID, scope, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(revs) != 0 {
		t.Fatalf("a refused first write persisted %d revisions", len(revs))
	}
}

// testConditionalFirstWrite — the reserved sentinel succeeds exactly when the
// agent has no active revision, and it is a REAL precondition rather than a
// synonym for the empty token: the write lands, and the sentinel value never
// leaks into the recorded revision's own content hash.
func testConditionalFirstWrite(t *testing.T, mk Factory, scope agentcfg.ConfigScope) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	if _, ok, err := r.Active(ctx, id, agentID, scope); err != nil || ok {
		t.Fatalf("fixture broken: expected no active revision, ok=%v err=%v", ok, err)
	}

	first, err := r.SetRevision(ctx, id, agentID, scope, skillsPayload("a"),
		agentcfg.SetOptions{ExpectedContentHash: agentcfg.ExpectNoActiveRevision})
	if err != nil {
		t.Fatalf("the first-write sentinel must succeed on an agent with no active revision, got %v", err)
	}
	if first.ParentRevisionID != "" {
		t.Fatalf("first revision has parent %q, want none", first.ParentRevisionID)
	}
	if first.ContentHash == agentcfg.ExpectNoActiveRevision {
		t.Fatal("the sentinel leaked into the recorded content hash")
	}
	// The sentinel MUST NOT be a possible content hash — that is the whole
	// argument for putting it inside a hash-shaped field. A content hash is
	// sha256 hex: 64 lowercase hex characters.
	if !hashShaped(first.ContentHash) {
		t.Fatalf("content hash %q is not 64 lowercase hex characters — the sentinel's no-collision argument rests on this", first.ContentHash)
	}
	if hashShaped(agentcfg.ExpectNoActiveRevision) {
		t.Fatalf("the sentinel %q is hash-shaped and COULD collide with a real expectation", agentcfg.ExpectNoActiveRevision)
	}
	active, ok, err := r.Active(ctx, id, agentID, scope)
	if err != nil || !ok || active.RevisionID != first.RevisionID {
		t.Fatalf("first-write sentinel did not advance the active pointer: %q ok=%v err=%v", active.RevisionID, ok, err)
	}
}

// testConditionalFirstWriteRefused — once ANY revision is active the sentinel
// is refused with ErrRevisionConflict and persists nothing, on both the
// content-write door and the pointer-move door. A sentinel that degraded to
// "unconditional" once a revision existed would be the silent last-writer-wins
// it was added to remove.
func testConditionalFirstWriteRefused(t *testing.T, mk Factory, scope agentcfg.ConfigScope) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	base := mustSet(t, r, id, scope, skillsPayload("a"))
	before, err := r.ListRevisions(ctx, id, agentID, scope, 0)
	if err != nil {
		t.Fatalf("list before: %v", err)
	}

	if _, err := r.SetRevision(ctx, id, agentID, scope, skillsPayload("b"),
		agentcfg.SetOptions{ExpectedContentHash: agentcfg.ExpectNoActiveRevision}); !errors.Is(err, agentcfg.ErrRevisionConflict) {
		t.Fatalf("the first-write sentinel against an agent that HAS a revision must fail ErrRevisionConflict, got %v", err)
	}
	after, err := r.ListRevisions(ctx, id, agentID, scope, 0)
	if err != nil {
		t.Fatalf("list after: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("a refused first-write sentinel persisted a revision: %d -> %d", len(before), len(after))
	}
	active, ok, err := r.Active(ctx, id, agentID, scope)
	if err != nil || !ok || active.RevisionID != base.RevisionID {
		t.Fatalf("a refused first-write sentinel moved the active pointer: %q ok=%v err=%v", active.RevisionID, ok, err)
	}
	if _, rerr := r.Rollback(ctx, id, agentID, base.RevisionID, scope,
		agentcfg.SetOptions{ExpectedContentHash: agentcfg.ExpectNoActiveRevision}); !errors.Is(rerr, agentcfg.ErrRevisionConflict) {
		t.Fatalf("the first-write sentinel on Rollback must fail ErrRevisionConflict, got %v", rerr)
	}
}

// testConditionalFirstWriteLostUpdate is the row that reproduces the DEFECT the
// sentinel closes, and then shows it closed.
//
// Two independent contributors compose onto a fresh agent. Both read (`set:
// false` — no hash to echo) and both write. Without an expressible "expect
// none" token, B's only option is the empty token, which is unconditional, so
// A's contribution is silently gone and B is told 200. With the sentinel, B is
// refused and can re-read.
func testConditionalFirstWriteLostUpdate(t *testing.T, mk Factory, scope agentcfg.ConfigScope) {
	ctx := context.Background()
	r := mk(t)
	id := admin()

	// Both contributors read the same empty base.
	if _, ok, err := r.Active(ctx, id, agentID, scope); err != nil || ok {
		t.Fatalf("fixture broken: expected no active revision, ok=%v err=%v", ok, err)
	}
	// A writes first, holding the first-write token.
	if _, err := r.SetRevision(ctx, id, agentID, scope, skillsPayload("alpha"),
		agentcfg.SetOptions{ExpectedContentHash: agentcfg.ExpectNoActiveRevision}); err != nil {
		t.Fatalf("contributor A's first write: %v", err)
	}
	// B writes second, holding the SAME token it could express from its own
	// read. It must be refused rather than reverting A.
	_, bErr := r.SetRevision(ctx, id, agentID, scope, skillsPayload("beta"),
		agentcfg.SetOptions{ExpectedContentHash: agentcfg.ExpectNoActiveRevision})
	if !errors.Is(bErr, agentcfg.ErrRevisionConflict) {
		t.Fatalf("contributor B's write = %v, want ErrRevisionConflict — B reverted A silently", bErr)
	}
	active, ok, err := r.Active(ctx, id, agentID, scope)
	if err != nil || !ok {
		t.Fatalf("Active: ok=%v err=%v", ok, err)
	}
	names := active.Payload.SkillNames()
	if len(names) != 1 || names[0] != "alpha" {
		t.Fatalf("final content = %v, want [alpha] — contributor A's write was silently reverted", names)
	}
}

// hashShaped reports whether s has the shape agentcfg.ContentHash always
// produces: exactly 64 lowercase hex characters (sha256 hex). It is what makes
// the reserved sentinel provably un-collidable rather than merely unlikely.
func hashShaped(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// absentHash is a syntactically-valid SHA-256 hex string no payload hashes to.
// A literal keeps the no-active-revision row honest: the refusal must come
// from the ABSENCE of an active revision, not from a malformed token.
const absentHash = "0000000000000000000000000000000000000000000000000000000000000000"

// testConditionalEmptyToken — the zero SetOptions is the unconditional write:
// a caller that supplies no token still gets last-writer-wins, including over
// a base that moved. This is the compatibility row.
func testConditionalEmptyToken(t *testing.T, mk Factory, scope agentcfg.ConfigScope) {
	ctx := context.Background()
	r := mk(t)
	id := admin()
	mustSet(t, r, id, scope, skillsPayload("a"))
	mustSet(t, r, id, scope, skillsPayload("a", "b"))

	// No token: the write lands regardless of how far the base has moved.
	last, err := r.SetRevision(ctx, id, agentID, scope, skillsPayload("z"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("empty token must be unconditional, got %v", err)
	}
	active, ok, err := r.Active(ctx, id, agentID, scope)
	if err != nil || !ok {
		t.Fatalf("Active: ok=%v err=%v", ok, err)
	}
	if active.RevisionID != last.RevisionID {
		t.Fatalf("unconditional write did not win: active=%q want=%q", active.RevisionID, last.RevisionID)
	}
	names := active.Payload.SkillNames()
	if len(names) != 1 || names[0] != "z" {
		t.Fatalf("unconditional write content = %v, want [z]", names)
	}
	// Rollback with no token is likewise unconditional.
	revs, err := r.ListRevisions(ctx, id, agentID, scope, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(revs) < 2 {
		t.Fatalf("fixture broken: want >= 2 revisions, got %d", len(revs))
	}
	oldest := revs[len(revs)-1]
	if _, err := r.Rollback(ctx, id, agentID, oldest.RevisionID, scope, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("unconditional rollback must proceed, got %v", err)
	}
	active, ok, err = r.Active(ctx, id, agentID, scope)
	if err != nil || !ok || active.RevisionID != oldest.RevisionID {
		t.Fatalf("unconditional rollback did not repoint: %q want %q (ok=%v err=%v)",
			active.RevisionID, oldest.RevisionID, ok, err)
	}
}
