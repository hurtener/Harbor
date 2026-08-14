package localdb_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/drivers/localdb"
)

// installedFixture builds a valid atomic installed-package unit: a
// canonical complete package with `nFiles` ordered support entries, its
// versioned PackageHash, and a self-consistent stored semantic skill
// (ScopeUser, effective agent bound, canonical ContentHash).
func installedFixture(t *testing.T, name, agentID string, origin skills.Origin, version string, nFiles int) skills.InstalledPackage {
	t.Helper()
	now := time.Now().UTC()
	supports := make([]skills.SupportFile, 0, nFiles)
	for i := 0; i < nFiles; i++ {
		data := []byte(fmt.Sprintf(`{"file": %d, "name": %q}`, i, name))
		sum := sha256Sum(data)
		supports = append(supports, skills.SupportFile{
			Path:   fmt.Sprintf("examples/file-%02d.json", i),
			Mime:   "application/json",
			Size:   int64(len(data)),
			Digest: sum,
			Data:   data,
		})
	}
	pkg := skills.Package{
		Name:    name,
		Version: version,
		Skill: skills.PackageSkill{
			Name:          name,
			Title:         "Title " + name,
			Description:   "Description for " + name,
			Trigger:       "trigger:" + name,
			TaskType:      "code",
			Tags:          []string{"alpha", "beta"},
			Steps:         []string{"step one", "step two"},
			Preconditions: []string{"precondition"},
			FailureModes:  []string{"failure-mode"},
			RequiredTools: []string{"tool-a"},
			RequiredNS:    []string{"ns-a"},
			RequiredTags:  []string{"tag-a"},
		},
		Supports: supports,
	}
	hash, err := skills.PackageHash(pkg)
	if err != nil {
		t.Fatalf("PackageHash(%q): %v", name, err)
	}
	skill := skills.Skill{
		Name:          name,
		AgentID:       agentID,
		Title:         pkg.Skill.Title,
		Description:   pkg.Skill.Description,
		Trigger:       pkg.Skill.Trigger,
		TaskType:      pkg.Skill.TaskType,
		Tags:          append([]string(nil), pkg.Skill.Tags...),
		Steps:         append([]string(nil), pkg.Skill.Steps...),
		Preconditions: append([]string(nil), pkg.Skill.Preconditions...),
		FailureModes:  append([]string(nil), pkg.Skill.FailureModes...),
		RequiredTools: append([]string(nil), pkg.Skill.RequiredTools...),
		RequiredNS:    append([]string(nil), pkg.Skill.RequiredNS...),
		RequiredTags:  append([]string(nil), pkg.Skill.RequiredTags...),
		Origin:        origin,
		OriginRef:     "test:" + name,
		Scope:         skills.ScopeUser,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	skill.ContentHash = skills.CanonicalContentHash(skill)
	return skills.InstalledPackage{Skill: skill, Package: pkg, PackageHash: hash}
}

func sha256Sum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// installAbsent is the driver-local create helper.
func installAbsent(t *testing.T, store skills.SkillStore, ctx context.Context, agentID string, unit skills.InstalledPackage) skills.InstalledPackageReceipt {
	t.Helper()
	r, err := store.PutInstalledPackage(ctx, fixtureID, agentID, unit,
		skills.InstalledPackageCondition{ExpectedAbsent: true}, false)
	if err != nil {
		t.Fatalf("PutInstalledPackage(create %q): %v", unit.Package.Name, err)
	}
	return r
}

// assertUnitEqual compares the identity-bearing fields of two units:
// hash, skill envelope + body, package envelope + logical content, and
// the ordered support manifest (metadata + bytes).
func assertUnitEqual(t *testing.T, got, want skills.InstalledPackage, msg string) {
	t.Helper()
	if got.PackageHash != want.PackageHash {
		t.Fatalf("%s: PackageHash got %q want %q", msg, got.PackageHash, want.PackageHash)
	}
	if got.Skill.Name != want.Skill.Name || got.Skill.AgentID != want.Skill.AgentID ||
		got.Skill.Origin != want.Skill.Origin || got.Skill.Scope != want.Skill.Scope ||
		got.Skill.ContentHash != want.Skill.ContentHash {
		t.Fatalf("%s: skill envelope mismatch: got %+v want %+v", msg, got.Skill, want.Skill)
	}
	if got.Skill.Trigger != want.Skill.Trigger || got.Skill.Description != want.Skill.Description ||
		!stringSlicesEqual(got.Skill.Steps, want.Skill.Steps) || !stringSlicesEqual(got.Skill.Tags, want.Skill.Tags) {
		t.Fatalf("%s: skill body mismatch: got %+v want %+v", msg, got.Skill, want.Skill)
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
			t.Fatalf("%s: support[%d] bytes differ (%d vs %d)", msg, i, len(g.Data), len(w.Data))
		}
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestInstalledPackage_LegacyFence_Direct pins the fail-loud legacy-mutation
// fence directly against the SQLite driver: once an installed package is
// committed at the session-zeroed (tenant, user, effective-agent, name) key,
// the legacy Upsert / DeleteAgent paths that share that key refuse with
// ErrInstalledPackageReadOnly BEFORE any state is touched, while keys
// without an installed package keep their exact legacy behavior.
func TestInstalledPackage_LegacyFence_Direct(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	const agent = "agent-fence-direct"
	unit := installedFixture(t, "fence-direct", agent, skills.OriginGenerated, "1.0.0", 2)
	r := installAbsent(t, store, ctx, agent, unit)

	// Upsert of the ScopeUser/agent-bound row of the same name is refused.
	hostile := unit.Skill
	hostile.Description = "HOSTILE OVERWRITE"
	hostile.ContentHash = skills.CanonicalContentHash(hostile)
	if err := store.Upsert(ctx, fixtureID, hostile); !errors.Is(err, skills.ErrInstalledPackageReadOnly) {
		t.Fatalf("Upsert on installed key err=%v, want ErrInstalledPackageReadOnly", err)
	}

	// DeleteAgent at the ScopeUser rung shares the membership-row key.
	if err := store.DeleteAgent(ctx, fixtureID, agent, "fence-direct", skills.ScopeUser); !errors.Is(err, skills.ErrInstalledPackageReadOnly) {
		t.Fatalf("DeleteAgent(ScopeUser) on installed key err=%v, want ErrInstalledPackageReadOnly", err)
	}

	// The agent-less legacy Delete cannot reach the installed membership
	// row: not-found, unchanged.
	if err := store.Delete(ctx, fixtureID, "fence-direct", skills.ScopeUser); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("agent-less Delete err=%v, want ErrSkillNotFound", err)
	}

	// A session-pinned DeleteAgent (any other scope) never reaches the
	// session-zeroed installed row either.
	if err := store.DeleteAgent(ctx, fixtureID, agent, "fence-direct", skills.ScopeProject); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("session-pinned DeleteAgent err=%v, want ErrSkillNotFound", err)
	}

	// Every refusal left the exact atomic unit intact, including the
	// legacy scope read and the resolve surface.
	got, err := store.GetInstalledPackage(ctx, fixtureID, agent, "fence-direct")
	if err != nil {
		t.Fatalf("GetInstalledPackage after refusals: %v", err)
	}
	assertUnitEqual(t, got, unit, "unit after legacy refusals")
	if _, err := store.GetScopeAgent(ctx, fixtureID, agent, "fence-direct", skills.ScopeUser); err != nil {
		t.Fatalf("GetScopeAgent after refusals: %v", err)
	}
	uri, err := skills.NewPackageURI(unit.PackageHash, unit.Package.Supports[0].Path)
	if err != nil {
		t.Fatalf("NewPackageURI: %v", err)
	}
	if _, err := store.ResolveSupport(ctx, fixtureID, agent, "fence-direct", uri); err != nil {
		t.Fatalf("ResolveSupport after refusals: %v", err)
	}

	// Keys WITHOUT an installed package keep the exact legacy behavior:
	// idempotent Upsert and an ordinary DeleteAgent of a legacy row.
	legacy := mustHash(skills.Skill{
		Name: "fence-legacy-direct", Trigger: "trg", Steps: []string{"s"},
		Origin: skills.OriginGenerated, Scope: skills.ScopeProject, AgentID: agent,
	})
	if err := store.Upsert(ctx, fixtureID, legacy); err != nil {
		t.Fatalf("legacy upsert (no package): %v", err)
	}
	if err := store.Upsert(ctx, fixtureID, legacy); err != nil {
		t.Fatalf("idempotent legacy re-upsert (no package): %v", err)
	}
	if err := store.DeleteAgent(ctx, fixtureID, agent, "fence-legacy-direct", legacy.Scope); err != nil {
		t.Fatalf("legacy DeleteAgent (no package): %v", err)
	}

	// The dedicated package-aware path remains the only erasure path.
	deleted, err := store.DeleteInstalledPackage(ctx, fixtureID, agent, "fence-direct", r)
	if err != nil || !deleted {
		t.Fatalf("DeleteInstalledPackage: deleted=%v err=%v", deleted, err)
	}
	if _, err := store.GetInstalledPackage(ctx, fixtureID, agent, "fence-direct"); !errors.Is(err, skills.ErrInstalledPackageNotFound) {
		t.Fatalf("package survived dedicated erasure: err=%v", err)
	}
}

// TestInstalledPackage_RestoreBindsPriorToTargetKey pins the canonical-
// key binding of the exact-receipt restore: a receipt legitimately bound
// to name A must never let a `prior` unit named B be written anywhere.
// Before the binding fix, the recorded-prior hash match was the only
// gate on `prior` and the write keyed off the prior's OWN name — so a
// plan-constructed receipt for A whose recorded prior hash matched a
// foreign package B would overwrite B's winner (bypassing B's
// condition/origin winner) while leaving A unrestored. The driver must
// reject the mismatch with ErrInstalledPackageInvalid BEFORE any write,
// leaving both winners exactly unchanged.
func TestInstalledPackage_RestoreBindsPriorToTargetKey(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	const agent = "agent-restore-bind"

	// Key A: v1A displaced by v2A — the receipt's written version is
	// still A's winner.
	v1A := installedFixture(t, "restore-bind-a", agent, skills.OriginGenerated, "1.0.0", 1)
	v2A := installedFixture(t, "restore-bind-a", agent, skills.OriginGenerated, "2.0.0", 1)
	installAbsent(t, store, ctx, agent, v1A)
	if _, err := store.PutInstalledPackage(ctx, fixtureID, agent, v2A,
		skills.InstalledPackageCondition{ExpectedHash: v1A.PackageHash, ExpectedVersion: v1A.Package.Version}, true); err != nil {
		t.Fatalf("seed A replace: %v", err)
	}

	// Key B: a pack-origin winner — the origin gate PutInstalledPackage
	// enforces (a generated prior can never overwrite it).
	v1BPack := installedFixture(t, "restore-bind-b", agent, skills.OriginPack, "1.0.0", 1)
	installAbsent(t, store, ctx, agent, v1BPack)

	// A plan-constructed receipt legitimately bound to A (its written
	// version is still A's winner) but whose recorded prior hash matches
	// the FOREIGN package B. The prior unit is named B, not A.
	hostile := installedFixture(t, "restore-bind-b", agent, skills.OriginGenerated, "9.9.9", 1)
	planReceipt := skills.InstalledPackageReceipt{
		TenantID: fixtureID.TenantID, UserID: fixtureID.UserID, AgentID: agent, Name: "restore-bind-a",
		WrittenHash: v2A.PackageHash, WrittenVersion: v2A.Package.Version,
		PriorHash: hostile.PackageHash, PriorVersion: hostile.Package.Version,
	}

	restored, err := store.RestoreInstalledPackage(ctx, fixtureID, agent, "restore-bind-a", planReceipt, hostile)
	if !errors.Is(err, skills.ErrInstalledPackageInvalid) {
		t.Fatalf("foreign-name prior restore err=%v, want ErrInstalledPackageInvalid", err)
	}
	if restored {
		t.Fatal("foreign-name prior restore reported success")
	}

	// Neither winner moved: A keeps the receipt's written version, B
	// keeps its pack-origin winner.
	assertWinnerDirect(t, store, ctx, agent, "restore-bind-a", v2A, "A's winner must survive a foreign-name prior")
	assertWinnerDirect(t, store, ctx, agent, "restore-bind-b", v1BPack, "B's pack-origin winner must survive a foreign-name prior")
}

// TestInstalledPackage_ExactReceipt_NeverTouchesAnotherWinner pins the
// exact-receipt compensation contract end to end against SQLite: a
// receipt restores or deletes ONLY the version it wrote; another
// proposal's winner is never overwritten or deleted; a plan-constructed
// receipt (crash-recovery shape) works; wrong-prior and absent-prior
// compensation fail loudly without mutation.
func TestInstalledPackage_ExactReceipt_NeverTouchesAnotherWinner(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	const agent = "agent-comp-direct"

	v1 := installedFixture(t, "comp-direct", agent, skills.OriginGenerated, "1.0.0", 1)
	v2 := installedFixture(t, "comp-direct", agent, skills.OriginGenerated, "2.0.0", 1)
	v3 := installedFixture(t, "comp-direct", agent, skills.OriginGenerated, "3.0.0", 1)

	rA := installAbsent(t, store, ctx, agent, v1)
	rB, err := store.PutInstalledPackage(ctx, fixtureID, agent, v2,
		skills.InstalledPackageCondition{ExpectedHash: v1.PackageHash, ExpectedVersion: v1.Package.Version}, true)
	if err != nil {
		t.Fatalf("proposal B replace: %v", err)
	}
	if rB.PriorHash != v1.PackageHash || rB.PriorVersion != v1.Package.Version {
		t.Fatalf("replace receipt prior = %q/%q, want %q/%q", rB.PriorHash, rB.PriorVersion, v1.PackageHash, v1.Package.Version)
	}

	// A's receipt must not delete B's winner.
	deleted, err := store.DeleteInstalledPackage(ctx, fixtureID, agent, "comp-direct", rA)
	if err != nil {
		t.Fatalf("A compensation delete: %v", err)
	}
	if deleted {
		t.Fatal("A's stale receipt deleted B's winner")
	}

	// B's receipt restores its exact prior over its own write.
	restored, err := store.RestoreInstalledPackage(ctx, fixtureID, agent, "comp-direct", rB, v1)
	if err != nil || !restored {
		t.Fatalf("B compensation restore: restored=%v err=%v", restored, err)
	}
	got, err := store.GetInstalledPackage(ctx, fixtureID, agent, "comp-direct")
	if err != nil {
		t.Fatalf("GetInstalledPackage: %v", err)
	}
	assertUnitEqual(t, got, v1, "B's compensation restored the exact prior")

	// B's receipt is now stale: restoring again is a no-op, never a
	// mutation.
	restored, err = store.RestoreInstalledPackage(ctx, fixtureID, agent, "comp-direct", rB, v1)
	if err != nil {
		t.Fatalf("B stale restore: %v", err)
	}
	if restored {
		t.Fatal("stale restore mutated the winner")
	}

	// Proposal C replaces v1 with v3; B's stale receipt must not touch C.
	if _, err := store.PutInstalledPackage(ctx, fixtureID, agent, v3,
		skills.InstalledPackageCondition{ExpectedHash: v1.PackageHash, ExpectedVersion: v1.Package.Version}, true); err != nil {
		t.Fatalf("proposal C replace: %v", err)
	}
	restored, err = store.RestoreInstalledPackage(ctx, fixtureID, agent, "comp-direct", rB, v1)
	if err != nil {
		t.Fatalf("C-era stale restore: %v", err)
	}
	if restored {
		t.Fatal("B's stale receipt replaced C's winner")
	}
	assertWinnerDirect(t, store, ctx, agent, "comp-direct", v3, "C's winner must survive B's stale receipt")

	// A plan-constructed receipt (crash-recovery shape) compensates
	// without depending on the original put response. While the winner is
	// still v3 (planReceipt's written version), a wrong prior fails loudly
	// and mutates nothing.
	planReceipt := skills.InstalledPackageReceipt{
		TenantID: fixtureID.TenantID, UserID: fixtureID.UserID, AgentID: agent, Name: "comp-direct",
		WrittenHash: v3.PackageHash, WrittenVersion: v3.Package.Version,
		PriorHash: v1.PackageHash, PriorVersion: v1.Package.Version,
	}
	wrong := installedFixture(t, "comp-direct", agent, skills.OriginGenerated, "9.9.9", 1)
	if _, err := store.RestoreInstalledPackage(ctx, fixtureID, agent, "comp-direct", planReceipt, wrong); !errors.Is(err, skills.ErrInstalledPackageConditionFailed) {
		t.Fatalf("wrong-prior restore err=%v, want ErrInstalledPackageConditionFailed", err)
	}
	assertWinnerDirect(t, store, ctx, agent, "comp-direct", v3, "wrong-prior restore must not mutate the winner")

	restored, err = store.RestoreInstalledPackage(ctx, fixtureID, agent, "comp-direct", planReceipt, v1)
	if err != nil || !restored {
		t.Fatalf("plan-constructed receipt restore: restored=%v err=%v", restored, err)
	}

	// A create receipt (absent prior) is compensated by Delete, not
	// Restore — the ambiguity fails loudly.
	rCreate := installAbsent(t, store, ctx, agent, installedFixture(t, "comp-create", agent, skills.OriginGenerated, "1.0.0", 1))
	if _, err := store.RestoreInstalledPackage(ctx, fixtureID, agent, "comp-create", rCreate, v1); !errors.Is(err, skills.ErrInstalledPackageInvalid) {
		t.Fatalf("absent-prior restore err=%v, want ErrInstalledPackageInvalid", err)
	}

	// Exact-receipt erasure of the current winner (v1 — restored by the
	// plan receipt, so rA's written version is the winner again); an
	// already-erased receipt is a normal no-op.
	deleted, err = store.DeleteInstalledPackage(ctx, fixtureID, agent, "comp-direct", rA)
	if err != nil || !deleted {
		t.Fatalf("final exact erasure: deleted=%v err=%v", deleted, err)
	}
	deleted, err = store.DeleteInstalledPackage(ctx, fixtureID, agent, "comp-direct", rA)
	if err != nil || deleted {
		t.Fatalf("already-erased receipt: deleted=%v err=%v (want (false, nil))", deleted, err)
	}
}

func assertWinnerDirect(t *testing.T, store skills.SkillStore, ctx context.Context, agentID, name string, want skills.InstalledPackage, msg string) {
	t.Helper()
	got, err := store.GetInstalledPackage(ctx, fixtureID, agentID, name)
	if err != nil {
		t.Fatalf("%s: GetInstalledPackage: %v", msg, err)
	}
	assertUnitEqual(t, got, want, msg)
}

// TestInstalledPackage_IdentityRejectionEmits pins the identity-mandatory
// boundary: every installed-package method refuses an incomplete triple
// with ErrIdentityRequired AND emits `skill.identity_rejected` on the bus
// (the audit emit path is shared with the legacy surface).
func TestInstalledPackage_IdentityRejectionEmits(t *testing.T) {
	ctx := context.Background()
	bus := newBus(t)
	sub, err := bus.Subscribe(ctx, events.Filter{
		Admin: true,
		Types: []events.EventType{skills.EventTypeSkillIdentityRejected},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	store, err := localdb.New(skills.ConfigSnapshot{Driver: "localdb", DSN: ":memory:"}, skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("localdb.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(ctx) })

	bad := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u"}} // missing session
	unit := installedFixture(t, "id-emit", "agent-id-emit", skills.OriginGenerated, "1.0.0", 1)
	if _, err := store.PutInstalledPackage(ctx, bad, "agent-id-emit", unit,
		skills.InstalledPackageCondition{ExpectedAbsent: true}, false); !errors.Is(err, skills.ErrIdentityRequired) {
		t.Fatalf("PutInstalledPackage missing session err=%v, want ErrIdentityRequired", err)
	}
	select {
	case ev := <-sub.Events():
		if ev.Type != skills.EventTypeSkillIdentityRejected {
			t.Fatalf("emit: wrong type %q", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for skill.identity_rejected emit")
	}
}

// TestInstalledPackage_CanceledContext pins ctx cancellation on every
// installed-package method: a canceled context fails the operation with
// context.Canceled and never touches the store.
func TestInstalledPackage_CanceledContext(t *testing.T) {
	store := openStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	const agent = "agent-cancel-direct"
	unit := installedFixture(t, "cancel-direct", agent, skills.OriginGenerated, "1.0.0", 1)
	receipt := skills.InstalledPackageReceipt{
		TenantID: fixtureID.TenantID, UserID: fixtureID.UserID, AgentID: agent, Name: "cancel-direct",
		WrittenHash: unit.PackageHash, PriorHash: unit.PackageHash,
	}
	cases := []struct {
		name string
		err  error
	}{
		{"GetInstalledPackage", func() error {
			_, err := store.GetInstalledPackage(ctx, fixtureID, agent, "cancel-direct")
			return err
		}()},
		{"ResolveSupport", func() error {
			uri, _ := skills.NewPackageURI(unit.PackageHash, unit.Package.Supports[0].Path)
			_, err := store.ResolveSupport(ctx, fixtureID, agent, "cancel-direct", uri)
			return err
		}()},
		{"PutInstalledPackage", func() error {
			_, err := store.PutInstalledPackage(ctx, fixtureID, agent, unit,
				skills.InstalledPackageCondition{ExpectedAbsent: true}, false)
			return err
		}()},
		{"DeleteInstalledPackage", func() error {
			_, err := store.DeleteInstalledPackage(ctx, fixtureID, agent, "cancel-direct", receipt)
			return err
		}()},
		{"RestoreInstalledPackage", func() error {
			_, err := store.RestoreInstalledPackage(ctx, fixtureID, agent, "cancel-direct", receipt, unit)
			return err
		}()},
	}
	for _, c := range cases {
		if !errors.Is(c.err, context.Canceled) {
			t.Fatalf("%s with canceled ctx err=%v, want context.Canceled", c.name, c.err)
		}
	}
}

// TestInstalledPackage_RestartSurvival pins the durable restart hook
// directly: a committed package (body + every support byte) survives
// Close → reopen over the same file-backed DB, and a new session resolves
// it with the staging artifacts unavailable.
func TestInstalledPackage_RestartSurvival(t *testing.T) {
	ctx := context.Background()
	bus := newBus(t)
	dsn := filepath.Join(t.TempDir(), "installed-restart.sqlite")
	store, err := localdb.New(skills.ConfigSnapshot{Driver: "localdb", DSN: dsn}, skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("localdb.New: %v", err)
	}
	const agent = "agent-restart-direct"
	unit := installedFixture(t, "restart-direct", agent, skills.OriginPack, "1.0.0", 2)
	installAbsent(t, store, ctx, agent, unit)
	if err := store.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := localdb.New(skills.ConfigSnapshot{Driver: "localdb", DSN: dsn}, skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close(ctx) }()

	got, err := reopened.GetInstalledPackage(ctx, fixtureID, agent, "restart-direct")
	if err != nil {
		t.Fatalf("GetInstalledPackage after reopen: %v", err)
	}
	assertUnitEqual(t, got, unit, "restart survival")
	// The legacy membership row is durable too.
	row, err := reopened.GetScopeAgent(ctx, fixtureID, agent, "restart-direct", skills.ScopeUser)
	if err != nil || row.ContentHash != unit.Skill.ContentHash {
		t.Fatalf("GetScopeAgent after reopen: row=%+v err=%v", row, err)
	}
	// Support bytes resolve after restart — the installed form is
	// self-contained, never a staging dereference.
	uri, err := skills.NewPackageURI(unit.PackageHash, unit.Package.Supports[1].Path)
	if err != nil {
		t.Fatalf("NewPackageURI: %v", err)
	}
	f, err := reopened.ResolveSupport(ctx, fixtureID, agent, "restart-direct", uri)
	if err != nil {
		t.Fatalf("ResolveSupport after reopen: %v", err)
	}
	if !bytes.Equal(f.Data, unit.Package.Supports[1].Data) {
		t.Fatal("resolved bytes after reopen differ")
	}
}

// TestInstalledPackage_DeepCopyBoundary pins the concurrent-reuse deep-copy
// contract directly: mutating a returned unit or a caller-supplied unit
// never mutates store state.
func TestInstalledPackage_DeepCopyBoundary(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	const agent = "agent-copy-direct"
	unit := installedFixture(t, "copy-direct", agent, skills.OriginGenerated, "1.0.0", 1)
	installAbsent(t, store, ctx, agent, unit)

	// Read side: mutating the returned unit never mutates store state.
	got, err := store.GetInstalledPackage(ctx, fixtureID, agent, "copy-direct")
	if err != nil {
		t.Fatalf("GetInstalledPackage: %v", err)
	}
	got.Skill.Steps[0] = "MUTATED"
	got.Package.Supports[0].Data[0] = 'X'
	got.Package.Skill.Description = "MUTATED"
	got2, err := store.GetInstalledPackage(ctx, fixtureID, agent, "copy-direct")
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	assertUnitEqual(t, got2, unit, "roundtrip after returned-value mutation")

	// Write side: mutating the caller's unit after the put never mutates
	// store state.
	unit.Skill.Steps[0] = "MUTATED"
	unit.Package.Supports[0].Data[0] = 'X'
	got3, err := store.GetInstalledPackage(ctx, fixtureID, agent, "copy-direct")
	if err != nil {
		t.Fatalf("re-read after caller mutation: %v", err)
	}
	assertUnitEqual(t, got3, installedFixture(t, "copy-direct", agent, skills.OriginGenerated, "1.0.0", 1),
		"roundtrip after caller-side mutation")
}

// TestInstalledPackage_ConcurrentStorm is the driver-local race gate: a
// shared-key conditional CAS storm plus per-identity scripts on ONE shared
// store under `-race`. Writers alternate two versions, conditioning on the
// hash they just read; readers must never observe a torn or inconsistent
// unit; per-identity scripts must never bleed into each other. SQLite
// serializes on the driver's single pinned connection; busy_timeout
// absorbs contention and a lost CAS is a normal retried outcome.
func TestInstalledPackage_ConcurrentStorm(t *testing.T) {
	if testing.Short() {
		t.Skip("installed-package storm; -short skips")
	}
	store := openStore(t)
	baseline := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		scriptGoroutines = 24
		stormWriters     = 12
		stormReaders     = 12
	)
	var (
		wg        sync.WaitGroup
		errCount  atomic.Int64
		stormN    atomic.Int64
		stormErrN atomic.Int64
		bleedN    atomic.Int64
		failMsg   atomic.Value
	)
	record := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		if failMsg.Load() == nil {
			failMsg.Store(msg)
		}
		errCount.Add(1)
	}

	// Per-identity deterministic scripts: each goroutine owns a distinct
	// (tenant, user, agent, name) and must never observe another's state.
	runScript := func(gid int) {
		myID := identity.Quadruple{
			Identity: identity.Identity{
				TenantID:  fmt.Sprintf("t-ip-%d", gid),
				UserID:    fmt.Sprintf("u-ip-%d", gid),
				SessionID: fmt.Sprintf("s-ip-%d", gid),
			},
			RunID: fmt.Sprintf("r-ip-%d", gid),
		}
		myAgent := fmt.Sprintf("agent-ip-%d", gid)
		myName := fmt.Sprintf("ip-%d", gid)
		localCtx, localCancel := context.WithCancel(ctx)
		defer localCancel()

		v1 := installedFixture(t, myName, myAgent, skills.OriginGenerated, "1.0.0", 1)
		v2 := installedFixture(t, myName, myAgent, skills.OriginGenerated, "2.0.0", 1)

		r1, err := store.PutInstalledPackage(localCtx, myID, myAgent, v1,
			skills.InstalledPackageCondition{ExpectedAbsent: true}, false)
		if err != nil {
			record("g%d create: %v", gid, err)
			return
		}
		got, err := store.GetInstalledPackage(localCtx, myID, myAgent, myName)
		if err != nil {
			record("g%d get: %v", gid, err)
			return
		}
		if got.PackageHash != v1.PackageHash {
			bleedN.Add(1)
			record("g%d context bleed: hash=%q", gid, got.PackageHash)
			return
		}
		uri, uerr := skills.NewPackageURI(v1.PackageHash, v1.Package.Supports[0].Path)
		if uerr != nil {
			record("g%d NewPackageURI: %v", gid, uerr)
			return
		}
		f, err := store.ResolveSupport(localCtx, myID, myAgent, myName, uri)
		if err != nil || !bytes.Equal(f.Data, v1.Package.Supports[0].Data) {
			bleedN.Add(1)
			record("g%d resolve: err=%v", gid, err)
			return
		}
		r2, err := store.PutInstalledPackage(localCtx, myID, myAgent, v2,
			skills.InstalledPackageCondition{ExpectedHash: v1.PackageHash, ExpectedVersion: v1.Package.Version}, true)
		if err != nil {
			record("g%d replace: %v", gid, err)
			return
		}
		// The create receipt must NOT delete the replace winner.
		deleted, err := store.DeleteInstalledPackage(localCtx, myID, myAgent, myName, r1)
		if err != nil {
			record("g%d stale delete: %v", gid, err)
			return
		}
		if deleted {
			record("g%d stale receipt deleted another winner", gid)
			return
		}
		// Exact compensation: restore the prior over our own write.
		restored, err := store.RestoreInstalledPackage(localCtx, myID, myAgent, myName, r2, v1)
		if err != nil || !restored {
			record("g%d restore: restored=%v err=%v", gid, restored, err)
			return
		}
		// The replace receipt is now stale: deleting it is a no-op.
		deleted, err = store.DeleteInstalledPackage(localCtx, myID, myAgent, myName, r2)
		if err != nil || deleted {
			record("g%d post-restore stale delete: deleted=%v err=%v", gid, deleted, err)
			return
		}
		// Erase exactly the version the create receipt wrote.
		deleted, err = store.DeleteInstalledPackage(localCtx, myID, myAgent, myName, r1)
		if err != nil || !deleted {
			record("g%d final delete: deleted=%v err=%v", gid, deleted, err)
			return
		}
		if _, err := store.GetInstalledPackage(localCtx, myID, myAgent, myName); !errors.Is(err, skills.ErrInstalledPackageNotFound) {
			record("g%d post-erasure get err=%v", gid, err)
		}
	}

	for g := 0; g < scriptGoroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			runScript(gid)
		}(g)
	}

	// Shared-key conditional CAS storm: writers alternate two versions at
	// one key, always conditioning on the hash they just read; readers
	// never observe a torn or inconsistent unit.
	stormID := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t-ip-storm", UserID: "u-ip-storm", SessionID: "s-ip-storm"},
		RunID:    "r-ip-storm",
	}
	const stormAgent = "agent-ip-storm"
	const stormName = "ip-storm"
	stormA := installedFixture(t, stormName, stormAgent, skills.OriginGenerated, "1.0.0", 1)
	stormB := installedFixture(t, stormName, stormAgent, skills.OriginGenerated, "2.0.0", 1)

	for w := 0; w < stormWriters; w++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			target := stormA
			if seed%2 == 1 {
				target = stormB
			}
			for i := 0; i < 6; i++ {
				cur, err := store.GetInstalledPackage(ctx, stormID, stormAgent, stormName)
				if err != nil {
					if errors.Is(err, skills.ErrInstalledPackageNotFound) {
						if _, perr := store.PutInstalledPackage(ctx, stormID, stormAgent, target,
							skills.InstalledPackageCondition{ExpectedAbsent: true}, false); perr != nil && !errors.Is(perr, skills.ErrInstalledPackageExists) {
							stormErrN.Add(1)
							return
						}
						stormN.Add(1)
						continue
					}
					stormErrN.Add(1)
					return
				}
				_, perr := store.PutInstalledPackage(ctx, stormID, stormAgent, target,
					skills.InstalledPackageCondition{ExpectedHash: cur.PackageHash}, true)
				switch {
				case perr == nil:
					stormN.Add(1)
				case errors.Is(perr, skills.ErrInstalledPackageConditionFailed):
					// Lost CAS race — normal; retry.
				default:
					stormErrN.Add(1)
					return
				}
			}
		}(w)
	}
	for r := 0; r < stormReaders; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 6; i++ {
				got, err := store.GetInstalledPackage(ctx, stormID, stormAgent, stormName)
				if err != nil {
					if errors.Is(err, skills.ErrInstalledPackageNotFound) {
						continue
					}
					stormErrN.Add(1)
					return
				}
				if got.PackageHash != stormA.PackageHash && got.PackageHash != stormB.PackageHash {
					stormErrN.Add(1)
					return
				}
				if err := skills.VerifyPackageHash(got.Package, got.PackageHash); err != nil {
					stormErrN.Add(1)
					return
				}
				uri, uerr := skills.NewPackageURI(got.PackageHash, got.Package.Supports[0].Path)
				if uerr != nil {
					stormErrN.Add(1)
					return
				}
				resolved, rerr := store.ResolveSupport(ctx, stormID, stormAgent, stormName, uri)
				if rerr != nil {
					if errors.Is(rerr, skills.ErrSupportNotFound) {
						// Winner changed between Get and Resolve — the now
						// stale URI was refused, never served wrong bytes.
						continue
					}
					stormErrN.Add(1)
					return
				}
				if resolved.Digest != got.Package.Supports[0].Digest ||
					!bytes.Equal(resolved.Data, got.Package.Supports[0].Data) {
					stormErrN.Add(1)
					return
				}
			}
		}()
	}

	wg.Wait()
	if errCount.Load() != 0 {
		detail := ""
		if m := failMsg.Load(); m != nil {
			detail = fmt.Sprintf(" first: %v", m)
		}
		t.Fatalf("per-identity scripts: %d failures%s", errCount.Load(), detail)
	}
	if bleedN.Load() != 0 {
		t.Fatalf("context bleed detected: %d", bleedN.Load())
	}
	if stormErrN.Load() != 0 {
		t.Fatalf("shared-key storm: %d consistency failures", stormErrN.Load())
	}
	if stormN.Load() == 0 {
		t.Fatal("shared-key storm wrote no packages (test did not run)")
	}
	if err := store.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if delta := runtime.NumGoroutine() - baseline; delta > 8 {
		t.Fatalf("goroutine leak: baseline=%d final=%d delta=%d", baseline, baseline+delta, delta)
	}
}
