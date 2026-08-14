// package_store_suite.go — the shared complete installed-package
// contract scenarios. Every SkillStore driver MUST pass this suite
// (wired through `Run` → `RunInstalledPackageSuite`): the atomic
// installed-package unit (canonical semantic skill + versioned
// PackageHash + ordered support manifest with bounded immutable
// support bytes), the session-zeroed (tenant, user, effective-agent,
// name) target key, conditional put/replace with explicit-replace and
// origin precedence, exact-receipt conditional compensation, erasure,
// identity/agent isolation, one-transaction-per-package atomicity, and
// N>=100 mixed concurrent reuse under `-race`.
//
// The scenarios are declared against the `failureReporter` surface so
// the harness self-test can drive them with an adversarial store and
// prove missing/lying behavior fails loudly (see
// package_store_self_test.go).
package conformancetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
)

// RunInstalledPackageSuite executes the complete installed-package
// contract scenarios against the harness returned by `factory`. Each
// subtest gets its own harness so state is isolated.
func RunInstalledPackageSuite(t *testing.T, factory func(*testing.T) Harness) {
	t.Helper()

	scenarios := []struct {
		name string
		fn   func(failureReporter, Harness)
	}{
		{"installed_minimal_roundtrip", testInstalledMinimalRoundtrip},
		{"installed_multifile_roundtrip", testInstalledMultifileRoundtrip},
		{"installed_resolve_checks", testInstalledResolveChecks},
		{"installed_dangling_impossible", testInstalledDanglingImpossible},
		{"installed_replace_origin_matrix", testInstalledReplaceOriginMatrix},
		{"installed_response_loss_replay", testInstalledResponseLossReplay},
		{"installed_conditional_compensation", testInstalledConditionalCompensation},
		{"installed_identity_agent_isolation", testInstalledIdentityAgentIsolation},
		{"installed_erasure", testInstalledErasure},
		{"installed_legacy_mutation_fence", testInstalledLegacyMutationFence},
		{"installed_closed_sentinels", testInstalledClosedSentinels},
	}
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			h := factory(t)
			defer h.Cleanup()
			s.fn(t, h)
		})
	}

	// The restart hook is provided per-driver (Harness.ReopenedStore);
	// drivers without durable storage leave it nil and the subtest
	// skips, exactly like the legacy restart_survival subtest.
	t.Run("installed_restart_survival", func(t *testing.T) {
		h := factory(t)
		defer h.Cleanup()
		if h.ReopenedStore == nil {
			t.Skip("driver does not support reopen (set Harness.ReopenedStore to enable)")
		}
		testInstalledRestartSurvival(t, h)
	})

	t.Run("installed_concurrent_reuse", func(t *testing.T) {
		if testing.Short() {
			t.Skip("N>=100 mixed installed-package stress; -short skips")
		}
		h := factory(t)
		defer h.Cleanup()
		testInstalledConcurrentReuse(t, h)
	})
}

// installedFixtureUnit returns a valid atomic installed-package unit:
// a canonical complete package with `nFiles` ordered JSON support
// entries, its versioned PackageHash, and a self-consistent stored
// semantic skill (ScopeUser, agent-bound, canonical ContentHash).
func installedFixtureUnit(t failureReporter, name, agentID string, origin skills.Origin, version string, nFiles int) skills.InstalledPackage {
	t.Helper()
	now := time.Now().UTC()
	supports := make([]skills.SupportFile, 0, nFiles)
	for i := 0; i < nFiles; i++ {
		data := []byte(fmt.Sprintf(`{"file": %d, "name": %q}`, i, name))
		supports = append(supports, skills.SupportFile{
			Path:   fmt.Sprintf("examples/file-%02d.json", i),
			Mime:   "application/json",
			Size:   int64(len(data)),
			Digest: hexDigest(data),
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
		t.Fatalf("installedFixtureUnit(%q): PackageHash: %v", name, err)
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

// putAbsent is the suite's create helper: conditionally installs `unit`
// against an absent key and returns the exact receipt.
func putAbsent(t failureReporter, h Harness, ctx context.Context, agentID string, unit skills.InstalledPackage) skills.InstalledPackageReceipt {
	t.Helper()
	r, err := h.Store.PutInstalledPackage(ctx, fixtureID, agentID, unit, skills.InstalledPackageCondition{ExpectedAbsent: true}, false)
	if err != nil {
		t.Fatalf("PutInstalledPackage(create %q): %v", unit.Package.Name, err)
	}
	return r
}

// getUnit is the suite's read helper.
func getUnit(t failureReporter, h Harness, ctx context.Context, id identity.Quadruple, agentID, name string) skills.InstalledPackage {
	t.Helper()
	got, err := h.Store.GetInstalledPackage(ctx, id, agentID, name)
	if err != nil {
		t.Fatalf("GetInstalledPackage(%q): %v", name, err)
	}
	return got
}

// assertUnitConsistent verifies the atomic unit's closed shape AND its
// hash/size/digest truth: the PackageHash is the versioned hash of the
// Package, every manifest entry's bytes hash to its digest and satisfy
// its declared MIME, and the manifest order is preserved. A store that
// returns a torn or lying unit fails here.
func assertUnitConsistent(t failureReporter, got skills.InstalledPackage, msg string) {
	t.Helper()
	if err := skills.ValidateInstalledPackage(got); err != nil {
		t.Fatalf("%s: unit is not internally consistent: %v", msg, err)
	}
	// Manifest order is part of identity: the canonical serializer
	// orders by path, so the stored order must be stable and complete.
	if len(got.Package.Supports) == 0 {
		t.Fatalf("%s: installed package has an empty support manifest", msg)
	}
	for _, f := range got.Package.Supports {
		if f.Data == nil {
			t.Fatalf("%s: support %q carries no bytes (installed form must be self-contained)", msg, f.Path)
		}
		gotURI, err := skills.NewPackageURI(got.PackageHash, f.Path)
		if err != nil {
			t.Fatalf("%s: NewPackageURI(%q): %v", msg, f.Path, err)
		}
		if gotURI.String() == "" {
			t.Fatalf("%s: empty support URI for %q", msg, f.Path)
		}
	}
}

// assertUnitEqual compares the identity-bearing fields of two atomic
// units: the semantic skill envelope + logical body, the package
// envelope + logical content, the ordered support manifest (path,
// MIME, size, digest, bytes), and the versioned PackageHash.
func assertUnitEqual(t failureReporter, got, want skills.InstalledPackage, msg string) {
	t.Helper()
	if got.PackageHash != want.PackageHash {
		t.Fatalf("%s: PackageHash got %q want %q", msg, got.PackageHash, want.PackageHash)
	}
	if got.Skill.Name != want.Skill.Name || got.Skill.AgentID != want.Skill.AgentID ||
		got.Skill.Origin != want.Skill.Origin || got.Skill.Scope != want.Skill.Scope ||
		got.Skill.ContentHash != want.Skill.ContentHash {
		t.Fatalf("%s: skill envelope mismatch:\n  got  %+v\n  want %+v", msg, got.Skill, want.Skill)
	}
	for _, field := range []struct {
		name string
		got  string
		want string
	}{
		{"Title", got.Skill.Title, want.Skill.Title},
		{"Description", got.Skill.Description, want.Skill.Description},
		{"Trigger", got.Skill.Trigger, want.Skill.Trigger},
		{"TaskType", got.Skill.TaskType, want.Skill.TaskType},
	} {
		if field.got != field.want {
			t.Fatalf("%s: skill %s got %q want %q", msg, field.name, field.got, field.want)
		}
	}
	if !stringSlicesEqual(got.Skill.Steps, want.Skill.Steps) {
		t.Fatalf("%s: Steps got %v want %v", msg, got.Skill.Steps, want.Skill.Steps)
	}
	if !stringSlicesEqual(got.Skill.Tags, want.Skill.Tags) {
		t.Fatalf("%s: Tags got %v want %v", msg, got.Skill.Tags, want.Skill.Tags)
	}
	if got.Package.Name != want.Package.Name || got.Package.Version != want.Package.Version {
		t.Fatalf("%s: package envelope got %s@%s want %s@%s",
			msg, got.Package.Name, got.Package.Version, want.Package.Name, want.Package.Version)
	}
	if got.Package.Skill.Name != want.Package.Skill.Name || got.Package.Skill.Trigger != want.Package.Skill.Trigger {
		t.Fatalf("%s: package logical content mismatch", msg)
	}
	if len(got.Package.Supports) != len(want.Package.Supports) {
		t.Fatalf("%s: support manifest length got %d want %d", msg, len(got.Package.Supports), len(want.Package.Supports))
	}
	for i := range want.Package.Supports {
		w := want.Package.Supports[i]
		g := got.Package.Supports[i]
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

// testInstalledMinimalRoundtrip pins the minimal installed-package
// round trip: a package with one support file is committed under
// session A, read back byte-identically from session B (the stored
// session is zeroed on the ScopeUser rung), the legacy agent-bound
// read surface reflects the atomic skill row, and both the returned
// and the caller's unit are deep-copied (mutation never mutates store
// state).
func testInstalledMinimalRoundtrip(t failureReporter, h Harness) {
	ctx := context.Background()
	const agent = "agent-minimal"
	unit := installedFixtureUnit(t, "minimal", agent, skills.OriginGenerated, "1.0.0", 1)

	r := putAbsent(t, h, ctx, agent, unit)
	if r.WrittenHash != unit.PackageHash || r.PriorHash != "" {
		t.Fatalf("create receipt = %+v, want WrittenHash=%q PriorHash=\"\"", r, unit.PackageHash)
	}
	if r.TenantID != fixtureID.TenantID || r.UserID != fixtureID.UserID || r.AgentID != agent || r.Name != "minimal" {
		t.Fatalf("create receipt key = %+v, want the exact (tenant, user, agent, name)", r)
	}

	// Cross-session read: any session of the same (tenant, user) sees
	// the same package because the stored session component is zeroed.
	sessionB := fixtureID
	sessionB.SessionID = "s-conformance-pkg-B"
	got := getUnit(t, h, ctx, sessionB, agent, "minimal")
	assertUnitEqual(t, got, unit, "minimal roundtrip from session B")
	assertUnitConsistent(t, got, "minimal roundtrip")

	// The atomic unit includes the canonical stored skill row: the
	// legacy agent-bound user-scope read reflects it.
	row, err := h.Store.GetScopeAgent(ctx, sessionB, agent, "minimal", skills.ScopeUser)
	if err != nil {
		t.Fatalf("GetScopeAgent after put: %v", err)
	}
	if row.ContentHash != unit.Skill.ContentHash || row.AgentID != agent || row.Scope != skills.ScopeUser {
		t.Fatalf("GetScopeAgent row = %+v, want the installed skill row", row)
	}

	// Deep-copy guarantee, read side: mutating the returned unit never
	// mutates store state.
	got.Skill.Steps[0] = "MUTATED"
	got.Package.Supports[0].Data[0] = 'X'
	got.Package.Skill.Description = "MUTATED"
	got2 := getUnit(t, h, ctx, sessionB, agent, "minimal")
	assertUnitEqual(t, got2, unit, "roundtrip after returned-value mutation")

	// Deep-copy guarantee, write side: mutating the caller's unit after
	// the put never mutates store state.
	unit.Skill.Steps[0] = "MUTATED"
	unit.Package.Supports[0].Data[0] = 'X'
	got3 := getUnit(t, h, ctx, sessionB, agent, "minimal")
	assertUnitEqual(t, got3, installedFixtureUnit(t, "minimal", agent, skills.OriginGenerated, "1.0.0", 1),
		"roundtrip after caller-side mutation")
}

// testInstalledMultifileRoundtrip pins the multi-file round trip: the
// ordered support manifest (paths, MIME, exact sizes, digests, bytes)
// survives byte-identically across a commit and a read.
func testInstalledMultifileRoundtrip(t failureReporter, h Harness) {
	ctx := context.Background()
	const agent = "agent-multifile"
	unit := installedFixtureUnit(t, "multifile", agent, skills.OriginPack, "1.0.0", 4)
	putAbsent(t, h, ctx, agent, unit)

	got := getUnit(t, h, ctx, fixtureID, agent, "multifile")
	assertUnitEqual(t, got, unit, "multi-file roundtrip")
	assertUnitConsistent(t, got, "multi-file roundtrip")
	if len(got.Package.Supports) != 4 {
		t.Fatalf("multi-file manifest length = %d, want 4", len(got.Package.Supports))
	}
	// Ordered by canonical path: file-00 … file-03.
	for i, f := range got.Package.Supports {
		want := fmt.Sprintf("examples/file-%02d.json", i)
		if f.Path != want {
			t.Fatalf("manifest[%d].Path = %q, want %q (ordered support manifest)", i, f.Path, want)
		}
	}
}

// testInstalledRestartSurvival pins the restart hook: a committed
// package (body + every support byte) survives Close → reopen over the
// same backing storage, exactly as a new session would read it.
func testInstalledRestartSurvival(t failureReporter, h Harness) {
	ctx := context.Background()
	const agent = "agent-restart"
	unit := installedFixtureUnit(t, "restart", agent, skills.OriginPack, "1.0.0", 2)
	putAbsent(t, h, ctx, agent, unit)

	if err := h.Store.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	store, err := h.ReopenedStore()
	if err != nil {
		t.Fatalf("ReopenedStore: %v", err)
	}
	defer func() { _ = store.Close(ctx) }()

	got, err := store.GetInstalledPackage(ctx, fixtureID, agent, "restart")
	if err != nil {
		t.Fatalf("GetInstalledPackage after reopen: %v", err)
	}
	assertUnitEqual(t, got, unit, "restart survival")
	// Support bytes resolve after restart too — the installed form is
	// self-contained, never a staging dereference.
	uri, err := skills.NewPackageURI(unit.PackageHash, unit.Package.Supports[1].Path)
	if err != nil {
		t.Fatalf("NewPackageURI: %v", err)
	}
	f, err := store.ResolveSupport(ctx, fixtureID, agent, "restart", uri)
	if err != nil {
		t.Fatalf("ResolveSupport after reopen: %v", err)
	}
	if !bytes.Equal(f.Data, unit.Package.Supports[1].Data) {
		t.Fatalf("resolved bytes after reopen differ")
	}
}

// testInstalledResolveChecks pins the authorized bounded resolve-by-
// exact-URI surface: hash/size/MIME/digest truth per resolved support
// file, the URI round trip, and the typed refusals for foreign hashes,
// dangling paths, malformed URIs, and missing packages.
func testInstalledResolveChecks(t failureReporter, h Harness) {
	ctx := context.Background()
	const agent = "agent-resolve"
	unit := installedFixtureUnit(t, "resolve", agent, skills.OriginGenerated, "1.0.0", 3)
	putAbsent(t, h, ctx, agent, unit)

	// Every manifest entry resolves by its exact immutable URI with
	// exact path / MIME / size / digest / bytes.
	for _, want := range unit.Package.Supports {
		uri, err := skills.NewPackageURI(unit.PackageHash, want.Path)
		if err != nil {
			t.Fatalf("NewPackageURI(%q): %v", want.Path, err)
		}
		// URI canonical string round-trips through the strict parser.
		parsed, err := skills.ParsePackageURI(uri.String())
		if err != nil {
			t.Fatalf("ParsePackageURI(%q): %v", uri.String(), err)
		}
		if parsed.Hash != unit.PackageHash || parsed.Path != want.Path {
			t.Fatalf("URI round-trip = %+v, want hash %q path %q", parsed, unit.PackageHash, want.Path)
		}
		got, err := h.Store.ResolveSupport(ctx, fixtureID, agent, "resolve", uri)
		if err != nil {
			t.Fatalf("ResolveSupport(%q): %v", want.Path, err)
		}
		if got.Path != want.Path || got.Mime != want.Mime || got.Size != want.Size || got.Digest != want.Digest {
			t.Fatalf("ResolveSupport(%q) metadata = %+v, want %+v", want.Path, got, want)
		}
		if !bytes.Equal(got.Data, want.Data) {
			t.Fatalf("ResolveSupport(%q) bytes differ", want.Path)
		}
	}

	// A foreign package's hash never resolves against this package.
	other := installedFixtureUnit(t, "resolve-other", agent, skills.OriginGenerated, "1.0.0", 1)
	foreignURI, err := skills.NewPackageURI(other.PackageHash, unit.Package.Supports[0].Path)
	if err != nil {
		t.Fatalf("NewPackageURI(foreign): %v", err)
	}
	if _, err := h.Store.ResolveSupport(ctx, fixtureID, agent, "resolve", foreignURI); !errors.Is(err, skills.ErrSupportNotFound) {
		t.Fatalf("foreign-hash resolve err=%v, want ErrSupportNotFound", err)
	}

	// A dangling canonical path (valid but absent from the manifest)
	// never resolves.
	danglingURI, err := skills.NewPackageURI(unit.PackageHash, "examples/nope.json")
	if err != nil {
		t.Fatalf("NewPackageURI(dangling): %v", err)
	}
	if _, err := h.Store.ResolveSupport(ctx, fixtureID, agent, "resolve", danglingURI); !errors.Is(err, skills.ErrSupportNotFound) {
		t.Fatalf("dangling-path resolve err=%v, want ErrSupportNotFound", err)
	}

	// A malformed URI (never produced by ParsePackageURI) fails closed.
	if _, err := h.Store.ResolveSupport(ctx, fixtureID, agent, "resolve", skills.PackageURI{Hash: "not-a-hash", Path: "../evil"}); !errors.Is(err, skills.ErrSupportNotFound) {
		t.Fatalf("malformed-URI resolve err=%v, want ErrSupportNotFound", err)
	}

	// A missing package names the typed not-found sentinel.
	if _, err := h.Store.ResolveSupport(ctx, fixtureID, agent, "no-such-package", foreignURI); !errors.Is(err, skills.ErrInstalledPackageNotFound) {
		t.Fatalf("missing-package resolve err=%v, want ErrInstalledPackageNotFound", err)
	}
}

// testInstalledDanglingImpossible pins the "readers never see the body
// without every support byte" invariants: an atomic unit whose manifest
// lacks bytes or whose PackageHash lies is rejected BEFORE any state is
// written; a failed conditional put leaves no partial state; and every
// installed package is fully resolvable (nothing dangles within the
// installed unit).
func testInstalledDanglingImpossible(t failureReporter, h Harness) {
	ctx := context.Background()
	const agent = "agent-dangle"

	// Manifest entry without its bounded immutable support bytes: the
	// installed form must never force a staging dereference.
	noBytes := installedFixtureUnit(t, "dangle-nil", agent, skills.OriginGenerated, "1.0.0", 1)
	noBytes.Package.Supports[0].Data = nil
	if _, err := h.Store.PutInstalledPackage(ctx, fixtureID, agent, noBytes,
		skills.InstalledPackageCondition{ExpectedAbsent: true}, false); !errors.Is(err, skills.ErrInstalledPackageInvalid) {
		t.Fatalf("nil-Data put err=%v, want ErrInstalledPackageInvalid", err)
	}
	if _, err := h.Store.GetInstalledPackage(ctx, fixtureID, agent, "dangle-nil"); !errors.Is(err, skills.ErrInstalledPackageNotFound) {
		t.Fatalf("nil-Data put left partial state: GetInstalledPackage err=%v, want ErrInstalledPackageNotFound", err)
	}
	if _, err := h.Store.GetScopeAgent(ctx, fixtureID, agent, "dangle-nil", skills.ScopeUser); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("nil-Data put left a skill row: GetScopeAgent err=%v, want ErrSkillNotFound", err)
	}

	// A PackageHash that lies about the package content is rejected.
	lying := installedFixtureUnit(t, "dangle-lying", agent, skills.OriginGenerated, "1.0.0", 1)
	lying.PackageHash = "v1:0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := h.Store.PutInstalledPackage(ctx, fixtureID, agent, lying,
		skills.InstalledPackageCondition{ExpectedAbsent: true}, false); !errors.Is(err, skills.ErrInstalledPackageInvalid) {
		t.Fatalf("lying-hash put err=%v, want ErrInstalledPackageInvalid", err)
	}

	// A failed conditional put leaves no partial state: the prior
	// winner is untouched and the loser wrote nothing.
	v1 := installedFixtureUnit(t, "dangle-cond", agent, skills.OriginGenerated, "1.0.0", 1)
	putAbsent(t, h, ctx, agent, v1)
	v2 := installedFixtureUnit(t, "dangle-cond", agent, skills.OriginGenerated, "2.0.0", 2)
	if _, err := h.Store.PutInstalledPackage(ctx, fixtureID, agent, v2,
		skills.InstalledPackageCondition{ExpectedHash: "v1:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}, true); !errors.Is(err, skills.ErrInstalledPackageConditionFailed) {
		t.Fatalf("condition-failed put err=%v, want ErrInstalledPackageConditionFailed", err)
	}
	got := getUnit(t, h, ctx, fixtureID, agent, "dangle-cond")
	assertUnitEqual(t, got, v1, "failed conditional put must leave the exact prior winner")
	assertUnitConsistent(t, got, "failed conditional put")
	if _, err := h.Store.ResolveSupport(ctx, fixtureID, agent, "dangle-cond",
		supportURI(t, v1, v1.Package.Supports[0].Path)); err != nil {
		t.Fatalf("prior winner's support unresolvable after failed put: %v", err)
	}

	// Completeness: every support entry of an installed package
	// resolves with its exact bytes — nothing dangles within the unit.
	for _, f := range v1.Package.Supports {
		if _, err := h.Store.ResolveSupport(ctx, fixtureID, agent, "dangle-cond", supportURI(t, v1, f.Path)); err != nil {
			t.Fatalf("installed support %q unresolvable: %v", f.Path, err)
		}
	}
}

func supportURI(t failureReporter, unit skills.InstalledPackage, path string) skills.PackageURI {
	t.Helper()
	u, err := skills.NewPackageURI(unit.PackageHash, path)
	if err != nil {
		t.Fatalf("NewPackageURI(%q): %v", path, err)
	}
	return u
}

// testInstalledReplaceOriginMatrix pins the explicit-replace + origin-
// precedence matrix: every incoming/existing Origin pair has the exact
// create / no-replace / replace acceptance rows, generated input never
// overwrites a pack winner (even explicitly), and every refused pair
// leaves the exact prior winner untouched.
func testInstalledReplaceOriginMatrix(t failureReporter, h Harness) {
	ctx := context.Background()
	const agent = "agent-matrix"

	existing := []skills.Origin{skills.OriginPack, skills.OriginGenerated}
	incoming := []skills.Origin{skills.OriginPack, skills.OriginGenerated}
	for _, ex := range existing {
		for _, in := range incoming {
			name := "matrix-" + string(ex) + "-" + string(in)
			exUnit := installedFixtureUnit(t, name, agent, ex, "1.0.0", 1)
			putAbsent(t, h, ctx, agent, exUnit)

			inUnit := installedFixtureUnit(t, name, agent, in, "2.0.0", 1)
			cond := skills.InstalledPackageCondition{
				ExpectedHash:    exUnit.PackageHash,
				ExpectedVersion: exUnit.Package.Version,
			}

			// A present winner is never replaced implicitly.
			if _, err := h.Store.PutInstalledPackage(ctx, fixtureID, agent, inUnit, cond, false); !errors.Is(err, skills.ErrInstalledPackageReplaceRequired) {
				t.Fatalf("%s over %s replace=false: err=%v, want ErrInstalledPackageReplaceRequired", in, ex, err)
			}
			assertWinner(t, h, ctx, agent, name, exUnit, "replace=false refused pair")

			// Explicit replace: generated NEVER overwrites pack; every
			// other pair replaces.
			_, err := h.Store.PutInstalledPackage(ctx, fixtureID, agent, inUnit, cond, true)
			if in == skills.OriginGenerated && ex == skills.OriginPack {
				if !errors.Is(err, skills.ErrPackOverwriteRefused) {
					t.Fatalf("generated over pack err=%v, want ErrPackOverwriteRefused", err)
				}
				assertWinner(t, h, ctx, agent, name, exUnit, "generated-over-pack refused pair")
			} else {
				if err != nil {
					t.Fatalf("%s over %s replace=true: %v", in, ex, err)
				}
				assertWinner(t, h, ctx, agent, name, inUnit, "explicit replace pair")
			}
		}
	}

	// A condition that does not match the winner fails without touching
	// it, even with replace=true.
	unit := installedFixtureUnit(t, "matrix-cond", agent, skills.OriginPack, "1.0.0", 1)
	putAbsent(t, h, ctx, agent, unit)
	other := installedFixtureUnit(t, "matrix-cond", agent, skills.OriginPack, "2.0.0", 1)
	if _, err := h.Store.PutInstalledPackage(ctx, fixtureID, agent, other,
		skills.InstalledPackageCondition{ExpectedHash: unit.PackageHash, ExpectedVersion: "9.9.9"}, true); !errors.Is(err, skills.ErrInstalledPackageConditionFailed) {
		t.Fatalf("version-mismatch put err=%v, want ErrInstalledPackageConditionFailed", err)
	}
	assertWinner(t, h, ctx, agent, "matrix-cond", unit, "version-mismatch refused pair")
}

func assertWinner(t failureReporter, h Harness, ctx context.Context, agentID, name string, want skills.InstalledPackage, msg string) {
	t.Helper()
	got, err := h.Store.GetInstalledPackage(ctx, fixtureID, agentID, name)
	if err != nil {
		t.Fatalf("%s: GetInstalledPackage: %v", msg, err)
	}
	assertUnitEqual(t, got, want, msg)
}

// testInstalledResponseLossReplay pins idempotent exact replay: a put
// whose response was lost converges on the same terminal state when the
// caller retries the EXACT same package — including when the original
// create/replace condition is stale on the retry — and the replay
// receipt remains sufficient for exact conditional compensation.
func testInstalledResponseLossReplay(t failureReporter, h Harness) {
	ctx := context.Background()
	const agent = "agent-replay"
	condAbsent := skills.InstalledPackageCondition{ExpectedAbsent: true}

	// Create + response loss → exact retry converges.
	v1 := installedFixtureUnit(t, "replay", agent, skills.OriginGenerated, "1.0.0", 1)
	r1, err := h.Store.PutInstalledPackage(ctx, fixtureID, agent, v1, condAbsent, false)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	r2, err := h.Store.PutInstalledPackage(ctx, fixtureID, agent, v1, condAbsent, false)
	if err != nil {
		t.Fatalf("exact create replay must succeed, got %v", err)
	}
	if r2.WrittenHash != v1.PackageHash || r2.WrittenHash != r1.WrittenHash {
		t.Fatalf("replay receipt = %+v, want WrittenHash=%q", r2, v1.PackageHash)
	}
	assertWinner(t, h, ctx, agent, "replay", v1, "exact create replay")

	// Replace + response loss → the retry carries the ORIGINAL (now
	// stale) condition, yet converges because the winner already IS the
	// incoming package (exact replay precedes the condition check).
	v2 := installedFixtureUnit(t, "replay", agent, skills.OriginGenerated, "2.0.0", 1)
	condV1 := skills.InstalledPackageCondition{ExpectedHash: v1.PackageHash, ExpectedVersion: v1.Package.Version}
	if _, err := h.Store.PutInstalledPackage(ctx, fixtureID, agent, v2, condV1, true); err != nil {
		t.Fatalf("replace v1→v2: %v", err)
	}
	r3, err := h.Store.PutInstalledPackage(ctx, fixtureID, agent, v2, condV1, true)
	if err != nil {
		t.Fatalf("exact replace replay with stale condition must succeed, got %v", err)
	}
	if r3.WrittenHash != v2.PackageHash {
		t.Fatalf("replace-replay receipt WrittenHash=%q, want %q", r3.WrittenHash, v2.PackageHash)
	}
	assertWinner(t, h, ctx, agent, "replay", v2, "exact replace replay")

	// The replay receipt is sufficient for exact conditional
	// compensation: it deletes exactly the version it names.
	deleted, err := h.Store.DeleteInstalledPackage(ctx, fixtureID, agent, "replay", r3)
	if err != nil {
		t.Fatalf("DeleteInstalledPackage(replay receipt): %v", err)
	}
	if !deleted {
		t.Fatalf("replay receipt did not delete the exact version it wrote")
	}
	if _, err := h.Store.GetInstalledPackage(ctx, fixtureID, agent, "replay"); !errors.Is(err, skills.ErrInstalledPackageNotFound) {
		t.Fatalf("replay-compensated package still present: err=%v, want ErrInstalledPackageNotFound", err)
	}
}

// testInstalledConditionalCompensation pins the exact-receipt
// compensation contract: a receipt restores or deletes ONLY the version
// it wrote; another proposal's winner is never overwritten or deleted;
// compensation does not depend on the original put response (a
// plan-constructed receipt — the crash-recovery shape — works); and an
// invalid or wrong-prior restore fails loudly without mutation.
func testInstalledConditionalCompensation(t failureReporter, h Harness) {
	ctx := context.Background()
	const agent = "agent-comp"

	v1 := installedFixtureUnit(t, "comp", agent, skills.OriginGenerated, "1.0.0", 1)
	v2 := installedFixtureUnit(t, "comp", agent, skills.OriginGenerated, "2.0.0", 1)
	v3 := installedFixtureUnit(t, "comp", agent, skills.OriginGenerated, "3.0.0", 1)

	// Proposal A creates v1; proposal B explicitly replaces it with v2.
	rA := putAbsent(t, h, ctx, agent, v1)
	rB, err := h.Store.PutInstalledPackage(ctx, fixtureID, agent, v2,
		skills.InstalledPackageCondition{ExpectedHash: v1.PackageHash, ExpectedVersion: v1.Package.Version}, true)
	if err != nil {
		t.Fatalf("proposal B replace: %v", err)
	}
	if rB.PriorHash != v1.PackageHash || rB.PriorVersion != v1.Package.Version {
		t.Fatalf("replace receipt prior = %q/%q, want %q/%q",
			rB.PriorHash, rB.PriorVersion, v1.PackageHash, v1.Package.Version)
	}

	// A's compensation: delete ONLY the version A wrote. The winner is
	// B's v2, so A's receipt is a no-op — never another proposal's
	// winner.
	deleted, err := h.Store.DeleteInstalledPackage(ctx, fixtureID, agent, "comp", rA)
	if err != nil {
		t.Fatalf("A compensation delete: %v", err)
	}
	if deleted {
		t.Fatalf("A's stale receipt deleted B's winner")
	}
	assertWinner(t, h, ctx, agent, "comp", v2, "A's stale receipt must not touch B's winner")

	// B's compensation: restore the exact prior (v1) over B's own write.
	restored, err := h.Store.RestoreInstalledPackage(ctx, fixtureID, agent, "comp", rB, v1)
	if err != nil {
		t.Fatalf("B compensation restore: %v", err)
	}
	if !restored {
		t.Fatalf("B's current receipt did not restore its exact prior")
	}
	assertWinner(t, h, ctx, agent, "comp", v1, "B's compensation restored the exact prior")

	// B's compensation again: B's write is no longer the winner → no-op.
	restored, err = h.Store.RestoreInstalledPackage(ctx, fixtureID, agent, "comp", rB, v1)
	if err != nil {
		t.Fatalf("B stale restore: %v", err)
	}
	if restored {
		t.Fatalf("stale restore mutated the winner")
	}

	// Proposal C replaces v1 with v3; B's stale receipt must not touch
	// C's winner.
	if _, err := h.Store.PutInstalledPackage(ctx, fixtureID, agent, v3,
		skills.InstalledPackageCondition{ExpectedHash: v1.PackageHash, ExpectedVersion: v1.Package.Version}, true); err != nil {
		t.Fatalf("proposal C replace: %v", err)
	}
	restored, err = h.Store.RestoreInstalledPackage(ctx, fixtureID, agent, "comp", rB, v1)
	if err != nil {
		t.Fatalf("C-era stale restore: %v", err)
	}
	if restored {
		t.Fatalf("B's stale receipt replaced C's winner")
	}
	assertWinner(t, h, ctx, agent, "comp", v3, "C's winner must survive B's stale receipt")

	// C's compensation: delete exactly C's version.
	rC, err := h.Store.GetInstalledPackage(ctx, fixtureID, agent, "comp")
	if err != nil {
		t.Fatalf("GetInstalledPackage: %v", err)
	}
	cReceipt := skills.InstalledPackageReceipt{
		TenantID: fixtureID.TenantID, UserID: fixtureID.UserID, AgentID: agent, Name: "comp",
		WrittenHash: rC.PackageHash, WrittenVersion: rC.Package.Version,
		PriorHash: v1.PackageHash, PriorVersion: v1.Package.Version,
	}
	deleted, err = h.Store.DeleteInstalledPackage(ctx, fixtureID, agent, "comp", cReceipt)
	if err != nil || !deleted {
		t.Fatalf("C compensation delete: deleted=%v err=%v", deleted, err)
	}
	if _, err := h.Store.GetInstalledPackage(ctx, fixtureID, agent, "comp"); !errors.Is(err, skills.ErrInstalledPackageNotFound) {
		t.Fatalf("C-compensated package still present: err=%v", err)
	}
	deleted, err = h.Store.DeleteInstalledPackage(ctx, fixtureID, agent, "comp", cReceipt)
	if err != nil || deleted {
		t.Fatalf("already-compensated receipt: deleted=%v err=%v (want (false, nil))", deleted, err)
	}

	// Plan-constructed receipt (crash-recovery shape): compensation does
	// NOT depend on the original put response. The durable plan holds
	// {written v2, prior v1}; a receipt reconstructed from it restores
	// exactly the prior.
	putAbsent(t, h, ctx, agent, v1)
	if _, err := h.Store.PutInstalledPackage(ctx, fixtureID, agent, v2,
		skills.InstalledPackageCondition{ExpectedHash: v1.PackageHash, ExpectedVersion: v1.Package.Version}, true); err != nil {
		t.Fatalf("plan seed replace: %v", err)
	}
	planReceipt := skills.InstalledPackageReceipt{
		TenantID: fixtureID.TenantID, UserID: fixtureID.UserID, AgentID: agent, Name: "comp",
		WrittenHash: v2.PackageHash, WrittenVersion: v2.Package.Version,
		PriorHash: v1.PackageHash, PriorVersion: v1.Package.Version,
	}
	restored, err = h.Store.RestoreInstalledPackage(ctx, fixtureID, agent, "comp", planReceipt, v1)
	if err != nil || !restored {
		t.Fatalf("plan-constructed receipt restore: restored=%v err=%v", restored, err)
	}
	assertWinner(t, h, ctx, agent, "comp", v1, "plan-constructed compensation restored the exact prior")

	// A wrong prior (not the exact package the receipt displaced) fails
	// loudly and mutates nothing.
	v1Prior := installedFixtureUnit(t, "comp-prior", agent, skills.OriginGenerated, "1.0.0", 1)
	rCreate := putAbsent(t, h, ctx, agent, v1Prior)
	replaceUnit := installedFixtureUnit(t, "comp-prior", agent, skills.OriginGenerated, "2.0.0", 1)
	rReplace, err := h.Store.PutInstalledPackage(ctx, fixtureID, agent, replaceUnit,
		skills.InstalledPackageCondition{ExpectedHash: v1Prior.PackageHash, ExpectedVersion: v1Prior.Package.Version}, true)
	if err != nil {
		t.Fatalf("wrong-prior seed replace: %v", err)
	}
	wrongPrior := installedFixtureUnit(t, "comp-prior", agent, skills.OriginGenerated, "3.0.0", 1)
	if _, err := h.Store.RestoreInstalledPackage(ctx, fixtureID, agent, "comp-prior", rReplace, wrongPrior); !errors.Is(err, skills.ErrInstalledPackageConditionFailed) {
		t.Fatalf("wrong-prior restore err=%v, want ErrInstalledPackageConditionFailed", err)
	}
	assertWinner(t, h, ctx, agent, "comp-prior", replaceUnit, "wrong-prior restore must not mutate the winner")

	// A receipt recording an ABSENT prior (a create receipt) is
	// compensated by Delete, not Restore — the ambiguity fails loudly.
	if _, err := h.Store.RestoreInstalledPackage(ctx, fixtureID, agent, "comp-prior", rCreate, replaceUnit); !errors.Is(err, skills.ErrInstalledPackageInvalid) {
		t.Fatalf("absent-prior restore err=%v, want ErrInstalledPackageInvalid", err)
	}
}

// testInstalledIdentityAgentIsolation pins the identity-mandatory and
// agent-scoped contract of every installed-package method: missing
// triples fail closed, the target key is exactly (tenant, user,
// session-zeroed, effective-agent, name), and cross-session visibility
// coexists with cross-user / cross-tenant / cross-agent invisibility
// (the agent is selection metadata, never an isolation principal).
func testInstalledIdentityAgentIsolation(t failureReporter, h Harness) {
	ctx := context.Background()
	const agent = "agent-isolate"

	unit := installedFixtureUnit(t, "isolate", agent, skills.OriginGenerated, "1.0.0", 1)
	putAbsent(t, h, ctx, agent, unit)
	uri := supportURI(t, unit, unit.Package.Supports[0].Path)

	// Missing triple → ErrIdentityRequired on every method.
	bad := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u"}} // missing session
	cases := []struct {
		name string
		fn   func() error
	}{
		{"GetInstalledPackage", func() error { _, err := h.Store.GetInstalledPackage(ctx, bad, agent, "isolate"); return err }},
		{"ResolveSupport", func() error { _, err := h.Store.ResolveSupport(ctx, bad, agent, "isolate", uri); return err }},
		{"PutInstalledPackage", func() error {
			_, err := h.Store.PutInstalledPackage(ctx, bad, agent, unit, skills.InstalledPackageCondition{ExpectedAbsent: true}, false)
			return err
		}},
		{"DeleteInstalledPackage", func() error {
			_, err := h.Store.DeleteInstalledPackage(ctx, bad, agent, "isolate", skills.InstalledPackageReceipt{
				TenantID: bad.TenantID, UserID: bad.UserID, AgentID: agent, Name: "isolate", WrittenHash: unit.PackageHash,
			})
			return err
		}},
		{"RestoreInstalledPackage", func() error {
			_, err := h.Store.RestoreInstalledPackage(ctx, bad, agent, "isolate", skills.InstalledPackageReceipt{
				TenantID: bad.TenantID, UserID: bad.UserID, AgentID: agent, Name: "isolate", WrittenHash: unit.PackageHash,
				PriorHash: unit.PackageHash,
			}, unit)
			return err
		}},
	}
	for _, c := range cases {
		if err := c.fn(); err == nil {
			t.Fatalf("%s: expected ErrIdentityRequired, got nil", c.name)
		}
	}

	// Cross-session visibility (stored session zeroed on the ScopeUser
	// rung) with cross-user / cross-tenant / cross-agent invisibility.
	sessionB := fixtureID
	sessionB.SessionID = "s-conformance-pkg-B"
	otherUser := fixtureID
	otherUser.UserID = "u-conformance-pkg-OTHER"
	otherTenant := fixtureID
	otherTenant.TenantID = "t-conformance-pkg-OTHER"

	if _, err := h.Store.GetInstalledPackage(ctx, sessionB, agent, "isolate"); err != nil {
		t.Fatalf("cross-session GetInstalledPackage: %v (installed packages are session-zeroed)", err)
	}
	if _, err := h.Store.GetInstalledPackage(ctx, otherUser, agent, "isolate"); !errors.Is(err, skills.ErrInstalledPackageNotFound) {
		t.Fatalf("cross-user GetInstalledPackage err=%v, want ErrInstalledPackageNotFound", err)
	}
	if _, err := h.Store.GetInstalledPackage(ctx, otherTenant, agent, "isolate"); !errors.Is(err, skills.ErrInstalledPackageNotFound) {
		t.Fatalf("cross-tenant GetInstalledPackage err=%v, want ErrInstalledPackageNotFound", err)
	}
	if _, err := h.Store.GetInstalledPackage(ctx, fixtureID, "agent-OTHER", "isolate"); !errors.Is(err, skills.ErrInstalledPackageNotFound) {
		t.Fatalf("cross-agent GetInstalledPackage err=%v, want ErrInstalledPackageNotFound", err)
	}
	if _, err := h.Store.ResolveSupport(ctx, otherUser, agent, "isolate", uri); !errors.Is(err, skills.ErrInstalledPackageNotFound) {
		t.Fatalf("cross-user ResolveSupport err=%v, want ErrInstalledPackageNotFound", err)
	}
	if _, err := h.Store.ResolveSupport(ctx, fixtureID, "agent-OTHER", "isolate", uri); !errors.Is(err, skills.ErrInstalledPackageNotFound) {
		t.Fatalf("cross-agent ResolveSupport err=%v, want ErrInstalledPackageNotFound", err)
	}

	// The effective agent is bound to the key: a unit whose Skill.AgentID
	// disagrees with the target agent is rejected (no selectable agent).
	misbound := installedFixtureUnit(t, "isolate-misbound", "agent-OTHER", skills.OriginGenerated, "1.0.0", 1)
	if _, err := h.Store.PutInstalledPackage(ctx, fixtureID, agent, misbound,
		skills.InstalledPackageCondition{ExpectedAbsent: true}, false); !errors.Is(err, skills.ErrInstalledPackageInvalid) {
		t.Fatalf("misbound-agent put err=%v, want ErrInstalledPackageInvalid", err)
	}
	// No scope is selectable: a non-user scope fails closed.
	wrongScope := installedFixtureUnit(t, "isolate-scope", agent, skills.OriginGenerated, "1.0.0", 1)
	wrongScope.Skill.Scope = skills.ScopeSession
	if _, err := h.Store.PutInstalledPackage(ctx, fixtureID, agent, wrongScope,
		skills.InstalledPackageCondition{ExpectedAbsent: true}, false); !errors.Is(err, skills.ErrInstalledPackageInvalid) {
		t.Fatalf("non-user-scope put err=%v, want ErrInstalledPackageInvalid", err)
	}

	// Receipts never apply to a foreign key: compensating with a receipt
	// whose key disagrees with the caller's identity fails loudly.
	foreignReceipt := skills.InstalledPackageReceipt{
		TenantID: "other-tenant", UserID: fixtureID.UserID, AgentID: agent, Name: "isolate", WrittenHash: unit.PackageHash,
	}
	if _, err := h.Store.DeleteInstalledPackage(ctx, fixtureID, agent, "isolate", foreignReceipt); !errors.Is(err, skills.ErrInstalledPackageInvalid) {
		t.Fatalf("foreign-key receipt delete err=%v, want ErrInstalledPackageInvalid", err)
	}

	// Cancellation is honored: a canceled ctx fails the operation.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := h.Store.GetInstalledPackage(canceled, fixtureID, agent, "isolate"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled GetInstalledPackage err=%v, want context.Canceled", err)
	}
	if _, err := h.Store.ResolveSupport(canceled, fixtureID, agent, "isolate", uri); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ResolveSupport err=%v, want context.Canceled", err)
	}
	if _, err := h.Store.PutInstalledPackage(canceled, fixtureID, agent, unit,
		skills.InstalledPackageCondition{ExpectedAbsent: true}, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled PutInstalledPackage err=%v, want context.Canceled", err)
	}
	if _, err := h.Store.DeleteInstalledPackage(canceled, fixtureID, agent, "isolate", skills.InstalledPackageReceipt{
		TenantID: fixtureID.TenantID, UserID: fixtureID.UserID, AgentID: agent, Name: "isolate", WrittenHash: unit.PackageHash,
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled DeleteInstalledPackage err=%v, want context.Canceled", err)
	}
}

// testInstalledErasure pins the delete/erasure semantics: erasing the
// atomic unit removes the package AND the legacy skill row AND the
// resolve surface; session erasure (DeleteSessionScope) removes
// session-local rows but NEVER the durable session-zeroed installed
// package; and an already-erased receipt is a no-op.
func testInstalledErasure(t failureReporter, h Harness) {
	ctx := context.Background()
	const agent = "agent-erase"

	unit := installedFixtureUnit(t, "erase", agent, skills.OriginGenerated, "1.0.0", 2)
	r := putAbsent(t, h, ctx, agent, unit)
	uri := supportURI(t, unit, unit.Package.Supports[0].Path)

	// Session erasure must NOT touch the durable installed package (it
	// is session-zeroed on the ScopeUser rung), even as it sweeps the
	// session's own legacy rows.
	sessRow := skills.Skill{
		Name: "erase-session-row", Trigger: "t", Steps: []string{"s"},
		Origin: skills.OriginGenerated, Scope: skills.ScopeSession,
	}
	sessRow.ContentHash = skills.CanonicalContentHash(sessRow)
	if err := h.Store.Upsert(ctx, fixtureID, sessRow); err != nil {
		t.Fatalf("seed session row: %v", err)
	}
	if err := h.Store.DeleteSessionScope(ctx, fixtureID); err != nil {
		t.Fatalf("DeleteSessionScope: %v", err)
	}
	if _, err := h.Store.Get(ctx, fixtureID, "erase-session-row"); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("session row survived erasure: err=%v", err)
	}
	if got, err := h.Store.GetInstalledPackage(ctx, fixtureID, agent, "erase"); err != nil {
		t.Fatalf("DeleteSessionScope erased the installed package: %v", err)
	} else {
		assertUnitEqual(t, got, unit, "session erasure must not touch installed packages")
	}

	// Exact-receipt erasure removes the whole atomic unit: package,
	// skill row, and resolve surface.
	deleted, err := h.Store.DeleteInstalledPackage(ctx, fixtureID, agent, "erase", r)
	if err != nil || !deleted {
		t.Fatalf("DeleteInstalledPackage: deleted=%v err=%v", deleted, err)
	}
	if _, err := h.Store.GetInstalledPackage(ctx, fixtureID, agent, "erase"); !errors.Is(err, skills.ErrInstalledPackageNotFound) {
		t.Fatalf("package survived erasure: err=%v", err)
	}
	if _, err := h.Store.GetScopeAgent(ctx, fixtureID, agent, "erase", skills.ScopeUser); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("skill row survived erasure: err=%v", err)
	}
	if _, err := h.Store.ResolveSupport(ctx, fixtureID, agent, "erase", uri); !errors.Is(err, skills.ErrInstalledPackageNotFound) {
		t.Fatalf("resolve survived erasure: err=%v", err)
	}

	// Already-erased receipt: no-op, never an error, never a mutation.
	deleted, err = h.Store.DeleteInstalledPackage(ctx, fixtureID, agent, "erase", r)
	if err != nil || deleted {
		t.Fatalf("already-erased receipt: deleted=%v err=%v (want (false, nil))", deleted, err)
	}
}

// testInstalledLegacyMutationFence pins the fail-loud legacy-mutation
// fence around the atomic installed unit: once PutInstalledPackage
// commits a package at the session-zeroed (tenant, user, effective-
// agent, name) key, the legacy mutation paths that share that key —
// DeleteAgent at the ScopeUser rung, and Upsert of a ScopeUser/agent-
// bound row of the same name (the legacy replacement path) — are
// refused with ErrInstalledPackageReadOnly BEFORE any state is touched.
// After every refusal both GetInstalledPackage and the legacy scope
// read return the exact same unit: the installed package can neither be
// torn (membership row deleted, package left) nor silently overwritten
// (row replaced, package stale). Keys with no installed package keep
// their exact legacy behavior (idempotent Upsert, not-found deletes,
// ordinary DeleteAgent of a legacy agent-bound row), and the dedicated
// DeleteInstalledPackage remains the only way to erase the unit.
func testInstalledLegacyMutationFence(t failureReporter, h Harness) {
	ctx := context.Background()
	const agent = "agent-fence"
	unit := installedFixtureUnit(t, "fence", agent, skills.OriginGenerated, "1.0.0", 2)
	r := putAbsent(t, h, ctx, agent, unit)
	uri := supportURI(t, unit, unit.Package.Supports[0].Path)

	assertInstalledUnitIntact(t, h, ctx, agent, "fence", unit, uri, "after install")

	// 1. DeleteAgent at the ScopeUser rung shares the installed
	// membership-row key: refused, never a partial tear.
	if err := h.Store.DeleteAgent(ctx, fixtureID, agent, "fence", skills.ScopeUser); !errors.Is(err, skills.ErrInstalledPackageReadOnly) {
		t.Fatalf("DeleteAgent on installed key err=%v, want ErrInstalledPackageReadOnly", err)
	}
	assertInstalledUnitIntact(t, h, ctx, agent, "fence", unit, uri, "after DeleteAgent refusal")

	// A session-pinned DeleteAgent (any other scope) cannot reach the
	// installed unit: no such legacy row exists, so the legacy
	// not-found result is unchanged.
	if err := h.Store.DeleteAgent(ctx, fixtureID, agent, "fence", skills.ScopeProject); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("session-pinned DeleteAgent err=%v, want ErrSkillNotFound (installed key has no session row)", err)
	}
	assertInstalledUnitIntact(t, h, ctx, agent, "fence", unit, uri, "after session-pinned DeleteAgent")

	// 2. Upsert of a ScopeUser/agent-bound row of the same name — the
	// legacy replacement path sharing the key — is refused: the unit is
	// never silently overwritten.
	hostile := unit.Skill
	hostile.Description = "HOSTILE OVERWRITE"
	hostile.ContentHash = skills.CanonicalContentHash(hostile)
	if err := h.Store.Upsert(ctx, fixtureID, hostile); !errors.Is(err, skills.ErrInstalledPackageReadOnly) {
		t.Fatalf("Upsert on installed key err=%v, want ErrInstalledPackageReadOnly", err)
	}
	assertInstalledUnitIntact(t, h, ctx, agent, "fence", unit, uri, "after Upsert refusal")

	// 3. The agent-less Delete (legacy surface, agentID="") cannot
	// reach the installed membership row: no unbound user-scope row
	// exists, so the legacy not-found result is unchanged.
	if err := h.Store.Delete(ctx, fixtureID, "fence", skills.ScopeUser); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("agent-less Delete err=%v, want ErrSkillNotFound (installed key has no unbound row)", err)
	}
	assertInstalledUnitIntact(t, h, ctx, agent, "fence", unit, uri, "after agent-less Delete")

	// 4. Keys WITHOUT an installed package keep exact legacy behavior.
	// 4a/4b. A legacy agent-bound row (non-user scope) upserts, and its
	// exact re-upsert stays idempotent.
	legacy := newSkill("fence-legacy")
	legacy.AgentID = agent // ScopeProject by default
	if err := h.Store.Upsert(ctx, fixtureID, legacy); err != nil {
		t.Fatalf("legacy upsert (no package): %v", err)
	}
	if err := h.Store.Upsert(ctx, fixtureID, legacy); err != nil {
		t.Fatalf("idempotent legacy re-upsert (no package): %v", err)
	}
	// 4c. Ordinary DeleteAgent of the legacy row still deletes it.
	if err := h.Store.DeleteAgent(ctx, fixtureID, agent, "fence-legacy", legacy.Scope); err != nil {
		t.Fatalf("legacy DeleteAgent (no package): %v", err)
	}
	if _, err := h.Store.GetScopeAgent(ctx, fixtureID, agent, "fence-legacy", legacy.Scope); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("legacy row survived DeleteAgent: err=%v, want ErrSkillNotFound", err)
	}

	// 5. The dedicated package-aware path remains the ONLY mutation
	// path for the installed unit: exact-receipt erasure removes the
	// whole unit (package + membership row + resolve surface).
	deleted, err := h.Store.DeleteInstalledPackage(ctx, fixtureID, agent, "fence", r)
	if err != nil || !deleted {
		t.Fatalf("DeleteInstalledPackage after fence: deleted=%v err=%v", deleted, err)
	}
	if _, err := h.Store.GetInstalledPackage(ctx, fixtureID, agent, "fence"); !errors.Is(err, skills.ErrInstalledPackageNotFound) {
		t.Fatalf("package survived dedicated erasure: err=%v, want ErrInstalledPackageNotFound", err)
	}
	if _, err := h.Store.GetScopeAgent(ctx, fixtureID, agent, "fence", skills.ScopeUser); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("membership row survived dedicated erasure: err=%v, want ErrSkillNotFound", err)
	}
}

// assertInstalledUnitIntact verifies the atomic unit is byte-identical
// after a refused legacy mutation: GetInstalledPackage returns the
// exact `want` unit (hash, body, manifest, bytes), the legacy scope
// read (GetScopeAgent at the ScopeUser rung) still reflects the same
// membership row, and the support bytes still resolve. A driver that
// tears or silently overwrites the unit fails here.
func assertInstalledUnitIntact(t failureReporter, h Harness, ctx context.Context, agentID, name string, want skills.InstalledPackage, uri skills.PackageURI, msg string) {
	t.Helper()
	got, err := h.Store.GetInstalledPackage(ctx, fixtureID, agentID, name)
	if err != nil {
		t.Fatalf("%s: GetInstalledPackage: %v", msg, err)
	}
	assertUnitEqual(t, got, want, msg)
	assertUnitConsistent(t, got, msg)
	row, err := h.Store.GetScopeAgent(ctx, fixtureID, agentID, name, skills.ScopeUser)
	if err != nil {
		t.Fatalf("%s: GetScopeAgent after refusal: %v", msg, err)
	}
	if row.ContentHash != want.Skill.ContentHash || row.AgentID != agentID || row.Scope != skills.ScopeUser {
		t.Fatalf("%s: legacy scope read diverged from the installed unit: %+v", msg, row)
	}
	if _, err := h.Store.ResolveSupport(ctx, fixtureID, agentID, name, uri); err != nil {
		t.Fatalf("%s: ResolveSupport after refusal: %v", msg, err)
	}
}

// testInstalledClosedSentinels pins the closed/store sentinel: every
// installed-package method fails with ErrStoreClosed after Close.
func testInstalledClosedSentinels(t failureReporter, h Harness) {
	ctx := context.Background()
	const agent = "agent-closed"

	if err := h.Store.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	unit := installedFixtureUnit(t, "closed", agent, skills.OriginGenerated, "1.0.0", 1)
	calls := []struct {
		name string
		err  error
	}{
		{"GetInstalledPackage", func() error {
			_, err := h.Store.GetInstalledPackage(ctx, fixtureID, agent, "closed")
			return err
		}()},
		{"ResolveSupport", func() error {
			_, err := h.Store.ResolveSupport(ctx, fixtureID, agent, "closed", skills.PackageURI{Hash: unit.PackageHash, Path: unit.Package.Supports[0].Path})
			return err
		}()},
		{"PutInstalledPackage", func() error {
			_, err := h.Store.PutInstalledPackage(ctx, fixtureID, agent, unit, skills.InstalledPackageCondition{ExpectedAbsent: true}, false)
			return err
		}()},
		{"DeleteInstalledPackage", func() error {
			_, err := h.Store.DeleteInstalledPackage(ctx, fixtureID, agent, "closed", skills.InstalledPackageReceipt{
				TenantID: fixtureID.TenantID, UserID: fixtureID.UserID, AgentID: agent, Name: "closed", WrittenHash: unit.PackageHash,
			})
			return err
		}()},
		{"RestoreInstalledPackage", func() error {
			_, err := h.Store.RestoreInstalledPackage(ctx, fixtureID, agent, "closed", skills.InstalledPackageReceipt{
				TenantID: fixtureID.TenantID, UserID: fixtureID.UserID, AgentID: agent, Name: "closed",
				WrittenHash: unit.PackageHash, PriorHash: unit.PackageHash,
			}, unit)
			return err
		}()},
	}
	for _, c := range calls {
		if !errors.Is(c.err, skills.ErrStoreClosed) {
			t.Fatalf("%s after Close: err=%v, want ErrStoreClosed", c.name, c.err)
		}
	}
}

// testInstalledConcurrentReuse pins the concurrent-reuse contract
// for the installed-package surface: N>=100 mixed operations on ONE
// shared store under `-race` — per-identity isolation (no context
// bleed), per-goroutine cancellation (no cross-talk), deterministic
// create/replace/get/resolve/delete/restore scripts (exact-receipt
// compensation under concurrency), a shared-key conditional CAS storm
// (one winner, never torn), and a final goroutine-count baseline.
func testInstalledConcurrentReuse(t failureReporter, h Harness) {
	const (
		scriptGoroutines = 100
		stormWriters     = 28
		stormReaders     = 28
	)

	baseline := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		wg          sync.WaitGroup
		opCount     atomic.Int64
		errCount    atomic.Int64
		bleedN      atomic.Int64
		stormN      atomic.Int64
		stormErrN   atomic.Int64
		failDetail  atomic.Value // first failure message, for the final report
		stormDetail atomic.Value // first storm consistency failure, for the final report
	)
	record := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		if failDetail.Load() == nil {
			failDetail.Store(msg)
		}
		errCount.Add(1)
	}
	recordStorm := func(format string, args ...any) {
		if stormDetail.Load() == nil {
			stormDetail.Store(fmt.Sprintf(format, args...))
		}
		stormErrN.Add(1)
	}

	// Deterministic per-identity scripts. Each goroutine owns a
	// distinct (tenant, user, agent, name) and must never observe another
	// goroutine's state. The script exercises the full lifecycle
	// including exact-receipt compensation and the never-another-winner
	// property. Failures are recorded (never t.Fatalf from a worker
	// goroutine) and asserted by the test goroutine after join.
	runScript := func(gid int, canceled bool) {
		myID := identity.Quadruple{
			Identity: identity.Identity{
				TenantID:  fmt.Sprintf("t-conc-%d", gid),
				UserID:    fmt.Sprintf("u-conc-%d", gid),
				SessionID: fmt.Sprintf("s-conc-%d", gid),
			},
			RunID: fmt.Sprintf("r-conc-%d", gid),
		}
		myAgent := fmt.Sprintf("agent-conc-%d", gid)
		myName := fmt.Sprintf("conc-%d", gid)
		localCtx, localCancel := context.WithCancel(ctx)
		defer localCancel()

		v1 := installedFixtureUnit(t, myName, myAgent, skills.OriginGenerated, "1.0.0", 1)
		v2 := installedFixtureUnit(t, myName, myAgent, skills.OriginGenerated, "2.0.0", 1)

		if canceled {
			// Cancellation cross-talk cohort: the context is canceled
			// BEFORE the first operation, so the operation must fail
			// with context.Canceled and never touch the store.
			localCancel()
			_, err := h.Store.PutInstalledPackage(localCtx, myID, myAgent, v1,
				skills.InstalledPackageCondition{ExpectedAbsent: true}, false)
			if !errors.Is(err, context.Canceled) {
				record("g%d canceled create err=%v, want context.Canceled", gid, err)
			}
			opCount.Add(1)
			return
		}
		// The live-cohort context stays live for the whole script; the
		// canceled cohort's cancellations must never cross-talk into
		// siblings (each goroutine owns its localCtx derived from the
		// shared parent).

		fail := func(format string, args ...any) bool {
			record("g%d: "+format, append([]any{gid}, args...)...)
			return false
		}

		op := func() bool {
			// 1. Create.
			r1, err := h.Store.PutInstalledPackage(localCtx, myID, myAgent, v1, skills.InstalledPackageCondition{ExpectedAbsent: true}, false)
			if err != nil {
				return fail("create: %v", err)
			}
			opCount.Add(1)
			// 2. Read back exactly what was written.
			got, err := h.Store.GetInstalledPackage(localCtx, myID, myAgent, myName)
			if err != nil {
				return fail("get after create: %v", err)
			}
			if got.PackageHash != v1.PackageHash {
				bleedN.Add(1)
				return fail("context bleed after create: hash=%q", got.PackageHash)
			}
			opCount.Add(1)
			// 3. Resolve the support bytes.
			uri, uerr := skills.NewPackageURI(v1.PackageHash, v1.Package.Supports[0].Path)
			if uerr != nil {
				return fail("NewPackageURI: %v", uerr)
			}
			f, err := h.Store.ResolveSupport(localCtx, myID, myAgent, myName, uri)
			if err != nil {
				return fail("resolve: %v", err)
			}
			if !bytes.Equal(f.Data, v1.Package.Supports[0].Data) {
				bleedN.Add(1)
				return fail("resolve bytes mismatch")
			}
			opCount.Add(1)
			// 4. Explicit replace.
			r2, err := h.Store.PutInstalledPackage(localCtx, myID, myAgent, v2,
				skills.InstalledPackageCondition{ExpectedHash: v1.PackageHash, ExpectedVersion: v1.Package.Version}, true)
			if err != nil {
				return fail("replace: %v", err)
			}
			opCount.Add(1)
			// 5. The create receipt must NOT delete the replace winner.
			deleted, err := h.Store.DeleteInstalledPackage(localCtx, myID, myAgent, myName, r1)
			if err != nil {
				return fail("stale delete: %v", err)
			}
			if deleted {
				return fail("stale receipt deleted another winner")
			}
			opCount.Add(1)
			// 6. Exact compensation: restore the prior over our own write.
			restored, err := h.Store.RestoreInstalledPackage(localCtx, myID, myAgent, myName, r2, v1)
			if err != nil {
				return fail("restore: %v", err)
			}
			if !restored {
				return fail("current receipt did not restore its prior")
			}
			opCount.Add(1)
			// 7. The replace receipt is now stale — deleting it is a no-op.
			deleted, err = h.Store.DeleteInstalledPackage(localCtx, myID, myAgent, myName, r2)
			if err != nil {
				return fail("post-restore stale delete: %v", err)
			}
			if deleted {
				return fail("stale replace receipt deleted the restored winner")
			}
			opCount.Add(1)
			// 8. Erase exactly the version the create receipt wrote.
			deleted, err = h.Store.DeleteInstalledPackage(localCtx, myID, myAgent, myName, r1)
			if err != nil {
				return fail("final delete: %v", err)
			}
			if !deleted {
				return fail("create receipt did not delete its own version")
			}
			opCount.Add(1)
			// 9. Erased → not found; stale receipt restore is a no-op.
			if _, err := h.Store.GetInstalledPackage(localCtx, myID, myAgent, myName); !errors.Is(err, skills.ErrInstalledPackageNotFound) {
				return fail("post-erasure get err=%v, want ErrInstalledPackageNotFound", err)
			}
			restored, err = h.Store.RestoreInstalledPackage(localCtx, myID, myAgent, myName, r2, v1)
			if err != nil {
				return fail("post-erasure stale restore: %v", err)
			}
			if restored {
				return fail("stale receipt restored over an erased key")
			}
			opCount.Add(1)
			return true
		}
		op()
	}

	for g := 0; g < scriptGoroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			runScript(gid, false)
		}(g)
	}
	// Cancellation cross-talk: 8 goroutines run a script whose context
	// is canceled before the first operation; each must fail with
	// context.Canceled and never touch the store.
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			runScript(scriptGoroutines+gid, true)
		}(g)
	}

	// Shared-key conditional CAS storm plus concurrent consistent
	// readers. Writers alternate two package versions at ONE
	// key, always conditioning on the hash they just read; a lost CAS
	// race (ErrInstalledPackageConditionFailed) is a normal concurrent
	// outcome and is retried. Readers must never observe a torn or
	// inconsistent unit.
	stormID := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t-storm", UserID: "u-storm", SessionID: "s-storm"},
		RunID:    "r-storm",
	}
	const stormAgent = "agent-storm"
	const stormName = "storm"
	stormA := installedFixtureUnit(t, stormName, stormAgent, skills.OriginGenerated, "1.0.0", 1)
	stormB := installedFixtureUnit(t, stormName, stormAgent, skills.OriginGenerated, "2.0.0", 1)
	stormCtx, stormCancel := context.WithCancel(ctx)
	defer stormCancel()

	for w := 0; w < stormWriters; w++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			target := stormA
			if seed%2 == 1 {
				target = stormB
			}
			for i := 0; i < 8; i++ {
				cur, err := h.Store.GetInstalledPackage(stormCtx, stormID, stormAgent, stormName)
				if err != nil {
					if errors.Is(err, skills.ErrInstalledPackageNotFound) {
						if _, perr := h.Store.PutInstalledPackage(stormCtx, stormID, stormAgent, target,
							skills.InstalledPackageCondition{ExpectedAbsent: true}, false); perr != nil && !errors.Is(perr, skills.ErrInstalledPackageExists) {
							recordStorm("writer create: %v", perr)
							return
						}
						stormN.Add(1)
						continue
					}
					recordStorm("writer read: %v", err)
					return
				}
				_, perr := h.Store.PutInstalledPackage(stormCtx, stormID, stormAgent, target,
					skills.InstalledPackageCondition{ExpectedHash: cur.PackageHash}, true)
				switch {
				case perr == nil:
					stormN.Add(1)
				case errors.Is(perr, skills.ErrInstalledPackageConditionFailed):
					// Lost CAS race — normal; retry.
				default:
					recordStorm("writer replace: %v", perr)
					return
				}
			}
		}(w)
	}
	for r := 0; r < stormReaders; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 8; i++ {
				got, err := h.Store.GetInstalledPackage(stormCtx, stormID, stormAgent, stormName)
				if err != nil {
					if errors.Is(err, skills.ErrInstalledPackageNotFound) {
						continue
					}
					recordStorm("reader read: %v", err)
					return
				}
				// The winner is always exactly one of the written
				// versions — never torn, never partial.
				if got.PackageHash != stormA.PackageHash && got.PackageHash != stormB.PackageHash {
					recordStorm("reader hash %q is neither written version", got.PackageHash)
					return
				}
				if err := skills.VerifyPackageHash(got.Package, got.PackageHash); err != nil {
					recordStorm("reader hash-truth: %v", err)
					return
				}
				for _, f := range got.Package.Supports {
					uri, uerr := skills.NewPackageURI(got.PackageHash, f.Path)
					if uerr != nil {
						recordStorm("reader NewPackageURI: %v", uerr)
						return
					}
					resolved, rerr := h.Store.ResolveSupport(stormCtx, stormID, stormAgent, stormName, uri)
					if rerr != nil {
						if errors.Is(rerr, skills.ErrSupportNotFound) {
							// The winner changed between the Get and the
							// Resolve: the now-stale URI was refused
							// (foreign hash) instead of serving wrong
							// bytes — the exact contract. Not a
							// violation.
							continue
						}
						recordStorm("reader resolve %q: %v", f.Path, rerr)
						return
					}
					// A successful resolve is against the same snapshot
					// (a changed winner would have refused the stale
					// hash), so the entry must match this unit's
					// manifest exactly.
					if resolved.Digest != f.Digest || resolved.Size != f.Size || resolved.Mime != f.Mime ||
						!bytes.Equal(resolved.Data, f.Data) {
						recordStorm("reader resolve %q metadata/bytes mismatch (digest %q vs %q)", f.Path, resolved.Digest, f.Digest)
						return
					}
				}
			}
		}()
	}

	wg.Wait()

	if errCount.Load() != 0 {
		detail := ""
		if d := failDetail.Load(); d != nil {
			detail = fmt.Sprintf(" first: %v", d)
		}
		t.Fatalf("deterministic scripts: %d unexpected failures%s", errCount.Load(), detail)
	}
	if bleedN.Load() != 0 {
		t.Fatalf("context bleed detected: %d", bleedN.Load())
	}
	if stormErrN.Load() != 0 {
		detail := ""
		if d := stormDetail.Load(); d != nil {
			detail = fmt.Sprintf(" first: %v", d)
		}
		t.Fatalf("shared-key storm: %d consistency failures%s", stormErrN.Load(), detail)
	}
	if stormN.Load() == 0 {
		t.Fatalf("shared-key storm wrote no packages (test did not run)")
	}
	if err := h.Store.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // allow driver helper goroutines to drain
	if delta := runtime.NumGoroutine() - baseline; delta > 8 {
		t.Fatalf("goroutine leak: baseline=%d final=%d delta=%d", baseline, baseline+delta, delta)
	}
}
