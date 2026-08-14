package protocol_test

// bootpack_guards_test.go — the boot-owned mutation guard matrix for the
// operator pack verbs: upsert/commit/remove refusals (typed, before any
// mutation), the pure rollback helper, the durable-shadow remove carve-out,
// list durability, config-removal semantics, the exact (tenant, agent, name)
// reader key, and N>=100 concurrent mixed guards under -race.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/bootpacks"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tools"
)

// The eager immutable bootpacks index satisfies the narrow injected reader
// directly (its OwnsName predicate is the exact (tenant, agent, canonical
// name) key the guards consume) — the integration owner wires it without any
// adapter.
var _ agentcfgprotocol.BootOwnership = (*bootpacks.Index)(nil)

// fakeBootOwnership is the narrow injected reader for the guard tests: an
// exact (tenant, agent, canonical-name) ownership map plus an OwnsName call
// counter. A name is owned ONLY for the exact pairs registered — a foreign
// tenant or agent is never refused.
type fakeBootOwnership struct {
	ownedByKey map[string]bool
	calls      atomic.Int64
}

func bootOwnerKey(tenantID, agentID, name string) string {
	return tenantID + "\x00" + agentID + "\x00" + skills.CanonicalPackName(name)
}

func (f *fakeBootOwnership) OwnsName(tenantID, agentID, name string) bool {
	f.calls.Add(1)
	return f.ownedByKey[bootOwnerKey(tenantID, agentID, name)]
}

func (f *fakeBootOwnership) ownsCalls() int64 { return f.calls.Load() }

// bootOwner builds a reader that owns names for ONE exact (tenant, agent)
// pair. The tenant is the fixture identity's fixed "t" (see agentQuad).
func bootOwner(agentID string, names ...string) *fakeBootOwnership {
	f := &fakeBootOwnership{ownedByKey: make(map[string]bool, len(names))}
	for _, name := range names {
		f.ownedByKey[bootOwnerKey("t", agentID, name)] = true
	}
	return f
}

// bootOwnedContext returns a request context carrying the injected reader.
func bootOwnedContext(owner agentcfgprotocol.BootOwnership) context.Context {
	return agentcfgprotocol.WithBootOwnership(context.Background(), owner)
}

// countingRegistry wraps a real registry and counts the two mutation doors
// (SetRevision / Rollback) so a refusal test can assert zero mutation calls.
type countingRegistry struct {
	agentcfg.Registry
	mu          sync.Mutex
	setRevision int
	rollback    int
}

func (r *countingRegistry) SetRevision(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope, payload agentcfg.ConfigPayload, opts agentcfg.SetOptions) (agentcfg.Revision, error) {
	r.mu.Lock()
	r.setRevision++
	r.mu.Unlock()
	return r.Registry.SetRevision(ctx, id, agentID, scope, payload, opts)
}

func (r *countingRegistry) Rollback(ctx context.Context, id identity.Quadruple, agentID, revisionID string, scope agentcfg.ConfigScope, opts agentcfg.SetOptions) (agentcfg.Revision, error) {
	r.mu.Lock()
	r.rollback++
	r.mu.Unlock()
	return r.Registry.Rollback(ctx, id, agentID, revisionID, scope, opts)
}

func (r *countingRegistry) setRevisionCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.setRevision
}

func (r *countingRegistry) rollbackCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rollback
}

// countingStateStore wraps the durable proposal ledger and counts the
// mutation doors (Save / SaveIf / DeleteIf) so a refusal test can assert the
// proposal receipt is never advanced or consumed.
type countingStateStore struct {
	state.StateStore
	mu       sync.Mutex
	save     int
	saveIf   int
	deleteIf int
}

func (s *countingStateStore) Save(ctx context.Context, record state.StateRecord) error {
	s.mu.Lock()
	s.save++
	s.mu.Unlock()
	return s.StateStore.Save(ctx, record)
}

func (s *countingStateStore) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	s.mu.Lock()
	s.saveIf++
	s.mu.Unlock()
	return s.StateStore.SaveIf(ctx, expectations, next)
}

func (s *countingStateStore) DeleteIf(ctx context.Context, expectation state.SlotExpectation) (bool, error) {
	s.mu.Lock()
	s.deleteIf++
	s.mu.Unlock()
	return s.StateStore.DeleteIf(ctx, expectation)
}

func (s *countingStateStore) saveCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.save
}

func (s *countingStateStore) saveIfCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveIf
}

func (s *countingStateStore) deleteIfCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteIf
}

func packItem(name string) skills.AgentPackItem {
	return skills.AgentPackItem{Name: name, Trigger: "trigger", Steps: []string{"do it"}}
}

func bootPackItemWire(name string) prototypes.AgentConfigAgentPackItem {
	return prototypes.AgentConfigAgentPackItem{Name: name, Trigger: "trigger", Steps: []string{"do it"}}
}

func upsertPackRequest(name string) prototypes.AgentConfigAgentPacksUpsertRequest {
	return prototypes.AgentConfigAgentPacksUpsertRequest{Identity: scope(), AgentID: testAgentID, Skill: bootPackItemWire(name)}
}

func upsertPackRequestFor(id prototypes.IdentityScope, agentID, name string) prototypes.AgentConfigAgentPacksUpsertRequest {
	return prototypes.AgentConfigAgentPacksUpsertRequest{Identity: id, AgentID: agentID, Skill: bootPackItemWire(name)}
}

func removePackRequest(name string) prototypes.AgentConfigAgentPacksRemoveRequest {
	return prototypes.AgentConfigAgentPacksRemoveRequest{Identity: scope(), AgentID: testAgentID, Name: name}
}

func listPackRequest() prototypes.AgentConfigAgentPacksListRequest {
	return prototypes.AgentConfigAgentPacksListRequest{Identity: scope(), AgentID: testAgentID}
}

func agentQuad() identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
}

func TestAgentPacksUpsert_BootOwnedNameRejectedAtEqualAndDifferentHash(t *testing.T) {
	ctx := context.Background()
	reg := &countingRegistry{Registry: newRegistry(t)}
	s, err := agentcfgprotocol.NewService(reg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := reg.SetRevision(ctx, agentQuad(), testAgentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("seed base revision: %v", err)
	}
	// Seed a legacy durable shadow "playbook" WITHOUT an injected reader (the
	// pre-baseline state), so the equal-hash body below is the stored body.
	if _, err := s.AgentPacksUpsert(ctx, upsertPackRequest("playbook")); err != nil {
		t.Fatalf("seed durable shadow: %v", err)
	}
	shadowSetCalls := reg.setRevisionCalls()

	guarded := bootOwnedContext(bootOwner(testAgentID, "playbook"))

	// Equal hash: the submitted body is byte-identical to the stored shadow.
	if _, err := s.AgentPacksUpsert(guarded, upsertPackRequest("playbook")); !errors.Is(err, agentcfgprotocol.ErrBootPackOwned) {
		t.Fatalf("equal-hash upsert error = %v, want ErrBootPackOwned", err)
	}
	// Different hash: a modified body is refused identically.
	different := upsertPackRequest("playbook")
	different.Skill.Steps = []string{"changed", "body"}
	if _, err := s.AgentPacksUpsert(guarded, different); !errors.Is(err, agentcfgprotocol.ErrBootPackOwned) {
		t.Fatalf("different-hash upsert error = %v, want ErrBootPackOwned", err)
	}
	if got := reg.setRevisionCalls(); got != shadowSetCalls {
		t.Fatalf("rejected upserts mutated the revision: SetRevision calls = %d, want %d", got, shadowSetCalls)
	}
	// Control: a non-boot-owned name is fully mutable under the same reader.
	if _, err := s.AgentPacksUpsert(guarded, upsertPackRequest("other")); err != nil {
		t.Fatalf("non-boot-owned upsert failed: %v", err)
	}
	if got := reg.setRevisionCalls(); got != shadowSetCalls+1 {
		t.Fatalf("control upsert did not record exactly one revision: got %d, want %d", got, shadowSetCalls+1)
	}
}

func TestAgentPacksRemove_BootOnlyNameIsTypedReadOnlyRefusal(t *testing.T) {
	ctx := context.Background()
	reg := &countingRegistry{Registry: newRegistry(t)}
	s, err := agentcfgprotocol.NewService(reg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	// No durable shadow for "playbook": the boot baseline owns the name and
	// the durable revision does not contain it (boot-only).
	guarded := bootOwnedContext(bootOwner(testAgentID, "playbook"))
	if _, err := s.AgentPacksRemove(guarded, removePackRequest("playbook")); err == nil {
		t.Fatal("boot-only remove unexpectedly succeeded (false success)")
	} else if !errors.Is(err, agentcfgprotocol.ErrBootPackOwned) {
		t.Fatalf("boot-only remove error = %v, want ErrBootPackOwned", err)
	} else if errors.Is(err, agentcfgprotocol.ErrAgentPackNotFound) {
		t.Fatalf("boot-only remove must be DISTINCT from ErrAgentPackNotFound: %v", err)
	}
	if got := reg.setRevisionCalls(); got != 0 {
		t.Fatalf("boot-only remove mutated the revision: SetRevision calls = %d, want 0", got)
	}
	// The durable revision is untouched.
	list, err := s.AgentPacksList(ctx, listPackRequest())
	if err != nil || len(list.Items) != 0 {
		t.Fatalf("list after boot-only remove = %+v err=%v, want empty", list.Items, err)
	}
}

func TestAgentPacksRemove_DeletesLegacyDurableShadowLeavesBoot(t *testing.T) {
	ctx := context.Background()
	reg := &countingRegistry{Registry: newRegistry(t)}
	s, err := agentcfgprotocol.NewService(reg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	// Seed a REAL legacy durable revision shadow of the now-boot-owned name.
	if _, err := s.AgentPacksUpsert(ctx, upsertPackRequest("playbook")); err != nil {
		t.Fatalf("seed durable shadow: %v", err)
	}
	before := reg.setRevisionCalls()

	guarded := bootOwnedContext(bootOwner(testAgentID, "playbook"))
	if _, err := s.AgentPacksRemove(guarded, removePackRequest("playbook")); err != nil {
		t.Fatalf("remove of the actual legacy shadow failed: %v", err)
	}
	if got := reg.setRevisionCalls(); got != before+1 {
		t.Fatalf("shadow remove must record exactly one revision: got %d, want %d", got, before+1)
	}
	// The durable revision no longer contains the shadow…
	list, err := s.AgentPacksList(ctx, listPackRequest())
	if err != nil || len(list.Items) != 0 {
		t.Fatalf("list after shadow remove = %+v err=%v, want empty (shadow gone)", list.Items, err)
	}
	// …while boot still owns the name (the baseline is untouched by remove).
	if owner := bootOwner(testAgentID, "playbook"); !owner.OwnsName("t", testAgentID, "playbook") {
		t.Fatal("boot baseline lost ownership after shadow remove")
	}
}

func TestAgentPacksRemove_NonBootOwnedBehaviorUnchanged(t *testing.T) {
	ctx := context.Background()
	reg := &countingRegistry{Registry: newRegistry(t)}
	s, err := agentcfgprotocol.NewService(reg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := s.AgentPacksUpsert(ctx, upsertPackRequest("plain")); err != nil {
		t.Fatalf("seed plain item: %v", err)
	}
	guarded := bootOwnedContext(bootOwner(testAgentID, "playbook"))
	// Present, non-boot-owned → normal removal.
	if _, err := s.AgentPacksRemove(guarded, removePackRequest("plain")); err != nil {
		t.Fatalf("remove non-boot-owned present name failed: %v", err)
	}
	// Absent, non-boot-owned → the pre-baseline not-found behavior, unchanged.
	if _, err := s.AgentPacksRemove(guarded, removePackRequest("absent")); !errors.Is(err, agentcfgprotocol.ErrAgentPackNotFound) {
		t.Fatalf("remove absent non-boot-owned name error = %v, want ErrAgentPackNotFound", err)
	}
}

func TestAgentPacksCommit_BootOwnedNameRejectedOnInitialCommit(t *testing.T) {
	ctx := context.Background()
	reg := &countingRegistry{Registry: newRegistry(t)}
	proposals := &countingStateStore{StateStore: mustProposalStore(t)}
	catalog := tools.NewCatalog()
	if err := catalog.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "guard_tool"}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) { return tools.ToolResult{}, nil }}); err != nil {
		t.Fatalf("register catalog tool: %v", err)
	}
	s, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithAgentPackProposer(agentPackTestProposer{item: packItem("playbook")}),
		agentcfgprotocol.WithAgentPackProposalState(proposals),
		agentcfgprotocol.WithAgentPackCatalog(catalog))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	base, err := reg.SetRevision(ctx, agentQuad(), testAgentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("seed base revision: %v", err)
	}
	proposal, err := s.AgentPacksPropose(ctx, prototypes.AgentConfigAgentPacksProposeRequest{
		Identity: scope(), AgentID: testAgentID, Intent: "make a playbook", ExpectedContentHash: base.ContentHash,
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	commit := prototypes.AgentConfigAgentPacksCommitRequest{
		Identity: scope(), AgentID: testAgentID, Skill: proposal.Skill, ReviewedHash: proposal.Hash,
		Provenance: proposal.Provenance, ProposalID: proposal.ProposalID, ExpectedContentHash: proposal.ExpectedContentHash,
	}
	guarded := bootOwnedContext(bootOwner(testAgentID, "playbook"))
	if _, err := s.AgentPacksCommit(guarded, commit); !errors.Is(err, agentcfgprotocol.ErrBootPackOwned) {
		t.Fatalf("initial commit of boot-owned name error = %v, want ErrBootPackOwned", err)
	}
	// Nothing mutated: no revision write, and the proposal receipt was never
	// advanced to committing or consumed.
	if got := reg.setRevisionCalls(); got != 1 {
		t.Fatalf("rejected commit mutated the revision: SetRevision calls = %d, want 1 (base only)", got)
	}
	if got := proposals.saveIfCalls(); got != 0 {
		t.Fatalf("rejected commit marked the receipt committing: SaveIf calls = %d, want 0", got)
	}
	if got := proposals.deleteIfCalls(); got != 0 {
		t.Fatalf("rejected commit consumed the receipt: DeleteIf calls = %d, want 0", got)
	}
	receipt, err := proposals.Load(ctx, agentQuad(), "agentcfg.agent_pack.proposal."+proposal.ProposalID)
	if err != nil {
		t.Fatalf("proposal receipt lost: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(receipt.Bytes, &fields); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}
	if phase, _ := fields["phase"].(string); phase != "" {
		t.Fatalf("rejected commit advanced receipt phase to %q, want unadvanced", phase)
	}
}

// mustProposalStore builds a fresh in-memory StateStore for the proposal
// ledger, failing the test on construction error.
func mustProposalStore(t *testing.T) state.StateStore {
	t.Helper()
	st, err := newStateStore(t)
	if err != nil {
		t.Fatalf("newStateStore: %v", err)
	}
	return st
}

// commitServiceWithCrashWindow builds the first-service stack whose commit
// lands the prepared target revision but crashes before active publication
// (the preparedButUnpublishedRegistry seam), returning the first service,
// the underlying real registry, and the counting proposal ledger.
func commitServiceWithCrashWindow(t *testing.T) (*agentcfgprotocol.Service, *countingRegistry, *countingStateStore) {
	t.Helper()
	reg, proposals := newRegistryWithState(t)
	countingProposals := &countingStateStore{StateStore: proposals}
	catalog := tools.NewCatalog()
	if err := catalog.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "guard_tool"}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) { return tools.ToolResult{}, nil }}); err != nil {
		t.Fatalf("register catalog tool: %v", err)
	}
	first, err := agentcfgprotocol.NewService(&preparedButUnpublishedRegistry{Registry: reg},
		agentcfgprotocol.WithAgentPackProposer(agentPackTestProposer{item: packItem("other")}),
		agentcfgprotocol.WithAgentPackProposalState(countingProposals),
		agentcfgprotocol.WithAgentPackCatalog(catalog))
	if err != nil {
		t.Fatalf("NewService (crash window): %v", err)
	}
	return first, &countingRegistry{Registry: reg}, countingProposals
}

func TestAgentPacksCommit_BootOwnedNameRejectedOnPreparedResumeAndResponseLossReplay(t *testing.T) {
	ctx := context.Background()
	first, countingReg, countingProposals := commitServiceWithCrashWindow(t)

	base, err := countingReg.SetRevision(ctx, agentQuad(), testAgentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("seed base revision: %v", err)
	}
	proposal, err := first.AgentPacksPropose(ctx, prototypes.AgentConfigAgentPacksProposeRequest{
		Identity: scope(), AgentID: testAgentID, Intent: "make a playbook", ExpectedContentHash: base.ContentHash,
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	commit := prototypes.AgentConfigAgentPacksCommitRequest{
		Identity: scope(), AgentID: testAgentID, Skill: proposal.Skill, ReviewedHash: proposal.Hash,
		Provenance: proposal.Provenance, ProposalID: proposal.ProposalID, ExpectedContentHash: proposal.ExpectedContentHash,
	}
	if _, err := first.AgentPacksCommit(ctx, commit); err == nil {
		t.Fatal("first commit unexpectedly succeeded across the injected crash window")
	}
	receipt, err := countingProposals.Load(ctx, agentQuad(), "agentcfg.agent_pack.proposal."+proposal.ProposalID)
	if err != nil {
		t.Fatalf("load committing receipt: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(receipt.Bytes, &fields); err != nil {
		t.Fatalf("decode committing receipt: %v", err)
	}
	stringField := func(name string) string { value, _ := fields[name].(string); return value }
	targetRevisionID, targetContentHash := stringField("target_revision_id"), stringField("target_content_hash")
	if stringField("phase") != "committing" || targetRevisionID == "" || targetContentHash == "" {
		t.Fatalf("receipt did not capture the prepared target: %v", fields)
	}

	// Retry #1 — prepared/committing resume with a reader that NOW owns the
	// name (a boot baseline declared after the proposal was authored): fresh
	// ownership must refuse before re-publication, leaving the receipt and
	// the prepared target untouched.
	retry, err := agentcfgprotocol.NewService(countingReg,
		agentcfgprotocol.WithAgentPackProposalState(countingProposals),
		agentcfgprotocol.WithAgentPackCatalog(catalogFor(t)))
	if err != nil {
		t.Fatalf("NewService retry: %v", err)
	}
	guarded := bootOwnedContext(bootOwner(testAgentID, "other"))
	setBefore := countingReg.setRevisionCalls()
	if _, err := retry.AgentPacksCommit(guarded, commit); !errors.Is(err, agentcfgprotocol.ErrBootPackOwned) {
		t.Fatalf("prepared-resume commit error = %v, want ErrBootPackOwned", err)
	}
	if got := countingReg.setRevisionCalls(); got != setBefore {
		t.Fatalf("prepared-resume refusal mutated the revision: SetRevision calls = %d, want %d", got, setBefore)
	}
	if got := countingProposals.deleteIfCalls(); got != 0 {
		t.Fatalf("prepared-resume refusal consumed the receipt: DeleteIf calls = %d, want 0", got)
	}

	// Publish the prepared target (the crash happened after the durable
	// write but before active publication) — a response-loss replay now sees
	// the target ACTIVE.
	if _, err := countingReg.Rollback(ctx, agentQuad(), testAgentID, targetRevisionID, agentcfg.ConfigScopeAgent, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("publish prepared target: %v", err)
	}
	if _, err := retry.AgentPacksCommit(guarded, commit); !errors.Is(err, agentcfgprotocol.ErrBootPackOwned) {
		t.Fatalf("response-loss replay commit error = %v, want ErrBootPackOwned", err)
	}
	if got := countingReg.setRevisionCalls(); got != setBefore {
		t.Fatalf("replay refusal mutated the revision: SetRevision calls = %d, want %d", got, setBefore)
	}
	if got := countingProposals.deleteIfCalls(); got != 0 {
		t.Fatalf("replay refusal consumed the receipt: DeleteIf calls = %d, want 0", got)
	}
	// The published target stays active — the refusal never compensates it.
	active, activeSet, err := countingReg.Active(ctx, agentQuad(), testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !activeSet || active.RevisionID != targetRevisionID {
		t.Fatalf("active after replay refusal = %+v (set=%t) err=%v, want the published target", active, activeSet, err)
	}
}

func catalogFor(t *testing.T) tools.ToolCatalog {
	t.Helper()
	catalog := tools.NewCatalog()
	if err := catalog.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "guard_tool"}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) { return tools.ToolResult{}, nil }}); err != nil {
		t.Fatalf("register catalog tool: %v", err)
	}
	return catalog
}

func TestAgentPacksCommit_PayloadCarryingBootOwnedShadowRejectedBeforePublication(t *testing.T) {
	ctx := context.Background()
	reg := &countingRegistry{Registry: newRegistry(t)}
	proposals := &countingStateStore{StateStore: mustProposalStore(t)}
	s, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithAgentPackProposer(agentPackTestProposer{item: packItem("other")}),
		agentcfgprotocol.WithAgentPackProposalState(proposals),
		agentcfgprotocol.WithAgentPackCatalog(catalogFor(t)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	// Seed a legacy durable revision shadow of the boot-owned name.
	if _, err := s.AgentPacksUpsert(ctx, upsertPackRequest("playbook")); err != nil {
		t.Fatalf("seed durable shadow: %v", err)
	}
	active, activeSet, err := reg.Active(ctx, agentQuad(), testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !activeSet {
		t.Fatalf("load active revision: set=%t err=%v", activeSet, err)
	}
	proposal, err := s.AgentPacksPropose(ctx, prototypes.AgentConfigAgentPacksProposeRequest{
		Identity: scope(), AgentID: testAgentID, Intent: "make a playbook", ExpectedContentHash: active.ContentHash,
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	commit := prototypes.AgentConfigAgentPacksCommitRequest{
		Identity: scope(), AgentID: testAgentID, Skill: proposal.Skill, ReviewedHash: proposal.Hash,
		Provenance: proposal.Provenance, ProposalID: proposal.ProposalID, ExpectedContentHash: proposal.ExpectedContentHash,
	}
	// The committed item "other" is NOT boot-owned, but the composed payload
	// carries the boot-owned shadow "playbook" forward — the pre-publication
	// guard must refuse the whole publication, so no proposal path can
	// reintroduce a boot-owned name into the durable revision.
	guarded := bootOwnedContext(bootOwner(testAgentID, "playbook"))
	setBefore := reg.setRevisionCalls()
	if _, err := s.AgentPacksCommit(guarded, commit); !errors.Is(err, agentcfgprotocol.ErrBootPackOwned) {
		t.Fatalf("commit carrying boot-owned shadow error = %v, want ErrBootPackOwned", err)
	}
	if got := reg.setRevisionCalls(); got != setBefore {
		t.Fatalf("shadow-carrying commit mutated the revision: SetRevision calls = %d, want %d", got, setBefore)
	}
	if got := proposals.saveIfCalls(); got != 0 {
		t.Fatalf("shadow-carrying commit marked the receipt committing: SaveIf calls = %d, want 0", got)
	}
	if got := proposals.deleteIfCalls(); got != 0 {
		t.Fatalf("shadow-carrying commit consumed the receipt: DeleteIf calls = %d, want 0", got)
	}
}

func TestAgentPacksBootGuards_RollbackHelperRefusesBootOwnedTarget(t *testing.T) {
	owner := bootOwner(testAgentID, "playbook")
	// Nil reader → inert (no baseline bound on this runtime).
	if err := agentcfgprotocol.GuardBootOwnedRevision(nil, "t", testAgentID, []skills.AgentPackItem{packItem("playbook")}); err != nil {
		t.Fatalf("nil owner must be inert, got %v", err)
	}
	// Empty target → pass.
	if err := agentcfgprotocol.GuardBootOwnedRevision(owner, "t", testAgentID, nil); err != nil {
		t.Fatalf("empty target must pass, got %v", err)
	}
	// Non-owned names → pass.
	if err := agentcfgprotocol.GuardBootOwnedRevision(owner, "t", testAgentID, []skills.AgentPackItem{packItem("other")}); err != nil {
		t.Fatalf("non-owned target must pass, got %v", err)
	}
	// An owned name — even in non-canonical casing — is refused with the
	// typed error naming it.
	if err := agentcfgprotocol.GuardBootOwnedRevision(owner, "t", testAgentID, []skills.AgentPackItem{packItem("other"), packItem("PLAYBOOK ")}); !errors.Is(err, agentcfgprotocol.ErrBootPackOwned) {
		t.Fatalf("owned target error = %v, want ErrBootPackOwned", err)
	}
	// The key is EXACT (tenant, agent): a foreign agent or tenant never
	// refuses.
	if err := agentcfgprotocol.GuardBootOwnedRevision(owner, "t", "agent-y", []skills.AgentPackItem{packItem("playbook")}); err != nil {
		t.Fatalf("foreign agent must pass, got %v", err)
	}
	if err := agentcfgprotocol.GuardBootOwnedRevision(owner, "other-tenant", testAgentID, []skills.AgentPackItem{packItem("playbook")}); err != nil {
		t.Fatalf("foreign tenant must pass, got %v", err)
	}
}

func TestAgentPacksRollback_BootOwnedTargetRefusedNoMutation(t *testing.T) {
	ctx := context.Background()
	reg := &countingRegistry{Registry: newRegistry(t)}
	s, err := agentcfgprotocol.NewService(reg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := reg.SetRevision(ctx, agentQuad(), testAgentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("seed base revision: %v", err)
	}
	// Seed a legacy durable revision SHADOW of the now-boot-owned name — the
	// rollback target. Its pack content is byte-identical to the boot entry
	// (the same-hash state): an equal hash proves nothing, boot wins.
	shadow, err := s.AgentPacksUpsert(ctx, upsertPackRequest("playbook"))
	if err != nil {
		t.Fatalf("seed durable shadow: %v", err)
	}
	// Move the active pointer OFF the shadow revision (a free-name upsert),
	// so a rollback back to the shadow revision is a REAL repoint.
	if _, err := s.AgentPacksUpsert(ctx, upsertPackRequest("other")); err != nil {
		t.Fatalf("seed head revision: %v", err)
	}
	active, activeSet, err := reg.Active(ctx, agentQuad(), testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !activeSet {
		t.Fatalf("load active revision: set=%t err=%v", activeSet, err)
	}
	rollbackCallsBefore := reg.rollbackCalls()

	guarded := bootOwnedContext(bootOwner(testAgentID, "playbook"))
	rollback := func(revisionID string) error {
		_, err := s.Rollback(guarded, prototypes.AgentConfigRollbackRequest{
			Identity: scope(), AgentID: testAgentID, RevisionID: revisionID,
		})
		return err
	}
	// The shadow revision contains a boot-owned canonical name → typed
	// refusal BEFORE any repoint, even at equal hash.
	if err := rollback(shadow.Revision.RevisionID); !errors.Is(err, agentcfgprotocol.ErrBootPackOwned) {
		t.Fatalf("rollback to boot-owned shadow error = %v, want ErrBootPackOwned", err)
	}
	// The ACTIVE revision still carries the same boot-owned shadow (the head
	// upsert preserved it) — the same-hash state where the target's pack
	// section equals the boot entry bytes. Refused identically.
	if err := rollback(active.RevisionID); !errors.Is(err, agentcfgprotocol.ErrBootPackOwned) {
		t.Fatalf("rollback to same-hash boot-owned target error = %v, want ErrBootPackOwned", err)
	}
	// No registry repoint ever ran and the active pointer never moved.
	if got := reg.rollbackCalls(); got != rollbackCallsBefore {
		t.Fatalf("refused rollbacks mutated the pointer: registry Rollback calls = %d, want %d", got, rollbackCallsBefore)
	}
	after, afterSet, err := reg.Active(ctx, agentQuad(), testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !afterSet || after.RevisionID != active.RevisionID {
		t.Fatalf("active after refused rollbacks = %+v (set=%t) err=%v, want unchanged %s", after, afterSet, err, active.RevisionID)
	}
}

func TestAgentPacksRollback_BootFreeTargetPermitted(t *testing.T) {
	ctx := context.Background()
	reg := &countingRegistry{Registry: newRegistry(t)}
	s, err := agentcfgprotocol.NewService(reg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	base, err := reg.SetRevision(ctx, agentQuad(), testAgentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("seed base revision: %v", err)
	}
	// Seed a durable shadow so the SAME guarded context faces a boot-owned
	// active revision; the rollback target (base) carries no boot-owned name.
	if _, err := s.AgentPacksUpsert(ctx, upsertPackRequest("playbook")); err != nil {
		t.Fatalf("seed durable shadow: %v", err)
	}
	rollbackCallsBefore := reg.rollbackCalls()

	guarded := bootOwnedContext(bootOwner(testAgentID, "playbook"))
	// A revision without boot-owned names is fully rollback-able under the
	// SAME boot-owned reader — the guard only refuses boot-owned targets.
	got, err := s.Rollback(guarded, prototypes.AgentConfigRollbackRequest{
		Identity: scope(), AgentID: testAgentID, RevisionID: base.RevisionID,
	})
	if err != nil {
		t.Fatalf("rollback to boot-free target failed: %v", err)
	}
	if got.Revision.RevisionID != base.RevisionID {
		t.Fatalf("rolled back revision = %q, want %q", got.Revision.RevisionID, base.RevisionID)
	}
	if got := reg.rollbackCalls(); got != rollbackCallsBefore+1 {
		t.Fatalf("permitted rollback must record exactly one repoint: got %d, want %d", got, rollbackCallsBefore+1)
	}
	// No reader bound (a pre-baseline runtime): rolling back to a revision
	// carrying the boot-owned shadow stays fully permitted — the guard is
	// inert without a baseline, so the door keeps its exact pre-baseline
	// behavior.
	shadow, err := s.AgentPacksUpsert(ctx, upsertPackRequest("playbook"))
	if err != nil {
		t.Fatalf("re-seed durable shadow: %v", err)
	}
	if _, err := s.Rollback(ctx, prototypes.AgentConfigRollbackRequest{
		Identity: scope(), AgentID: testAgentID, RevisionID: shadow.Revision.RevisionID,
	}); err != nil {
		t.Fatalf("nil-reader rollback to boot-owned target must retain pre-baseline behavior, got %v", err)
	}
}

func TestAgentPacksBootGuards_KeyedExactTenantAgent(t *testing.T) {
	reg := &countingRegistry{Registry: newRegistry(t)}
	s, err := agentcfgprotocol.NewService(reg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	owner := bootOwner(testAgentID, "playbook")
	guarded := bootOwnedContext(owner)
	// The reader owns "playbook" for (t, agent-x) ONLY — the same name for a
	// different agent or a different tenant is not boot-owned and must be
	// fully mutable. The guard still CONSULTS the reader (the foreign lookups
	// count against it); the reader's exact key is what declines.
	if _, err := s.AgentPacksUpsert(guarded, upsertPackRequestFor(scope(), "agent-y", "playbook")); err != nil {
		t.Fatalf("same name under a different agent must not be refused: %v", err)
	}
	foreignTenant := prototypes.IdentityScope{Tenant: "other", User: "u", Session: "s"}
	if _, err := s.AgentPacksUpsert(guarded, upsertPackRequestFor(foreignTenant, testAgentID, "playbook")); err != nil {
		t.Fatalf("same name under a different tenant must not be refused: %v", err)
	}
	if owner.ownsCalls() == 0 {
		t.Fatal("guard never consulted the injected reader")
	}
	// And the exact pair IS refused (control).
	if _, err := s.AgentPacksUpsert(guarded, upsertPackRequest("playbook")); !errors.Is(err, agentcfgprotocol.ErrBootPackOwned) {
		t.Fatalf("exact-pair upsert error = %v, want ErrBootPackOwned", err)
	}
}

func TestAgentPacksList_DurableRevisionAuthoringOnly(t *testing.T) {
	ctx := context.Background()
	reg := &countingRegistry{Registry: newRegistry(t)}
	s, err := agentcfgprotocol.NewService(reg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	guarded := bootOwnedContext(bootOwner(testAgentID, "playbook"))
	// A boot-only name (owned, no durable shadow) never appears in the list.
	list, err := s.AgentPacksList(guarded, listPackRequest())
	if err != nil || len(list.Items) != 0 {
		t.Fatalf("list with boot-only name = %+v err=%v, want empty (durable-revision authoring only)", list.Items, err)
	}
	// A legacy durable shadow of the boot-owned name DOES appear — the list
	// reflects the durable revision, exactly like any other revisioned item.
	if _, err := s.AgentPacksUpsert(ctx, upsertPackRequest("playbook")); err != nil {
		t.Fatalf("seed durable shadow: %v", err)
	}
	list, err = s.AgentPacksList(guarded, listPackRequest())
	if err != nil || len(list.Items) != 1 || list.Items[0].Name != "playbook" {
		t.Fatalf("list with durable shadow = %+v err=%v, want the shadow item", list.Items, err)
	}
}

func TestAgentPacksBootGuards_ConfigRemovalSemantics(t *testing.T) {
	ctx := context.Background()
	reg := &countingRegistry{Registry: newRegistry(t)}
	s, err := agentcfgprotocol.NewService(reg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	// Previous deployment declared the baseline (seeded the durable shadow);
	// the CURRENT deployment removed the baseline from the boot config, so
	// the injected reader no longer owns the name. The legacy durable
	// revision remains and is fully mutable — config removal removes boot
	// only, on the next deployment.
	if _, err := s.AgentPacksUpsert(ctx, upsertPackRequest("playbook")); err != nil {
		t.Fatalf("seed legacy durable revision: %v", err)
	}
	currentDeployment := bootOwnedContext(bootOwner("t", testAgentID)) // owns nothing
	// Upsert is fully mutable again.
	if _, err := s.AgentPacksUpsert(currentDeployment, upsertPackRequest("playbook")); err != nil {
		t.Fatalf("upsert after config removal must succeed: %v", err)
	}
	// Remove of the still-present name succeeds normally.
	if _, err := s.AgentPacksRemove(currentDeployment, removePackRequest("playbook")); err != nil {
		t.Fatalf("remove after config removal must succeed: %v", err)
	}
	// Remove of an absent name is the pre-baseline not-found, never a boot
	// refusal.
	if _, err := s.AgentPacksRemove(currentDeployment, removePackRequest("playbook")); !errors.Is(err, agentcfgprotocol.ErrAgentPackNotFound) {
		t.Fatalf("remove absent name after config removal error = %v, want ErrAgentPackNotFound", err)
	}
	// The legacy durable revision was read and rewritten, never erased: the
	// revision chain still holds the seeded playbook revision (config removal
	// removes boot only on the next deployment; durable revisions remain).
	revisions, err := reg.ListRevisions(ctx, agentQuad(), testAgentID, agentcfg.ConfigScopeAgent, 0)
	if err != nil || len(revisions) < 2 {
		t.Fatalf("revision chain after config-removal mutations = %d err=%v, want ≥2 (seed + remove)", len(revisions), err)
	}
	legacyPlaybookRetained := false
	for _, rev := range revisions {
		for _, item := range rev.Payload.AgentPacks {
			if skills.CanonicalPackName(item.Name) == "playbook" {
				legacyPlaybookRetained = true
			}
		}
	}
	if !legacyPlaybookRetained {
		t.Fatal("config removal erased the legacy durable playbook revision; it must remain")
	}
}

// TestAgentPacksBootGuards_ConcurrentMixedGuards is the concurrent-reuse
// gate for the guards: N>=100 mixed guard invocations against ONE shared
// Service + reader + registry under -race. Every rejection must be the typed
// refusal with ZERO mutation calls; every allowed mutation must succeed; the
// final durable revision must hold exactly the free-name upserts and never a
// boot-owned name; the goroutine baseline must be restored.
func TestAgentPacksBootGuards_ConcurrentMixedGuards(t *testing.T) {
	ctx := context.Background()
	reg := &countingRegistry{Registry: newRegistry(t)}
	proposals := &countingStateStore{StateStore: mustProposalStore(t)}
	s, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithAgentPackProposer(agentPackTestProposer{item: packItem("playbook")}),
		agentcfgprotocol.WithAgentPackProposalState(proposals),
		agentcfgprotocol.WithAgentPackCatalog(catalogFor(t)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	// The reader owns "playbook" for BOTH the upsert/remove/list leg
	// (agent-x) and the commit leg (agent-c, whose base stays frozen).
	owner := bootOwner(testAgentID, "playbook")
	for key, value := range bootOwner("agent-c", "playbook").ownedByKey {
		owner.ownedByKey[key] = value
	}
	guarded := bootOwnedContext(owner)

	// Seed agent-c's stable base so every propose's expected-revision token
	// stays valid while agent-x mutates concurrently.
	if _, err := reg.SetRevision(ctx, agentQuad(), "agent-c", agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("seed agent-c base: %v", err)
	}
	baseC, baseCSet, err := reg.Active(ctx, agentQuad(), "agent-c", agentcfg.ConfigScopeAgent)
	if err != nil || !baseCSet {
		t.Fatalf("load agent-c base: set=%t err=%v", baseCSet, err)
	}

	const n = 125 // 25 goroutines per leg — N>=100, 5 legs
	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 5 {
			case 0: // upsert of a boot-owned name → typed refusal, zero mutation
				_, err := s.AgentPacksUpsert(guarded, upsertPackRequest("playbook"))
				if !errors.Is(err, agentcfgprotocol.ErrBootPackOwned) {
					errs <- fmt.Errorf("upsert boot-owned: %w", err)
				}
			case 1: // remove of a boot-only name → typed refusal, zero mutation
				_, err := s.AgentPacksRemove(guarded, removePackRequest("playbook"))
				if !errors.Is(err, agentcfgprotocol.ErrBootPackOwned) {
					errs <- fmt.Errorf("remove boot-only: %w", err)
				}
			case 2: // upsert of a distinct free name → success
				if _, err := s.AgentPacksUpsert(guarded, upsertPackRequest(fmt.Sprintf("free-%d", i))); err != nil {
					errs <- fmt.Errorf("upsert free-%d: %w", i, err)
				}
			case 3: // list → success, boot-only names never appear
				list, err := s.AgentPacksList(guarded, listPackRequest())
				if err != nil {
					errs <- fmt.Errorf("list: %w", err)
					return
				}
				for _, item := range list.Items {
					if skills.CanonicalPackName(item.Name) == "playbook" {
						errs <- fmt.Errorf("list surfaced boot-owned name %q", item.Name)
						return
					}
				}
			case 4: // propose+commit of a boot-owned name → commit typed refusal
				proposal, err := s.AgentPacksPropose(guarded, prototypes.AgentConfigAgentPacksProposeRequest{
					Identity: scope(), AgentID: "agent-c", Intent: "make a playbook", ExpectedContentHash: baseC.ContentHash,
				})
				if err != nil {
					errs <- fmt.Errorf("propose: %w", err)
					return
				}
				_, err = s.AgentPacksCommit(guarded, prototypes.AgentConfigAgentPacksCommitRequest{
					Identity: scope(), AgentID: "agent-c", Skill: proposal.Skill, ReviewedHash: proposal.Hash,
					Provenance: proposal.Provenance, ProposalID: proposal.ProposalID, ExpectedContentHash: proposal.ExpectedContentHash,
				})
				if !errors.Is(err, agentcfgprotocol.ErrBootPackOwned) {
					errs <- fmt.Errorf("commit boot-owned: %w", err)
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent mixed guards: %v", err)
	}

	// Exactly the free-name upserts mutated the revision: 25 SetRevision
	// calls (agent-x free upserts) + 1 (the agent-c base seed above); every
	// rejection contributed zero. The commit leg's proposes wrote 25 proposal
	// receipts and ZERO receipt advances/consumptions.
	if got := reg.setRevisionCalls(); got != 26 {
		t.Fatalf("SetRevision calls = %d, want 26 (25 free upserts + agent-c base seed; rejections mutate nothing)", got)
	}
	if got := proposals.saveCalls(); got != 25 {
		t.Fatalf("proposal Save calls = %d, want 25 (one propose per commit leg)", got)
	}
	if got := proposals.saveIfCalls(); got != 0 {
		t.Fatalf("proposal SaveIf calls = %d, want 0 (no commit advanced a receipt)", got)
	}
	if got := proposals.deleteIfCalls(); got != 0 {
		t.Fatalf("proposal DeleteIf calls = %d, want 0 (no commit consumed a receipt)", got)
	}

	// Final durable revision: all 25 free names present, boot-owned name
	// absent.
	list, err := s.AgentPacksList(ctx, listPackRequest())
	if err != nil {
		t.Fatalf("final list: %v", err)
	}
	if len(list.Items) != 25 {
		t.Fatalf("final pack size = %d, want 25", len(list.Items))
	}
	for _, item := range list.Items {
		if skills.CanonicalPackName(item.Name) == "playbook" {
			t.Fatalf("final revision contains boot-owned name %q", item.Name)
		}
	}

	// Goroutine baseline restored — no guard path leaks (§11).
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
