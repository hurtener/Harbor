package protocol_test

// composition_preview_test.go — the read-only effective-composition preview
// service tests: the strict-composer parity (deterministic items, hashes,
// boot|revision|both provenance), the own/elevated/foreign/cross-tenant
// authorization matrix, signed session+agent reach, retirement, the
// before-read widened audit, no-write guarantees, two-user isolation, fresh
// revision reads, config-removal revision retention and absent-both
// snapshots, immutable deep copies, and the agent_packs.list meaning
// preservation.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/bootpacks"
)

// ---------------------------------------------------------------------------
// Fixtures and helpers
// ---------------------------------------------------------------------------

var (
	previewUserA = identity.Identity{TenantID: "t", UserID: "ua", SessionID: "sa"}
	previewUserB = identity.Identity{TenantID: "t", UserID: "ub", SessionID: "sb"}
	previewAdmin = identity.Identity{TenantID: "t", UserID: "admin", SessionID: "admin-session"}
)

// previewRegistry builds the real statestore-backed agent-config registry and
// exposes it as the RetirementRegistry the preview reader seam needs.
func previewRegistry(t *testing.T) agentcfg.RetirementRegistry {
	t.Helper()
	reg := newRegistry(t)
	rr, ok := reg.(agentcfg.RetirementRegistry)
	if !ok {
		t.Fatalf("statestore registry does not implement RetirementRegistry")
	}
	return rr
}

// previewBootEntry freezes ONE boot-baseline entry the way the eager loader
// would: a parsed pack skill with Origin=pack / Scope=project and the
// canonical semantic hash of its own body (the strict composer re-validates
// the pairing).
func previewBootEntry(name, title, trigger string, steps []string) bootpacks.Entry {
	skill := skills.Skill{
		Name: name, Title: title, Description: "desc " + name,
		Trigger: trigger, Steps: append([]string(nil), steps...),
		Origin: skills.OriginPack, Scope: skills.ScopeProject,
	}
	return bootpacks.Entry{Skill: skill, SemanticHash: skills.CanonicalContentHash(skill)}
}

// packItemWire is ONE durable pack item in the wire shape the admin
// agent_packs.upsert door accepts.
func packItemWire(name, title, trigger string, steps []string) prototypes.AgentConfigAgentPackItem {
	return prototypes.AgentConfigAgentPackItem{
		Name: name, Title: title, Description: "desc " + name,
		Trigger: trigger, Steps: append([]string(nil), steps...),
	}
}

// fakeBootIndex is the frozen eager boot-pack index reader seam. It is
// immutable after construction; Lookup returns deep copies in canonical-name
// order exactly like the real bootpacks.Index.
type fakeBootIndex struct {
	byKey map[bootpacks.Key][]bootpacks.Entry
}

func (f *fakeBootIndex) Lookup(tenantID, agentID string) ([]bootpacks.Entry, bool) {
	entries, ok := f.byKey[bootpacks.Key{TenantID: tenantID, AgentID: agentID}]
	if !ok {
		return nil, false
	}
	out := make([]bootpacks.Entry, len(entries))
	for i, e := range entries {
		e.Skill = clonePreviewTestSkill(e.Skill)
		out[i] = e
	}
	return out, true
}

func clonePreviewTestSkill(skill skills.Skill) skills.Skill {
	out := skill
	out.Tags = append([]string(nil), skill.Tags...)
	out.Steps = append([]string(nil), skill.Steps...)
	out.Preconditions = append([]string(nil), skill.Preconditions...)
	out.FailureModes = append([]string(nil), skill.FailureModes...)
	out.RequiredTools = append([]string(nil), skill.RequiredTools...)
	out.RequiredNS = append([]string(nil), skill.RequiredNS...)
	out.RequiredTags = append([]string(nil), skill.RequiredTags...)
	if len(skill.Extra) > 0 {
		out.Extra = make(map[string]any, len(skill.Extra))
		for k, v := range skill.Extra {
			out.Extra[k] = v
		}
	}
	return out
}

// standardBootIndex declares three boot keys: the previewed agent-x (alpha +
// beta), a retired-only agent, and a second agent (gamma) used by the
// two-user isolation test.
func standardBootIndex() *fakeBootIndex {
	return &fakeBootIndex{byKey: map[bootpacks.Key][]bootpacks.Entry{
		{TenantID: "t", AgentID: testAgentID}: {
			previewBootEntry("alpha", "Alpha", "alpha trigger", []string{"alpha step"}),
			previewBootEntry("beta", "Beta", "beta trigger", []string{"beta step"}),
		},
		{TenantID: "t", AgentID: "agent-retired"}: {
			previewBootEntry("alpha", "Alpha", "alpha trigger", []string{"alpha step"}),
		},
		{TenantID: "t", AgentID: "agent-y"}: {
			previewBootEntry("gamma", "Gamma", "gamma trigger", []string{"gamma step"}),
		},
	}}
}

// previewFixture couples the preview service under test with the admin
// service + registry used to seed and inspect durable state.
type previewFixture struct {
	preview *agentcfgprotocol.CompositionPreviewService
	admin   *agentcfgprotocol.Service
	reg     agentcfg.RetirementRegistry
	bootIdx *fakeBootIndex
}

// newPreviewFixture builds the fixture with the two signed-reach gates wired
// (the production posture); tests that need a different option set construct
// the service directly.
func newPreviewFixture(t *testing.T, bootIdx *fakeBootIndex) *previewFixture {
	t.Helper()
	reg := previewRegistry(t)
	preview, err := agentcfgprotocol.NewCompositionPreviewService(reg, bootIdx,
		agentcfgprotocol.WithPreviewSessionReach(auth.NewSessionReachAuthorizer()),
		agentcfgprotocol.WithPreviewAgentReach(auth.NewAgentReachAuthorizer()),
	)
	if err != nil {
		t.Fatalf("NewCompositionPreviewService: %v", err)
	}
	admin, err := agentcfgprotocol.NewService(reg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return &previewFixture{preview: preview, admin: admin, reg: reg, bootIdx: bootIdx}
}

// previewCtx seats the verified identity, scopes, and signed reach sets on
// the request context the way the auth middleware would.
func previewCtx(t *testing.T, id identity.Identity, scopes []auth.Scope, sessionReach, agentReach []string) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatalf("WithVerified: %v", err)
	}
	if len(scopes) > 0 {
		ctx = auth.WithScopes(ctx, scopes)
	}
	if sessionReach != nil {
		ctx = auth.WithSessionReach(ctx, sessionReach)
	}
	if agentReach != nil {
		ctx = auth.WithAgentReach(ctx, agentReach)
	}
	return ctx
}

func previewReq(user, session, agent string) agentcfgprotocol.CompositionPreviewRequest {
	return agentcfgprotocol.CompositionPreviewRequest{TenantID: "t", UserID: user, SessionID: session, AgentID: agent}
}

// seedPackItem durably authoring ONE pack item through the real admin door
// (agent_packs.upsert) — the exact durable revision-authoring path the
// preview must never disturb.
func seedPackItem(t *testing.T, svc *agentcfgprotocol.Service, item prototypes.AgentConfigAgentPackItem) {
	t.Helper()
	if _, err := svc.AgentPacksUpsert(context.Background(), prototypes.AgentConfigAgentPacksUpsertRequest{
		Identity: scope(), AgentID: testAgentID, Skill: item,
	}); err != nil {
		t.Fatalf("seed pack item %q: %v", item.Name, err)
	}
}

// expectedTier composes the SAME strict composer over the SAME inputs the
// preview service reads (the fresh active revision's durable agent_packs
// section + the frozen boot baseline) — the parity oracle for every
// available-preview assertion.
func expectedTier(t *testing.T, reg agentcfg.RetirementRegistry, bootIdx agentcfgprotocol.BootPackReader, tenant, user, session, agentID string) (sessionoverlay.OperatorTier, agentcfg.Revision, bool) {
	t.Helper()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: user, SessionID: session}}
	rev, set, err := reg.Active(context.Background(), q, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		t.Fatalf("expected active: %v", err)
	}
	var revSkills []skills.Skill
	if set && len(rev.Payload.AgentPacks) > 0 {
		revSkills, err = skills.PackItemsToSkills(rev.Payload.AgentPacks)
		if err != nil {
			t.Fatalf("expected pack convert: %v", err)
		}
	}
	boot, _ := bootIdx.Lookup(tenant, agentID)
	tier, err := sessionoverlay.ComposeOperatorTier(boot, revSkills)
	if err != nil {
		t.Fatalf("expected compose: %v", err)
	}
	return tier, rev, set
}

// assertPreviewMatchesTier pins an available response to the strict composer's
// independent output: items (canonical order + name/hash/source), the three
// set hashes, and the fresh revision identity.
func assertPreviewMatchesTier(t *testing.T, resp agentcfgprotocol.CompositionPreviewResponse, tier sessionoverlay.OperatorTier, rev agentcfg.Revision, set bool) {
	t.Helper()
	if resp.Outcome != agentcfgprotocol.PreviewOutcomeAvailable {
		t.Fatalf("outcome=%q want available", resp.Outcome)
	}
	if resp.BootPackSetHash != tier.BootPackSetHash() {
		t.Errorf("boot_pack_set_hash=%q want %q", resp.BootPackSetHash, tier.BootPackSetHash())
	}
	if resp.CombinedHash != tier.CombinedHash() {
		t.Errorf("combined_hash=%q want %q", resp.CombinedHash, tier.CombinedHash())
	}
	if resp.RevisionHash != tier.RevisionHash() {
		t.Errorf("revision_hash=%q want %q", resp.RevisionHash, tier.RevisionHash())
	}
	if set {
		if resp.RevisionID != rev.RevisionID || resp.ContentHash != rev.ContentHash {
			t.Errorf("revision id=%q/%q want %q/%q", resp.RevisionID, resp.ContentHash, rev.RevisionID, rev.ContentHash)
		}
	} else if resp.RevisionID != "" || resp.ContentHash != "" {
		t.Errorf("revision identity present without an active revision: %q/%q", resp.RevisionID, resp.ContentHash)
	}
	items := tier.Items()
	if len(resp.Items) != len(items) {
		t.Fatalf("items len=%d want %d", len(resp.Items), len(items))
	}
	for i, want := range items {
		got := resp.Items[i]
		if got.Name != strings.ToLower(strings.TrimSpace(want.Skill.Name)) {
			t.Errorf("item %d name=%q want %q", i, got.Name, want.Skill.Name)
		}
		if got.SemanticHash != want.SemanticHash {
			t.Errorf("item %d hash=%q want %q", i, got.SemanticHash, want.SemanticHash)
		}
		if got.Source != want.Source {
			t.Errorf("item %d source=%q want %q", i, got.Source, want.Source)
		}
		if !reflect.DeepEqual(got.Skill, want.Skill) {
			t.Errorf("item %d body differs from the strict composer output", i)
		}
	}
}

// recordingPreviewBus captures published events (mutex-guarded, so the
// concurrent tests can share one instance) and optionally stamps a shared
// order log so tests can assert audit-before-read ordering.
type recordingPreviewBus struct {
	mu     sync.Mutex
	events []events.Event
	log    *previewOrderLog
}

func (b *recordingPreviewBus) Publish(_ context.Context, ev events.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.log != nil {
		b.log.add("audit")
	}
	b.events = append(b.events, ev)
	return nil
}

func (b *recordingPreviewBus) Subscribe(context.Context, events.Filter) (events.Subscription, error) {
	return nil, errors.New("recording bus: subscribe not wired")
}

func (b *recordingPreviewBus) Close(context.Context) error { return nil }

func (b *recordingPreviewBus) adminEvents() []events.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]events.Event, 0, len(b.events))
	for _, ev := range b.events {
		if ev.Type == events.EventTypeAdminScopeUsed {
			out = append(out, ev)
		}
	}
	return out
}

// previewOrderLog is a shared, mutex-guarded append log used to prove the
// widened audit is emitted BEFORE any registry read.
type previewOrderLog struct {
	mu      sync.Mutex
	entries []string
}

func (l *previewOrderLog) add(entry string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, entry)
}

func (l *previewOrderLog) first(entry string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, e := range l.entries {
		if e == entry {
			return i
		}
	}
	return -1
}

// previewReaderSpy wraps the real registry and records every read on the
// shared order log (the preview reader seam is structurally read-only).
type previewReaderSpy struct {
	agentcfg.RetirementRegistry
	log *previewOrderLog
}

func (s *previewReaderSpy) Active(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope) (agentcfg.Revision, bool, error) {
	s.log.add("active")
	return s.RetirementRegistry.Active(ctx, id, agentID, scope)
}

func (s *previewReaderSpy) RetirementStatus(ctx context.Context, id identity.Quadruple, agentID string) (agentcfg.RetirementStatus, bool, error) {
	s.log.add("retirement")
	return s.RetirementRegistry.RetirementStatus(ctx, id, agentID)
}

// retirePreviewAgent installs the terminal lifecycle tombstone for a fresh
// (revision-less) agent, the same direct-registry path the service tests use.
func retirePreviewAgent(t *testing.T, reg agentcfg.RetirementRegistry, agentID string) {
	t.Helper()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	if _, err := reg.Retire(context.Background(), q, agentID, agentcfg.RetirementRequest{
		OperationID: "preview-retire-" + agentID, ExpectedContentHash: agentcfg.ExpectNoActiveRevision,
	}); err != nil {
		t.Fatalf("retire %s: %v", agentID, err)
	}
}

// ---------------------------------------------------------------------------
// Composition + provenance
// ---------------------------------------------------------------------------

// TestCompositionPreview_OwnTriple_ComposesBootRevisionAndBoth proves the
// ordinary caller's own-triple preview returns the exact strict-composer
// output: deterministic canonical-order items with provenance exactly
// boot|revision|both, the deterministic boot_pack_set_hash, and the fresh
// revision identity. The pack is seeded through the durable agent_packs
// authoring door, and the parity oracle recomposes the same inputs.
func TestCompositionPreview_OwnTriple_ComposesBootRevisionAndBoth(t *testing.T) {
	fx := newPreviewFixture(t, standardBootIndex())
	// alpha: identical content in boot AND revision → source=both.
	seedPackItem(t, fx.admin, packItemWire("alpha", "Alpha", "alpha trigger", []string{"alpha step"}))
	// gamma: revision-only.
	seedPackItem(t, fx.admin, packItemWire("gamma", "Gamma", "gamma trigger", []string{"gamma step"}))

	ctx := previewCtx(t, previewUserA, nil, nil, []string{testAgentID})
	resp, err := fx.preview.CompositionPreview(ctx, previewReq("ua", "sa", testAgentID))
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	tier, rev, set := expectedTier(t, fx.reg, fx.bootIdx, "t", "ua", "sa", testAgentID)
	assertPreviewMatchesTier(t, resp, tier, rev, set)

	if resp.Widened {
		t.Error("ordinary own preview must not be marked widened")
	}
	if len(resp.Items) != 3 {
		t.Fatalf("items=%d want 3 (alpha both, beta boot, gamma revision)", len(resp.Items))
	}
	byName := map[string]agentcfgprotocol.CompositionPreviewItem{}
	for _, item := range resp.Items {
		byName[item.Name] = item
	}
	if got := byName["alpha"].Source; got != skills.OperatorTierSourceBoth {
		t.Errorf("alpha source=%q want both", got)
	}
	if got := byName["beta"].Source; got != skills.OperatorTierSourceBoot {
		t.Errorf("beta source=%q want boot", got)
	}
	if got := byName["gamma"].Source; got != skills.OperatorTierSourceRevision {
		t.Errorf("gamma source=%q want revision", got)
	}
	// The `both` item retains the boot body (the higher-authority source).
	if byName["alpha"].Skill.Origin != skills.OriginPack || byName["alpha"].Skill.Trigger != "alpha trigger" {
		t.Errorf("both item does not retain the boot body: %+v", byName["alpha"].Skill)
	}
}

// TestCompositionPreview_DeterministicOrderAndHashes proves two previews of
// the same state are byte-identical and the items are canonical-name ordered.
func TestCompositionPreview_DeterministicOrderAndHashes(t *testing.T) {
	fx := newPreviewFixture(t, standardBootIndex())
	seedPackItem(t, fx.admin, packItemWire("Gamma", "Gamma", "gamma trigger", []string{"gamma step"}))
	seedPackItem(t, fx.admin, packItemWire("beta", "Beta", "beta trigger", []string{"beta step"})) // same-hash as boot beta → both

	ctx := previewCtx(t, previewUserA, nil, nil, []string{testAgentID})
	first, err := fx.preview.CompositionPreview(ctx, previewReq("ua", "sa", testAgentID))
	if err != nil {
		t.Fatalf("preview 1: %v", err)
	}
	second, err := fx.preview.CompositionPreview(ctx, previewReq("ua", "sa", testAgentID))
	if err != nil {
		t.Fatalf("preview 2: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("two previews of identical state are not byte-identical")
	}
	names := make([]string, 0, len(first.Items))
	for _, item := range first.Items {
		names = append(names, item.Name)
	}
	want := []string{"alpha", "beta", "gamma"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("item order=%v want %v (canonical name order)", names, want)
	}
}

// TestCompositionPreview_SameHashMigrationShadow_DedupesBoth proves the
// migration shadow: a boot-declared item moved unchanged into the durable
// revision dedupes to ONE combined item marked both — never a split or an
// overwrite.
func TestCompositionPreview_SameHashMigrationShadow_DedupesBoth(t *testing.T) {
	fx := newPreviewFixture(t, standardBootIndex())
	seedPackItem(t, fx.admin, packItemWire("alpha", "Alpha", "alpha trigger", []string{"alpha step"}))

	ctx := previewCtx(t, previewUserA, nil, nil, []string{testAgentID})
	resp, err := fx.preview.CompositionPreview(ctx, previewReq("ua", "sa", testAgentID))
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if resp.Outcome != agentcfgprotocol.PreviewOutcomeAvailable {
		t.Fatalf("outcome=%q want available", resp.Outcome)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("items=%d want 2 (alpha both, beta boot) — the migration shadow must dedupe", len(resp.Items))
	}
	alpha := resp.Items[0]
	if alpha.Name != "alpha" || alpha.Source != skills.OperatorTierSourceBoth {
		t.Fatalf("alpha=%+v want name=alpha source=both", alpha)
	}
	bootHash := skills.CanonicalContentHash(standardBootIndex().byKey[bootpacks.Key{TenantID: "t", AgentID: testAgentID}][0].Skill)
	if alpha.SemanticHash != bootHash {
		t.Errorf("alpha semantic hash=%q want boot hash %q", alpha.SemanticHash, bootHash)
	}
	// The unique combined tier stays 2 — the composer deduped rather than
	// splitting the moved body.
	tier, rev, set := expectedTier(t, fx.reg, fx.bootIdx, "t", "ua", "sa", testAgentID)
	assertPreviewMatchesTier(t, resp, tier, rev, set)
	if tier.Len() != 2 {
		t.Fatalf("composer tier len=%d want 2", tier.Len())
	}
}

// TestCompositionPreview_DifferentHash_FailsTypedConflict proves the typed
// boot conflict: the same canonical name in boot and revision with differing
// semantic content fails the strict composer and surfaces as the conflict
// outcome with the deterministic offending name — never a silent overwrite.
func TestCompositionPreview_DifferentHash_FailsTypedConflict(t *testing.T) {
	fx := newPreviewFixture(t, standardBootIndex())
	// alpha differs from the boot body (different trigger → different hash).
	seedPackItem(t, fx.admin, packItemWire("alpha", "Alpha", "revision trigger", []string{"alpha step"}))

	ctx := previewCtx(t, previewUserA, nil, nil, []string{testAgentID})
	resp, err := fx.preview.CompositionPreview(ctx, previewReq("ua", "sa", testAgentID))
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if resp.Outcome != agentcfgprotocol.PreviewOutcomeConflict {
		t.Fatalf("outcome=%q want conflict", resp.Outcome)
	}
	if resp.ConflictName != "alpha" {
		t.Errorf("conflict_name=%q want alpha", resp.ConflictName)
	}
	if len(resp.Items) != 0 || resp.BootPackSetHash != "" || resp.CombinedHash != "" {
		t.Errorf("conflict response must carry no composed items or hashes: %+v", resp)
	}
	// The composer itself must report the typed conflict on the same inputs.
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "ua", SessionID: "sa"}}
	rev, set, err := fx.reg.Active(context.Background(), q, testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !set {
		t.Fatalf("active: set=%v err=%v", set, err)
	}
	revSkills, err := skills.PackItemsToSkills(rev.Payload.AgentPacks)
	if err != nil {
		t.Fatalf("pack convert: %v", err)
	}
	boot, _ := fx.bootIdx.Lookup("t", testAgentID)
	if _, err := sessionoverlay.ComposeOperatorTier(boot, revSkills); !errors.Is(err, sessionoverlay.ErrOperatorTierConflict) {
		t.Fatalf("composer err=%v want ErrOperatorTierConflict", err)
	}
}

// TestBootPackPreview_BootOnly_NoActiveRevision proves an agent with a boot
// baseline and NO active revision still previews: boot-only items, the boot
// set hash, and no revision identity.
func TestBootPackPreview_BootOnly_NoActiveRevision(t *testing.T) {
	fx := newPreviewFixture(t, standardBootIndex())
	ctx := previewCtx(t, previewUserA, nil, nil, []string{testAgentID})
	resp, err := fx.preview.CompositionPreview(ctx, previewReq("ua", "sa", testAgentID))
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	tier, rev, set := expectedTier(t, fx.reg, fx.bootIdx, "t", "ua", "sa", testAgentID)
	assertPreviewMatchesTier(t, resp, tier, rev, set)
	if set {
		t.Fatal("expected no active revision")
	}
	if resp.RevisionID != "" || resp.RevisionHash != "" {
		t.Errorf("boot-only preview must carry no revision identity/hash: %+v", resp)
	}
	if len(resp.Items) != 2 || resp.Items[0].Source != skills.OperatorTierSourceBoot {
		t.Errorf("boot-only items wrong: %+v", resp.Items)
	}
	if resp.BootPackSetHash == "" || resp.CombinedHash == "" {
		t.Errorf("boot-only preview must carry the boot set hash and combined hash: %+v", resp)
	}
}

// ---------------------------------------------------------------------------
// Authorization matrix: own / elevated / foreign / cross-tenant
// ---------------------------------------------------------------------------

// TestCompositionPreview_ForeignTriple_NonOracularUnavailable proves an
// ordinary caller targeting any triple other than its own gets the SAME
// unavailable outcome as a target that does not exist — no existence oracle.
func TestCompositionPreview_ForeignTriple_NonOracularUnavailable(t *testing.T) {
	fx := newPreviewFixture(t, standardBootIndex())
	seedPackItem(t, fx.admin, packItemWire("gamma", "Gamma", "gamma trigger", []string{"gamma step"}))

	ctx := previewCtx(t, previewUserA, nil, nil, []string{testAgentID})
	foreign, err := fx.preview.CompositionPreview(ctx, previewReq("ub", "sb", testAgentID))
	if err != nil {
		t.Fatalf("foreign preview: %v", err)
	}
	// A totally-missing triple of the same shape must be indistinguishable.
	missing, err := fx.preview.CompositionPreview(ctx, previewReq("u-missing", "s-missing", testAgentID))
	if err != nil {
		t.Fatalf("missing preview: %v", err)
	}
	if foreign.Outcome != agentcfgprotocol.PreviewOutcomeUnavailable {
		t.Fatalf("foreign outcome=%q want unavailable", foreign.Outcome)
	}
	if !reflect.DeepEqual(foreign, missing) {
		t.Fatal("foreign and missing targets must be byte-identical (non-oracular)")
	}
}

// TestCompositionPreview_CrossTenant_NonOracularUnavailable proves an
// elevated caller targeting another tenant gets the SAME unavailable outcome
// as a missing target, with NO audit (nothing was read) — the cross-tenant
// boundary never becomes an oracle.
func TestCompositionPreview_CrossTenant_NonOracularUnavailable(t *testing.T) {
	fx := newPreviewFixture(t, standardBootIndex())
	bus := &recordingPreviewBus{}
	reg := previewRegistry(t)
	preview, err := agentcfgprotocol.NewCompositionPreviewService(reg, fx.bootIdx,
		agentcfgprotocol.WithPreviewSessionReach(auth.NewSessionReachAuthorizer()),
		agentcfgprotocol.WithPreviewAgentReach(auth.NewAgentReachAuthorizer()),
		agentcfgprotocol.WithPreviewBus(bus),
	)
	if err != nil {
		t.Fatalf("NewCompositionPreviewService: %v", err)
	}
	adminSvc, err := agentcfgprotocol.NewService(reg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	seedPackItem(t, adminSvc, packItemWire("gamma", "Gamma", "gamma trigger", []string{"gamma step"}))

	ctx := previewCtx(t, previewAdmin, []auth.Scope{auth.ScopeAdmin}, nil, []string{testAgentID})
	cross, err := preview.CompositionPreview(ctx, agentcfgprotocol.CompositionPreviewRequest{
		TenantID: "other-tenant", UserID: "ua", SessionID: "sa", AgentID: testAgentID,
	})
	if err != nil {
		t.Fatalf("cross-tenant preview: %v", err)
	}
	// Same-tenant missing boot key (ordinary path) must be indistinguishable.
	ordinaryMissing, err := fx.preview.CompositionPreview(
		previewCtx(t, previewUserA, nil, nil, []string{testAgentID, "agent-undeclared"}),
		previewReq("ua", "sa", "agent-undeclared"),
	)
	if err != nil {
		t.Fatalf("ordinary missing preview: %v", err)
	}
	if cross.Outcome != agentcfgprotocol.PreviewOutcomeUnavailable {
		t.Fatalf("cross-tenant outcome=%q want unavailable", cross.Outcome)
	}
	if !reflect.DeepEqual(cross, ordinaryMissing) {
		t.Fatal("cross-tenant and missing targets must be byte-identical (non-oracular)")
	}
	if n := len(bus.adminEvents()); n != 0 {
		t.Fatalf("cross-tenant refusal emitted %d admin audits, want 0 (nothing was read)", n)
	}
}

// TestCompositionPreview_ElevatedSameTenantWidened_AuditedBeforeRead proves
// the admin/fleet widening: a same-tenant user is previewable with signed
// effective-agent reach, the widened read is audited on the canonical event
// BEFORE any registry read, and the response is marked widened.
func TestCompositionPreview_ElevatedSameTenantWidened_AuditedBeforeRead(t *testing.T) {
	reg := previewRegistry(t)
	log := &previewOrderLog{}
	spy := &previewReaderSpy{RetirementRegistry: reg, log: log}
	bus := &recordingPreviewBus{log: log}
	bootIdx := standardBootIndex()
	preview, err := agentcfgprotocol.NewCompositionPreviewService(spy, bootIdx,
		agentcfgprotocol.WithPreviewSessionReach(auth.NewSessionReachAuthorizer()),
		agentcfgprotocol.WithPreviewAgentReach(auth.NewAgentReachAuthorizer()),
		agentcfgprotocol.WithPreviewBus(bus),
	)
	if err != nil {
		t.Fatalf("NewCompositionPreviewService: %v", err)
	}
	adminSvc, err := agentcfgprotocol.NewService(reg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	seedPackItem(t, adminSvc, packItemWire("gamma", "Gamma", "gamma trigger", []string{"gamma step"}))

	ctx := previewCtx(t, previewAdmin, []auth.Scope{auth.ScopeAdmin}, nil, []string{testAgentID})
	resp, err := preview.CompositionPreview(ctx, previewReq("ub", "sb", testAgentID))
	if err != nil {
		t.Fatalf("widened preview: %v", err)
	}
	tier, rev, set := expectedTier(t, reg, bootIdx, "t", "ub", "sb", testAgentID)
	assertPreviewMatchesTier(t, resp, tier, rev, set)
	if !resp.Widened {
		t.Error("elevated same-tenant preview must be marked widened")
	}

	audits := bus.adminEvents()
	if len(audits) != 1 {
		t.Fatalf("admin audits=%d want 1", len(audits))
	}
	payload, ok := audits[0].Payload.(agentcfgprotocol.CompositionPreviewAdminPayload)
	if !ok {
		t.Fatalf("audit payload type %T, want CompositionPreviewAdminPayload", audits[0].Payload)
	}
	if payload.Actor != previewAdmin {
		t.Errorf("audit actor=%+v want %+v", payload.Actor, previewAdmin)
	}
	if payload.Target != (identity.Identity{TenantID: "t", UserID: "ub", SessionID: "sb"}) {
		t.Errorf("audit target=%+v want (t, ub, sb)", payload.Target)
	}
	if payload.AgentID != testAgentID {
		t.Errorf("audit agent_id=%q want %q", payload.AgentID, testAgentID)
	}
	if payload.Method != "agent_config.composition.preview" {
		t.Errorf("audit method=%q want agent_config.composition.preview", payload.Method)
	}
	if audits[0].Identity.Identity != previewAdmin {
		t.Errorf("audit event identity=%+v want the actor %+v", audits[0].Identity.Identity, previewAdmin)
	}

	// The audit MUST precede the retirement-gate read and the active-revision
	// read.
	if auditAt, activeAt, retiredAt := log.first("audit"), log.first("active"), log.first("retirement"); auditAt < 0 || activeAt < 0 || retiredAt < 0 || auditAt > activeAt || auditAt > retiredAt {
		t.Fatalf("order log=%v want audit before active/retirement reads", log.entries)
	}
}

// TestCompositionPreview_ElevatedConsoleFleet_WidenedProvesFleetEntitlement
// proves console:fleet carries the same widening entitlement as admin.
func TestCompositionPreview_ElevatedConsoleFleet_WidenedProvesFleetEntitlement(t *testing.T) {
	fx := newPreviewFixture(t, standardBootIndex())
	bus := &recordingPreviewBus{}
	reg := previewRegistry(t)
	preview, err := agentcfgprotocol.NewCompositionPreviewService(reg, fx.bootIdx,
		agentcfgprotocol.WithPreviewSessionReach(auth.NewSessionReachAuthorizer()),
		agentcfgprotocol.WithPreviewAgentReach(auth.NewAgentReachAuthorizer()),
		agentcfgprotocol.WithPreviewBus(bus),
	)
	if err != nil {
		t.Fatalf("NewCompositionPreviewService: %v", err)
	}
	adminSvc, err := agentcfgprotocol.NewService(reg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	seedPackItem(t, adminSvc, packItemWire("gamma", "Gamma", "gamma trigger", []string{"gamma step"}))

	ctx := previewCtx(t, previewAdmin, []auth.Scope{auth.ScopeConsoleFleet}, nil, []string{testAgentID})
	resp, err := preview.CompositionPreview(ctx, previewReq("ub", "sb", testAgentID))
	if err != nil {
		t.Fatalf("fleet widened preview: %v", err)
	}
	if resp.Outcome != agentcfgprotocol.PreviewOutcomeAvailable || !resp.Widened {
		t.Fatalf("fleet widened preview=%+v want available + widened", resp)
	}
	if n := len(bus.adminEvents()); n != 1 {
		t.Fatalf("fleet widened audit=%d want 1", n)
	}
}

// ---------------------------------------------------------------------------
// Signed reach + retirement gates
// ---------------------------------------------------------------------------

// TestCompositionPreview_SessionReach_ClaimDeniesAndAllows proves the signed
// session-reach gate: a PRESENT claim must contain the target session (loud
// denial), an absent claim preserves dynamic selection, and an empty claim
// denies.
func TestCompositionPreview_SessionReach_ClaimDeniesAndAllows(t *testing.T) {
	fx := newPreviewFixture(t, standardBootIndex())

	// Present claim without the session → loud denial.
	denied, err := fx.preview.CompositionPreview(
		previewCtx(t, previewUserA, nil, []string{"session-other"}, []string{testAgentID}),
		previewReq("ua", "sa", testAgentID),
	)
	if err == nil || !errors.Is(err, agentcfgprotocol.ErrPreviewSessionReachDenied) {
		t.Fatalf("session-reach denial: resp=%+v err=%v want ErrPreviewSessionReachDenied", denied, err)
	}

	// Present claim containing the session → pass.
	if _, err := fx.preview.CompositionPreview(
		previewCtx(t, previewUserA, nil, []string{"sa"}, []string{testAgentID}),
		previewReq("ua", "sa", testAgentID),
	); err != nil {
		t.Fatalf("session-reach allow: %v", err)
	}

	// Absent claim → dynamic selection preserved (pass).
	if _, err := fx.preview.CompositionPreview(
		previewCtx(t, previewUserA, nil, nil, []string{testAgentID}),
		previewReq("ua", "sa", testAgentID),
	); err != nil {
		t.Fatalf("session-reach absent: %v", err)
	}

	// Explicitly empty claim → denies every session.
	if _, err := fx.preview.CompositionPreview(
		previewCtx(t, previewUserA, nil, []string{}, []string{testAgentID}),
		previewReq("ua", "sa", testAgentID),
	); !errors.Is(err, agentcfgprotocol.ErrPreviewSessionReachDenied) {
		t.Fatalf("empty session-reach err=%v want ErrPreviewSessionReachDenied", err)
	}
}

// TestCompositionPreview_AgentReach_ClaimDeniesAndAllows proves the signed
// effective-agent gate: the agent must be a member of the caller's verified
// reach, a missing ctx reach fails closed, and an unwired gate fails closed.
func TestCompositionPreview_AgentReach_ClaimDeniesAndAllows(t *testing.T) {
	fx := newPreviewFixture(t, standardBootIndex())

	// Reach claim without the effective agent → loud denial.
	if _, err := fx.preview.CompositionPreview(
		previewCtx(t, previewUserA, nil, nil, []string{"agent-other"}),
		previewReq("ua", "sa", testAgentID),
	); !errors.Is(err, agentcfgprotocol.ErrPreviewAgentReachDenied) {
		t.Fatalf("agent-reach denial err=%v want ErrPreviewAgentReachDenied", err)
	}

	// No reach on ctx → fails closed (the middleware always seats a reach).
	if _, err := fx.preview.CompositionPreview(
		previewCtx(t, previewUserA, nil, nil, nil),
		previewReq("ua", "sa", testAgentID),
	); !errors.Is(err, agentcfgprotocol.ErrPreviewAgentReachDenied) {
		t.Fatalf("absent agent-reach err=%v want ErrPreviewAgentReachDenied", err)
	}

	// Reach containing the agent → pass.
	if _, err := fx.preview.CompositionPreview(
		previewCtx(t, previewUserA, nil, nil, []string{testAgentID}),
		previewReq("ua", "sa", testAgentID),
	); err != nil {
		t.Fatalf("agent-reach allow: %v", err)
	}

	// Unwired gate → FAILS CLOSED (never a silent widening).
	reg := previewRegistry(t)
	unwired, err := agentcfgprotocol.NewCompositionPreviewService(reg, fx.bootIdx,
		agentcfgprotocol.WithPreviewSessionReach(auth.NewSessionReachAuthorizer()),
	)
	if err != nil {
		t.Fatalf("NewCompositionPreviewService(unwired): %v", err)
	}
	if _, err := unwired.CompositionPreview(
		previewCtx(t, previewUserA, nil, nil, []string{testAgentID}),
		previewReq("ua", "sa", testAgentID),
	); !errors.Is(err, agentcfgprotocol.ErrPreviewAgentReachDenied) {
		t.Fatalf("unwired gate err=%v want ErrPreviewAgentReachDenied", err)
	}
}

// TestCompositionPreview_RetiredAgent_RetiredOutcome proves the retirement
// gate: a tombstoned effective agent previews as the typed retired outcome —
// never a composition, never an error.
func TestCompositionPreview_RetiredAgent_RetiredOutcome(t *testing.T) {
	fx := newPreviewFixture(t, standardBootIndex())
	retirePreviewAgent(t, fx.reg, "agent-retired")

	ctx := previewCtx(t, previewUserA, nil, nil, []string{"agent-retired"})
	resp, err := fx.preview.CompositionPreview(ctx, previewReq("ua", "sa", "agent-retired"))
	if err != nil {
		t.Fatalf("retired preview: %v", err)
	}
	if resp.Outcome != agentcfgprotocol.PreviewOutcomeRetired {
		t.Fatalf("outcome=%q want retired", resp.Outcome)
	}
	if len(resp.Items) != 0 || resp.BootPackSetHash != "" || resp.CombinedHash != "" {
		t.Errorf("retired response must carry no composition: %+v", resp)
	}
}

// ---------------------------------------------------------------------------
// Audit edge paths + no-write
// ---------------------------------------------------------------------------

// TestCompositionPreview_AdminAudit_NotEmittedForOrdinaryCaller proves an
// ordinary own-triple preview never emits the widened audit.
func TestCompositionPreview_AdminAudit_NotEmittedForOrdinaryCaller(t *testing.T) {
	fx := newPreviewFixture(t, standardBootIndex())
	bus := &recordingPreviewBus{}
	reg := previewRegistry(t)
	preview, err := agentcfgprotocol.NewCompositionPreviewService(reg, fx.bootIdx,
		agentcfgprotocol.WithPreviewSessionReach(auth.NewSessionReachAuthorizer()),
		agentcfgprotocol.WithPreviewAgentReach(auth.NewAgentReachAuthorizer()),
		agentcfgprotocol.WithPreviewBus(bus),
	)
	if err != nil {
		t.Fatalf("NewCompositionPreviewService: %v", err)
	}
	if _, err := preview.CompositionPreview(
		previewCtx(t, previewUserA, nil, nil, []string{testAgentID}),
		previewReq("ua", "sa", testAgentID),
	); err != nil {
		t.Fatalf("ordinary preview: %v", err)
	}
	if n := len(bus.adminEvents()); n != 0 {
		t.Fatalf("ordinary preview emitted %d admin audits, want 0", n)
	}
}

// TestCompositionPreview_AdminAudit_RedactionFailureAndNoBus proves the audit
// edge paths: a redaction failure means "do not emit" (the preview still
// succeeds), and an unwired bus logs instead of publishing (never silent).
func TestCompositionPreview_AdminAudit_RedactionFailureAndNoBus(t *testing.T) {
	for _, tc := range []struct {
		name     string
		redactor auditPreviewRedactor
		bus      *recordingPreviewBus
	}{
		{name: "redaction failure", redactor: erroringPreviewRedactor{}, bus: &recordingPreviewBus{}},
		{name: "no bus", bus: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newPreviewFixture(t, standardBootIndex())
			opts := []agentcfgprotocol.CompositionPreviewOption{
				agentcfgprotocol.WithPreviewSessionReach(auth.NewSessionReachAuthorizer()),
				agentcfgprotocol.WithPreviewAgentReach(auth.NewAgentReachAuthorizer()),
			}
			if tc.redactor != nil {
				opts = append(opts, agentcfgprotocol.WithPreviewRedactor(tc.redactor))
			}
			if tc.bus != nil {
				opts = append(opts, agentcfgprotocol.WithPreviewBus(tc.bus))
			}
			preview, err := agentcfgprotocol.NewCompositionPreviewService(fx.reg, fx.bootIdx, opts...)
			if err != nil {
				t.Fatalf("NewCompositionPreviewService: %v", err)
			}
			ctx := previewCtx(t, previewAdmin, []auth.Scope{auth.ScopeAdmin}, nil, []string{testAgentID})
			resp, err := preview.CompositionPreview(ctx, previewReq("ua", "sa", testAgentID))
			if err != nil {
				t.Fatalf("widened preview: %v", err)
			}
			if resp.Outcome != agentcfgprotocol.PreviewOutcomeAvailable {
				t.Fatalf("outcome=%q want available (audit edges never break the read)", resp.Outcome)
			}
			if tc.bus != nil {
				if n := len(tc.bus.adminEvents()); n != 0 {
					t.Fatalf("redaction failure published %d admin audits, want 0 (do-not-emit contract)", n)
				}
			}
		})
	}
}

// auditPreviewRedactor mirrors the redactor seam for the audit-edge tests.
type auditPreviewRedactor interface {
	Redact(ctx context.Context, payload any) (any, error)
}

type erroringPreviewRedactor struct{}

func (erroringPreviewRedactor) Redact(context.Context, any) (any, error) {
	return nil, errors.New("redaction failed")
}

// TestCompositionPreview_NoWrite_RegistryAndPackUnchanged proves the preview
// is read-only: after own, widened, foreign, cross-tenant, unavailable, and
// retired previews the durable registry state (revision chain, active
// content hash, and the agent_packs section the admin list verb reads) is
// byte-identical, and the boot reader saw only read-only Lookup calls.
func TestCompositionPreview_NoWrite_RegistryAndPackUnchanged(t *testing.T) {
	fx := newPreviewFixture(t, standardBootIndex())
	seedPackItem(t, fx.admin, packItemWire("gamma", "Gamma", "gamma trigger", []string{"gamma step"}))
	retirePreviewAgent(t, fx.reg, "agent-retired")

	snapshot := func() (int, string, []prototypes.AgentConfigAgentPackItem) {
		t.Helper()
		get, err := fx.admin.Get(context.Background(), prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		list, err := fx.admin.ListRevisions(context.Background(), prototypes.AgentConfigListRevisionsRequest{Identity: scope(), AgentID: testAgentID})
		if err != nil {
			t.Fatalf("list revisions: %v", err)
		}
		packs, err := fx.admin.AgentPacksList(context.Background(), prototypes.AgentConfigAgentPacksListRequest{Identity: scope(), AgentID: testAgentID})
		if err != nil {
			t.Fatalf("agent packs list: %v", err)
		}
		hash := ""
		if get.Set && get.Revision != nil {
			hash = get.Revision.ContentHash
		}
		return len(list.Revisions), hash, packs.Items
	}

	beforeRevs, beforeHash, beforePacks := snapshot()

	// Exercise every preview path against the same durable state.
	ownCtx := previewCtx(t, previewUserA, nil, nil, []string{testAgentID, "agent-retired", "agent-undeclared"})
	if _, err := fx.preview.CompositionPreview(ownCtx, previewReq("ua", "sa", testAgentID)); err != nil {
		t.Fatalf("own preview: %v", err)
	}
	if _, err := fx.preview.CompositionPreview(ownCtx, previewReq("ub", "sb", testAgentID)); err != nil {
		t.Fatalf("foreign preview: %v", err)
	}
	if _, err := fx.preview.CompositionPreview(
		previewCtx(t, previewUserA, nil, nil, []string{"agent-retired", "agent-undeclared"}),
		previewReq("ua", "sa", "agent-undeclared"),
	); err != nil {
		t.Fatalf("unavailable preview: %v", err)
	}
	if _, err := fx.preview.CompositionPreview(
		previewCtx(t, previewUserA, nil, nil, []string{"agent-retired"}),
		previewReq("ua", "sa", "agent-retired"),
	); err != nil {
		t.Fatalf("retired preview: %v", err)
	}
	adminCtx := previewCtx(t, previewAdmin, []auth.Scope{auth.ScopeAdmin}, nil, []string{testAgentID})
	if _, err := fx.preview.CompositionPreview(adminCtx, previewReq("ub", "sb", testAgentID)); err != nil {
		t.Fatalf("widened preview: %v", err)
	}
	if _, err := fx.preview.CompositionPreview(adminCtx, agentcfgprotocol.CompositionPreviewRequest{
		TenantID: "other-tenant", UserID: "ua", SessionID: "sa", AgentID: testAgentID,
	}); err != nil {
		t.Fatalf("cross-tenant preview: %v", err)
	}

	afterRevs, afterHash, afterPacks := snapshot()
	if afterRevs != beforeRevs {
		t.Errorf("revision chain grew: before=%d after=%d (preview must never write a revision)", beforeRevs, afterRevs)
	}
	if afterHash != beforeHash {
		t.Errorf("active content hash changed: before=%q after=%q", beforeHash, afterHash)
	}
	if !reflect.DeepEqual(afterPacks, beforePacks) {
		t.Errorf("agent_packs section changed under preview: before=%+v after=%+v", beforePacks, afterPacks)
	}
}

// TestCompositionPreview_AgentPacksList_MeaningPreserved pins the durable
// revision-authoring meaning of agent_packs.list: the preview reads the same
// section and the list verb still returns the EXACT durable pack the upserts
// authored (provenance refs included).
func TestCompositionPreview_AgentPacksList_MeaningPreserved(t *testing.T) {
	fx := newPreviewFixture(t, standardBootIndex())
	seedPackItem(t, fx.admin, packItemWire("gamma", "Gamma", "gamma trigger", []string{"gamma step"}))

	// The durable authored pack, read through the admin list verb.
	list, err := fx.admin.AgentPacksList(context.Background(), prototypes.AgentConfigAgentPacksListRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil {
		t.Fatalf("agent packs list: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].Name != "gamma" || list.Items[0].OriginRef == "" {
		t.Fatalf("durable pack misread: %+v", list.Items)
	}
	// Preview the same agent — the section must be untouched by the read.
	if _, err := fx.preview.CompositionPreview(
		previewCtx(t, previewUserA, nil, nil, []string{testAgentID}),
		previewReq("ua", "sa", testAgentID),
	); err != nil {
		t.Fatalf("preview: %v", err)
	}
	after, err := fx.admin.AgentPacksList(context.Background(), prototypes.AgentConfigAgentPacksListRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil {
		t.Fatalf("agent packs list after: %v", err)
	}
	if !reflect.DeepEqual(after.Items, list.Items) {
		t.Fatalf("agent_packs.list meaning changed under preview: before=%+v after=%+v", list.Items, after.Items)
	}
}

// ---------------------------------------------------------------------------
// Two users, fresh reads, new service snapshots, immutability
// ---------------------------------------------------------------------------

// TestCompositionPreview_TwoUsers_IsolationAndParity proves the agent-scope
// composition is tenant+agent keyed (user A and user B's own previews agree,
// and an elevated widened preview of user B agrees too), while a different
// agent's boot baseline never bleeds in.
func TestCompositionPreview_TwoUsers_IsolationAndParity(t *testing.T) {
	fx := newPreviewFixture(t, standardBootIndex())
	seedPackItem(t, fx.admin, packItemWire("gamma", "Gamma", "gamma trigger", []string{"gamma step"}))

	ownA, err := fx.preview.CompositionPreview(
		previewCtx(t, previewUserA, nil, nil, []string{testAgentID}),
		previewReq("ua", "sa", testAgentID),
	)
	if err != nil {
		t.Fatalf("user A preview: %v", err)
	}
	ownB, err := fx.preview.CompositionPreview(
		previewCtx(t, previewUserB, nil, nil, []string{testAgentID}),
		previewReq("ub", "sb", testAgentID),
	)
	if err != nil {
		t.Fatalf("user B preview: %v", err)
	}
	widenedB, err := fx.preview.CompositionPreview(
		previewCtx(t, previewAdmin, []auth.Scope{auth.ScopeAdmin}, nil, []string{testAgentID}),
		previewReq("ub", "sb", testAgentID),
	)
	if err != nil {
		t.Fatalf("admin widened preview of B: %v", err)
	}
	if !reflect.DeepEqual(ownA.Items, ownB.Items) || !reflect.DeepEqual(ownB.Items, widenedB.Items) {
		t.Fatal("same-agent composition must not depend on the user/session (tenant+agent keyed)")
	}
	if ownA.BootPackSetHash != ownB.BootPackSetHash || ownB.BootPackSetHash != widenedB.BootPackSetHash {
		t.Fatal("boot_pack_set_hash must agree across users of the same agent")
	}
	if !widenedB.Widened {
		t.Error("admin widened preview must be marked widened")
	}

	// A different agent with a different boot baseline composes only its own
	// baseline — no bleed from agent-x.
	agentY, err := fx.preview.CompositionPreview(
		previewCtx(t, previewUserA, nil, nil, []string{"agent-y"}),
		previewReq("ua", "sa", "agent-y"),
	)
	if err != nil {
		t.Fatalf("agent-y preview: %v", err)
	}
	if len(agentY.Items) != 1 || agentY.Items[0].Name != "gamma" || agentY.Items[0].Source != skills.OperatorTierSourceBoot {
		t.Fatalf("agent-y composition bled from agent-x: %+v", agentY.Items)
	}
	if agentY.BootPackSetHash == ownA.BootPackSetHash {
		t.Fatal("distinct agents must have distinct boot set hashes")
	}
}

// TestCompositionPreview_FreshRevisionRead_AndConfigRemoval proves the exact
// active revision is read FRESH at every preview (a new durable pack revision
// is reflected immediately) and that a NEW service snapshot over a boot index
// without the key (config removal) retains the durable revision as a
// revision-only composition, while a changed baseline perturbs the boot set
// hash.
func TestCompositionPreview_FreshRevisionRead_AndConfigRemoval(t *testing.T) {
	fx := newPreviewFixture(t, standardBootIndex())

	// 1. Boot-only preview: no revision identity yet.
	first, err := fx.preview.CompositionPreview(
		previewCtx(t, previewUserA, nil, nil, []string{testAgentID}),
		previewReq("ua", "sa", testAgentID),
	)
	if err != nil {
		t.Fatalf("first preview: %v", err)
	}
	if first.RevisionID != "" {
		t.Fatalf("first preview revision_id=%q want empty (no active revision)", first.RevisionID)
	}

	// 2. A new durable pack revision is read FRESH by the next preview.
	seedPackItem(t, fx.admin, packItemWire("gamma", "Gamma", "gamma trigger", []string{"gamma step"}))
	second, err := fx.preview.CompositionPreview(
		previewCtx(t, previewUserA, nil, nil, []string{testAgentID}),
		previewReq("ua", "sa", testAgentID),
	)
	if err != nil {
		t.Fatalf("second preview: %v", err)
	}
	if second.RevisionID == "" || second.RevisionID == first.RevisionID {
		t.Fatalf("second preview must read the fresh active revision: first=%q second=%q", first.RevisionID, second.RevisionID)
	}
	if len(second.Items) != 3 || second.Items[2].Name != "gamma" {
		t.Fatalf("second preview must include the new durable pack item: %+v", second.Items)
	}
	if second.CombinedHash == first.CombinedHash {
		t.Fatal("combined hash must change when the fresh revision adds a pack item")
	}

	// 3. Config removal: a NEW service snapshot over a boot index that no
	// longer declares the (tenant, agent) key. The boot baseline vanishes
	// but the independently persisted active revision survives: the preview
	// stays AVAILABLE, composed revision-only — the durable pack is never
	// erased, shadowed, or rewritten by config removal.
	removedIdx := &fakeBootIndex{byKey: map[bootpacks.Key][]bootpacks.Entry{
		{TenantID: "t", AgentID: "agent-retired"}: {
			previewBootEntry("alpha", "Alpha", "alpha trigger", []string{"alpha step"}),
		},
	}}
	removed, err := agentcfgprotocol.NewCompositionPreviewService(fx.reg, removedIdx,
		agentcfgprotocol.WithPreviewSessionReach(auth.NewSessionReachAuthorizer()),
		agentcfgprotocol.WithPreviewAgentReach(auth.NewAgentReachAuthorizer()),
	)
	if err != nil {
		t.Fatalf("NewCompositionPreviewService(removed): %v", err)
	}
	removedResp, err := removed.CompositionPreview(
		previewCtx(t, previewUserA, nil, nil, []string{testAgentID}),
		previewReq("ua", "sa", testAgentID),
	)
	if err != nil {
		t.Fatalf("removed-key preview: %v", err)
	}
	removedTier, removedRev, removedSet := expectedTier(t, fx.reg, removedIdx, "t", "ua", "sa", testAgentID)
	assertPreviewMatchesTier(t, removedResp, removedTier, removedRev, removedSet)
	if !removedSet {
		t.Fatal("config removal must not erase the durable active revision")
	}
	// Revision-only provenance: no boot entries, no boot set hash, and the
	// revision/combined hashes ride over the SAME single revision item.
	if len(removedResp.Items) != 1 || removedResp.Items[0].Name != "gamma" ||
		removedResp.Items[0].Source != skills.OperatorTierSourceRevision {
		t.Fatalf("config-removal items must be revision-only gamma: %+v", removedResp.Items)
	}
	if removedResp.BootPackSetHash != "" {
		t.Errorf("config-removal boot_pack_set_hash=%q want empty (no boot baseline)", removedResp.BootPackSetHash)
	}
	if removedResp.RevisionHash == "" || removedResp.CombinedHash == "" {
		t.Errorf("config-removal revision/combined hashes must be present over the revision-only tier: %+v", removedResp)
	}
	// The revision identity is the SAME durable revision the pre-removal
	// snapshot read — config removal changed nothing durable.
	if removedResp.RevisionID != second.RevisionID || removedResp.ContentHash != second.ContentHash {
		t.Errorf("config removal must retain the durable revision: removed id=%q/%q want second id=%q/%q",
			removedResp.RevisionID, removedResp.ContentHash, second.RevisionID, second.ContentHash)
	}

	// 4. A changed boot baseline in a new snapshot perturbs the boot set hash.
	changedIdx := &fakeBootIndex{byKey: map[bootpacks.Key][]bootpacks.Entry{
		{TenantID: "t", AgentID: testAgentID}: {
			previewBootEntry("alpha", "Alpha v2", "new trigger", []string{"new step"}),
		},
	}}
	changed, err := agentcfgprotocol.NewCompositionPreviewService(fx.reg, changedIdx,
		agentcfgprotocol.WithPreviewSessionReach(auth.NewSessionReachAuthorizer()),
		agentcfgprotocol.WithPreviewAgentReach(auth.NewAgentReachAuthorizer()),
	)
	if err != nil {
		t.Fatalf("NewCompositionPreviewService(changed): %v", err)
	}
	changedResp, err := changed.CompositionPreview(
		previewCtx(t, previewUserA, nil, nil, []string{testAgentID}),
		previewReq("ua", "sa", testAgentID),
	)
	if err != nil {
		t.Fatalf("changed-baseline preview: %v", err)
	}
	if changedResp.BootPackSetHash == second.BootPackSetHash {
		t.Fatal("a changed boot baseline must perturb the boot_pack_set_hash")
	}
	// The changed baseline composes its OWN alpha (new trigger) with the
	// fresh active revision's gamma — the revision pack is still read fresh.
	if len(changedResp.Items) != 2 {
		t.Fatalf("changed-baseline items=%d want 2 (changed alpha boot + gamma revision): %+v", len(changedResp.Items), changedResp.Items)
	}
	alpha := changedResp.Items[0]
	if alpha.Name != "alpha" || alpha.Source != skills.OperatorTierSourceBoot || alpha.Skill.Trigger != "new trigger" {
		t.Fatalf("changed-baseline alpha wrong: %+v", alpha)
	}
	gamma := changedResp.Items[1]
	if gamma.Name != "gamma" || gamma.Source != skills.OperatorTierSourceRevision {
		t.Fatalf("changed-baseline gamma wrong: %+v", gamma)
	}
}

// TestCompositionPreview_AbsentBoot_NoActiveRevision_NonOracularUnavailable
// proves the absent-both edge: when the boot baseline does not declare the
// (tenant, agent) key AND no active durable revision exists, there is nothing
// to compose — the preview stays the SAME non-oracular unavailable outcome,
// byte-identical to a foreign/missing target, and reveals no composition
// content. The fresh revision read still runs (the durable revision is the
// independent source of truth), but the response carries no items or hashes.
func TestCompositionPreview_AbsentBoot_NoActiveRevision_NonOracularUnavailable(t *testing.T) {
	fx := newPreviewFixture(t, standardBootIndex())

	ctx := previewCtx(t, previewUserA, nil, nil, []string{"agent-undeclared"})
	resp, err := fx.preview.CompositionPreview(ctx, previewReq("ua", "sa", "agent-undeclared"))
	if err != nil {
		t.Fatalf("absent-both preview: %v", err)
	}
	if resp.Outcome != agentcfgprotocol.PreviewOutcomeUnavailable {
		t.Fatalf("absent-both outcome=%q want unavailable", resp.Outcome)
	}
	if len(resp.Items) != 0 || resp.BootPackSetHash != "" || resp.CombinedHash != "" || resp.RevisionHash != "" ||
		resp.RevisionID != "" || resp.ContentHash != "" {
		t.Errorf("absent-both response must carry no composition: %+v", resp)
	}
	// Non-oracular: byte-identical to the unavailable outcome a foreign
	// triple of the same shape gets.
	foreign, err := fx.preview.CompositionPreview(
		previewCtx(t, previewUserA, nil, nil, []string{"agent-undeclared"}),
		previewReq("ub", "sb", "agent-undeclared"),
	)
	if err != nil {
		t.Fatalf("foreign preview: %v", err)
	}
	if !reflect.DeepEqual(resp, foreign) {
		t.Fatal("absent-both and foreign targets must be byte-identical (non-oracular)")
	}
}

// TestCompositionPreview_DeepCopiesImmutable proves every returned item is an
// immutable deep copy: mutating one response (slices, scalars, and nested
// Extra) never affects a later preview or another caller's copy.
func TestCompositionPreview_DeepCopiesImmutable(t *testing.T) {
	fx := newPreviewFixture(t, standardBootIndex())
	gamma := packItemWire("gamma", "Gamma", "gamma trigger", []string{"gamma step"})
	gamma.Extra = map[string]string{"env": "prod"}
	seedPackItem(t, fx.admin, gamma)

	ctx := previewCtx(t, previewUserA, nil, nil, []string{testAgentID})
	first, err := fx.preview.CompositionPreview(ctx, previewReq("ua", "sa", testAgentID))
	if err != nil {
		t.Fatalf("first preview: %v", err)
	}
	if len(first.Items) != 3 {
		t.Fatalf("items=%d want 3", len(first.Items))
	}

	// Mutate the first response aggressively: scalars, slices, and the nested
	// Extra maps of BOTH a boot item (nil-extra) and the revision item.
	mutated := first.Items[0].Skill // alpha — boot source
	mutated.Steps = append(mutated.Steps, "mutated step")
	mutated.Tags = append(mutated.Tags, "mutated-tag")
	mutated.Trigger = "mutated trigger"
	first.Items[0].Skill = mutated
	if extra := first.Items[2].Skill.Extra; extra != nil { // gamma — revision source
		extra["env"] = "mutated"
		extra["extra-key"] = "injected"
	}

	// A second preview must return pristine bodies — nothing leaked.
	second, err := fx.preview.CompositionPreview(ctx, previewReq("ua", "sa", testAgentID))
	if err != nil {
		t.Fatalf("second preview: %v", err)
	}
	if got := second.Items[0].Skill.Trigger; got != "alpha trigger" {
		t.Errorf("mutated trigger leaked into the composed body: %q", got)
	}
	if len(second.Items[0].Skill.Steps) != 1 || second.Items[0].Skill.Steps[0] != "alpha step" {
		t.Errorf("mutated steps leaked into the composed body: %v", second.Items[0].Skill.Steps)
	}
	if len(second.Items[0].Skill.Tags) != 0 {
		t.Errorf("mutated tags leaked into the composed body: %v", second.Items[0].Skill.Tags)
	}
	if got := second.Items[2].Skill.Extra["env"]; got != "prod" {
		t.Errorf("mutated Extra leaked into the composed body: env=%v want prod", got)
	}
	if _, injected := second.Items[2].Skill.Extra["extra-key"]; injected {
		t.Error("injected Extra key leaked into the composed body")
	}
}
