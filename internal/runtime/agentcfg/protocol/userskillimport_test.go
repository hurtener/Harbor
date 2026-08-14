package protocol_test

// userskillimport_test.go — the verified-caller two-phase complete-skill-package
// import service tests: the ZERO-mutation validate contract, the
// validate→commit round trip that installs ONE atomic package+membership unit
// (never a second legacy membership write), idempotent response-loss replay,
// explicit replacement consent, the full stale/foreign/expired/policy/config/
// ceiling/boot-owned refusal matrix, exact-receipt compensation, cross-session
// and cross-user isolation, focused same-proposal commit races, and the N>=100
// concurrent mixed-identity run under -race.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/artifacts"
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
	"runtime"
)

// ---------------------------------------------------------------------------
// Fixtures and helpers
// ---------------------------------------------------------------------------

// importUserA is the default caller triple used by the round-trip tests.
var importUserA = identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}

// importSkillMD is a complete resource-free SKILL.md package with an
// applicability requirement the default capability snapshot permits.
const importSkillMD = "---\nname: demo-skill\ntrigger: when asked about the demo\nrequired_tools: [tool-a]\n---\nA demo skill body.\n\n## Steps\n- do the thing\n"

// importAnnotatedMD names a required tool OUTSIDE the default permitted set —
// a non-fatal warning, never a grant.
const importAnnotatedMD = "---\nname: annotated-skill\ntrigger: when asked about annotations\nrequired_tools: [tool-a, tool-x]\nrequired_namespaces: [ns-x]\n---\nAn annotated skill body.\n\n## Steps\n- do the thing\n"

// packageZip builds a complete-skill-package archive in memory with a
// deterministic entry order.
func packageZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("Create(%q): %v", name, err)
		}
		if _, err := w.Write([]byte(entries[name])); err != nil {
			t.Fatalf("Write(%q): %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// uploadPackage stores a package archive under the caller's exact triple and
// returns its immutable content-addressed artifact id.
func (fx *importFixture) uploadPackage(t *testing.T, id identity.Identity, entries map[string]string) string {
	t.Helper()
	data := packageZip(t, entries)
	ref, err := fx.artifacts.PutBytes(context.Background(), artifacts.ArtifactScope{
		TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID,
	}, data, artifacts.PutOpts{Namespace: "import"})
	if err != nil {
		t.Fatalf("upload package: %v", err)
	}
	return ref.ID
}

// fakeClock is the controllable time source the service stamps from.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

// fakeCapability is the injectable capability-policy seam. The pointer is
// shared with the fixture so tests can revoke the policy mid-flow.
type fakeCapability struct {
	mu     sync.Mutex
	policy agentcfgprotocol.UserSkillImportPolicy
	err    error
}

func (f *fakeCapability) Policy(ctx context.Context, id identity.Identity, agentID string) (agentcfgprotocol.UserSkillImportPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return agentcfgprotocol.UserSkillImportPolicy{}, f.err
	}
	out := f.policy
	out.PermittedTools = append([]string(nil), f.policy.PermittedTools...)
	out.PermittedNS = append([]string(nil), f.policy.PermittedNS...)
	out.PermittedTags = append([]string(nil), f.policy.PermittedTags...)
	return out, nil
}

func (f *fakeCapability) set(policy agentcfgprotocol.UserSkillImportPolicy) {
	f.mu.Lock()
	f.policy = policy
	f.mu.Unlock()
}

// defaultImportCapability permits tool-a / tool-b for every agent.
func defaultImportCapability() agentcfgprotocol.UserSkillImportPolicy {
	return agentcfgprotocol.UserSkillImportPolicy{
		ID: "harbor.user-skill-import", Version: "1",
		PermittedTools: []string{"tool-a", "tool-b"},
	}
}

// importSkillStoreSpy wraps the real localdb store and counts every MUTATION
// method — the ZERO-write validate contract and the exactly-once commit
// contract are asserted against these counters.
type importSkillStoreSpy struct {
	skills.SkillStore
	mu sync.Mutex
	// mutation counters
	putInstalled     int
	upsert           int
	delete           int
	deleteAgent      int
	deleteInstalled  int
	restoreInstalled int
}

func (s *importSkillStoreSpy) PutInstalledPackage(ctx context.Context, id identity.Quadruple, agentID string, pkg skills.InstalledPackage, cond skills.InstalledPackageCondition, replace bool) (skills.InstalledPackageReceipt, error) {
	s.mu.Lock()
	s.putInstalled++
	s.mu.Unlock()
	return s.SkillStore.PutInstalledPackage(ctx, id, agentID, pkg, cond, replace)
}

func (s *importSkillStoreSpy) Upsert(ctx context.Context, id identity.Quadruple, skill skills.Skill) error {
	s.mu.Lock()
	s.upsert++
	s.mu.Unlock()
	return s.SkillStore.Upsert(ctx, id, skill)
}

func (s *importSkillStoreSpy) Delete(ctx context.Context, id identity.Quadruple, name string, scope skills.Scope) error {
	s.mu.Lock()
	s.delete++
	s.mu.Unlock()
	return s.SkillStore.Delete(ctx, id, name, scope)
}

func (s *importSkillStoreSpy) DeleteAgent(ctx context.Context, id identity.Quadruple, agentID, name string, scope skills.Scope) error {
	s.mu.Lock()
	s.deleteAgent++
	s.mu.Unlock()
	return s.SkillStore.DeleteAgent(ctx, id, agentID, name, scope)
}

func (s *importSkillStoreSpy) DeleteInstalledPackage(ctx context.Context, id identity.Quadruple, agentID, name string, receipt skills.InstalledPackageReceipt) (bool, error) {
	s.mu.Lock()
	s.deleteInstalled++
	s.mu.Unlock()
	return s.SkillStore.DeleteInstalledPackage(ctx, id, agentID, name, receipt)
}

func (s *importSkillStoreSpy) RestoreInstalledPackage(ctx context.Context, id identity.Quadruple, agentID, name string, receipt skills.InstalledPackageReceipt, prior skills.InstalledPackage) (bool, error) {
	s.mu.Lock()
	s.restoreInstalled++
	s.mu.Unlock()
	return s.SkillStore.RestoreInstalledPackage(ctx, id, agentID, name, receipt, prior)
}

func (s *importSkillStoreSpy) counts() (put, upsert, del, delAgent, delInstalled, restore int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putInstalled, s.upsert, s.delete, s.deleteAgent, s.deleteInstalled, s.restoreInstalled
}

// fakeBootOwner is the injected boot-baseline ownership reader (the HA-66
// guard seam): a canonical name it owns is read-only to the control plane.
type fakeBootOwner struct{ names map[string]struct{} }

func (f *fakeBootOwner) OwnsName(tenantID, agentID, name string) bool {
	_, ok := f.names[name]
	return ok
}

var _ agentcfgprotocol.BootOwnership = (*fakeBootOwner)(nil)

// importFixture couples the import service under test with the stores it
// writes through, the admin Service (for seeding/inspecting durable config),
// and the controllable clock + capability.
type importFixture struct {
	svc       *agentcfgprotocol.UserSkillImportService
	admin     *agentcfgprotocol.Service
	imp       importer.Importer
	artifacts artifacts.ArtifactStore
	proposals state.StateStore
	store     *importSkillStoreSpy
	reg       agentcfg.RetirementRegistry
	cap       *fakeCapability
	clock     *fakeClock
}

// newImportFixture builds the production-posture fixture: both signed-reach
// gates wired, real importer + localdb store + statestore registry + in-memory
// artifact/proposal stores, and the default capability snapshot.
func newImportFixture(t *testing.T) *importFixture {
	t.Helper()
	return newImportFixtureOpts(t, func(f *importFixture) {})
}

func newImportFixtureOpts(t *testing.T, mutate func(*importFixture)) *importFixture {
	t.Helper()
	artifactStore, err := artifactsinmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatalf("artifacts inmem: %v", err)
	}
	imp, err := importer.New(importer.Deps{Store: artifactStore})
	if err != nil {
		t.Fatalf("importer.New: %v", err)
	}
	proposals, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("proposals inmem: %v", err)
	}
	reg, _ := newRegistryWithState(t)
	rr, ok := reg.(agentcfg.RetirementRegistry)
	if !ok {
		t.Fatalf("statestore registry does not implement RetirementRegistry")
	}
	cap := &fakeCapability{policy: defaultImportCapability()}
	clock := &fakeClock{t: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)}
	fx := &importFixture{
		imp: imp, artifacts: artifactStore, proposals: proposals,
		store: &importSkillStoreSpy{SkillStore: newSkills(t)},
		reg:   rr, cap: cap, clock: clock,
	}
	mutate(fx)
	svc, err := agentcfgprotocol.NewUserSkillImportService(imp, artifactStore, proposals, fx.store, rr, cap,
		agentcfgprotocol.WithImportSessionReach(auth.NewSessionReachAuthorizer()),
		agentcfgprotocol.WithImportAgentReach(auth.NewAgentReachAuthorizer()),
		agentcfgprotocol.WithImportClock(clock.Now),
	)
	if err != nil {
		t.Fatalf("NewUserSkillImportService: %v", err)
	}
	admin, err := agentcfgprotocol.NewService(reg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	fx.svc = svc
	fx.admin = admin
	t.Cleanup(func() {
		_ = imp.Close(context.Background())
		_ = artifactStore.Close(context.Background())
		_ = proposals.Close(context.Background())
		_ = fx.store.Close(context.Background())
	})
	return fx
}

// importCtx seats the verified identity plus the two signed reach sets the
// way the auth middleware would (a present session_reach containing the
// caller's own session, and agent_reach containing the effective agent).
func importCtx(id identity.Identity, agentID string) context.Context {
	ctx, _ := identity.WithVerified(context.Background(), id)
	ctx = auth.WithSessionReach(ctx, []string{id.SessionID})
	ctx = auth.WithAgentReach(ctx, []string{agentID})
	return ctx
}

func importQuad(id identity.Identity) identity.Quadruple {
	return identity.Quadruple{Identity: id}
}

// validateReq builds the first-phase request for the fixture package.
func (fx *importFixture) validateReq(id identity.Identity, agentID, artifactID string) agentcfgprotocol.UserSkillImportValidateRequest {
	return agentcfgprotocol.UserSkillImportValidateRequest{ArtifactID: artifactID, AgentID: agentID}
}

// commitReqFromValidate echoes the proposal inputs the way the caller would.
func commitReqFromValidate(validate agentcfgprotocol.UserSkillImportValidateResponse, id identity.Identity, agentID string, replace bool) agentcfgprotocol.UserSkillImportCommitRequest {
	return agentcfgprotocol.UserSkillImportCommitRequest{
		ProposalID:          validate.ProposalID,
		AgentID:             agentID,
		Name:                validate.Review.Name,
		ReviewedPackageHash: validate.PackageHash,
		ExpectedContentHash: validate.ExpectedContentHash,
		Replace:             replace,
	}
}

// validateCommit is the happy-path helper: upload, validate, commit.
func (fx *importFixture) validateCommit(t *testing.T, id identity.Identity, agentID string, entries map[string]string, replace bool) (agentcfgprotocol.UserSkillImportValidateResponse, agentcfgprotocol.UserSkillImportCommitResponse, error) {
	t.Helper()
	artifactID := fx.uploadPackage(t, id, entries)
	ctx := importCtx(id, agentID)
	validated, err := fx.svc.Validate(ctx, fx.validateReq(id, agentID, artifactID))
	if err != nil {
		return agentcfgprotocol.UserSkillImportValidateResponse{}, agentcfgprotocol.UserSkillImportCommitResponse{}, err
	}
	committed, err := fx.svc.Commit(ctx, commitReqFromValidate(validated, id, agentID, replace))
	return validated, committed, err
}

// tamperProposal rewrites the durable proposal slot's JSON (used to exercise
// the ceiling-snapshot and committing-resume paths).
func (fx *importFixture) tamperProposal(t *testing.T, q identity.Quadruple, kind string, mutate func(map[string]any)) {
	t.Helper()
	rec, err := fx.proposals.Load(context.Background(), q, kind)
	if err != nil {
		t.Fatalf("load proposal for tamper: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Bytes, &m); err != nil {
		t.Fatalf("decode proposal for tamper: %v", err)
	}
	mutate(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-encode proposal for tamper: %v", err)
	}
	if err := fx.proposals.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: b}); err != nil {
		t.Fatalf("save tampered proposal: %v", err)
	}
}

func (fx *importFixture) proposalKind(proposalID string) string {
	return "agentcfg.user_skill_import.proposal." + proposalID
}

// assertZeroMutations asserts the validate contract: no SkillStore mutation of
// any kind happened.
func (fx *importFixture) assertZeroMutations(t *testing.T) {
	t.Helper()
	put, upsert, del, delAgent, delInstalled, restore := fx.store.counts()
	if put != 0 || upsert != 0 || del != 0 || delAgent != 0 || delInstalled != 0 || restore != 0 {
		t.Fatalf("validate performed a durable skill mutation: put=%d upsert=%d delete=%d deleteAgent=%d deleteInstalled=%d restore=%d",
			put, upsert, del, delAgent, delInstalled, restore)
	}
}

// ---------------------------------------------------------------------------
// Constructor
// ---------------------------------------------------------------------------

func TestUserSkillImport_NewService_MissingDependencyFailsLoud(t *testing.T) {
	ctx := context.Background()
	artifactStore, _ := artifactsinmem.New(config.ArtifactsConfig{})
	defer artifactStore.Close(ctx) //nolint:errcheck // test teardown
	imp, _ := importer.New(importer.Deps{Store: artifactStore})
	proposals, _ := stateinmem.New(config.StateConfig{Driver: "inmem"})
	defer proposals.Close(ctx) //nolint:errcheck // test teardown
	reg := newRegistry(t)
	rr, ok := reg.(agentcfg.RetirementRegistry)
	if !ok {
		t.Fatalf("registry does not implement RetirementRegistry")
	}
	cap := &fakeCapability{policy: defaultImportCapability()}

	if _, err := agentcfgprotocol.NewUserSkillImportService(nil, artifactStore, proposals, nil, rr, cap); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportMisconfigured) {
		t.Fatalf("nil importer err=%v, want ErrUserSkillImportMisconfigured", err)
	}
	if _, err := agentcfgprotocol.NewUserSkillImportService(imp, nil, proposals, nil, rr, cap); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportMisconfigured) {
		t.Fatalf("nil artifact store err=%v, want ErrUserSkillImportMisconfigured", err)
	}
	if _, err := agentcfgprotocol.NewUserSkillImportService(imp, artifactStore, nil, nil, rr, cap); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportMisconfigured) {
		t.Fatalf("nil proposal ledger err=%v, want ErrUserSkillImportMisconfigured", err)
	}
	if _, err := agentcfgprotocol.NewUserSkillImportService(imp, artifactStore, proposals, nil, rr, cap); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportMisconfigured) {
		t.Fatalf("nil skill store err=%v, want ErrUserSkillImportMisconfigured", err)
	}
	if _, err := agentcfgprotocol.NewUserSkillImportService(imp, artifactStore, proposals, fxStore(t), nil, cap); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportMisconfigured) {
		t.Fatalf("nil registry err=%v, want ErrUserSkillImportMisconfigured", err)
	}
	if _, err := agentcfgprotocol.NewUserSkillImportService(imp, artifactStore, proposals, fxStore(t), rr, nil); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportMisconfigured) {
		t.Fatalf("nil capability err=%v, want ErrUserSkillImportMisconfigured", err)
	}
}

func fxStore(t *testing.T) skills.SkillStore { return newSkills(t) }

// ---------------------------------------------------------------------------
// Validate: ZERO writes, closed review, warnings, durability
// ---------------------------------------------------------------------------

func TestUserSkillImport_Validate_ZeroMutations(t *testing.T) {
	fx := newImportFixture(t)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	ctx := importCtx(importUserA, testAgentID)

	resp, err := fx.svc.Validate(ctx, fx.validateReq(importUserA, testAgentID, artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}

	// Closed review shape: canonical name, reviewed hash, stored content
	// hash, trigger, steps, support manifest, no authority fields.
	if resp.ProposalID == "" {
		t.Fatalf("validate returned no proposal id")
	}
	if resp.PackageHash == "" || !strings.HasPrefix(resp.PackageHash, "v1:") {
		t.Fatalf("package hash %q is not versioned", resp.PackageHash)
	}
	r := resp.Review
	if r.Name != "demo-skill" || r.Trigger != "when asked about the demo" || r.StepCount != 1 {
		t.Fatalf("review = %+v", r)
	}
	if r.ContentHash == "" || r.ContentHash == resp.PackageHash {
		t.Fatalf("content hash %q must be distinct from package hash %q", r.ContentHash, resp.PackageHash)
	}
	if r.PackageHash != resp.PackageHash {
		t.Fatalf("review package hash %q != response %q", r.PackageHash, resp.PackageHash)
	}
	if resp.ExpectedContentHash != agentcfg.ExpectNoActiveRevision {
		t.Fatalf("expected config hash %q, want the no-active sentinel (no user revision yet)", resp.ExpectedContentHash)
	}
	if !resp.ExpiresAt.After(fx.clock.Now()) {
		t.Fatalf("expiry %v not after now %v", resp.ExpiresAt, fx.clock.Now())
	}
	// No capability warnings for a permitted required tool.
	if len(resp.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %+v", resp.Warnings)
	}

	// ZERO durable SkillStore/package mutation.
	fx.assertZeroMutations(t)

	// ZERO agent-config membership mutation: the caller still has no user
	// revision.
	ug, err := fx.admin.UserGet(ctx, prototypes.AgentConfigUserGetRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil {
		t.Fatalf("user get: %v", err)
	}
	if ug.Set {
		t.Fatalf("validate wrote a user-scope config revision")
	}

	// The proposal IS durably recorded (the two-phase ledger, not a skill
	// write).
	rec, err := fx.proposals.Load(context.Background(), importQuad(importUserA), fx.proposalKind(resp.ProposalID))
	if err != nil {
		t.Fatalf("proposal not durably recorded: %v", err)
	}
	if len(rec.Bytes) == 0 {
		t.Fatalf("proposal record is empty")
	}
}

func TestUserSkillImport_Validate_WarningsForNonVisibleRequirements(t *testing.T) {
	fx := newImportFixture(t)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importAnnotatedMD})
	ctx := importCtx(importUserA, testAgentID)

	resp, err := fx.svc.Validate(ctx, fx.validateReq(importUserA, testAgentID, artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	// tool-x and ns-x are outside the permitted snapshot: two non-fatal
	// warnings, never a grant, never a refusal.
	if len(resp.Warnings) != 2 {
		t.Fatalf("warnings = %+v, want the two non-visible requirements", resp.Warnings)
	}
	fx.assertZeroMutations(t)
}

func TestUserSkillImport_Validate_RejectsAuthorityAndUnknownFrontmatter(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	for _, keyValue := range []string{
		"scope: user",
		"origin: remote",
		"tenant: acme",
		"user: alice",
		"agent: a-1",
		"authority: admin",
		"audience: public",
		"x-custom: v",
	} {
		md := "---\nname: auth-skill\ntrigger: t\n" + keyValue + "\n---\n## Steps\n- s\n"
		artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": md})
		_, err := fx.svc.Validate(ctx, fx.validateReq(importUserA, testAgentID, artifactID))
		if !errors.Is(err, importer.ErrPackageFrontmatterDisallowed) {
			t.Fatalf("frontmatter %q err=%v, want ErrPackageFrontmatterDisallowed", keyValue, err)
		}
		if !errors.Is(err, agentcfgprotocol.ErrUserSkillImportPackageInvalid) {
			t.Fatalf("frontmatter %q err=%v, want wrapped ErrUserSkillImportPackageInvalid", keyValue, err)
		}
	}
	fx.assertZeroMutations(t)
}

func TestUserSkillImport_Validate_RejectsMalformedPackages(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	validMD := "---\ntrigger: t\n---\n## Steps\n- s\n"
	cases := []struct {
		name    string
		entries map[string]string
		want    error
	}{
		{"traversal", map[string]string{"../escape": "x"}, skills.ErrArchiveTraversal},
		{"missing skillmd", map[string]string{"README.md": "hi"}, skills.ErrSkillMDMissing},
		{"case collision", map[string]string{"SKILL.md": validMD, "skill.md": validMD}, skills.ErrArchivePathCollision},
		{"missing trigger", map[string]string{"SKILL.md": "---\nname: x\n---\n## Steps\n- s\n"}, skills.ErrSkillMDMissingTrigger},
		{"dangling support ref", map[string]string{
			"SKILL.md": "---\nname: x\ntrigger: t\n---\n![missing](assets/nope.png)\n\n## Steps\n- s\n",
		}, importer.ErrPackageSupportRefMissing},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			artifactID := fx.uploadPackage(t, importUserA, c.entries)
			_, err := fx.svc.Validate(ctx, fx.validateReq(importUserA, testAgentID, artifactID))
			if err == nil || !errors.Is(err, c.want) {
				t.Fatalf("err=%v, want errors.Is %v", err, c.want)
			}
			if !errors.Is(err, agentcfgprotocol.ErrUserSkillImportPackageInvalid) {
				t.Fatalf("err=%v, want wrapped ErrUserSkillImportPackageInvalid", err)
			}
		})
	}
	fx.assertZeroMutations(t)
}

func TestUserSkillImport_Validate_ArtifactNotCallerOwned(t *testing.T) {
	fx := newImportFixture(t)
	// Uploaded under user A's triple.
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	// User B (same tenant, same agent) cannot validate it: the read under
	// B's exact triple does not resolve — non-oracular not-found.
	userB := identity.Identity{TenantID: "t", UserID: "v", SessionID: "s1"}
	ctxB := importCtx(userB, testAgentID)
	if _, err := fx.svc.Validate(ctxB, fx.validateReq(userB, testAgentID, artifactID)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportArtifactNotFound) {
		t.Fatalf("cross-user validate err=%v, want ErrUserSkillImportArtifactNotFound", err)
	}
	// A guessed / erased id is the SAME typed outcome — no enumeration.
	if _, err := fx.svc.Validate(ctxB, fx.validateReq(userB, testAgentID, "guessed-id")); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportArtifactNotFound) {
		t.Fatalf("guessed id err=%v, want ErrUserSkillImportArtifactNotFound", err)
	}
	// Cross-tenant too.
	userOther := identity.Identity{TenantID: "other", UserID: "u", SessionID: "s1"}
	ctxOther := importCtx(userOther, testAgentID)
	if _, err := fx.svc.Validate(ctxOther, fx.validateReq(userOther, testAgentID, artifactID)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportArtifactNotFound) {
		t.Fatalf("cross-tenant validate err=%v, want ErrUserSkillImportArtifactNotFound", err)
	}
	fx.assertZeroMutations(t)
}

// ---------------------------------------------------------------------------
// Commit: the ONE atomic package+membership write
// ---------------------------------------------------------------------------

func TestUserSkillImport_Commit_InstallsAtomicUnit(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{
		"SKILL.md":           importSkillMD,
		"examples/demo.json": `{"demo": true}`,
		"docs/usage.txt":     "usage notes",
	})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(importUserA, testAgentID, artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(validated.Review.SupportFiles) != 2 {
		t.Fatalf("support manifest review = %+v, want 2 entries", validated.Review.SupportFiles)
	}
	committed, err := fx.svc.Commit(ctx, commitReqFromValidate(validated, importUserA, testAgentID, false))
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if committed.Replayed {
		t.Fatalf("first commit must not be a replay")
	}
	if committed.Receipt.WrittenHash != validated.PackageHash {
		t.Fatalf("receipt hash %q != reviewed %q", committed.Receipt.WrittenHash, validated.PackageHash)
	}
	if committed.Receipt.TenantID != "t" || committed.Receipt.UserID != "u" || committed.Receipt.AgentID != testAgentID || committed.Receipt.Name != "demo-skill" {
		t.Fatalf("receipt key = %+v", committed.Receipt)
	}
	// The atomic unit IS the membership: exactly one put, no second legacy
	// membership write.
	put, upsert, del, delAgent, delInstalled, restore := fx.store.counts()
	if put != 1 || upsert != 0 || del != 0 || delAgent != 0 || delInstalled != 0 || restore != 0 {
		t.Fatalf("commit mutation counters put=%d upsert=%d delete=%d deleteAgent=%d deleteInstalled=%d restore=%d",
			put, upsert, del, delAgent, delInstalled, restore)
	}
	// No user-scope config revision was written either.
	ug, err := fx.admin.UserGet(ctx, prototypes.AgentConfigUserGetRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil {
		t.Fatalf("user get: %v", err)
	}
	if ug.Set {
		t.Fatalf("commit wrote a legacy user-scope config revision")
	}

	// The stored unit is the durable package: ScopeUser forced, the
	// effective agent forced, canonical name, complete support bytes.
	stored, err := fx.store.GetInstalledPackage(ctx, importQuad(importUserA), testAgentID, "demo-skill")
	if err != nil {
		t.Fatalf("get installed package: %v", err)
	}
	if stored.Skill.Scope != skills.ScopeUser {
		t.Fatalf("stored scope = %q, want user", stored.Skill.Scope)
	}
	if stored.Skill.AgentID != testAgentID {
		t.Fatalf("stored agent = %q, want %q", stored.Skill.AgentID, testAgentID)
	}
	if stored.Skill.Origin != skills.OriginPack {
		t.Fatalf("stored origin = %q, want pack", stored.Skill.Origin)
	}
	if stored.Skill.Name != "demo-skill" || stored.Skill.ContentHash != validated.Review.ContentHash {
		t.Fatalf("stored skill = %+v", stored.Skill)
	}
	if len(stored.Package.Supports) != 2 {
		t.Fatalf("stored supports = %+v", stored.Package.Supports)
	}
	for _, f := range stored.Package.Supports {
		if len(f.Data) == 0 {
			t.Fatalf("support %q lost its bytes", f.Path)
		}
	}
}

func TestUserSkillImport_Commit_ResponseLossReplayIsIdempotent(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(importUserA, testAgentID, artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	req := commitReqFromValidate(validated, importUserA, testAgentID, false)

	first, err := fx.svc.Commit(ctx, req)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if first.Replayed {
		t.Fatalf("first commit must not be a replay")
	}

	// The response is lost; the caller retries with the exact same inputs.
	second, err := fx.svc.Commit(ctx, req)
	if err != nil {
		t.Fatalf("replay commit: %v", err)
	}
	if !second.Replayed {
		t.Fatalf("retry after a landed commit must be recognized as a replay")
	}
	if second.Receipt.WrittenHash != first.Receipt.WrittenHash || second.PackageHash != first.PackageHash {
		t.Fatalf("replay returned a different terminal result: first=%+v second=%+v", first, second)
	}

	// The atomic write happened EXACTLY ONCE across the two calls.
	put, _, _, _, _, _ := fx.store.counts()
	if put != 1 {
		t.Fatalf("replay performed a second package write: put=%d, want 1", put)
	}
}

func TestUserSkillImport_Commit_CommittingResumeRecognizesLandedWrite(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(importUserA, testAgentID, artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	req := commitReqFromValidate(validated, importUserA, testAgentID, false)
	if _, err := fx.svc.Commit(ctx, req); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Simulate the mid-flight window: the atomic write landed but the
	// terminal receipt was never recorded (the slot still says
	// "committing", no receipt).
	fx.tamperProposal(t, importQuad(importUserA), fx.proposalKind(validated.ProposalID), func(m map[string]any) {
		m["phase"] = "committing"
		delete(m, "receipt")
	})

	resumed, err := fx.svc.Commit(ctx, req)
	if err != nil {
		t.Fatalf("resume commit: %v", err)
	}
	if !resumed.Replayed {
		t.Fatalf("resume of a landed write must be recognized as a replay")
	}
	if resumed.Receipt.WrittenHash != validated.PackageHash {
		t.Fatalf("resumed receipt hash %q != reviewed %q", resumed.Receipt.WrittenHash, validated.PackageHash)
	}
	put, _, _, _, _, _ := fx.store.counts()
	if put != 1 {
		t.Fatalf("resume performed a second package write: put=%d, want 1", put)
	}
}

func TestUserSkillImport_Commit_ExplicitReplaceConsent(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	// Version 1 of the package.
	artifactV1 := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	v1, err := fx.svc.Validate(ctx, fx.validateReq(importUserA, testAgentID, artifactV1))
	if err != nil {
		t.Fatalf("validate v1: %v", err)
	}
	if _, err := fx.svc.Commit(ctx, commitReqFromValidate(v1, importUserA, testAgentID, false)); err != nil {
		t.Fatalf("commit v1: %v", err)
	}

	// Version 2: same canonical name, different body → different hash.
	mdV2 := "---\nname: demo-skill\ntrigger: when asked about the demo\n---\nA changed demo body.\n\n## Steps\n- do the thing\n- also this\n"
	artifactV2 := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": mdV2})
	v2, err := fx.svc.Validate(ctx, fx.validateReq(importUserA, testAgentID, artifactV2))
	if err != nil {
		t.Fatalf("validate v2: %v", err)
	}
	if v2.PackageHash == v1.PackageHash {
		t.Fatalf("fixture versions must differ")
	}

	// Without explicit consent the different winner is refused BEFORE any
	// write.
	if _, err := fx.svc.Commit(ctx, commitReqFromValidate(v2, importUserA, testAgentID, false)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportReplaceRequired) {
		t.Fatalf("no-consent commit err=%v, want ErrUserSkillImportReplaceRequired", err)
	}
	stored, err := fx.store.GetInstalledPackage(ctx, importQuad(importUserA), testAgentID, "demo-skill")
	if err != nil {
		t.Fatalf("get installed: %v", err)
	}
	if stored.PackageHash != v1.PackageHash {
		t.Fatalf("refused replacement touched the winner: %q != %q", stored.PackageHash, v1.PackageHash)
	}
	put, _, _, _, _, _ := fx.store.counts()
	if put != 1 {
		t.Fatalf("refused replacement wrote: put=%d, want 1", put)
	}

	// With explicit consent the replace lands as ONE atomic write.
	replaced, err := fx.svc.Commit(ctx, commitReqFromValidate(v2, importUserA, testAgentID, true))
	if err != nil {
		t.Fatalf("consented commit: %v", err)
	}
	if replaced.Receipt.WrittenHash != v2.PackageHash {
		t.Fatalf("replace receipt hash %q != reviewed %q", replaced.Receipt.WrittenHash, v2.PackageHash)
	}
	stored, err = fx.store.GetInstalledPackage(ctx, importQuad(importUserA), testAgentID, "demo-skill")
	if err != nil {
		t.Fatalf("get installed after replace: %v", err)
	}
	if stored.PackageHash != v2.PackageHash {
		t.Fatalf("replace did not land: %q != %q", stored.PackageHash, v2.PackageHash)
	}
	put, _, _, _, _, _ = fx.store.counts()
	if put != 2 {
		t.Fatalf("replace mutation count = %d, want 2 (create + replace)", put)
	}
}

// ---------------------------------------------------------------------------
// Commit refusal matrix: stale / foreign / expired / moved / revoked
// ---------------------------------------------------------------------------

func TestUserSkillImport_Commit_ExpiredProposalRefused(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(importUserA, testAgentID, artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	// The review window elapses.
	fx.clock.advance(25 * time.Hour)
	if _, err := fx.svc.Commit(ctx, commitReqFromValidate(validated, importUserA, testAgentID, false)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportExpired) {
		t.Fatalf("expired commit err=%v, want ErrUserSkillImportExpired", err)
	}
	fx.assertZeroMutations(t)
}

func TestUserSkillImport_Commit_MovedConfigBaseRefused(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(importUserA, testAgentID, artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	// The caller's user-scope config base moves between validate and
	// commit (e.g. another verb wrote a user revision).
	if _, err := fx.admin.UserSetRevision(ctx, prototypes.AgentConfigUserSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigUserPayload{UserPrompt: "moved base"},
	}); err != nil {
		t.Fatalf("seed user revision: %v", err)
	}
	if _, err := fx.svc.Commit(ctx, commitReqFromValidate(validated, importUserA, testAgentID, false)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportConfigMoved) {
		t.Fatalf("moved-base commit err=%v, want ErrUserSkillImportConfigMoved", err)
	}
	fx.assertZeroMutations(t)
}

func TestUserSkillImport_Commit_PolicyRevocationRefused(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(importUserA, testAgentID, artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	// The capability snapshot is revoked between validate and commit.
	fx.cap.set(agentcfgprotocol.UserSkillImportPolicy{ID: "harbor.user-skill-import", Version: "1", PermittedTools: []string{"tool-b"}})
	if _, err := fx.svc.Commit(ctx, commitReqFromValidate(validated, importUserA, testAgentID, false)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportPolicyRevoked) {
		t.Fatalf("revoked-policy commit err=%v, want ErrUserSkillImportPolicyRevoked", err)
	}
	fx.assertZeroMutations(t)
}

func TestUserSkillImport_Commit_LostReachRefused(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(importUserA, testAgentID, artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	req := commitReqFromValidate(validated, importUserA, testAgentID, false)

	// No agent reach on ctx → fails closed.
	noReach, err := identity.WithVerified(context.Background(), importUserA)
	if err != nil {
		t.Fatalf("noReach ctx: %v", err)
	}
	if _, err := fx.svc.Commit(noReach, req); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportAgentReachDenied) {
		t.Fatalf("no-reach commit err=%v, want ErrUserSkillImportAgentReachDenied", err)
	}
	// Agent reach for a DIFFERENT agent → denied.
	otherAgent, err := identity.WithVerified(context.Background(), importUserA)
	if err != nil {
		t.Fatalf("otherAgent ctx: %v", err)
	}
	otherAgent = auth.WithSessionReach(otherAgent, []string{importUserA.SessionID})
	otherAgent = auth.WithAgentReach(otherAgent, []string{"agent-other"})
	if _, err := fx.svc.Commit(otherAgent, req); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportAgentReachDenied) {
		t.Fatalf("other-agent reach commit err=%v, want ErrUserSkillImportAgentReachDenied", err)
	}
	// Session reach not containing the caller's session → denied.
	noSession, err := identity.WithVerified(context.Background(), importUserA)
	if err != nil {
		t.Fatalf("noSession ctx: %v", err)
	}
	noSession = auth.WithSessionReach(noSession, []string{"other-session"})
	noSession = auth.WithAgentReach(noSession, []string{testAgentID})
	if _, err := fx.svc.Commit(noSession, req); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportSessionReachDenied) {
		t.Fatalf("other-session reach commit err=%v, want ErrUserSkillImportSessionReachDenied", err)
	}
	fx.assertZeroMutations(t)
}

func TestUserSkillImport_Commit_HashNameConfigMismatchRefused(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(importUserA, testAgentID, artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	base := commitReqFromValidate(validated, importUserA, testAgentID, false)

	// A different reviewed hash is a changed review.
	wrongHash := base
	wrongHash.ReviewedPackageHash = "v1:" + strings.Repeat("ab", 32)
	if _, err := fx.svc.Commit(ctx, wrongHash); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportHashMismatch) {
		t.Fatalf("wrong hash err=%v, want ErrUserSkillImportHashMismatch", err)
	}
	// A different reviewed name is a foreign/stale proposal.
	wrongName := base
	wrongName.Name = "other-skill"
	if _, err := fx.svc.Commit(ctx, wrongName); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportProposalInvalid) {
		t.Fatalf("wrong name err=%v, want ErrUserSkillImportProposalInvalid", err)
	}
	// A different expected config hash is a stale base echo.
	wrongConfig := base
	wrongConfig.ExpectedContentHash = strings.Repeat("cd", 32)
	if _, err := fx.svc.Commit(ctx, wrongConfig); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportProposalInvalid) {
		t.Fatalf("wrong expected config hash err=%v, want ErrUserSkillImportProposalInvalid", err)
	}
	// An unknown proposal id.
	unknown := base
	unknown.ProposalID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	if _, err := fx.svc.Commit(ctx, unknown); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportProposalInvalid) {
		t.Fatalf("unknown proposal err=%v, want ErrUserSkillImportProposalInvalid", err)
	}
	fx.assertZeroMutations(t)
}

func TestUserSkillImport_Commit_ForeignIdentityRefused(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(importUserA, testAgentID, artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	req := commitReqFromValidate(validated, importUserA, testAgentID, false)

	// A different USER with the same proposal id cannot load the slot.
	userB := identity.Identity{TenantID: "t", UserID: "v", SessionID: "s1"}
	ctxB := importCtx(userB, testAgentID)
	if _, err := fx.svc.Commit(ctxB, req); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportProposalInvalid) {
		t.Fatalf("cross-user commit err=%v, want ErrUserSkillImportProposalInvalid", err)
	}
	// A different SESSION of the same user: the proposal's actor triple
	// must equal the caller's exact triple.
	userASess2 := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s2"}
	ctxS2 := importCtx(userASess2, testAgentID)
	if _, err := fx.svc.Commit(ctxS2, req); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportProposalInvalid) {
		t.Fatalf("cross-session commit err=%v, want ErrUserSkillImportProposalInvalid", err)
	}
	// A different AGENT with the same proposal id: reach is signed for the
	// OTHER agent, so the reach gate passes and the proposal-binding check
	// refuses (the proposal is bound to testAgentID).
	ctxOtherAgent := importCtx(importUserA, "agent-other")
	crossAgent := req
	crossAgent.AgentID = "agent-other"
	if _, err := fx.svc.Commit(ctxOtherAgent, crossAgent); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportProposalInvalid) {
		t.Fatalf("cross-agent commit err=%v, want ErrUserSkillImportProposalInvalid", err)
	}
	fx.assertZeroMutations(t)
}

func TestUserSkillImport_Commit_ChangedCeilingSnapshotRefused(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(importUserA, testAgentID, artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	// Tamper the reviewed ceiling snapshot: the archive entry bound is no
	// longer the canonical value.
	fx.tamperProposal(t, importQuad(importUserA), fx.proposalKind(validated.ProposalID), func(m map[string]any) {
		limits, ok := m["archive_limits"].(map[string]any)
		if !ok {
			t.Fatalf("proposal JSON lacks archive_limits: %v", m)
		}
		limits["MaxEntries"] = 999
	})
	if _, err := fx.svc.Commit(ctx, commitReqFromValidate(validated, importUserA, testAgentID, false)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportCeilingChanged) {
		t.Fatalf("changed-ceiling commit err=%v, want ErrUserSkillImportCeilingChanged", err)
	}
	fx.assertZeroMutations(t)
}

func TestUserSkillImport_Commit_ChangedArtifactRefused(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(importUserA, testAgentID, artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	// The caller erases the source artifact after validation: the commit
	// re-resolution must refuse (non-oracular not-found), never install.
	if _, err := fx.artifacts.Delete(ctx, artifacts.ArtifactScope{TenantID: "t", UserID: "u", SessionID: "s"}, artifactID); err != nil {
		t.Fatalf("delete artifact: %v", err)
	}
	if _, err := fx.svc.Commit(ctx, commitReqFromValidate(validated, importUserA, testAgentID, false)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportArtifactNotFound) {
		t.Fatalf("erased-artifact commit err=%v, want ErrUserSkillImportArtifactNotFound", err)
	}
	fx.assertZeroMutations(t)
}

// ---------------------------------------------------------------------------
// Lifecycle fence and fail-closed wiring
// ---------------------------------------------------------------------------

func TestUserSkillImport_RetiredAgentRefused(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	if _, err := fx.reg.Retire(ctx, importQuad(importUserA), testAgentID, agentcfg.RetirementRequest{
		OperationID: "import-retire", ExpectedContentHash: agentcfg.ExpectNoActiveRevision,
	}); err != nil {
		t.Fatalf("retire: %v", err)
	}
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	if _, err := fx.svc.Validate(ctx, fx.validateReq(importUserA, testAgentID, artifactID)); !errors.Is(err, agentcfg.ErrAgentRetired) {
		t.Fatalf("retired validate err=%v, want ErrAgentRetired", err)
	}
	if _, err := fx.svc.Commit(ctx, agentcfgprotocol.UserSkillImportCommitRequest{
		ProposalID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", AgentID: testAgentID, Name: "x",
		ReviewedPackageHash: "v1:" + strings.Repeat("ab", 32),
	}); !errors.Is(err, agentcfg.ErrAgentRetired) {
		t.Fatalf("retired commit err=%v, want ErrAgentRetired", err)
	}
	fx.assertZeroMutations(t)
}

func TestUserSkillImport_UnwiredAgentReachFailsClosed(t *testing.T) {
	// A service constructed WITHOUT the effective-agent gate serves nothing:
	// an unwired gate is an honest "cannot verify reach", never a silent
	// widening.
	fx := newImportFixture(t)
	artifactStore := fx.artifacts
	imp := fx.imp
	proposals := fx.proposals
	svc, err := agentcfgprotocol.NewUserSkillImportService(imp, artifactStore, proposals, fx.store, fx.reg, fx.cap)
	if err != nil {
		t.Fatalf("NewUserSkillImportService (no reach gates): %v", err)
	}
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	if _, err := svc.Validate(ctx, fx.validateReq(importUserA, testAgentID, artifactID)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportAgentReachDenied) {
		t.Fatalf("unwired-gate validate err=%v, want ErrUserSkillImportAgentReachDenied", err)
	}
	fx.assertZeroMutations(t)
}

// ---------------------------------------------------------------------------
// HA-66 boot-owned guard
// ---------------------------------------------------------------------------

func TestUserSkillImport_BootOwnedNameRefusedEverywhere(t *testing.T) {
	fx := newImportFixture(t)
	owner := &fakeBootOwner{names: map[string]struct{}{"demo-skill": {}}}
	guarded := agentcfgprotocol.WithBootOwnership(importCtx(importUserA, testAgentID), owner)

	// Validate refuses BEFORE a proposal exists.
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	if _, err := fx.svc.Validate(guarded, fx.validateReq(importUserA, testAgentID, artifactID)); !errors.Is(err, agentcfgprotocol.ErrBootPackOwned) {
		t.Fatalf("boot-owned validate err=%v, want ErrBootPackOwned", err)
	}
	fx.assertZeroMutations(t)

	// Commit refuses on EVERY path even when the proposal was created
	// before the baseline bound the name.
	unguarded := importCtx(importUserA, testAgentID)
	validated, err := fx.svc.Validate(unguarded, fx.validateReq(importUserA, testAgentID, artifactID))
	if err != nil {
		t.Fatalf("validate (unguarded): %v", err)
	}
	req := commitReqFromValidate(validated, importUserA, testAgentID, false)
	if _, err := fx.svc.Commit(guarded, req); !errors.Is(err, agentcfgprotocol.ErrBootPackOwned) {
		t.Fatalf("boot-owned commit err=%v, want ErrBootPackOwned", err)
	}
	fx.assertZeroMutations(t)
}

// ---------------------------------------------------------------------------
// Cross-user / cross-session isolation
// ---------------------------------------------------------------------------

func TestUserSkillImport_CrossUserIsolation(t *testing.T) {
	fx := newImportFixture(t)
	userA := importUserA
	userB := identity.Identity{TenantID: "t", UserID: "v", SessionID: "s1"}

	// A commits its own skill.
	if _, _, err := fx.validateCommit(t, userA, testAgentID, map[string]string{"SKILL.md": importSkillMD}, false); err != nil {
		t.Fatalf("A validate+commit: %v", err)
	}
	// B's (tenant, user, agent, name) key has nothing — no leakage.
	ctxB := importCtx(userB, testAgentID)
	if _, err := fx.store.GetInstalledPackage(ctxB, importQuad(userB), testAgentID, "demo-skill"); !errors.Is(err, skills.ErrInstalledPackageNotFound) {
		t.Fatalf("B sees A's installed package: err=%v", err)
	}
	// B cannot validate A's artifact either (already covered above).
	if _, err := fx.svc.Validate(ctxB, fx.validateReq(userB, testAgentID, fx.uploadPackage(t, userA, map[string]string{"SKILL.md": importSkillMD}))); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportArtifactNotFound) {
		t.Fatalf("B validating A's artifact err=%v, want not found", err)
	}
}

func TestUserSkillImport_CrossSessionIsolation(t *testing.T) {
	fx := newImportFixture(t)
	// Validate in session s, attempt the commit in session s2 of the SAME
	// user: the proposal's actor triple must equal the caller's exact
	// triple — the artifact is session-scoped and the review is
	// session-bound.
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(importUserA, testAgentID, artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	userASess2 := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s2"}
	ctxS2 := importCtx(userASess2, testAgentID)
	if _, err := fx.svc.Commit(ctxS2, commitReqFromValidate(validated, userASess2, testAgentID, false)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportProposalInvalid) {
		t.Fatalf("cross-session commit err=%v, want ErrUserSkillImportProposalInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// Exact-receipt compensation: only its own complete unit/version
// ---------------------------------------------------------------------------

func TestUserSkillImport_Compensation_ExactReceiptUndoesOnlyItsOwnUnit(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)

	mdV1 := importSkillMD
	mdV2 := "---\nname: demo-skill\ntrigger: when asked about the demo\n---\nA changed demo body.\n\n## Steps\n- do the thing\n- also this\n"
	_, c1, err := fx.validateCommit(t, importUserA, testAgentID, map[string]string{"SKILL.md": mdV1}, false)
	if err != nil {
		t.Fatalf("commit v1: %v", err)
	}
	_, c2, err := fx.validateCommit(t, importUserA, testAgentID, map[string]string{"SKILL.md": mdV2}, true)
	if err != nil {
		t.Fatalf("commit v2 (replace): %v", err)
	}
	if c1.Receipt.WrittenHash == c2.Receipt.WrittenHash {
		t.Fatalf("fixture versions must differ")
	}

	// The v1 receipt must NEVER delete v2's winner — it undoes only the
	// exact unit/version it wrote.
	deleted, err := fx.store.DeleteInstalledPackage(ctx, importQuad(importUserA), testAgentID, "demo-skill", c1.Receipt)
	if err != nil {
		t.Fatalf("delete with stale receipt: %v", err)
	}
	if deleted {
		t.Fatalf("a stale receipt deleted the new winner")
	}
	winner, err := fx.store.GetInstalledPackage(ctx, importQuad(importUserA), testAgentID, "demo-skill")
	if err != nil || winner.PackageHash != c2.Receipt.WrittenHash {
		t.Fatalf("v2 winner disturbed: err=%v hash=%q", err, winner.PackageHash)
	}

	// The v2 receipt removes the COMPLETE unit: package + support rows +
	// the legacy ScopeUser membership row — no orphan body or membership.
	deleted, err = fx.store.DeleteInstalledPackage(ctx, importQuad(importUserA), testAgentID, "demo-skill", c2.Receipt)
	if err != nil || !deleted {
		t.Fatalf("delete with current receipt: deleted=%v err=%v", deleted, err)
	}
	if _, err := fx.store.GetInstalledPackage(ctx, importQuad(importUserA), testAgentID, "demo-skill"); !errors.Is(err, skills.ErrInstalledPackageNotFound) {
		t.Fatalf("unit survived exact deletion: %v", err)
	}
	// No legacy ScopeUser membership row survives either (the unit was the
	// membership).
	legacy, err := fx.store.List(ctx, importQuad(importUserA), skills.ListFilter{Scope: skills.ScopeUser, AgentID: testAgentID})
	if err != nil {
		t.Fatalf("legacy list: %v", err)
	}
	for _, sk := range legacy {
		if sk.Name == "demo-skill" {
			t.Fatalf("orphan membership row survived: %+v", sk)
		}
	}
}

// ---------------------------------------------------------------------------
// Focused races and concurrency
// ---------------------------------------------------------------------------

// TestUserSkillImport_ConcurrentSameProposal_OneWinner races N commits of the
// SAME proposal: the durable SaveIf serialization plus the package CAS must
// produce exactly one put, and every caller either receives the terminal
// result or a typed refusal — never a double write, never a panic.
func TestUserSkillImport_ConcurrentSameProposal_OneWinner(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(importUserA, testAgentID, artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	req := commitReqFromValidate(validated, importUserA, testAgentID, false)

	const n = 32
	var wg sync.WaitGroup
	errs := make([]error, n)
	results := make([]agentcfgprotocol.UserSkillImportCommitResponse, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = fx.svc.Commit(ctx, req)
		}(i)
	}
	wg.Wait()

	put, _, _, _, _, _ := fx.store.counts()
	if put != 1 {
		t.Fatalf("concurrent same-proposal commits wrote %d packages, want exactly 1", put)
	}
	successes := 0
	for i := 0; i < n; i++ {
		if errs[i] == nil {
			successes++
			if results[i].PackageHash != validated.PackageHash {
				t.Fatalf("concurrent success returned a different package: %+v", results[i])
			}
			continue
		}
		// Every non-success must be a typed refusal, never a partial write.
		if !errors.Is(errs[i], agentcfgprotocol.ErrUserSkillImportProposalInvalid) &&
			!errors.Is(errs[i], agentcfgprotocol.ErrUserSkillImportConcurrentWinner) {
			t.Fatalf("concurrent commit returned an unexpected error: %v", errs[i])
		}
	}
	if successes == 0 {
		t.Fatalf("no concurrent commit returned the terminal result")
	}
	// The unit IS installed with the reviewed hash.
	winner, err := fx.store.GetInstalledPackage(ctx, importQuad(importUserA), testAgentID, "demo-skill")
	if err != nil {
		t.Fatalf("get installed: %v", err)
	}
	if winner.PackageHash != validated.PackageHash {
		t.Fatalf("winner hash %q != reviewed %q", winner.PackageHash, validated.PackageHash)
	}
}

// TestUserSkillImport_ConcurrentMixedIdentities_N100 drives N>=100 concurrent
// validate+commit flows across distinct (user, session, package) triples on
// one shared service instance under -race: no data race, no context bleed, no
// cross-identity visibility, no goroutine leak.
func TestUserSkillImport_ConcurrentMixedIdentities_N100(t *testing.T) {
	fx := newImportFixture(t)
	baseline := runtime.NumGoroutine()

	const n = 100
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user := identity.Identity{TenantID: "t", UserID: fmt.Sprintf("user-%03d", i), SessionID: fmt.Sprintf("session-%03d", i)}
			md := fmt.Sprintf("---\nname: skill-%03d\ntrigger: when asked about %d\n---\nA skill for user %d.\n\n## Steps\n- do the thing\n", i, i, i)
			ctx := importCtx(user, testAgentID)
			artifactID := fx.uploadPackage(t, user, map[string]string{"SKILL.md": md})
			validated, err := fx.svc.Validate(ctx, fx.validateReq(user, testAgentID, artifactID))
			if err != nil {
				errs[i] = fmt.Errorf("validate: %w", err)
				return
			}
			committed, err := fx.svc.Commit(ctx, commitReqFromValidate(validated, user, testAgentID, false))
			if err != nil {
				errs[i] = fmt.Errorf("commit: %w", err)
				return
			}
			if committed.Replayed || committed.Receipt.WrittenHash != validated.PackageHash {
				errs[i] = fmt.Errorf("terminal result mismatch: %+v", committed)
				return
			}
			// The user can read back exactly its own unit.
			stored, err := fx.store.GetInstalledPackage(ctx, importQuad(user), testAgentID, fmt.Sprintf("skill-%03d", i))
			if err != nil {
				errs[i] = fmt.Errorf("read back: %w", err)
				return
			}
			if stored.Skill.AgentID != testAgentID || stored.Skill.Scope != skills.ScopeUser {
				errs[i] = fmt.Errorf("stored envelope wrong: %+v", stored.Skill)
			}
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
	}

	// Exactly n atomic writes, one per identity.
	put, _, _, _, _, _ := fx.store.counts()
	if put != n {
		t.Fatalf("atomic package writes = %d, want %d", put, n)
	}

	// No goroutine leak: the shared service spawns nothing per call.
	var after int
	for range 50 {
		after = runtime.NumGoroutine()
		if after <= baseline+2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if after > baseline+2 {
		t.Errorf("goroutine leak: baseline=%d after=%d", baseline, after)
	}
}
