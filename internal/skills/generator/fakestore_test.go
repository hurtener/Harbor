package generator_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

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

func (f *failingUpsertStore) GetScope(ctx context.Context, id identity.Quadruple, name string, scope skills.Scope) (skills.Skill, error) {
	return f.inner.GetScope(ctx, id, name, scope)
}

func (f *failingUpsertStore) GetScopeAgent(ctx context.Context, id identity.Quadruple, agentID, name string, scope skills.Scope) (skills.Skill, error) {
	return f.inner.GetScopeAgent(ctx, id, agentID, name, scope)
}

func (f *failingUpsertStore) List(ctx context.Context, id identity.Quadruple, filter skills.ListFilter) ([]skills.Skill, error) {
	return f.inner.List(ctx, id, filter)
}

func (f *failingUpsertStore) Search(ctx context.Context, id identity.Quadruple, q string, limit int) ([]skills.RankedSkill, error) {
	return f.inner.Search(ctx, id, q, limit)
}

func (f *failingUpsertStore) SearchAgent(ctx context.Context, id identity.Quadruple, agentID, q string, limit int) ([]skills.RankedSkill, error) {
	return f.inner.SearchAgent(ctx, id, agentID, q, limit)
}

func (f *failingUpsertStore) SearchSnapshot(ctx context.Context, id identity.Quadruple, q string, candidates []skills.Skill, limit int) ([]skills.RankedSkill, error) {
	return f.inner.SearchSnapshot(ctx, id, q, candidates, limit)
}

func (f *failingUpsertStore) Delete(ctx context.Context, id identity.Quadruple, name string, scope skills.Scope) error {
	f.deleteCalls++
	if f.deleteErr != nil {
		return f.deleteErr
	}
	return f.inner.Delete(ctx, id, name, scope)
}

func (f *failingUpsertStore) DeleteAgent(ctx context.Context, id identity.Quadruple, agentID, name string, scope skills.Scope) error {
	return f.inner.DeleteAgent(ctx, id, agentID, name, scope)
}

func (f *failingUpsertStore) DeleteSessionScope(ctx context.Context, id identity.Quadruple) error {
	return f.inner.DeleteSessionScope(ctx, id)
}

func (f *failingUpsertStore) Close(ctx context.Context) error {
	return f.inner.Close(ctx)
}

// ---- Mandatory installed-package surface (interface parity) ----
//
// failingUpsertStore is a fault-injection WRAPPER: it exists to make
// the legacy mutation surface (`Upsert` / `Delete`) fail on demand
// while every other method delegates to the wrapped store. The atomic
// installed-package surface (`GetInstalledPackage` / `ResolveSupport`
// / `PutInstalledPackage` / `DeleteInstalledPackage` /
// `RestoreInstalledPackage`) is a distinct self-contained contract
// this wrapper never overrides, so the truthful parity move is exact
// pass-through: the wrapped store's conditional puts, exact-receipt
// compensation, deep copies, and canonical typed errors are preserved
// unchanged. An in-memory look-alike inside the wrapper would silently
// diverge from the wrapped store's legacy key space, and a fail-loud
// stub would lie about a surface a real driver provides — delegation
// is the only coherent answer. The generator itself never exercises
// these methods (`Propose` / `Promote` use only Get / Upsert / Delete);
// they exist so the wrapper remains a valid skills.SkillStore.
func (f *failingUpsertStore) GetInstalledPackage(ctx context.Context, id identity.Quadruple, agentID, name string) (skills.InstalledPackage, error) {
	return f.inner.GetInstalledPackage(ctx, id, agentID, name)
}

func (f *failingUpsertStore) ResolveSupport(ctx context.Context, id identity.Quadruple, agentID, name string, uri skills.PackageURI) (skills.SupportFile, error) {
	return f.inner.ResolveSupport(ctx, id, agentID, name, uri)
}

func (f *failingUpsertStore) PutInstalledPackage(ctx context.Context, id identity.Quadruple, agentID string, pkg skills.InstalledPackage, cond skills.InstalledPackageCondition, replace bool) (skills.InstalledPackageReceipt, error) {
	return f.inner.PutInstalledPackage(ctx, id, agentID, pkg, cond, replace)
}

func (f *failingUpsertStore) DeleteInstalledPackage(ctx context.Context, id identity.Quadruple, agentID, name string, receipt skills.InstalledPackageReceipt) (bool, error) {
	return f.inner.DeleteInstalledPackage(ctx, id, agentID, name, receipt)
}

func (f *failingUpsertStore) RestoreInstalledPackage(ctx context.Context, id identity.Quadruple, agentID, name string, receipt skills.InstalledPackageReceipt, prior skills.InstalledPackage) (bool, error) {
	return f.inner.RestoreInstalledPackage(ctx, id, agentID, name, receipt, prior)
}

func TestFailingUpsertStore_DeleteSessionScopeDelegates(t *testing.T) {
	t.Parallel()
	bus := newTestBus(t)
	inner := newTestStore(t, bus)
	id := testIdentity()
	skill := skills.Skill{
		Name: "session-delete-delegate", Title: "delegate", Trigger: "delegate",
		Steps: []string{"delegate"}, Origin: skills.OriginGenerated, Scope: skills.ScopeSession,
	}
	skill.ContentHash = skills.CanonicalContentHash(skill)
	if err := inner.Upsert(context.Background(), id, skill); err != nil {
		t.Fatalf("seed session skill: %v", err)
	}
	wrapper := &failingUpsertStore{inner: inner}
	if err := wrapper.DeleteSessionScope(context.Background(), id); err != nil {
		t.Fatalf("DeleteSessionScope: %v", err)
	}
	if _, err := inner.Get(context.Background(), id, skill.Name); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("inner session skill survived delegated sweep: err=%v", err)
	}
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

// TestFailingUpsertStore_InstalledPackageMethodsDelegate pins the
// mandatory installed-package surface on the wrapper: every method is
// an exact pass-through to the wrapped store, so the driver's
// conditional puts, exact-receipt compensation, and canonical typed
// errors reach the caller unchanged. The full create → replace → read
// → resolve → restore → stale-delete no-op → exact-delete loop runs
// through the wrapper.
func TestFailingUpsertStore_InstalledPackageMethodsDelegate(t *testing.T) {
	t.Parallel()
	bus := newTestBus(t)
	inner := newTestStore(t, bus)
	wrapper := &failingUpsertStore{inner: inner}
	ctx := context.Background()
	id := testIdentity()
	const agent = "a-installed"
	const name = "installed-delegate"

	// Create v1 against an absent key.
	v1 := installedPackageFixture(t, name, agent, "1.0.0")
	r1, err := wrapper.PutInstalledPackage(ctx, id, agent, v1,
		skills.InstalledPackageCondition{ExpectedAbsent: true}, false)
	if err != nil {
		t.Fatalf("PutInstalledPackage(create) via wrapper: %v", err)
	}
	if r1.WrittenHash != v1.PackageHash || r1.PriorHash != "" {
		t.Fatalf("create receipt = %+v, want WrittenHash=%q PriorHash=\"\"", r1, v1.PackageHash)
	}

	// Replace v1 with v2 under an exact hash+version condition.
	v2 := installedPackageFixture(t, name, agent, "2.0.0")
	r2, err := wrapper.PutInstalledPackage(ctx, id, agent, v2,
		skills.InstalledPackageCondition{ExpectedHash: v1.PackageHash, ExpectedVersion: v1.Package.Version}, true)
	if err != nil {
		t.Fatalf("PutInstalledPackage(replace) via wrapper: %v", err)
	}
	if r2.PriorHash != v1.PackageHash || r2.PriorVersion != v1.Package.Version {
		t.Fatalf("replace receipt prior = %q/%q, want %q/%q",
			r2.PriorHash, r2.PriorVersion, v1.PackageHash, v1.Package.Version)
	}

	got, err := wrapper.GetInstalledPackage(ctx, id, agent, name)
	if err != nil {
		t.Fatalf("GetInstalledPackage via wrapper: %v", err)
	}
	assertInstalledPackageEqual(t, got, v2, "round trip through wrapper")

	support, err := wrapper.ResolveSupport(ctx, id, agent, name, supportURI(t, v2))
	if err != nil {
		t.Fatalf("ResolveSupport via wrapper: %v", err)
	}
	if support.Path != v2.Package.Supports[0].Path || !bytes.Equal(support.Data, v2.Package.Supports[0].Data) {
		t.Fatalf("resolved support = %+v, want the exact manifest entry", support)
	}

	// Restore the exact prior (v1) over the version r2 wrote.
	restored, err := wrapper.RestoreInstalledPackage(ctx, id, agent, name, r2, v1)
	if err != nil || !restored {
		t.Fatalf("RestoreInstalledPackage via wrapper = (%v, %v), want (true, nil)", restored, err)
	}
	got, err = wrapper.GetInstalledPackage(ctx, id, agent, name)
	if err != nil {
		t.Fatalf("GetInstalledPackage after restore: %v", err)
	}
	assertInstalledPackageEqual(t, got, v1, "restore through wrapper")

	// r2 wrote v2; the winner is now v1 → stale receipt is a no-op.
	staleDeleted, err := wrapper.DeleteInstalledPackage(ctx, id, agent, name, r2)
	if err != nil || staleDeleted {
		t.Fatalf("stale DeleteInstalledPackage via wrapper = (%v, %v), want (false, nil)", staleDeleted, err)
	}
	got, err = wrapper.GetInstalledPackage(ctx, id, agent, name)
	if err != nil {
		t.Fatalf("GetInstalledPackage after stale delete: %v", err)
	}
	assertInstalledPackageEqual(t, got, v1, "stale receipt must not touch the winner")

	// r1 wrote the current winner v1 → exact delete succeeds.
	deleted, err := wrapper.DeleteInstalledPackage(ctx, id, agent, name, r1)
	if err != nil || !deleted {
		t.Fatalf("exact DeleteInstalledPackage via wrapper = (%v, %v), want (true, nil)", deleted, err)
	}
	if _, err := wrapper.GetInstalledPackage(ctx, id, agent, name); !errors.Is(err, skills.ErrInstalledPackageNotFound) {
		t.Fatalf("GetInstalledPackage after exact delete err=%v, want ErrInstalledPackageNotFound", err)
	}
}

// installedPackageFixture returns a valid atomic installed-package
// unit (canonical complete package + versioned PackageHash +
// self-consistent stored semantic skill) mirroring the canonical
// conformance fixture shape.
func installedPackageFixture(t *testing.T, name, agentID, version string) skills.InstalledPackage {
	t.Helper()
	const data = `{"example": true}`
	supports := []skills.SupportFile{{
		Path:   "examples/example.json",
		Mime:   "application/json",
		Size:   int64(len(data)),
		Digest: hexDigest([]byte(data)),
		Data:   []byte(data),
	}}
	pkg := skills.Package{
		Name:    name,
		Version: version,
		Skill: skills.PackageSkill{
			Name:    name,
			Title:   "Title " + name,
			Trigger: "trigger:" + name,
			Steps:   []string{"step one", "step two"},
		},
		Supports: supports,
	}
	hash, err := skills.PackageHash(pkg)
	if err != nil {
		t.Fatalf("installedPackageFixture(%q): PackageHash: %v", name, err)
	}
	skill := skills.Skill{
		Name:      name,
		AgentID:   agentID,
		Title:     pkg.Skill.Title,
		Trigger:   pkg.Skill.Trigger,
		Steps:     append([]string(nil), pkg.Skill.Steps...),
		Origin:    skills.OriginGenerated,
		OriginRef: "test:" + name,
		Scope:     skills.ScopeUser,
		CreatedAt: fixedFixtureTime,
		UpdatedAt: fixedFixtureTime,
	}
	skill.ContentHash = skills.CanonicalContentHash(skill)
	return skills.InstalledPackage{Skill: skill, Package: pkg, PackageHash: hash}
}

// supportURI returns the immutable skillpkg URI of the fixture's first
// support file.
func supportURI(t *testing.T, unit skills.InstalledPackage) skills.PackageURI {
	t.Helper()
	uri, err := skills.NewPackageURI(unit.PackageHash, unit.Package.Supports[0].Path)
	if err != nil {
		t.Fatalf("NewPackageURI: %v", err)
	}
	return uri
}

// assertInstalledPackageEqual compares the identity-bearing fields of
// two atomic units: the versioned PackageHash, the semantic skill
// envelope + body, the package envelope, and the ordered support
// manifest (path, MIME, size, digest, bytes).
func assertInstalledPackageEqual(t *testing.T, got, want skills.InstalledPackage, msg string) {
	t.Helper()
	if err := skills.ValidateInstalledPackage(got); err != nil {
		t.Fatalf("%s: got is not internally consistent: %v", msg, err)
	}
	if got.PackageHash != want.PackageHash {
		t.Fatalf("%s: PackageHash got %q want %q", msg, got.PackageHash, want.PackageHash)
	}
	if got.Skill.Name != want.Skill.Name || got.Skill.AgentID != want.Skill.AgentID ||
		got.Skill.Origin != want.Skill.Origin || got.Skill.Scope != want.Skill.Scope ||
		got.Skill.ContentHash != want.Skill.ContentHash {
		t.Fatalf("%s: skill envelope mismatch:\n  got  %+v\n  want %+v", msg, got.Skill, want.Skill)
	}
	if got.Package.Name != want.Package.Name || got.Package.Version != want.Package.Version {
		t.Fatalf("%s: package envelope got %s@%s want %s@%s",
			msg, got.Package.Name, got.Package.Version, want.Package.Name, want.Package.Version)
	}
	if len(got.Package.Supports) != len(want.Package.Supports) {
		t.Fatalf("%s: support manifest length got %d want %d", msg, len(got.Package.Supports), len(want.Package.Supports))
	}
	for i := range want.Package.Supports {
		w, g := want.Package.Supports[i], got.Package.Supports[i]
		if g.Path != w.Path || g.Mime != w.Mime || g.Size != w.Size || g.Digest != w.Digest {
			t.Fatalf("%s: support[%d] metadata got %+v want %+v", msg, i, g, w)
		}
		if !bytes.Equal(g.Data, w.Data) {
			t.Fatalf("%s: support[%d] bytes differ", msg, i)
		}
	}
}

// hexDigest returns the lowercase hex sha256 of `b`.
func hexDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fixedFixtureTime is a deterministic timestamp for installed-package
// fixtures so round trips are stable.
var fixedFixtureTime = time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
