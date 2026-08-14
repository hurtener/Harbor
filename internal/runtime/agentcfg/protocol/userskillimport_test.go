package protocol_test

// userskillimport_test.go — the verified-caller two-phase complete-skill-package
// import service tests: the ZERO-write validate contract (no SkillStore
// mutation AND no StateStore proposal-ledger write — the review rides inside
// a sealed stateless token), the validate→commit round trip that installs ONE
// atomic package+membership unit (never a second legacy membership write),
// idempotent response-loss replay, explicit replacement consent, the full
// stale/foreign/expired/tampered/policy/config/artifact/boot-owned refusal
// matrix, exact-receipt compensation, cross-session and cross-user isolation,
// focused same-token commit races, and the N>=100 concurrent mixed-identity
// run under -race.

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
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
	toolsauth "github.com/hurtener/Harbor/internal/tools/auth"
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

// importTestSealer returns the deterministic dev token sealer. The KEK is a
// documented test-only dummy (the fixed-byte pattern mirrors the auth sealer
// conformance fixture) and is NEVER used outside tests.
func importTestSealer(t *testing.T) agentcfgprotocol.UserSkillImportProposalSealer {
	t.Helper()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i)
	}
	sealer, err := toolsauth.NewAESGCMSealer(kek)
	if err != nil {
		t.Fatalf("NewAESGCMSealer: %v", err)
	}
	return sealer
}

// Failure-injection sentinels (test-scope only, never used by production):
// each is wrapped by the spy together with its 1-based call index, so tests
// assert errors.Is through the service's own wrapping (including the joined
// compensation-failure path).
var (
	errImportSimulatedPut      = errors.New("simulated installed-package put backend failure")
	errImportSimulatedSaveIf   = errors.New("simulated state saveif backend failure")
	errImportSimulatedDeleteIf = errors.New("simulated state deleteif backend failure")
)

// importSkillPutCall is ONE recorded PutInstalledPackage invocation: the
// 1-based call index, the effective agent, the atomic unit, the exact
// conditional predicate, and the replace consent. The failure-injection tests
// assert the exact write shape from this log instead of guessing.
type importSkillPutCall struct {
	idx     int
	agentID string
	pkg     skills.InstalledPackage
	cond    skills.InstalledPackageCondition
	replace bool
}

// importStateSaveIfCall is ONE recorded SaveIf invocation: the 1-based call
// index, the exact expectation set, and the next record (whose EventID is the
// generation the following conditional write must bind).
type importStateSaveIfCall struct {
	idx          int
	expectations []state.SlotExpectation
	next         state.StateRecord
}

// importStateDeleteIfCall is ONE recorded DeleteIf invocation: the 1-based
// call index and the exact slot expectation it targeted.
type importStateDeleteIfCall struct {
	idx int
	exp state.SlotExpectation
}

// cloneStateRecord deep-copies a StateRecord's Bytes so the call log never
// aliases driver-returned or caller-owned memory.
func cloneStateRecord(r state.StateRecord) state.StateRecord {
	out := r
	out.Bytes = append([]byte(nil), r.Bytes...)
	return out
}

// importSkillStoreSpy wraps the real localdb store and counts every MUTATION
// method — the ZERO-write validate contract and the exactly-once commit
// contract are asserted against these counters. The fail-at-index knob fails
// the PutInstalledPackage call at exactly one deterministic 1-based index
// (0 disables) with a genuine backend error — never a global flag.
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
	// fail-at-exact-call-index knob + ordered put call log.
	failPutInstalledAt int
	putLog             []importSkillPutCall
}

func (s *importSkillStoreSpy) PutInstalledPackage(ctx context.Context, id identity.Quadruple, agentID string, pkg skills.InstalledPackage, cond skills.InstalledPackageCondition, replace bool) (skills.InstalledPackageReceipt, error) {
	s.mu.Lock()
	s.putInstalled++
	idx := s.putInstalled
	fail := s.failPutInstalledAt > 0 && idx == s.failPutInstalledAt
	s.putLog = append(s.putLog, importSkillPutCall{idx: idx, agentID: agentID, pkg: pkg, cond: cond, replace: replace})
	s.mu.Unlock()
	if fail {
		return skills.InstalledPackageReceipt{}, fmt.Errorf("%w (put call %d)", errImportSimulatedPut, idx)
	}
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

// setFailPutInstalledAt arms the spy to fail the PutInstalledPackage call at
// exactly this 1-based index with a genuine backend error (0 disarms).
func (s *importSkillStoreSpy) setFailPutInstalledAt(idx int) {
	s.mu.Lock()
	s.failPutInstalledAt = idx
	s.mu.Unlock()
}

// putLogSnapshot returns a copy of every recorded put invocation in call
// order.
func (s *importSkillStoreSpy) putLogSnapshot() []importSkillPutCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]importSkillPutCall(nil), s.putLog...)
}

// importStateStoreSpy wraps the in-memory StateStore and counts every MUTATION
// method (Save / SaveIf / DeleteIf / Delete). The ZERO-write validate
// contract asserts these counters stay zero; the failWrites mode additionally
// turns any attempted write into an error so a regression fails loudly at the
// call site, not just at the assertion. The fail-at-index knobs fail the
// SaveIf / DeleteIf invocation at exactly one deterministic 1-based index
// (0 disables) with a GENUINE backend error — never a global flag.
type importStateStoreSpy struct {
	state.StateStore
	mu         sync.Mutex
	saves      int
	saveIfs    int
	deleteIfs  int
	deletes    int
	failWrites bool
	// fail-at-exact-call-index knobs + ordered conditional-write call logs.
	failSaveIfAt   int
	failDeleteIfAt int
	saveIfLog      []importStateSaveIfCall
	deleteIfLog    []importStateDeleteIfCall
}

func (s *importStateStoreSpy) Save(ctx context.Context, r state.StateRecord) error {
	s.mu.Lock()
	s.saves++
	fail := s.failWrites
	s.mu.Unlock()
	if fail {
		return fmt.Errorf("state write forbidden during validate")
	}
	return s.StateStore.Save(ctx, r)
}

func (s *importStateStoreSpy) SaveIf(ctx context.Context, exp []state.SlotExpectation, next state.StateRecord) error {
	s.mu.Lock()
	s.saveIfs++
	idx := s.saveIfs
	fail := s.failWrites || (s.failSaveIfAt > 0 && idx == s.failSaveIfAt)
	s.saveIfLog = append(s.saveIfLog, importStateSaveIfCall{
		idx: idx, expectations: append([]state.SlotExpectation(nil), exp...), next: cloneStateRecord(next),
	})
	s.mu.Unlock()
	if fail {
		return fmt.Errorf("%w (saveif call %d)", errImportSimulatedSaveIf, idx)
	}
	return s.StateStore.SaveIf(ctx, exp, next)
}

func (s *importStateStoreSpy) DeleteIf(ctx context.Context, exp state.SlotExpectation) (bool, error) {
	s.mu.Lock()
	s.deleteIfs++
	idx := s.deleteIfs
	fail := s.failWrites || (s.failDeleteIfAt > 0 && idx == s.failDeleteIfAt)
	s.deleteIfLog = append(s.deleteIfLog, importStateDeleteIfCall{idx: idx, exp: exp})
	s.mu.Unlock()
	if fail {
		return false, fmt.Errorf("%w (deleteif call %d)", errImportSimulatedDeleteIf, idx)
	}
	return s.StateStore.DeleteIf(ctx, exp)
}

func (s *importStateStoreSpy) Delete(ctx context.Context, id identity.Quadruple, kind string) error {
	s.mu.Lock()
	s.deletes++
	fail := s.failWrites
	s.mu.Unlock()
	if fail {
		return fmt.Errorf("state write forbidden during validate")
	}
	return s.StateStore.Delete(ctx, id, kind)
}

func (s *importStateStoreSpy) stateCounts() (saves, saveIfs, deleteIfs, deletes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves, s.saveIfs, s.deleteIfs, s.deletes
}

func (s *importStateStoreSpy) setFailWrites(fail bool) {
	s.mu.Lock()
	s.failWrites = fail
	s.mu.Unlock()
}

// setFailSaveIfAt arms the spy to fail the SaveIf call at exactly this 1-based
// index with a genuine backend error (0 disarms).
func (s *importStateStoreSpy) setFailSaveIfAt(idx int) {
	s.mu.Lock()
	s.failSaveIfAt = idx
	s.mu.Unlock()
}

// setFailDeleteIfAt arms the spy to fail the DeleteIf call at exactly this
// 1-based index with a genuine backend error (0 disarms).
func (s *importStateStoreSpy) setFailDeleteIfAt(idx int) {
	s.mu.Lock()
	s.failDeleteIfAt = idx
	s.mu.Unlock()
}

// saveIfLogSnapshot returns a copy of every recorded SaveIf invocation in
// call order.
func (s *importStateStoreSpy) saveIfLogSnapshot() []importStateSaveIfCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]importStateSaveIfCall(nil), s.saveIfLog...)
}

// deleteIfLogSnapshot returns a copy of every recorded DeleteIf invocation in
// call order.
func (s *importStateStoreSpy) deleteIfLogSnapshot() []importStateDeleteIfCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]importStateDeleteIfCall(nil), s.deleteIfLog...)
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
	proposals *importStateStoreSpy
	store     *importSkillStoreSpy
	reg       agentcfg.RetirementRegistry
	cap       *fakeCapability
	clock     *fakeClock
	sealer    agentcfgprotocol.UserSkillImportProposalSealer
}

// newImportFixture builds the production-posture fixture: both signed-reach
// gates wired, real importer + localdb store + statestore registry +
// in-memory artifact store + write-counting StateStore, the deterministic dev
// token sealer, and the default capability snapshot.
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
		imp: imp, artifacts: artifactStore,
		proposals: &importStateStoreSpy{StateStore: proposals},
		store:     &importSkillStoreSpy{SkillStore: newSkills(t)},
		reg:       rr, cap: cap, clock: clock,
		sealer: importTestSealer(t),
	}
	mutate(fx)
	svc, err := agentcfgprotocol.NewUserSkillImportService(imp, artifactStore, fx.sealer, fx.proposals, fx.store, rr, cap,
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
		_ = fx.proposals.Close(context.Background())
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

// validateReq builds the first-phase request for the fixture package. The
// identity and effective agent ride the request ctx (see importCtx), not the
// wire request, which carries only the artifact id (agent is the fixture's
// fixed testAgentID).
func (fx *importFixture) validateReq(artifactID string) agentcfgprotocol.UserSkillImportValidateRequest {
	return agentcfgprotocol.UserSkillImportValidateRequest{ArtifactID: artifactID, AgentID: testAgentID}
}

// commitReqFromValidate echoes the proposal inputs the way the caller would.
// The identity rides the request ctx, not the wire request, and the agent is
// the fixture's fixed testAgentID.
func commitReqFromValidate(validate agentcfgprotocol.UserSkillImportValidateResponse, replace bool) agentcfgprotocol.UserSkillImportCommitRequest {
	return agentcfgprotocol.UserSkillImportCommitRequest{
		ProposalToken:       validate.ProposalToken,
		AgentID:             testAgentID,
		Name:                validate.Review.Name,
		ReviewedPackageHash: validate.PackageHash,
		ExpectedContentHash: validate.ExpectedContentHash,
		Replace:             replace,
	}
}

// validateCommit is the happy-path helper: upload, validate, commit. The
// effective agent is the fixture's fixed testAgentID.
func (fx *importFixture) validateCommit(t *testing.T, id identity.Identity, entries map[string]string, replace bool) (agentcfgprotocol.UserSkillImportValidateResponse, agentcfgprotocol.UserSkillImportCommitResponse, error) {
	t.Helper()
	artifactID := fx.uploadPackage(t, id, entries)
	ctx := importCtx(id, testAgentID)
	validated, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
	if err != nil {
		return agentcfgprotocol.UserSkillImportValidateResponse{}, agentcfgprotocol.UserSkillImportCommitResponse{}, err
	}
	committed, err := fx.svc.Commit(ctx, commitReqFromValidate(validated, replace))
	return validated, committed, err
}

// commitKind derives the durable commit-ledger slot kind of a proposal token
// (SHA-256 of the token bytes — the same derivation the service uses).
func (fx *importFixture) commitKind(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "agentcfg.user_skill_import.commit." + hex.EncodeToString(sum[:])
}

// writeLedgerRecord installs a raw commit-ledger JSON payload at a token's
// derived slot. Tests use it to deterministically construct the mid-flight
// (committing) and terminal (committed) ledger states the state machine must
// resume or refuse.
func (fx *importFixture) writeLedgerRecord(t *testing.T, q identity.Quadruple, token string, payload map[string]any) {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode ledger record: %v", err)
	}
	rec := state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: fx.commitKind(token), Bytes: b}
	if err := fx.proposals.Save(context.Background(), rec); err != nil {
		t.Fatalf("save ledger record: %v", err)
	}
}

// ledgerRecord decodes the durable commit-ledger slot of a token so tests can
// assert the exact slot contents (phase, written hash, receipt presence).
func (fx *importFixture) ledgerRecord(t *testing.T, q identity.Quadruple, token string) map[string]any {
	t.Helper()
	rec, err := fx.proposals.Load(context.Background(), q, fx.commitKind(token))
	if err != nil {
		t.Fatalf("load ledger: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Bytes, &m); err != nil {
		t.Fatalf("decode ledger: %v", err)
	}
	return m
}

// assertNoInstalledWinner asserts the target key holds NO installed package
// and NO legacy ScopeUser membership row — the atomic unit IS the membership,
// so a failed transactional put must leave neither (no orphan body, no orphan
// membership).
func (fx *importFixture) assertNoInstalledWinner(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	q := importQuad(importUserA)
	if _, err := fx.store.GetInstalledPackage(ctx, q, testAgentID, name); !errors.Is(err, skills.ErrInstalledPackageNotFound) {
		t.Fatalf("installed winner at %q must be absent: err=%v", name, err)
	}
	legacy, err := fx.store.List(ctx, q, skills.ListFilter{Scope: skills.ScopeUser, AgentID: testAgentID})
	if err != nil {
		t.Fatalf("legacy membership list: %v", err)
	}
	for _, sk := range legacy {
		if sk.Name == name {
			t.Fatalf("orphan membership row survived: %+v", sk)
		}
	}
}

// tamperLedger rewrites the durable commit-ledger slot's JSON (used to
// exercise the committing-resume path).
func (fx *importFixture) tamperLedger(t *testing.T, q identity.Quadruple, token string, mutate func(map[string]any)) {
	t.Helper()
	kind := fx.commitKind(token)
	rec, err := fx.proposals.Load(context.Background(), q, kind)
	if err != nil {
		t.Fatalf("load commit ledger for tamper: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Bytes, &m); err != nil {
		t.Fatalf("decode commit ledger for tamper: %v", err)
	}
	mutate(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-encode commit ledger for tamper: %v", err)
	}
	if err := fx.proposals.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: b}); err != nil {
		t.Fatalf("save tampered commit ledger: %v", err)
	}
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

// assertZeroStateWrites asserts the validate contract: no StateStore write
// (Save / SaveIf / DeleteIf / Delete) of any kind happened — validate seals
// the review into the token instead of persisting a proposal.
func (fx *importFixture) assertZeroStateWrites(t *testing.T) {
	t.Helper()
	saves, saveIfs, deleteIfs, deletes := fx.proposals.stateCounts()
	if saves != 0 || saveIfs != 0 || deleteIfs != 0 || deletes != 0 {
		t.Fatalf("validate performed a durable state write: saves=%d saveIfs=%d deleteIfs=%d deletes=%d",
			saves, saveIfs, deleteIfs, deletes)
	}
}

// ---------------------------------------------------------------------------
// Constructor
// ---------------------------------------------------------------------------

func TestUserSkillImport_NewService_MissingDependencyFailsLoud(t *testing.T) {
	ctx := context.Background()
	artifactStore, _ := artifactsinmem.New(config.ArtifactsConfig{})
	defer artifactStore.Close(ctx)
	imp, _ := importer.New(importer.Deps{Store: artifactStore})
	proposals, _ := stateinmem.New(config.StateConfig{Driver: "inmem"})
	defer proposals.Close(ctx)
	reg := newRegistry(t)
	rr, ok := reg.(agentcfg.RetirementRegistry)
	if !ok {
		t.Fatalf("registry does not implement RetirementRegistry")
	}
	cap := &fakeCapability{policy: defaultImportCapability()}
	sealer := importTestSealer(t)

	if _, err := agentcfgprotocol.NewUserSkillImportService(nil, artifactStore, sealer, proposals, nil, rr, cap); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportMisconfigured) {
		t.Fatalf("nil importer err=%v, want ErrUserSkillImportMisconfigured", err)
	}
	if _, err := agentcfgprotocol.NewUserSkillImportService(imp, nil, sealer, proposals, nil, rr, cap); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportMisconfigured) {
		t.Fatalf("nil artifact store err=%v, want ErrUserSkillImportMisconfigured", err)
	}
	if _, err := agentcfgprotocol.NewUserSkillImportService(imp, artifactStore, nil, proposals, nil, rr, cap); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportMisconfigured) {
		t.Fatalf("nil sealer err=%v, want ErrUserSkillImportMisconfigured", err)
	}
	if _, err := agentcfgprotocol.NewUserSkillImportService(imp, artifactStore, sealer, nil, nil, rr, cap); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportMisconfigured) {
		t.Fatalf("nil commit ledger err=%v, want ErrUserSkillImportMisconfigured", err)
	}
	if _, err := agentcfgprotocol.NewUserSkillImportService(imp, artifactStore, sealer, proposals, nil, rr, cap); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportMisconfigured) {
		t.Fatalf("nil skill store err=%v, want ErrUserSkillImportMisconfigured", err)
	}
	if _, err := agentcfgprotocol.NewUserSkillImportService(imp, artifactStore, sealer, proposals, fxStore(t), nil, cap); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportMisconfigured) {
		t.Fatalf("nil registry err=%v, want ErrUserSkillImportMisconfigured", err)
	}
	if _, err := agentcfgprotocol.NewUserSkillImportService(imp, artifactStore, sealer, proposals, fxStore(t), rr, nil); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportMisconfigured) {
		t.Fatalf("nil capability err=%v, want ErrUserSkillImportMisconfigured", err)
	}
}

func fxStore(t *testing.T) skills.SkillStore { return newSkills(t) }

// ---------------------------------------------------------------------------
// Validate: ZERO writes (skill + state), closed review, warnings, sealed token
// ---------------------------------------------------------------------------

func TestUserSkillImport_Validate_ZeroMutations(t *testing.T) {
	fx := newImportFixture(t)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	ctx := importCtx(importUserA, testAgentID)

	resp, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}

	// Closed review shape: canonical name, reviewed hash, stored content
	// hash, trigger, steps, support manifest, no authority fields.
	if resp.ProposalToken == "" {
		t.Fatalf("validate returned no proposal token")
	}
	// The token is the base64url of the sealed envelope: valid base64url
	// and round-trip decodable.
	if _, err := base64.RawURLEncoding.Strict().DecodeString(resp.ProposalToken); err != nil {
		t.Fatalf("proposal token is not valid base64url: %v", err)
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

	// ZERO StateStore write: the review lives inside the sealed token, not
	// in a durable proposal ledger.
	fx.assertZeroStateWrites(t)

	// ZERO agent-config membership mutation: the caller still has no user
	// revision.
	ug, err := fx.admin.UserGet(ctx, prototypes.AgentConfigUserGetRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil {
		t.Fatalf("user get: %v", err)
	}
	if ug.Set {
		t.Fatalf("validate wrote a user-scope config revision")
	}
}

// TestUserSkillImport_Validate_WriteCountingStateStoreRefusesWrites asserts
// the P1 contract with a state wrapper that FAILS any Save / SaveIf /
// DeleteIf / Delete attempt during validate: validate must not persist a
// proposal-ledger record (the review is sealed into the stateless token).
func TestUserSkillImport_Validate_WriteCountingStateStoreRefusesWrites(t *testing.T) {
	fx := newImportFixture(t)
	fx.proposals.setFailWrites(true)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	ctx := importCtx(importUserA, testAgentID)

	resp, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if resp.ProposalToken == "" {
		t.Fatalf("validate returned no proposal token")
	}
	// All four mutation surfaces stayed at zero.
	fx.assertZeroStateWrites(t)
	fx.assertZeroMutations(t)
}

// TestUserSkillImport_Validate_SealedTokensAreFresh asserts every validate
// seals a FRESH token (a fresh claims nonce + sealer nonce): two validates of
// the SAME package under the SAME identity must never produce the same token,
// and the closed review is identical.
func TestUserSkillImport_Validate_SealedTokensAreFresh(t *testing.T) {
	fx := newImportFixture(t)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	ctx := importCtx(importUserA, testAgentID)

	first, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
	if err != nil {
		t.Fatalf("validate 1: %v", err)
	}
	second, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
	if err != nil {
		t.Fatalf("validate 2: %v", err)
	}
	if first.ProposalToken == second.ProposalToken {
		t.Fatalf("two validates of the same package produced the same token (nonce reuse)")
	}
	if first.PackageHash != second.PackageHash || first.Review.Name != second.Review.Name {
		t.Fatalf("re-validated review drifted: first=%+v second=%+v", first.Review, second.Review)
	}
	fx.assertZeroStateWrites(t)
	fx.assertZeroMutations(t)
}

func TestUserSkillImport_Validate_WarningsForNonVisibleRequirements(t *testing.T) {
	fx := newImportFixture(t)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importAnnotatedMD})
	ctx := importCtx(importUserA, testAgentID)

	resp, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	// tool-x and ns-x are outside the permitted snapshot: two non-fatal
	// warnings, never a grant, never a refusal.
	if len(resp.Warnings) != 2 {
		t.Fatalf("warnings = %+v, want the two non-visible requirements", resp.Warnings)
	}
	fx.assertZeroMutations(t)
	fx.assertZeroStateWrites(t)
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
		_, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
		if !errors.Is(err, importer.ErrPackageFrontmatterDisallowed) {
			t.Fatalf("frontmatter %q err=%v, want ErrPackageFrontmatterDisallowed", keyValue, err)
		}
		if !errors.Is(err, agentcfgprotocol.ErrUserSkillImportPackageInvalid) {
			t.Fatalf("frontmatter %q err=%v, want wrapped ErrUserSkillImportPackageInvalid", keyValue, err)
		}
	}
	fx.assertZeroMutations(t)
	fx.assertZeroStateWrites(t)
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
			_, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
			if err == nil || !errors.Is(err, c.want) {
				t.Fatalf("err=%v, want errors.Is %v", err, c.want)
			}
			if !errors.Is(err, agentcfgprotocol.ErrUserSkillImportPackageInvalid) {
				t.Fatalf("err=%v, want wrapped ErrUserSkillImportPackageInvalid", err)
			}
		})
	}
	fx.assertZeroMutations(t)
	fx.assertZeroStateWrites(t)
}

func TestUserSkillImport_Validate_ArtifactNotCallerOwned(t *testing.T) {
	fx := newImportFixture(t)
	// Uploaded under user A's triple.
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	// User B (same tenant, same agent) cannot validate it: the read under
	// B's exact triple does not resolve — non-oracular not-found.
	userB := identity.Identity{TenantID: "t", UserID: "v", SessionID: "s1"}
	ctxB := importCtx(userB, testAgentID)
	if _, err := fx.svc.Validate(ctxB, fx.validateReq(artifactID)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportArtifactNotFound) {
		t.Fatalf("cross-user validate err=%v, want ErrUserSkillImportArtifactNotFound", err)
	}
	// A guessed / erased id is the SAME typed outcome — no enumeration.
	if _, err := fx.svc.Validate(ctxB, fx.validateReq("guessed-id")); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportArtifactNotFound) {
		t.Fatalf("guessed id err=%v, want ErrUserSkillImportArtifactNotFound", err)
	}
	// Cross-tenant too.
	userOther := identity.Identity{TenantID: "other", UserID: "u", SessionID: "s1"}
	ctxOther := importCtx(userOther, testAgentID)
	if _, err := fx.svc.Validate(ctxOther, fx.validateReq(artifactID)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportArtifactNotFound) {
		t.Fatalf("cross-tenant validate err=%v, want ErrUserSkillImportArtifactNotFound", err)
	}
	fx.assertZeroMutations(t)
	fx.assertZeroStateWrites(t)
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
	validated, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(validated.Review.SupportFiles) != 2 {
		t.Fatalf("support manifest review = %+v, want 2 entries", validated.Review.SupportFiles)
	}
	committed, err := fx.svc.Commit(ctx, commitReqFromValidate(validated, false))
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

	// The token-derived commit ledger records the exact terminal receipt.
	ledgerRec, err := fx.proposals.Load(context.Background(), importQuad(importUserA), fx.commitKind(validated.ProposalToken))
	if err != nil {
		t.Fatalf("commit ledger not durably recorded: %v", err)
	}
	var ledger map[string]any
	if err := json.Unmarshal(ledgerRec.Bytes, &ledger); err != nil {
		t.Fatalf("decode commit ledger: %v", err)
	}
	if ledger["phase"] != "committed" {
		t.Fatalf("commit ledger phase = %v, want committed", ledger["phase"])
	}
	if _, ok := ledger["receipt"]; !ok {
		t.Fatalf("commit ledger lacks the exact receipt")
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
	validated, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	req := commitReqFromValidate(validated, false)

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
	validated, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	req := commitReqFromValidate(validated, false)
	if _, err := fx.svc.Commit(ctx, req); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Simulate the mid-flight window: the atomic write landed but the
	// terminal receipt was never recorded (the token-derived ledger slot
	// still says "committing", no receipt).
	fx.tamperLedger(t, importQuad(importUserA), validated.ProposalToken, func(m map[string]any) {
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
	v1, err := fx.svc.Validate(ctx, fx.validateReq(artifactV1))
	if err != nil {
		t.Fatalf("validate v1: %v", err)
	}
	if _, err := fx.svc.Commit(ctx, commitReqFromValidate(v1, false)); err != nil {
		t.Fatalf("commit v1: %v", err)
	}

	// Version 2: same canonical name, different body → different hash.
	mdV2 := "---\nname: demo-skill\ntrigger: when asked about the demo\n---\nA changed demo body.\n\n## Steps\n- do the thing\n- also this\n"
	artifactV2 := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": mdV2})
	v2, err := fx.svc.Validate(ctx, fx.validateReq(artifactV2))
	if err != nil {
		t.Fatalf("validate v2: %v", err)
	}
	if v2.PackageHash == v1.PackageHash {
		t.Fatalf("fixture versions must differ")
	}

	// Without explicit consent the different winner is refused BEFORE any
	// write.
	if _, err := fx.svc.Commit(ctx, commitReqFromValidate(v2, false)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportReplaceRequired) {
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
	replaced, err := fx.svc.Commit(ctx, commitReqFromValidate(v2, true))
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
// Commit refusal matrix: stale / foreign / expired / tampered / moved / revoked
// ---------------------------------------------------------------------------

func TestUserSkillImport_Commit_ExpiredProposalRefused(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	// The review window elapses.
	fx.clock.advance(25 * time.Hour)
	if _, err := fx.svc.Commit(ctx, commitReqFromValidate(validated, false)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportExpired) {
		t.Fatalf("expired commit err=%v, want ErrUserSkillImportExpired", err)
	}
	fx.assertZeroMutations(t)
	fx.assertZeroStateWrites(t)
}

func TestUserSkillImport_Commit_MovedConfigBaseRefused(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
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
	if _, err := fx.svc.Commit(ctx, commitReqFromValidate(validated, false)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportConfigMoved) {
		t.Fatalf("moved-base commit err=%v, want ErrUserSkillImportConfigMoved", err)
	}
	fx.assertZeroMutations(t)
	fx.assertZeroStateWrites(t)
}

func TestUserSkillImport_Commit_PolicyRevocationRefused(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	// The capability snapshot is revoked between validate and commit.
	fx.cap.set(agentcfgprotocol.UserSkillImportPolicy{ID: "harbor.user-skill-import", Version: "1", PermittedTools: []string{"tool-b"}})
	if _, err := fx.svc.Commit(ctx, commitReqFromValidate(validated, false)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportPolicyRevoked) {
		t.Fatalf("revoked-policy commit err=%v, want ErrUserSkillImportPolicyRevoked", err)
	}
	fx.assertZeroMutations(t)
	fx.assertZeroStateWrites(t)
}

func TestUserSkillImport_Commit_LostReachRefused(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	req := commitReqFromValidate(validated, false)

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
	fx.assertZeroStateWrites(t)
}

func TestUserSkillImport_Commit_HashNameConfigMismatchRefused(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	base := commitReqFromValidate(validated, false)

	// A different reviewed hash is a changed review.
	wrongHash := base
	wrongHash.ReviewedPackageHash = "v1:" + strings.Repeat("ab", 32)
	if _, err := fx.svc.Commit(ctx, wrongHash); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportHashMismatch) {
		t.Fatalf("wrong hash err=%v, want ErrUserSkillImportHashMismatch", err)
	}
	// A different reviewed name is a foreign/stale token.
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
	// An unknown token (valid base64url over an envelope the sealer
	// refuses).
	unknown := base
	unknown.ProposalToken = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	if _, err := fx.svc.Commit(ctx, unknown); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportProposalInvalid) {
		t.Fatalf("unknown token err=%v, want ErrUserSkillImportProposalInvalid", err)
	}
	fx.assertZeroMutations(t)
	fx.assertZeroStateWrites(t)
}

// TestUserSkillImport_Commit_TokenTamperRefused exercises every stale token
// form (bit-flipped ciphertext, oversize, malformed base64, sealed garbage)
// — each returns the SAME typed proposal error before any write: the sealer
// and the strict claims decoder refuse without an oracle.
func TestUserSkillImport_Commit_TokenTamperRefused(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	req := commitReqFromValidate(validated, false)

	// One flipped character in the base64url token: AEAD authentication
	// must refuse — the same typed error as every other stale form (no
	// oracle), before any write.
	tampered := validated.ProposalToken
	last := tampered[len(tampered)-1]
	replacement := byte('A')
	if last == 'A' {
		replacement = 'B'
	}
	tampered = tampered[:len(tampered)-1] + string(replacement)
	bitFlip := req
	bitFlip.ProposalToken = tampered
	if _, err := fx.svc.Commit(ctx, bitFlip); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportProposalInvalid) {
		t.Fatalf("tampered-token commit err=%v, want ErrUserSkillImportProposalInvalid", err)
	}
	fx.assertZeroMutations(t)
	fx.assertZeroStateWrites(t)

	// Oversize token: refused on length before any decode.
	oversize := req
	oversize.ProposalToken = strings.Repeat("A", 1<<24)
	if _, err := fx.svc.Commit(ctx, oversize); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportProposalInvalid) {
		t.Fatalf("oversize-token commit err=%v, want ErrUserSkillImportProposalInvalid", err)
	}
	fx.assertZeroMutations(t)
	fx.assertZeroStateWrites(t)

	// Malformed base64: refused before any decode.
	badBase64 := req
	badBase64.ProposalToken = "!!!not-base64url!!!"
	if _, err := fx.svc.Commit(ctx, badBase64); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportProposalInvalid) {
		t.Fatalf("malformed-base64 commit err=%v, want ErrUserSkillImportProposalInvalid", err)
	}
	fx.assertZeroMutations(t)
	fx.assertZeroStateWrites(t)

	// Sealed garbage: valid base64url over an envelope the sealer refuses
	// (too short / bad version / bad tag).
	garbage := req
	garbage.ProposalToken = base64.RawURLEncoding.EncodeToString([]byte("garbage-envelope-bytes"))
	if _, err := fx.svc.Commit(ctx, garbage); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportProposalInvalid) {
		t.Fatalf("sealed-garbage commit err=%v, want ErrUserSkillImportProposalInvalid", err)
	}
	fx.assertZeroMutations(t)
	fx.assertZeroStateWrites(t)

	// Malformed claims: crafted JSON sealed with the same dev sealer must
	// be refused by the strict decoder (unknown field / unsupported schema
	// version / zero nonce / trailing data) — the SAME typed error, before
	// any write.
	seal := func(payload string) string {
		env, err := fx.sealer.Seal([]byte(payload))
		if err != nil {
			t.Fatalf("seal crafted claims: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(env)
	}
	for _, payload := range []string{
		`{"schema_version":1,"bogus":true}`,
		`{"schema_version":99}`,
		`{"schema_version":1,"nonce":"AAAAAAAAAAAAAAAAAAAAAA"}`,
		`{"schema_version":1}` + " trailing",
	} {
		bad := req
		bad.ProposalToken = seal(payload)
		if _, err := fx.svc.Commit(ctx, bad); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportProposalInvalid) {
			t.Fatalf("malformed-claims commit err=%v, want ErrUserSkillImportProposalInvalid (payload %q)", err, payload)
		}
		fx.assertZeroMutations(t)
		fx.assertZeroStateWrites(t)
	}
}

func TestUserSkillImport_Commit_ForeignIdentityRefused(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	req := commitReqFromValidate(validated, false)

	// A different USER with the same token: the sealed actor must equal
	// the caller's exact triple.
	userB := identity.Identity{TenantID: "t", UserID: "v", SessionID: "s1"}
	ctxB := importCtx(userB, testAgentID)
	if _, err := fx.svc.Commit(ctxB, req); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportProposalInvalid) {
		t.Fatalf("cross-user commit err=%v, want ErrUserSkillImportProposalInvalid", err)
	}
	// A different SESSION of the same user: the token's actor triple must
	// equal the caller's exact triple.
	userASess2 := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s2"}
	ctxS2 := importCtx(userASess2, testAgentID)
	if _, err := fx.svc.Commit(ctxS2, req); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportProposalInvalid) {
		t.Fatalf("cross-session commit err=%v, want ErrUserSkillImportProposalInvalid", err)
	}
	// A different AGENT with the same token: reach is signed for the OTHER
	// agent, so the reach gate passes and the claims-binding check refuses
	// (the token is bound to testAgentID).
	ctxOtherAgent := importCtx(importUserA, "agent-other")
	crossAgent := req
	crossAgent.AgentID = "agent-other"
	if _, err := fx.svc.Commit(ctxOtherAgent, crossAgent); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportProposalInvalid) {
		t.Fatalf("cross-agent commit err=%v, want ErrUserSkillImportProposalInvalid", err)
	}
	fx.assertZeroMutations(t)
	fx.assertZeroStateWrites(t)
}

func TestUserSkillImport_Commit_ChangedArtifactRefused(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	// The caller erases the source artifact after validation: the commit
	// re-resolution must refuse (non-oracular not-found), never install.
	if _, err := fx.artifacts.Delete(ctx, artifacts.ArtifactScope{TenantID: "t", UserID: "u", SessionID: "s"}, artifactID); err != nil {
		t.Fatalf("delete artifact: %v", err)
	}
	if _, err := fx.svc.Commit(ctx, commitReqFromValidate(validated, false)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportArtifactNotFound) {
		t.Fatalf("erased-artifact commit err=%v, want ErrUserSkillImportArtifactNotFound", err)
	}
	fx.assertZeroMutations(t)
	fx.assertZeroStateWrites(t)
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
	if _, err := fx.svc.Validate(ctx, fx.validateReq(artifactID)); !errors.Is(err, agentcfg.ErrAgentRetired) {
		t.Fatalf("retired validate err=%v, want ErrAgentRetired", err)
	}
	if _, err := fx.svc.Commit(ctx, agentcfgprotocol.UserSkillImportCommitRequest{
		ProposalToken: "01ARZ3NDEKTSV4RRFFQ69G5FAV", AgentID: testAgentID, Name: "x",
		ReviewedPackageHash: "v1:" + strings.Repeat("ab", 32),
	}); !errors.Is(err, agentcfg.ErrAgentRetired) {
		t.Fatalf("retired commit err=%v, want ErrAgentRetired", err)
	}
	fx.assertZeroMutations(t)
	fx.assertZeroStateWrites(t)
}

func TestUserSkillImport_UnwiredAgentReachFailsClosed(t *testing.T) {
	// A service constructed WITHOUT the effective-agent gate serves nothing:
	// an unwired gate is an honest "cannot verify reach", never a silent
	// widening.
	fx := newImportFixture(t)
	artifactStore := fx.artifacts
	imp := fx.imp
	proposals := fx.proposals
	svc, err := agentcfgprotocol.NewUserSkillImportService(imp, artifactStore, fx.sealer, proposals, fx.store, fx.reg, fx.cap)
	if err != nil {
		t.Fatalf("NewUserSkillImportService (no reach gates): %v", err)
	}
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	if _, err := svc.Validate(ctx, fx.validateReq(artifactID)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportAgentReachDenied) {
		t.Fatalf("unwired-gate validate err=%v, want ErrUserSkillImportAgentReachDenied", err)
	}
	fx.assertZeroMutations(t)
	fx.assertZeroStateWrites(t)
}

// ---------------------------------------------------------------------------
// HA-66 boot-owned guard
// ---------------------------------------------------------------------------

func TestUserSkillImport_BootOwnedNameRefusedEverywhere(t *testing.T) {
	fx := newImportFixture(t)
	owner := &fakeBootOwner{names: map[string]struct{}{"demo-skill": {}}}
	guarded := agentcfgprotocol.WithBootOwnership(importCtx(importUserA, testAgentID), owner)

	// Validate refuses BEFORE a token exists.
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	if _, err := fx.svc.Validate(guarded, fx.validateReq(artifactID)); !errors.Is(err, agentcfgprotocol.ErrBootPackOwned) {
		t.Fatalf("boot-owned validate err=%v, want ErrBootPackOwned", err)
	}
	fx.assertZeroMutations(t)
	fx.assertZeroStateWrites(t)

	// Commit refuses on EVERY path even when the token was created before
	// the baseline bound the name.
	unguarded := importCtx(importUserA, testAgentID)
	validated, err := fx.svc.Validate(unguarded, fx.validateReq(artifactID))
	if err != nil {
		t.Fatalf("validate (unguarded): %v", err)
	}
	req := commitReqFromValidate(validated, false)
	if _, err := fx.svc.Commit(guarded, req); !errors.Is(err, agentcfgprotocol.ErrBootPackOwned) {
		t.Fatalf("boot-owned commit err=%v, want ErrBootPackOwned", err)
	}
	fx.assertZeroMutations(t)
	fx.assertZeroStateWrites(t)
}

// ---------------------------------------------------------------------------
// Cross-user / cross-session isolation
// ---------------------------------------------------------------------------

func TestUserSkillImport_CrossUserIsolation(t *testing.T) {
	fx := newImportFixture(t)
	userA := importUserA
	userB := identity.Identity{TenantID: "t", UserID: "v", SessionID: "s1"}

	// A commits its own skill.
	if _, _, err := fx.validateCommit(t, userA, map[string]string{"SKILL.md": importSkillMD}, false); err != nil {
		t.Fatalf("A validate+commit: %v", err)
	}
	// B's (tenant, user, agent, name) key has nothing — no leakage.
	ctxB := importCtx(userB, testAgentID)
	if _, err := fx.store.GetInstalledPackage(ctxB, importQuad(userB), testAgentID, "demo-skill"); !errors.Is(err, skills.ErrInstalledPackageNotFound) {
		t.Fatalf("B sees A's installed package: err=%v", err)
	}
	// B cannot validate A's artifact either (already covered above).
	if _, err := fx.svc.Validate(ctxB, fx.validateReq(fx.uploadPackage(t, userA, map[string]string{"SKILL.md": importSkillMD}))); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportArtifactNotFound) {
		t.Fatalf("B validating A's artifact err=%v, want not found", err)
	}
}

func TestUserSkillImport_CrossSessionIsolation(t *testing.T) {
	fx := newImportFixture(t)
	// Validate in session s, attempt the commit in session s2 of the SAME
	// user: the token's actor triple must equal the caller's exact triple —
	// the artifact is session-scoped and the review is session-bound.
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	userASess2 := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s2"}
	ctxS2 := importCtx(userASess2, testAgentID)
	if _, err := fx.svc.Commit(ctxS2, commitReqFromValidate(validated, false)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportProposalInvalid) {
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
	_, c1, err := fx.validateCommit(t, importUserA, map[string]string{"SKILL.md": mdV1}, false)
	if err != nil {
		t.Fatalf("commit v1: %v", err)
	}
	_, c2, err := fx.validateCommit(t, importUserA, map[string]string{"SKILL.md": mdV2}, true)
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
// SAME token: the absent-slot SaveIf CAS (first commit) plus the package CAS
// must produce exactly one put, and every caller either receives the terminal
// result or a typed refusal — never a double write, never a panic.
func TestUserSkillImport_ConcurrentSameProposal_OneWinner(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	req := commitReqFromValidate(validated, false)

	const n = 32
	var wg sync.WaitGroup
	errs := make([]error, n)
	results := make([]agentcfgprotocol.UserSkillImportCommitResponse, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = fx.svc.Commit(ctx, req)
		}(i)
	}
	wg.Wait()

	put, _, _, _, _, _ := fx.store.counts()
	if put != 1 {
		t.Fatalf("concurrent same-token commits wrote %d packages, want exactly 1", put)
	}
	successes := 0
	for i := range n {
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
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user := identity.Identity{TenantID: "t", UserID: fmt.Sprintf("user-%03d", i), SessionID: fmt.Sprintf("session-%03d", i)}
			md := fmt.Sprintf("---\nname: skill-%03d\ntrigger: when asked about %d\n---\nA skill for user %d.\n\n## Steps\n- do the thing\n", i, i, i)
			ctx := importCtx(user, testAgentID)
			artifactID := fx.uploadPackage(t, user, map[string]string{"SKILL.md": md})
			validated, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
			if err != nil {
				errs[i] = fmt.Errorf("validate: %w", err)
				return
			}
			committed, err := fx.svc.Commit(ctx, commitReqFromValidate(validated, false))
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
	for i := range n {
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

// ---------------------------------------------------------------------------
// Deterministic state-machine failure injection (the HA-61 P1 review surface)
// ---------------------------------------------------------------------------

// TestUserSkillImport_Commit_PutFailureCompensatesExactMarker asserts the
// failed-put compensation contract: when the ONE atomic PutInstalledPackage
// write fails AFTER the token-derived committing marker landed, the service
// conditionally deletes EXACTLY that marker (never an empty generation, never
// a different kind), leaves no package or membership winner, fails loud, and
// a retry performs exactly one clean atomic install (no orphan, no false
// success, no cross-proposal deletion).
func TestUserSkillImport_Commit_PutFailureCompensatesExactMarker(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	req := commitReqFromValidate(validated, false)
	q := importQuad(importUserA)
	kind := fx.commitKind(validated.ProposalToken)

	// The FIRST (and only) put attempt fails with a genuine backend error.
	fx.store.setFailPutInstalledAt(1)
	_, err = fx.svc.Commit(ctx, req)
	if err == nil {
		t.Fatalf("put-failure commit must fail loud")
	}
	if !strings.Contains(err.Error(), "put installed package") {
		t.Fatalf("put-failure error = %v, want the loud put wrap", err)
	}
	put, upsert, del, delAgent, delInstalled, restore := fx.store.counts()
	if put != 1 || upsert != 0 || del != 0 || delAgent != 0 || delInstalled != 0 || restore != 0 {
		t.Fatalf("put-failure mutation counters put=%d upsert=%d delete=%d deleteAgent=%d deleteInstalled=%d restore=%d",
			put, upsert, del, delAgent, delInstalled, restore)
	}
	// The attempted write was a create-only absent-condition put of the
	// reviewed unit — a different proposal's winner can never be targeted.
	putCalls := fx.store.putLogSnapshot()
	if len(putCalls) != 1 || putCalls[0].cond != (skills.InstalledPackageCondition{ExpectedAbsent: true}) ||
		putCalls[0].replace || putCalls[0].pkg.PackageHash != validated.PackageHash {
		t.Fatalf("failed put call = %+v, want one create-only absent-condition put of the reviewed unit", putCalls)
	}

	// The compensation ran EXACTLY ONCE and targeted ONLY the marker this
	// commit wrote: same identity, same token-derived kind, and the exact
	// committing-marker EventID.
	markerCalls := fx.proposals.saveIfLogSnapshot()
	if len(markerCalls) != 1 {
		t.Fatalf("saveif calls = %d, want exactly the one marker", len(markerCalls))
	}
	marker := markerCalls[0]
	if marker.expectations[0].Identity != q || marker.expectations[0].Kind != kind || marker.expectations[0].ExpectedEventID != "" {
		t.Fatalf("marker SaveIf expectation = %+v, want the absent slot under %s", marker.expectations[0], kind)
	}
	if marker.next.Identity != q || marker.next.Kind != kind {
		t.Fatalf("marker SaveIf next = %+v, want identity+kind under %s", marker.next, kind)
	}
	delCalls := fx.proposals.deleteIfLogSnapshot()
	if len(delCalls) != 1 {
		t.Fatalf("deleteif calls = %d, want exactly the one compensation", len(delCalls))
	}
	comp := delCalls[0]
	if comp.exp.Identity != q || comp.exp.Kind != kind || comp.exp.ExpectedEventID != marker.next.ID {
		t.Fatalf("compensation DeleteIf = %+v, want the exact marker %s under %s", comp.exp, marker.next.ID, kind)
	}

	// The slot was restored to the genuinely absent pre-operation state and
	// no package/membership winner exists anywhere.
	if _, err := fx.proposals.Load(context.Background(), q, kind); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("commit-ledger slot after compensation err=%v, want ErrNotFound (absent restored)", err)
	}
	fx.assertNoInstalledWinner(t, ctx, "demo-skill")

	// A retry is a clean fresh commit: exactly ONE further atomic put, a
	// single winner, the terminal committed receipt.
	fx.store.setFailPutInstalledAt(0)
	retried, err := fx.svc.Commit(ctx, req)
	if err != nil {
		t.Fatalf("retry commit: %v", err)
	}
	if retried.Replayed {
		t.Fatalf("retry after compensation must not be a replay")
	}
	if retried.Receipt.WrittenHash != validated.PackageHash {
		t.Fatalf("retry receipt hash %q != reviewed %q", retried.Receipt.WrittenHash, validated.PackageHash)
	}
	put, _, _, _, _, _ = fx.store.counts()
	if put != 2 {
		t.Fatalf("retry put count = %d, want 2 (one failed attempt + one clean install)", put)
	}
	winner, err := fx.store.GetInstalledPackage(ctx, q, testAgentID, "demo-skill")
	if err != nil || winner.PackageHash != validated.PackageHash {
		t.Fatalf("winner after retry err=%v hash=%q", err, winner.PackageHash)
	}
	ledger := fx.ledgerRecord(t, q, validated.ProposalToken)
	if ledger["phase"] != "committed" {
		t.Fatalf("ledger phase after retry = %v, want committed", ledger["phase"])
	}
	if _, ok := ledger["receipt"]; !ok {
		t.Fatalf("committed ledger lacks the exact receipt")
	}
}

// TestUserSkillImport_Commit_MarkerSaveIfBackendFailure_ZeroPutStableRefusal
// asserts the absent-slot committing-marker SaveIf failure branch: a GENUINE
// backend error (not ErrConditionFailed) surfaces loud and typed, performs
// zero package writes, leaves the ledger slot absent, and is stable across
// retries while the backend is down; once the backend recovers, a retry is a
// clean single atomic install.
func TestUserSkillImport_Commit_MarkerSaveIfBackendFailure_ZeroPutStableRefusal(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	req := commitReqFromValidate(validated, false)
	q := importQuad(importUserA)
	kind := fx.commitKind(validated.ProposalToken)

	// The very first SaveIf (the absent-slot committing marker) fails with a
	// genuine backend error; the second attempt fails identically at the
	// next call index while the backend is still down.
	fx.proposals.setFailSaveIfAt(1)
	for attempt := range 2 {
		_, commitErr := fx.svc.Commit(ctx, req)
		if commitErr == nil {
			t.Fatalf("marker SaveIf failure (attempt %d) must fail loud", attempt)
		}
		if !strings.Contains(commitErr.Error(), "mark commit committing") {
			t.Fatalf("marker SaveIf failure error = %v, want the loud committing-marker wrap", commitErr)
		}
		if errors.Is(commitErr, state.ErrConditionFailed) {
			t.Fatalf("a genuine backend error must not surface as ErrConditionFailed: %v", commitErr)
		}
		if errors.Is(commitErr, agentcfgprotocol.ErrUserSkillImportProposalInvalid) {
			t.Fatalf("a genuine backend error must not surface as ErrUserSkillImportProposalInvalid: %v", commitErr)
		}
		put, _, _, _, _, _ := fx.store.counts()
		if put != 0 {
			t.Fatalf("marker SaveIf failure performed a package write: put=%d", put)
		}
		// The slot stays genuinely absent — no marker ever landed.
		if _, err := fx.proposals.Load(context.Background(), q, kind); !errors.Is(err, state.ErrNotFound) {
			t.Fatalf("ledger slot after marker SaveIf failure err=%v, want ErrNotFound", err)
		}
		// Arm the next SaveIf call so the next attempt fails identically.
		_, saveIfs, _, _ := fx.proposals.stateCounts()
		fx.proposals.setFailSaveIfAt(saveIfs + 1)
	}

	// Every failed marker SaveIf used the exact token-derived kind and the
	// absent-slot generation.
	calls := fx.proposals.saveIfLogSnapshot()
	if len(calls) != 2 {
		t.Fatalf("saveif calls = %d, want 2", len(calls))
	}
	for i, c := range calls {
		if c.expectations[0].Identity != q || c.expectations[0].Kind != kind || c.expectations[0].ExpectedEventID != "" {
			t.Fatalf("saveif %d expectation = %+v, want the absent slot under %s", i, c.expectations[0], kind)
		}
		if c.next.Identity != q || c.next.Kind != kind {
			t.Fatalf("saveif %d next = %+v, want identity+kind under %s", i, c.next, kind)
		}
	}

	// Backend recovers: exactly one atomic install, terminal ledger.
	fx.proposals.setFailSaveIfAt(0)
	committed, err := fx.svc.Commit(ctx, req)
	if err != nil {
		t.Fatalf("retry commit after backend recovery: %v", err)
	}
	if committed.Replayed || committed.Receipt.WrittenHash != validated.PackageHash {
		t.Fatalf("recovered commit = %+v", committed)
	}
	put, _, _, _, _, _ := fx.store.counts()
	if put != 1 {
		t.Fatalf("recovered commit put = %d, want exactly 1", put)
	}
	winner, err := fx.store.GetInstalledPackage(ctx, q, testAgentID, "demo-skill")
	if err != nil || winner.PackageHash != validated.PackageHash {
		t.Fatalf("winner after recovery err=%v hash=%q", err, winner.PackageHash)
	}
	ledger := fx.ledgerRecord(t, q, validated.ProposalToken)
	if ledger["phase"] != "committed" {
		t.Fatalf("ledger phase after recovery = %v, want committed", ledger["phase"])
	}
}

// TestUserSkillImport_Commit_CommittedTransitionBackendFailure_RetryRecognizesWinner
// asserts the committed-transition SaveIf failure branch: the put lands but
// the terminal bookkeeping write fails with a genuine backend error — the
// first call is loud ("the package IS installed"), the ledger stays in
// "committing" with the exact written hash, and a retry recognizes the exact
// installed winner, records the terminal receipt, and never calls Put a
// second time.
func TestUserSkillImport_Commit_CommittedTransitionBackendFailure_RetryRecognizesWinner(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	req := commitReqFromValidate(validated, false)
	q := importQuad(importUserA)
	kind := fx.commitKind(validated.ProposalToken)

	// SaveIf #1 (committing marker) succeeds, the put lands, SaveIf #2 (the
	// committed transition) fails with a genuine backend error.
	fx.proposals.setFailSaveIfAt(2)
	_, err = fx.svc.Commit(ctx, req)
	if err == nil {
		t.Fatalf("committed-transition failure must fail loud")
	}
	if !strings.Contains(err.Error(), "record committed marker") || !strings.Contains(err.Error(), "the package IS installed") {
		t.Fatalf("committed-transition error = %v, want the loud installed-package wrap", err)
	}
	put, _, _, _, _, _ := fx.store.counts()
	if put != 1 {
		t.Fatalf("committed-transition failure put = %d, want exactly 1 (the write landed)", put)
	}
	// The exact installed winner exists.
	winner, err := fx.store.GetInstalledPackage(ctx, q, testAgentID, "demo-skill")
	if err != nil || winner.PackageHash != validated.PackageHash {
		t.Fatalf("winner err=%v hash=%q", err, winner.PackageHash)
	}
	// The ledger is still the mid-flight marker with the exact written hash.
	ledger := fx.ledgerRecord(t, q, validated.ProposalToken)
	if ledger["phase"] != "committing" || ledger["written_package_hash"] != validated.PackageHash {
		t.Fatalf("ledger after failed transition = %+v, want committing %s", ledger, validated.PackageHash)
	}
	if _, ok := ledger["receipt"]; ok {
		t.Fatalf("failed transition must not record a receipt: %+v", ledger)
	}

	// The SaveIf generations chain: marker (absent) → transition (marker
	// generation) — the failed call carried the exact marker expectation.
	calls := fx.proposals.saveIfLogSnapshot()
	if len(calls) != 2 {
		t.Fatalf("saveif calls = %d, want 2", len(calls))
	}
	marker := calls[0]
	if marker.expectations[0].Identity != q || marker.expectations[0].Kind != kind || marker.expectations[0].ExpectedEventID != "" {
		t.Fatalf("marker SaveIf expectation = %+v, want the absent slot under %s", marker.expectations[0], kind)
	}
	if calls[1].expectations[0].Identity != q || calls[1].expectations[0].Kind != kind || calls[1].expectations[0].ExpectedEventID != marker.next.ID {
		t.Fatalf("transition SaveIf expectation = %+v, want the exact marker generation", calls[1].expectations[0])
	}

	// Retry: recognizes the exact installed winner, records the terminal
	// state, returns the reviewed stable result, and never puts again.
	fx.proposals.setFailSaveIfAt(0)
	resumed, err := fx.svc.Commit(ctx, req)
	if err != nil {
		t.Fatalf("retry commit: %v", err)
	}
	if !resumed.Replayed {
		t.Fatalf("retry of a landed write must be recognized as a replay")
	}
	if resumed.Receipt.WrittenHash != validated.PackageHash || resumed.PackageHash != validated.PackageHash {
		t.Fatalf("retry terminal result = %+v", resumed)
	}
	put, _, _, _, _, _ = fx.store.counts()
	if put != 1 {
		t.Fatalf("retry performed a second package write: put=%d, want 1", put)
	}
	ledger = fx.ledgerRecord(t, q, validated.ProposalToken)
	if ledger["phase"] != "committed" {
		t.Fatalf("ledger after retry = %+v, want committed", ledger)
	}
	// The resume's terminal-record SaveIf carried the same marker generation.
	calls = fx.proposals.saveIfLogSnapshot()
	if len(calls) != 3 {
		t.Fatalf("saveif calls = %d, want 3", len(calls))
	}
	if calls[2].expectations[0].Kind != kind || calls[2].expectations[0].ExpectedEventID != marker.next.ID {
		t.Fatalf("resume SaveIf expectation = %+v, want the marker generation", calls[2].expectations[0])
	}
}

// TestUserSkillImport_ResumeCommitting_TerminalRecordBackendFailure_RetryConverges
// asserts the resume-committing terminal-record SaveIf failure branch: with
// the write landed and the ledger reverted to the mid-flight marker, a resume
// whose terminal-record SaveIf fails with a genuine backend error is loud and
// performs no second put; a later retry converges to the committed terminal
// receipt.
func TestUserSkillImport_ResumeCommitting_TerminalRecordBackendFailure_RetryConverges(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	req := commitReqFromValidate(validated, false)
	q := importQuad(importUserA)

	// Land a normal commit (put=1), then rewind the ledger to the mid-flight
	// committing marker (the write landed, the terminal receipt was lost).
	if _, err := fx.svc.Commit(ctx, req); err != nil {
		t.Fatalf("commit: %v", err)
	}
	fx.tamperLedger(t, q, validated.ProposalToken, func(m map[string]any) {
		m["phase"] = "committing"
		delete(m, "receipt")
	})

	// The resume's terminal-record SaveIf is the NEXT SaveIf call; fail it
	// with a genuine backend error.
	_, saveIfs, _, _ := fx.proposals.stateCounts()
	fx.proposals.setFailSaveIfAt(saveIfs + 1)
	_, err = fx.svc.Commit(ctx, req)
	if err == nil {
		t.Fatalf("resume terminal-record failure must fail loud")
	}
	if !strings.Contains(err.Error(), "record committed marker") || !strings.Contains(err.Error(), "the package IS installed") {
		t.Fatalf("resume terminal-record error = %v, want the loud installed-package wrap", err)
	}
	put, _, _, _, _, _ := fx.store.counts()
	if put != 1 {
		t.Fatalf("failed resume performed a second write: put=%d, want 1", put)
	}
	// The ledger is still the mid-flight marker.
	ledger := fx.ledgerRecord(t, q, validated.ProposalToken)
	if ledger["phase"] != "committing" {
		t.Fatalf("ledger after failed resume = %+v, want committing", ledger)
	}

	// A later retry converges: terminal committed receipt, still one put.
	fx.proposals.setFailSaveIfAt(0)
	resumed, err := fx.svc.Commit(ctx, req)
	if err != nil {
		t.Fatalf("converging retry: %v", err)
	}
	if !resumed.Replayed || resumed.Receipt.WrittenHash != validated.PackageHash {
		t.Fatalf("converging retry = %+v", resumed)
	}
	put, _, _, _, _, _ = fx.store.counts()
	if put != 1 {
		t.Fatalf("converging retry performed a second write: put=%d, want 1", put)
	}
	ledger = fx.ledgerRecord(t, q, validated.ProposalToken)
	if ledger["phase"] != "committed" {
		t.Fatalf("ledger after converging retry = %+v, want committed", ledger)
	}
	winner, err := fx.store.GetInstalledPackage(ctx, q, testAgentID, "demo-skill")
	if err != nil || winner.PackageHash != validated.PackageHash {
		t.Fatalf("winner err=%v hash=%q", err, winner.PackageHash)
	}
}

// TestUserSkillImport_LedgerConstruction_RefusalsZeroPut deterministically
// constructs the three durable-ledger states the state machine must
// resume/refuse and asserts each refusal performs ZERO package writes and
// mutates no ledger slot:
//
//   - a committing ledger whose WrittenPackageHash differs from the current
//     winner → ErrUserSkillImportConcurrentWinner (one winner; a receipt or
//     marker never overwrites another commit's winner);
//   - a committing ledger whose written package never landed (no winner) →
//     ErrUserSkillImportProposalInvalid — re-validate, never a silent
//     re-install under a stale marker;
//   - a committed ledger whose current winner differs from the receipt →
//     ErrUserSkillImportConcurrentWinner (a replay never serves a receipt
//     over a different winner).
func TestUserSkillImport_LedgerConstruction_RefusalsZeroPut(t *testing.T) {
	mdV2 := "---\nname: demo-skill\ntrigger: when asked about the demo\n---\nA changed demo body.\n\n## Steps\n- do the thing\n- also this\n"
	ctx := importCtx(importUserA, testAgentID)
	q := importQuad(importUserA)

	t.Run("committing ledger different winner", func(t *testing.T) {
		fx := newImportFixture(t)
		// v1 wins the target key.
		v1, _, err := fx.validateCommit(t, importUserA, map[string]string{"SKILL.md": importSkillMD}, false)
		if err != nil {
			t.Fatalf("commit v1: %v", err)
		}
		// v2 is reviewed but a different winner holds the key.
		artifactV2 := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": mdV2})
		v2, err := fx.svc.Validate(ctx, fx.validateReq(artifactV2))
		if err != nil {
			t.Fatalf("validate v2: %v", err)
		}
		if v2.PackageHash == v1.PackageHash {
			t.Fatalf("fixture versions must differ")
		}
		// A mid-flight marker claiming the v2 write, with v1 still the
		// winner: resume must refuse loudly, never overwrite.
		fx.writeLedgerRecord(t, q, v2.ProposalToken, map[string]any{
			"phase": "committing", "name": "demo-skill", "written_package_hash": v2.PackageHash,
		})
		putBefore, _, _, _, _, _ := fx.store.counts()
		if _, err := fx.svc.Commit(ctx, commitReqFromValidate(v2, false)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportConcurrentWinner) {
			t.Fatalf("committing-different-winner commit err=%v, want ErrUserSkillImportConcurrentWinner", err)
		}
		put, _, _, _, _, _ := fx.store.counts()
		if put != putBefore {
			t.Fatalf("refusal performed a package write: put=%d before=%d", put, putBefore)
		}
		if _, _, deleteIfs, deletes := fx.proposals.stateCounts(); deleteIfs != 0 || deletes != 0 {
			t.Fatalf("refusal mutated the ledger: deleteIfs=%d deletes=%d", deleteIfs, deletes)
		}
		// v1 is still the winner.
		winner, err := fx.store.GetInstalledPackage(ctx, q, testAgentID, "demo-skill")
		if err != nil || winner.PackageHash != v1.PackageHash {
			t.Fatalf("winner disturbed: err=%v hash=%q", err, winner.PackageHash)
		}
	})

	t.Run("committing ledger no winner", func(t *testing.T) {
		fx := newImportFixture(t)
		artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
		validated, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		// The mid-flight marker says the write happened, but no winner ever
		// landed (a failed put whose inline compensation also failed): the
		// caller must re-validate, never re-install under the stale marker.
		fx.writeLedgerRecord(t, q, validated.ProposalToken, map[string]any{
			"phase": "committing", "name": "demo-skill", "written_package_hash": validated.PackageHash,
		})
		putBefore, _, _, _, _, _ := fx.store.counts()
		if _, err := fx.svc.Commit(ctx, commitReqFromValidate(validated, false)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportProposalInvalid) {
			t.Fatalf("committing-no-winner commit err=%v, want ErrUserSkillImportProposalInvalid", err)
		}
		put, _, _, _, _, _ := fx.store.counts()
		if put != putBefore {
			t.Fatalf("refusal performed a package write: put=%d before=%d", put, putBefore)
		}
		// The stale marker is refused loudly, never deleted or re-installed.
		ledger := fx.ledgerRecord(t, q, validated.ProposalToken)
		if ledger["phase"] != "committing" {
			t.Fatalf("stale marker mutated: %+v", ledger)
		}
		fx.assertNoInstalledWinner(t, ctx, "demo-skill")
	})

	t.Run("committed ledger different winner", func(t *testing.T) {
		fx := newImportFixture(t)
		v1, _, err := fx.validateCommit(t, importUserA, map[string]string{"SKILL.md": importSkillMD}, false)
		if err != nil {
			t.Fatalf("commit v1: %v", err)
		}
		artifactV2 := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": mdV2})
		v2, err := fx.svc.Validate(ctx, fx.validateReq(artifactV2))
		if err != nil {
			t.Fatalf("validate v2: %v", err)
		}
		if v2.PackageHash == v1.PackageHash {
			t.Fatalf("fixture versions must differ")
		}
		// A terminal ledger whose receipt names v2 — but v1 still wins the
		// key: the receipt can never be replayed over a different winner.
		fx.writeLedgerRecord(t, q, v2.ProposalToken, map[string]any{
			"phase": "committed", "name": "demo-skill", "written_package_hash": v2.PackageHash,
			"receipt": map[string]any{
				"TenantID": "t", "UserID": "u", "AgentID": testAgentID, "Name": "demo-skill",
				"WrittenHash": v2.PackageHash, "WrittenVersion": "0.1.0",
			},
		})
		putBefore, _, _, _, _, _ := fx.store.counts()
		if _, err := fx.svc.Commit(ctx, commitReqFromValidate(v2, false)); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportConcurrentWinner) {
			t.Fatalf("committed-different-winner commit err=%v, want ErrUserSkillImportConcurrentWinner", err)
		}
		put, _, _, _, _, _ := fx.store.counts()
		if put != putBefore {
			t.Fatalf("refusal performed a package write: put=%d before=%d", put, putBefore)
		}
		winner, err := fx.store.GetInstalledPackage(ctx, q, testAgentID, "demo-skill")
		if err != nil || winner.PackageHash != v1.PackageHash {
			t.Fatalf("winner disturbed: err=%v hash=%q", err, winner.PackageHash)
		}
	})
}

// TestUserSkillImport_Commit_PutAndCompensationBothFail_JoinedLoudNoForeignMutation
// asserts the failed-compensation branch: when the atomic put fails AND the
// exact-receipt compensation DeleteIf also fails, the commit returns a JOINED
// loud error naming both failures, the committing marker stays intact at its
// exact generation (never deleted, never overwritten, no other proposal's
// slot touched), no winner exists, and a later retry refuses the stale marker
// rather than silently re-installing or deleting anything.
func TestUserSkillImport_Commit_PutAndCompensationBothFail_JoinedLoudNoForeignMutation(t *testing.T) {
	fx := newImportFixture(t)
	ctx := importCtx(importUserA, testAgentID)
	artifactID := fx.uploadPackage(t, importUserA, map[string]string{"SKILL.md": importSkillMD})
	validated, err := fx.svc.Validate(ctx, fx.validateReq(artifactID))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	req := commitReqFromValidate(validated, false)
	q := importQuad(importUserA)
	kind := fx.commitKind(validated.ProposalToken)

	fx.store.setFailPutInstalledAt(1)
	fx.proposals.setFailDeleteIfAt(1)
	_, err = fx.svc.Commit(ctx, req)
	if err == nil {
		t.Fatalf("put+compensation failure must fail loud")
	}
	// The joined error carries BOTH the put failure and the compensation
	// failure.
	if !errors.Is(err, errImportSimulatedPut) {
		t.Fatalf("joined error must expose the put failure: %v", err)
	}
	if !errors.Is(err, errImportSimulatedDeleteIf) {
		t.Fatalf("joined error must expose the compensation failure: %v", err)
	}
	if !strings.Contains(err.Error(), "put installed package") {
		t.Fatalf("joined error = %v, want the put wrap", err)
	}
	put, upsert, del, delAgent, delInstalled, restore := fx.store.counts()
	if put != 1 || upsert != 0 || del != 0 || delAgent != 0 || delInstalled != 0 || restore != 0 {
		t.Fatalf("mutation counters put=%d upsert=%d delete=%d deleteAgent=%d deleteInstalled=%d restore=%d",
			put, upsert, del, delAgent, delInstalled, restore)
	}

	// The compensation targeted EXACTLY the marker this commit wrote (the
	// token-derived kind + the marker's EventID — never an empty generation,
	// never another kind) and the marker survived because the compensation
	// failed.
	delCalls := fx.proposals.deleteIfLogSnapshot()
	if len(delCalls) != 1 {
		t.Fatalf("deleteif calls = %d, want 1", len(delCalls))
	}
	markerCalls := fx.proposals.saveIfLogSnapshot()
	if len(markerCalls) != 1 {
		t.Fatalf("saveif calls = %d, want exactly the one marker", len(markerCalls))
	}
	marker := markerCalls[0]
	if delCalls[0].exp.Identity != q || delCalls[0].exp.Kind != kind || delCalls[0].exp.ExpectedEventID != marker.next.ID {
		t.Fatalf("compensation DeleteIf = %+v, want the exact marker %s under %s", delCalls[0].exp, marker.next.ID, kind)
	}
	rec, err := fx.proposals.Load(context.Background(), q, kind)
	if err != nil {
		t.Fatalf("marker must survive a failed compensation: %v", err)
	}
	if rec.ID != marker.next.ID {
		t.Fatalf("marker generation changed: got %s want %s", rec.ID, marker.next.ID)
	}
	ledger := fx.ledgerRecord(t, q, validated.ProposalToken)
	if ledger["phase"] != "committing" || ledger["written_package_hash"] != validated.PackageHash {
		t.Fatalf("ledger after failed compensation = %+v, want the intact committing marker", ledger)
	}
	fx.assertNoInstalledWinner(t, ctx, "demo-skill")

	// No other ledger slot was touched: the ONLY state mutations are this
	// token's marker SaveIf and its failed compensation DeleteIf — no
	// deletes, no foreign kinds.
	_, saveIfs, deleteIfs, deletes := fx.proposals.stateCounts()
	if saveIfs != 1 || deleteIfs != 1 || deletes != 0 {
		t.Fatalf("state mutation counts saveIfs=%d deleteIfs=%d deletes=%d, want 1/1/0", saveIfs, deleteIfs, deletes)
	}
	if marker.next.Kind != kind {
		t.Fatalf("marker kind %q != token-derived %q", marker.next.Kind, kind)
	}

	// A retry of the stale marker refuses loudly with the proposal-invalid
	// refusal (never a silent re-install under a stale marker, never a
	// second put, never a compensation delete of a marker it did not write).
	if _, err := fx.svc.Commit(ctx, req); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportProposalInvalid) {
		t.Fatalf("retry under the stale marker err=%v, want ErrUserSkillImportProposalInvalid", err)
	}
	put, _, _, _, _, _ = fx.store.counts()
	if put != 1 {
		t.Fatalf("retry under the stale marker performed a write: put=%d", put)
	}
}
