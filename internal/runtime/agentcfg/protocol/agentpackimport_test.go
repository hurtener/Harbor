package protocol_test

// agentpackimport_test.go — the shared Agent-pack destination of the
// two-phase skill-package import. Parser limits, token tamper/expiry and
// artifact-ownership refusals are covered once by the shared core in
// userskillimport_test.go; these tests pin what the pack destination adds:
// zero-write validate, installation readable by every Agent user, replay,
// witness conflict, explicit replace + package cleanup, and cross-agent /
// cross-tenant / cross-destination refusal.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	artifactsinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/importer"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

const packSkillMD = "---\nname: shared-guide\ntrigger: when the guide is needed\nrequired_tools: [tool-a, tool-x]\n---\nRead the [guide](docs/guide.md).\n\n## Steps\n- follow the guide\n"

func packEntries(guide string) map[string]string {
	return map[string]string{"SKILL.md": packSkillMD, "docs/guide.md": guide}
}

type packImportFixture struct {
	*importFixture
	admin  *agentcfgprotocol.Service
	ledger state.StateStore
}

// newPackImportFixture wires the shared import core into the admin Service
// exactly as the mux does (WithPackageImport + the registry's own state store
// as the pack ledger, so the publication fence is atomic with the revision).
func newPackImportFixture(t *testing.T) *packImportFixture {
	t.Helper()
	return newPackImportFixtureWrapped(t, nil)
}

func newPackImportFixtureWrapped(t *testing.T, wrap func(agentcfg.Registry) agentcfg.Registry) *packImportFixture {
	t.Helper()
	artifactStore, err := artifactsinmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatalf("artifacts inmem: %v", err)
	}
	imp, err := importer.New(importer.Deps{Store: artifactStore})
	if err != nil {
		t.Fatalf("importer.New: %v", err)
	}
	ledger, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("ledger inmem: %v", err)
	}
	reg, regState := newRegistryWithState(t)
	rr := reg.(agentcfg.RetirementRegistry)
	fx := &importFixture{
		imp: imp, artifacts: artifactStore,
		proposals: &importStateStoreSpy{StateStore: ledger},
		store:     &importSkillStoreSpy{SkillStore: newSkills(t)},
		reg:       rr, cap: &fakeCapability{policy: defaultImportCapability()},
		clock:  &fakeClock{t: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)},
		sealer: importTestSealer(t),
	}
	fx.svc, err = agentcfgprotocol.NewUserSkillImportService(imp, artifactStore, fx.sealer, fx.proposals, fx.store, rr, fx.cap,
		agentcfgprotocol.WithImportSessionReach(auth.NewSessionReachAuthorizer()),
		agentcfgprotocol.WithImportAgentReach(auth.NewAgentReachAuthorizer()),
		agentcfgprotocol.WithImportClock(fx.clock.Now),
	)
	if err != nil {
		t.Fatalf("NewUserSkillImportService: %v", err)
	}
	adminReg := reg
	if wrap != nil {
		adminReg = wrap(reg)
	}
	admin, err := agentcfgprotocol.NewService(adminReg,
		agentcfgprotocol.WithSkillStore(fx.store),
		agentcfgprotocol.WithAgentPackProposalState(regState),
		agentcfgprotocol.WithPackageImport(fx.svc),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(func() {
		_ = imp.Close(context.Background())
		_ = artifactStore.Close(context.Background())
		_ = ledger.Close(context.Background())
		_ = fx.store.Close(context.Background())
	})
	return &packImportFixture{importFixture: fx, admin: admin, ledger: regState}
}

func wireScope(id identity.Identity) prototypes.IdentityScope {
	return prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
}

func (fx *packImportFixture) validate(t *testing.T, id identity.Identity, entries map[string]string) prototypes.AgentConfigAgentPacksImportValidateResponse {
	t.Helper()
	agentID := testAgentID
	artifactID := fx.uploadPackage(t, id, entries)
	resp, err := fx.admin.AgentPacksImportValidate(importCtx(id, agentID), prototypes.AgentConfigAgentPacksImportValidateRequest{
		Identity: wireScope(id), AgentID: agentID, ArtifactID: artifactID,
	})
	if err != nil {
		t.Fatalf("import_validate: %v", err)
	}
	return resp
}

func packCommitReq(id identity.Identity, agentID string, v prototypes.AgentConfigAgentPacksImportValidateResponse, replace bool) prototypes.AgentConfigAgentPacksImportCommitRequest {
	return prototypes.AgentConfigAgentPacksImportCommitRequest{
		Identity: wireScope(id), AgentID: agentID, ProposalToken: v.ProposalToken, Name: v.Review.Name,
		ReviewedPackageHash: v.PackageHash, ExpectedContentHash: v.ExpectedContentHash, Replace: replace,
	}
}

func (fx *packImportFixture) installed(t *testing.T) (skills.InstalledPackage, bool) {
	t.Helper()
	const name = "shared-guide"
	agentID := testAgentID
	q, err := agentcfg.AgentScope("t", agentID)
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := fx.store.GetInstalledPackage(context.Background(), q, agentID, name)
	if errors.Is(err, skills.ErrInstalledPackageNotFound) {
		return skills.InstalledPackage{}, false
	}
	if err != nil {
		t.Fatalf("GetInstalledPackage: %v", err)
	}
	return pkg, true
}

func guideURI(t *testing.T, hash string) skills.PackageURI {
	t.Helper()
	uri, err := skills.NewPackageURI(hash, "docs/guide.md")
	if err != nil {
		t.Fatalf("NewPackageURI: %v", err)
	}
	return uri
}

func TestAgentPackImport_ZeroWriteValidateThenSharedInstall(t *testing.T) {
	fx := newPackImportFixture(t)
	v := fx.validate(t, importUserA, packEntries("guide v1"))

	// Zero-write: no package/skill mutation, no revision, no ledger slot.
	fx.assertZeroMutations(t)
	if _, set, _ := fx.reg.Active(context.Background(), importQuad(importUserA), testAgentID, agentcfg.ConfigScopeAgent); set {
		t.Fatalf("validate published a revision")
	}
	if v.ExpectedContentHash != agentcfg.ExpectNoActiveRevision {
		t.Fatalf("witness=%q, want %q for an Agent without a revision", v.ExpectedContentHash, agentcfg.ExpectNoActiveRevision)
	}
	// Same review shape as the personal import; required tools are
	// reported, never granted.
	if v.Review.Name != "shared-guide" || len(v.Review.SupportFiles) != 1 || v.Review.SupportFiles[0].Path != "docs/guide.md" {
		t.Fatalf("review=%+v", v.Review)
	}
	if len(v.Warnings) != 1 {
		t.Fatalf("warnings=%v, want exactly the non-visible tool-x", v.Warnings)
	}

	resp, err := fx.admin.AgentPacksImportCommit(importCtx(importUserA, testAgentID), packCommitReq(importUserA, testAgentID, v, false))
	if err != nil {
		t.Fatalf("import_commit: %v", err)
	}
	if resp.Replayed || resp.PackageHash != v.PackageHash || resp.Skill.Name != "shared-guide" {
		t.Fatalf("commit resp=%+v", resp)
	}
	list, err := fx.admin.AgentPacksList(context.Background(), prototypes.AgentConfigAgentPacksListRequest{Identity: wireScope(importUserA), AgentID: testAgentID})
	if err != nil || len(list.Items) != 1 || list.Items[0].PackageHash != v.PackageHash {
		t.Fatalf("pack list=%+v err=%v", list.Items, err)
	}
	if got := list.Items[0].RequiredTools; len(got) != 2 {
		t.Fatalf("required tools must stay metadata: %v", got)
	}

	// Another user of the same Agent (same tenant, own session, reach) reads
	// the runtime-owned support file; the importer's identity is irrelevant.
	other := identity.Identity{TenantID: "t", UserID: "u2", SessionID: "s2"}
	f, err := fx.svc.ResolveAgentPackSupport(importCtx(other, testAgentID), testAgentID, "shared-guide", guideURI(t, v.PackageHash))
	if err != nil || string(f.Data) != "guide v1" {
		t.Fatalf("other user resolve: %q err=%v", f.Data, err)
	}
	// No reach to the Agent: refused before any read.
	noReach, _ := identity.WithVerified(context.Background(), other)
	noReach = auth.WithAgentReach(noReach, []string{"agent-other"})
	if _, err := fx.svc.ResolveAgentPackSupport(noReach, testAgentID, "shared-guide", guideURI(t, v.PackageHash)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportAgentReachDenied) {
		t.Fatalf("no-reach resolve err=%v", err)
	}
	// Another tenant with the same Agent id sees nothing.
	foreign := identity.Identity{TenantID: "t2", UserID: "u", SessionID: "s"}
	if _, err := fx.svc.ResolveAgentPackSupport(importCtx(foreign, testAgentID), testAgentID, "shared-guide", guideURI(t, v.PackageHash)); !errors.Is(err, skills.ErrSupportNotFound) {
		t.Fatalf("cross-tenant resolve err=%v", err)
	}
}

func TestAgentPackImport_ReplayIsIdempotent(t *testing.T) {
	fx := newPackImportFixture(t)
	v := fx.validate(t, importUserA, packEntries("guide v1"))
	ctx := importCtx(importUserA, testAgentID)
	first, err := fx.admin.AgentPacksImportCommit(ctx, packCommitReq(importUserA, testAgentID, v, false))
	if err != nil {
		t.Fatalf("first commit: %v", err)
	}
	second, err := fx.admin.AgentPacksImportCommit(ctx, packCommitReq(importUserA, testAgentID, v, false))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.Replayed || second.Revision.RevisionID != first.Revision.RevisionID {
		t.Fatalf("replay=%+v first=%+v", second, first)
	}
	if put, _, _, _, _, _ := fx.store.counts(); put != 1 {
		t.Fatalf("replay wrote the package again: puts=%d", put)
	}
}

func TestAgentPackImport_WitnessMovedIsConflict(t *testing.T) {
	fx := newPackImportFixture(t)
	v := fx.validate(t, importUserA, packEntries("guide v1"))
	if _, err := fx.admin.AgentPacksUpsert(context.Background(), prototypes.AgentConfigAgentPacksUpsertRequest{
		Identity: wireScope(importUserA), AgentID: testAgentID,
		Skill: prototypes.AgentConfigAgentPackItem{Name: "other", Trigger: "t", Steps: []string{"s"}},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	_, err := fx.admin.AgentPacksImportCommit(importCtx(importUserA, testAgentID), packCommitReq(importUserA, testAgentID, v, false))
	if !errors.Is(err, agentcfg.ErrRevisionConflict) {
		t.Fatalf("moved witness err=%v, want ErrRevisionConflict", err)
	}
	if _, ok := fx.installed(t); ok {
		t.Fatalf("a refused commit installed the package")
	}
}

func TestAgentPackImport_ExplicitReplaceAndCleanup(t *testing.T) {
	fx := newPackImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	v1 := fx.validate(t, importUserA, packEntries("guide v1"))
	if _, err := fx.admin.AgentPacksImportCommit(ctx, packCommitReq(importUserA, testAgentID, v1, false)); err != nil {
		t.Fatalf("commit v1: %v", err)
	}

	v2 := fx.validate(t, importUserA, packEntries("guide v2"))
	if _, err := fx.admin.AgentPacksImportCommit(ctx, packCommitReq(importUserA, testAgentID, v2, false)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportReplaceRequired) {
		t.Fatalf("same-name import without consent err=%v", err)
	}
	if pkg, _ := fx.installed(t); pkg.PackageHash != v1.PackageHash {
		t.Fatalf("refused replace changed the package")
	}
	// The refused attempt released its ledger slot, so the same token
	// commits once consent is given.
	if _, err := fx.admin.AgentPacksImportCommit(ctx, packCommitReq(importUserA, testAgentID, v2, true)); err != nil {
		t.Fatalf("replace commit: %v", err)
	}
	if pkg, _ := fx.installed(t); pkg.PackageHash != v2.PackageHash {
		t.Fatalf("installed=%s, want v2", pkg.PackageHash)
	}
	if _, err := fx.svc.ResolveAgentPackSupport(ctx, testAgentID, "shared-guide", guideURI(t, v1.PackageHash)); !errors.Is(err, skills.ErrSupportNotFound) {
		t.Fatalf("replaced v1 support still resolves: err=%v", err)
	}

	// Removing the item through the ordinary pack verb releases its package.
	if _, err := fx.admin.AgentPacksRemove(context.Background(), prototypes.AgentConfigAgentPacksRemoveRequest{
		Identity: wireScope(importUserA), AgentID: testAgentID, Name: "shared-guide",
	}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok := fx.installed(t); ok {
		t.Fatalf("removed item left its package behind")
	}
}

func TestAgentPackImport_BodyOnlyReplaceReleasesPackage(t *testing.T) {
	fx := newPackImportFixture(t)
	v := fx.validate(t, importUserA, packEntries("guide v1"))
	if _, err := fx.admin.AgentPacksImportCommit(importCtx(importUserA, testAgentID), packCommitReq(importUserA, testAgentID, v, false)); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// A caller cannot author the package reference.
	smuggled := prototypes.AgentConfigAgentPackItem{Name: "shared-guide", Trigger: "t", Steps: []string{"s"}, PackageHash: v.PackageHash}
	if _, err := fx.admin.AgentPacksUpsert(context.Background(), prototypes.AgentConfigAgentPacksUpsertRequest{
		Identity: wireScope(importUserA), AgentID: testAgentID, Skill: smuggled,
	}); !errors.Is(err, agentcfgprotocol.ErrAgentPacksInvalid) {
		t.Fatalf("caller-supplied package_hash err=%v", err)
	}
	smuggled.PackageHash = ""
	if _, err := fx.admin.AgentPacksUpsert(context.Background(), prototypes.AgentConfigAgentPacksUpsertRequest{
		Identity: wireScope(importUserA), AgentID: testAgentID, Skill: smuggled,
	}); err != nil {
		t.Fatalf("body-only upsert: %v", err)
	}
	if _, ok := fx.installed(t); ok {
		t.Fatalf("body-only replacement left the package behind")
	}
}

func TestAgentPackImport_TokenBindingRefusals(t *testing.T) {
	fx := newPackImportFixture(t)
	v := fx.validate(t, importUserA, packEntries("guide v1"))

	// Cross-agent: the token is bound to agent-x even with reach to both.
	both, _ := identity.WithVerified(context.Background(), importUserA)
	both = auth.WithSessionReach(both, []string{importUserA.SessionID})
	both = auth.WithAgentReach(both, []string{testAgentID, "agent-y"})
	if _, err := fx.admin.AgentPacksImportCommit(both, packCommitReq(importUserA, "agent-y", v, false)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportProposalInvalid) {
		t.Fatalf("cross-agent commit err=%v", err)
	}
	// Cross-tenant: another tenant's operator cannot replay the token.
	foreign := identity.Identity{TenantID: "t2", UserID: "u", SessionID: "s"}
	if _, err := fx.admin.AgentPacksImportCommit(importCtx(foreign, testAgentID), packCommitReq(foreign, testAgentID, v, false)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportProposalInvalid) {
		t.Fatalf("cross-tenant commit err=%v", err)
	}
	// Cross-destination: a shared-pack token never lands as a personal
	// skill, and a personal token never lands in the shared pack.
	if _, err := fx.svc.Commit(importCtx(importUserA, testAgentID), agentcfgprotocol.UserSkillImportCommitRequest{
		ProposalToken: v.ProposalToken, AgentID: testAgentID, Name: v.Review.Name,
		ReviewedPackageHash: v.PackageHash, ExpectedContentHash: v.ExpectedContentHash,
	}); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportProposalInvalid) {
		t.Fatalf("pack token into personal commit err=%v", err)
	}
	personalArtifact := fx.uploadPackage(t, importUserA, packEntries("guide v1"))
	personal, err := fx.svc.Validate(importCtx(importUserA, testAgentID), fx.validateReq(personalArtifact))
	if err != nil {
		t.Fatalf("personal validate: %v", err)
	}
	if _, err := fx.admin.AgentPacksImportCommit(importCtx(importUserA, testAgentID), prototypes.AgentConfigAgentPacksImportCommitRequest{
		Identity: wireScope(importUserA), AgentID: testAgentID, ProposalToken: personal.ProposalToken, Name: personal.Review.Name,
		ReviewedPackageHash: personal.PackageHash, ExpectedContentHash: personal.ExpectedContentHash,
	}); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportProposalInvalid) {
		t.Fatalf("personal token into pack commit err=%v", err)
	}
	if put, _, _, _, _, _ := fx.store.counts(); put != 0 {
		t.Fatalf("a refused commit wrote a package: puts=%d", put)
	}
}

// failingSetRevision makes the next revision write report failure once armed;
// with landed set the write is applied first (an error after a durable write).
type failingSetRevision struct {
	agentcfg.Registry
	armed, landed bool
}

func (f *failingSetRevision) SetRevision(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope, payload agentcfg.ConfigPayload, opts agentcfg.SetOptions) (agentcfg.Revision, error) {
	if f.armed {
		f.armed = false
		if f.landed {
			if _, err := f.Registry.SetRevision(ctx, id, agentID, scope, payload, opts); err != nil {
				return agentcfg.Revision{}, err
			}
		}
		return agentcfg.Revision{}, errors.New("simulated revision write failure")
	}
	return f.Registry.SetRevision(ctx, id, agentID, scope, payload, opts)
}

func TestAgentPackImport_FailedPublicationTouchesNoPackage(t *testing.T) {
	failing := &failingSetRevision{}
	fx := newPackImportFixtureWrapped(t, func(r agentcfg.Registry) agentcfg.Registry { failing.Registry = r; return failing })
	ctx := importCtx(importUserA, testAgentID)

	v1 := fx.validate(t, importUserA, packEntries("guide v1"))
	failing.armed = true
	if _, err := fx.admin.AgentPacksImportCommit(ctx, packCommitReq(importUserA, testAgentID, v1, false)); err == nil {
		t.Fatalf("commit must surface the publication failure")
	}
	if _, ok := fx.installed(t); ok {
		t.Fatalf("an unpublished import installed its package")
	}
	if _, err := fx.admin.AgentPacksImportCommit(ctx, packCommitReq(importUserA, testAgentID, v1, false)); err != nil {
		t.Fatalf("retry commit: %v", err)
	}

	// A failed replacement leaves the previous package untouched.
	v2 := fx.validate(t, importUserA, packEntries("guide v2"))
	failing.armed = true
	if _, err := fx.admin.AgentPacksImportCommit(ctx, packCommitReq(importUserA, testAgentID, v2, true)); err == nil {
		t.Fatalf("replace commit must surface the publication failure")
	}
	f, err := fx.svc.ResolveAgentPackSupport(ctx, testAgentID, "shared-guide", guideURI(t, v1.PackageHash))
	if err != nil || string(f.Data) != "guide v1" {
		t.Fatalf("v1 support after failed replace: %q err=%v", f.Data, err)
	}
}

func TestAgentPackImport_LandedPublicationReportedAsFailureCompletes(t *testing.T) {
	failing := &failingSetRevision{landed: true}
	fx := newPackImportFixtureWrapped(t, func(r agentcfg.Registry) agentcfg.Registry { failing.Registry = r; return failing })
	ctx := importCtx(importUserA, testAgentID)
	v := fx.validate(t, importUserA, packEntries("guide v1"))
	failing.armed = true
	resp, err := fx.admin.AgentPacksImportCommit(ctx, packCommitReq(importUserA, testAgentID, v, false))
	if err != nil {
		t.Fatalf("a publication that landed must complete, got %v", err)
	}
	if pkg, ok := fx.installed(t); !ok || pkg.PackageHash != v.PackageHash || resp.PackageHash != v.PackageHash {
		t.Fatalf("installed=%v %s", ok, pkg.PackageHash)
	}
}

func TestAgentPackImport_DanglingReferenceFailsClosedAndRepairs(t *testing.T) {
	fx := newPackImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	v := fx.validate(t, importUserA, packEntries("guide v1"))
	fx.store.setFailPutInstalledAt(1)
	if _, err := fx.admin.AgentPacksImportCommit(ctx, packCommitReq(importUserA, testAgentID, v, false)); err == nil {
		t.Fatalf("a failed package write must surface")
	}
	// Published but not installed: resolution fails closed.
	if _, err := fx.svc.ResolveAgentPackSupport(ctx, testAgentID, "shared-guide", guideURI(t, v.PackageHash)); err == nil {
		t.Fatalf("a dangling reference resolved")
	}
	// The same token repairs it without publishing again.
	resp, err := fx.admin.AgentPacksImportCommit(ctx, packCommitReq(importUserA, testAgentID, v, false))
	if err != nil || !resp.Replayed {
		t.Fatalf("repair: %+v err=%v", resp, err)
	}
	f, err := fx.svc.ResolveAgentPackSupport(ctx, testAgentID, "shared-guide", guideURI(t, v.PackageHash))
	if err != nil || string(f.Data) != "guide v1" {
		t.Fatalf("after repair: %q err=%v", f.Data, err)
	}
	// A finished commit leaves no ledger slot behind.
	sum := sha256.Sum256([]byte(v.ProposalToken))
	if _, err := fx.ledger.Load(context.Background(), importQuad(importUserA), "agentcfg.agent_pack.import."+hex.EncodeToString(sum[:])); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("ledger slot survived the commit")
	}
}

func TestAgentPackImport_RollbackReleasesUnreferencedPackage(t *testing.T) {
	fx := newPackImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	base, err := fx.admin.AgentPacksUpsert(context.Background(), prototypes.AgentConfigAgentPacksUpsertRequest{
		Identity: wireScope(importUserA), AgentID: testAgentID,
		Skill: prototypes.AgentConfigAgentPackItem{Name: "other", Trigger: "t", Steps: []string{"s"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	v := fx.validate(t, importUserA, packEntries("guide v1"))
	if _, err := fx.admin.AgentPacksImportCommit(ctx, packCommitReq(importUserA, testAgentID, v, false)); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.admin.Rollback(context.Background(), prototypes.AgentConfigRollbackRequest{
		Identity: wireScope(importUserA), AgentID: testAgentID, RevisionID: base.Revision.RevisionID,
	}); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, ok := fx.installed(t); ok {
		t.Fatalf("rollback left an unreferenced package")
	}
}

func TestAgentPackImport_ReservedUserRefused(t *testing.T) {
	fx := newPackImportFixture(t)
	v := fx.validate(t, importUserA, packEntries("guide v1"))
	if _, err := fx.admin.AgentPacksImportCommit(importCtx(importUserA, testAgentID), packCommitReq(importUserA, testAgentID, v, false)); err != nil {
		t.Fatal(err)
	}
	// A caller occupying the control-plane user slot cannot reach the
	// Agent's packages through the user rung.
	reserved := identity.Identity{TenantID: "t", UserID: agentcfg.ReservedAgentConfigUser, SessionID: testAgentID}
	if _, err := fx.svc.ResolveAgentPackSupport(importCtx(reserved, testAgentID), testAgentID, "shared-guide", guideURI(t, v.PackageHash)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportIdentityRequired) {
		t.Fatalf("reserved-user resolve err=%v", err)
	}
	if _, err := fx.admin.UserSkillsList(context.Background(), prototypes.AgentConfigUserSkillsListRequest{Identity: wireScope(reserved), AgentID: testAgentID}); !errors.Is(err, agentcfg.ErrReservedUser) {
		t.Fatalf("reserved-user personal skills list err=%v", err)
	}
}

func TestAgentPackImport_ConcurrentSameTokenConverges(t *testing.T) {
	fx := newPackImportFixture(t)
	// A second Service over the same stores stands in for another replica:
	// the process-local agent lock does not serialize them.
	replica, err := agentcfgprotocol.NewService(fx.reg,
		agentcfgprotocol.WithSkillStore(fx.store),
		agentcfgprotocol.WithAgentPackProposalState(fx.ledger),
		agentcfgprotocol.WithPackageImport(fx.svc),
	)
	if err != nil {
		t.Fatal(err)
	}
	v := fx.validate(t, importUserA, packEntries("guide v1"))
	req := packCommitReq(importUserA, testAgentID, v, false)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, svc := range []*agentcfgprotocol.Service{fx.admin, replica} {
		wg.Add(1)
		go func(i int, svc *agentcfgprotocol.Service) {
			defer wg.Done()
			_, errs[i] = svc.AgentPacksImportCommit(importCtx(importUserA, testAgentID), req)
		}(i, svc)
	}
	wg.Wait()
	if errs[0] != nil && errs[1] != nil {
		t.Fatalf("no commit succeeded: %v / %v", errs[0], errs[1])
	}
	// Whatever interleaving happened, a retry converges on one installed
	// package referenced by the active item.
	if _, err := fx.admin.AgentPacksImportCommit(importCtx(importUserA, testAgentID), req); err != nil {
		t.Fatalf("converging retry: %v", err)
	}
	f, err := fx.svc.ResolveAgentPackSupport(importCtx(importUserA, testAgentID), testAgentID, "shared-guide", guideURI(t, v.PackageHash))
	if err != nil || string(f.Data) != "guide v1" {
		t.Fatalf("after concurrent commits: %q err=%v", f.Data, err)
	}
}

func TestAgentPackImport_UnwiredFailsLoud(t *testing.T) {
	admin, err := agentcfgprotocol.NewService(newRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	_, err = admin.AgentPacksImportValidate(importCtx(importUserA, testAgentID), prototypes.AgentConfigAgentPacksImportValidateRequest{
		Identity: wireScope(importUserA), AgentID: testAgentID, ArtifactID: "a",
	})
	if !errors.Is(err, agentcfgprotocol.ErrAgentPackImportUnavailable) {
		t.Fatalf("unwired err=%v", err)
	}
}

// A drafter-style single SKILL.md artifact imports into the shared pack
// through the same shared ingest, with the hash of the equivalent zip.
func TestAgentPackImport_SingleMarkdownDraft(t *testing.T) {
	fx := newPackImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	zipped := fx.validate(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	resp, err := fx.admin.AgentPacksImportValidate(ctx, prototypes.AgentConfigAgentPacksImportValidateRequest{
		Identity: wireScope(importUserA), AgentID: testAgentID, ArtifactID: fx.uploadMarkdown(t, importUserA, importSkillMD),
	})
	if err != nil {
		t.Fatalf("validate markdown draft: %v", err)
	}
	if resp.PackageHash != zipped.PackageHash || len(resp.Review.SupportFiles) != 0 {
		t.Fatalf("markdown review = %+v hash %q, zip hash %q", resp.Review, resp.PackageHash, zipped.PackageHash)
	}
	committed, err := fx.admin.AgentPacksImportCommit(ctx, packCommitReq(importUserA, testAgentID, resp, false))
	if err != nil || committed.PackageHash != resp.PackageHash {
		t.Fatalf("commit markdown draft: %+v err=%v", committed, err)
	}
	list, err := fx.admin.AgentPacksList(context.Background(), prototypes.AgentConfigAgentPacksListRequest{Identity: wireScope(importUserA), AgentID: testAgentID})
	if err != nil || len(list.Items) != 1 || list.Items[0].PackageHash != resp.PackageHash {
		t.Fatalf("pack list=%+v err=%v", list.Items, err)
	}
	if _, err := fx.admin.AgentPacksImportValidate(ctx, prototypes.AgentConfigAgentPacksImportValidateRequest{
		Identity: wireScope(importUserA), AgentID: testAgentID, ArtifactID: fx.uploadMarkdown(t, importUserA, "not a skill"),
	}); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportPackageInvalid) {
		t.Fatalf("plain text err=%v, want package invalid", err)
	}
}

// externalDescribedSkillMD is the common external SKILL.md shape: a
// frontmatter `description` and no `trigger`.
const externalDescribedSkillMD = "---\nname: described-skill\ndescription: Explains the demo to a person.\n---\nBody prose.\n\n## Steps\n- explain the demo\n"

// TestSkillImport_DescriptionOnlySkillSurfacesDescription pins the
// description through both import destinations: the shared review, the
// personal commit, the pack commit summary and the pack listing carry it,
// and the trigger is derived from it.
func TestSkillImport_DescriptionOnlySkillSurfacesDescription(t *testing.T) {
	const wantTrigger = "Explains the demo to a person."
	const wantDesc = wantTrigger + "\n\nBody prose."

	// Personal destination.
	personal := newImportFixture(t)
	validated, committed, err := personal.validateCommit(t, importUserA, map[string]string{"SKILL.md": externalDescribedSkillMD}, false)
	if err != nil {
		t.Fatalf("personal validate/commit: %v", err)
	}
	if validated.Review.Description != wantDesc || validated.Review.Trigger != wantTrigger {
		t.Fatalf("personal review = %+v", validated.Review)
	}
	if wire := agentcfgprotocol.UserSkillImportReviewToWire(validated.Review); wire.Description != wantDesc {
		t.Fatalf("wire review description = %q", wire.Description)
	}
	if committed.Skill.Description != wantDesc || committed.Skill.Trigger != wantTrigger {
		t.Fatalf("personal commit skill = %+v", committed.Skill)
	}

	// Shared Agent-pack destination.
	fx := newPackImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	v, err := fx.admin.AgentPacksImportValidate(ctx, prototypes.AgentConfigAgentPacksImportValidateRequest{
		Identity: wireScope(importUserA), AgentID: testAgentID, ArtifactID: fx.uploadMarkdown(t, importUserA, externalDescribedSkillMD),
	})
	if err != nil {
		t.Fatalf("pack validate: %v", err)
	}
	if v.Review.Description != wantDesc || v.Review.Trigger != wantTrigger {
		t.Fatalf("pack review = %+v", v.Review)
	}
	resp, err := fx.admin.AgentPacksImportCommit(ctx, packCommitReq(importUserA, testAgentID, v, false))
	if err != nil || resp.Skill.Description != wantDesc {
		t.Fatalf("pack commit = %+v err=%v", resp, err)
	}
	list, err := fx.admin.AgentPacksList(context.Background(), prototypes.AgentConfigAgentPacksListRequest{Identity: wireScope(importUserA), AgentID: testAgentID})
	if err != nil || len(list.Items) != 1 || list.Items[0].Description != wantDesc || list.Items[0].Trigger != wantTrigger {
		t.Fatalf("pack list = %+v err=%v", list.Items, err)
	}
}

// agentSkillsPackage is a standard Agent Skills package: name +
// description frontmatter with the advisory license / allowed-tools /
// metadata keys, a free-form body, and a bundled reference file.
func agentSkillsPackage() map[string]string {
	return map[string]string{
		"SKILL.md": "---\nname: release-notes\ndescription: Drafts release notes from merged changes.\nlicense: Apache-2.0\nallowed-tools: Read Grep\nmetadata:\n  version: \"1.0\"\n---\n" +
			"# Release notes\n\n## Overview\n\nSee the [style guide](references/style.md).\n\n## Instructions\n\n1. Group the changes.\n2. Write one line per change.\n\n## Examples\n\n```text\n- Export to CSV.\n```\n",
		"references/style.md": "# Style\n\nUse plain words.\n",
	}
}

// TestSkillImport_AgentSkillsPackageBothDestinations installs a standard
// Agent Skills package through the personal and the shared-pack import:
// both accept it, keep the body as the description, derive the trigger,
// carry the support file, and report no required tools.
func TestSkillImport_AgentSkillsPackageBothDestinations(t *testing.T) {
	const wantTrigger = "Drafts release notes from merged changes."
	personal := newImportFixture(t)
	validated, committed, err := personal.validateCommit(t, importUserA, agentSkillsPackage(), false)
	if err != nil {
		t.Fatalf("personal validate/commit: %v", err)
	}
	if validated.Review.Trigger != wantTrigger || validated.Review.StepCount != 1 || len(validated.Review.RequiredTools) != 0 ||
		len(validated.Review.SupportFiles) != 1 || validated.Review.SupportFiles[0].Path != "references/style.md" {
		t.Fatalf("personal review = %+v", validated.Review)
	}
	if !strings.Contains(committed.Skill.Description, "## Instructions\n\n1. Group the changes.") || committed.Skill.Steps[0] != importer.FreeFormStep {
		t.Fatalf("personal skill = %+v", committed.Skill)
	}

	fx := newPackImportFixture(t)
	v := fx.validate(t, importUserA, agentSkillsPackage())
	if v.Review.Trigger != wantTrigger || len(v.Review.SupportFiles) != 1 || len(v.Warnings) != 0 {
		t.Fatalf("pack review = %+v warnings=%v", v.Review, v.Warnings)
	}
	if _, err := fx.admin.AgentPacksImportCommit(importCtx(importUserA, testAgentID), packCommitReq(importUserA, testAgentID, v, false)); err != nil {
		t.Fatalf("pack commit: %v", err)
	}
	list, err := fx.admin.AgentPacksList(context.Background(), prototypes.AgentConfigAgentPacksListRequest{Identity: wireScope(importUserA), AgentID: testAgentID})
	if err != nil || len(list.Items) != 1 || !strings.Contains(list.Items[0].Description, "## Examples") || list.Items[0].PackageHash != v.PackageHash {
		t.Fatalf("pack list = %+v err=%v", list.Items, err)
	}
}
