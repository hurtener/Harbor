package stream_test

// agentconfig_bootpack_handler_test.go — the typed wire mapping for the
// boot-owned agent-pack mutation guards: upsert / commit / rollback refusals
// and the boot-only remove refusal all surface as the SAME typed
// client-visible 400/read-only family as the boot-declared connection
// precedent (CodeInvalidRequest / 400 naming the owned pack) — never a
// generic 500 and never a leaked internal failure.
//
// The boot-ownership reader is injected per request through the context seam
// ([agentcfgprotocol.WithBootOwnership]), exactly as the integration owner
// will wire it once the immutable boot index flows into requests.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/runsnapshot"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	"github.com/hurtener/Harbor/internal/skills"
	localdb "github.com/hurtener/Harbor/internal/skills/drivers/localdb"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
)

// packBootOwner is the narrow injected reader for the boot-pack wire tests:
// an exact (tenant, agent, canonical-name) ownership set mirroring the eager
// bootpacks.Index OwnsName key (internal/skills/bootpacks). A name is owned
// ONLY for the exact pairs registered — a foreign tenant or agent is never
// refused.
type packBootOwner map[string]bool

func packOwnerKey(tenantID, agentID, name string) string {
	return tenantID + "\x00" + agentID + "\x00" + skills.CanonicalPackName(name)
}

func (o packBootOwner) OwnsName(tenantID, agentID, name string) bool {
	return o[packOwnerKey(tenantID, agentID, name)]
}

// packOwnerFor builds a reader that owns names for ONE exact (tenant, agent)
// pair.
func packOwnerFor(tenantID, agentID string, names ...string) packBootOwner {
	o := packBootOwner{}
	for _, n := range names {
		o[packOwnerKey(tenantID, agentID, n)] = true
	}
	return o
}

// acReqOwned is acReqReach plus the injected per-request boot-ownership
// reader (the context seam the guard code consumes; the handler forwards
// r.Context() into every service verb).
func acReqOwned(t *testing.T, h http.Handler, route, body string, id *identity.Identity, scopes []auth.Scope, owner agentcfgprotocol.BootOwnership) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/agent_config/"+route, strings.NewReader(body))
	if id != nil {
		req.Header.Set(stream.HeaderTenant, id.TenantID)
		req.Header.Set(stream.HeaderUser, id.UserID)
		req.Header.Set(stream.HeaderSession, id.SessionID)
	}
	if scopes != nil {
		req = req.WithContext(auth.WithScopes(req.Context(), scopes))
	}
	if id != nil {
		req = req.WithContext(auth.WithAgentReach(req.Context(), []string{acAgent}))
	}
	req = req.WithContext(agentcfgprotocol.WithBootOwnership(req.Context(), owner))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// packTestItem is a minimal valid pack item body.
func packTestItem(name string) skills.AgentPackItem {
	return skills.AgentPackItem{Name: name, Trigger: "trigger", Steps: []string{"step"}}
}

// packTestProposer is the deterministic draft seam: it always drafts the
// configured item, so propose/commit through the wire stay fully reachable
// without an LLM.
type packTestProposer struct {
	item skills.AgentPackItem
}

func (p packTestProposer) Draft(context.Context, identity.Quadruple, string, string, string, agentcfgprotocol.AgentPackAuthoringPolicy) (agentcfgprotocol.AgentPackDraft, error) {
	return agentcfgprotocol.AgentPackDraft{Item: p.item}, nil
}

// newPackSessionHandlerFixture is the standard agent-config wire harness
// (see newSessionHandlerFixture) PLUS the governed pack stack — proposer,
// durable proposal ledger, and the visible capability catalog — so the
// propose/commit legs are reachable through the HTTP surface.
func newPackSessionHandlerFixture(t *testing.T) sessionHandlerFixture {
	t.Helper()
	ctx := context.Background()
	rawState, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	st := &stateMutationSpy{StateStore: rawState}
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32,
		IdleTimeout: 30 * time.Second, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	reg, err := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	skStore, err := localdb.New(skills.ConfigSnapshot{Driver: "localdb", DSN: ":memory:"}, skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("skills: %v", err)
	}
	ov, err := sessionoverlay.NewStore(st, nil)
	if err != nil {
		t.Fatalf("overlay: %v", err)
	}
	authority, err := serve.NewSessionPersonalSkillAuthority(ctx, st, skStore, []config.SessionPersonalCutoverTenant{{
		TenantID: "t1", Epoch: "handler-test", RosterDigest: "empty-legacy-roster", LegacyWritersDrained: true,
	}})
	if err != nil {
		t.Fatalf("session personal authority: %v", err)
	}
	if _, err := reg.SetRevision(ctx, identity.Quadruple{Identity: *acID()}, acAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("activate agent lifecycle: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(ctx)
		_ = skStore.Close(ctx)
		_ = ov.Close(ctx)
		_ = bus.Close(ctx)
		_ = st.Close(ctx)
	})
	catalog := tools.NewCatalog()
	if err := catalog.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "guard_tool"}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) { return tools.ToolResult{}, nil }}); err != nil {
		t.Fatalf("register catalog tool: %v", err)
	}
	svc, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithSkillStore(skStore),
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithSessionOverlay(ov),
		agentcfgprotocol.WithSessionPersonalSkillController(authority.Controller),
		agentcfgprotocol.WithRunSnapshotGate(runsnapshot.NewGate()),
		agentcfgprotocol.WithAgentPackProposer(packTestProposer{item: packTestItem("playbook")}),
		agentcfgprotocol.WithAgentPackProposalState(st),
		agentcfgprotocol.WithAgentPackCatalog(catalog),
	)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	h, err := stream.NewAgentConfigHandler(svc,
		stream.WithAgentConfigReachAuthorizer(auth.NewAgentReachAuthorizer()))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	return sessionHandlerFixture{handler: h, state: st, registry: reg, mutations: st}
}

// TestAgentConfigHandler_AgentPacksUpsert_BootOwnedTypedRefusal proves the
// upsert of a boot-owned canonical name is a typed 400/read-only wire
// refusal naming the pack — and that a free name stays fully mutable under
// the same reader.
func TestAgentConfigHandler_AgentPacksUpsert_BootOwnedTypedRefusal(t *testing.T) {
	h := sessionHandler(t)
	owner := packOwnerFor("t1", acAgent, "playbook")
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x","skill":{"name":"playbook","trigger":"trigger","steps":["step"]}}`
	code, resp := acReqOwned(t, h, "agent_packs/upsert", body, acID(), []auth.Scope{auth.ScopeAdmin}, owner)
	if code != http.StatusBadRequest {
		t.Fatalf("boot-owned upsert status = %d body=%s, want 400", code, resp)
	}
	if c := errCode(t, resp); c != protoerrors.CodeInvalidRequest {
		t.Fatalf("boot-owned upsert code = %q, want %q", c, protoerrors.CodeInvalidRequest)
	}
	if !strings.Contains(string(resp), "playbook") {
		t.Fatalf("boot-owned upsert message %s does not name the owned pack", resp)
	}
	// Control: the same body under a name the baseline does not own succeeds.
	free := strings.Replace(body, "playbook", "free-pack", 1)
	if code, resp := acReq(t, h, "agent_packs/upsert", free, acID(), []auth.Scope{auth.ScopeAdmin}); code != http.StatusOK {
		t.Fatalf("free-name upsert status = %d body=%s, want 200", code, resp)
	}
}

// TestAgentConfigHandler_AgentPacksRemove_BootOnlyTypedRefusal proves a
// boot-only remove (owned by the baseline, absent from the durable revision)
// is the typed 400 read-only refusal — DISTINCT from the not-found 404 a
// non-boot-only absent name would produce.
func TestAgentConfigHandler_AgentPacksRemove_BootOnlyTypedRefusal(t *testing.T) {
	h := sessionHandler(t)
	owner := packOwnerFor("t1", acAgent, "playbook")
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x","name":"playbook"}`
	code, resp := acReqOwned(t, h, "agent_packs/remove", body, acID(), []auth.Scope{auth.ScopeAdmin}, owner)
	if code != http.StatusBadRequest {
		t.Fatalf("boot-only remove status = %d body=%s, want 400 (typed read-only refusal, never 404)", code, resp)
	}
	if c := errCode(t, resp); c != protoerrors.CodeInvalidRequest {
		t.Fatalf("boot-only remove code = %q, want %q", c, protoerrors.CodeInvalidRequest)
	}
	if !strings.Contains(string(resp), "playbook") {
		t.Fatalf("boot-only remove message %s does not name the owned pack", resp)
	}
}

// TestAgentConfigHandler_AgentPacksRollback_BootOwnedTypedRefusal proves the
// rollback door refuses (typed 400) a target revision whose agent_packs
// carries a boot-owned canonical name — the Service.Rollback guard surfaced
// through the wire.
func TestAgentConfigHandler_AgentPacksRollback_BootOwnedTypedRefusal(t *testing.T) {
	h := sessionHandler(t)
	upsertBody := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x","skill":{"name":"playbook","trigger":"trigger","steps":["step"]}}`
	code, resp := acReq(t, h, "agent_packs/upsert", upsertBody, acID(), []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusOK {
		t.Fatalf("seed shadow upsert status=%d body=%s", code, resp)
	}
	var seeded prototypes.AgentConfigAgentPacksUpsertResponse
	if err := json.Unmarshal(resp, &seeded); err != nil {
		t.Fatalf("decode seed upsert: %v body=%s", err, resp)
	}
	shadowRev := seeded.Revision.RevisionID
	// Move the active pointer OFF the shadow revision so the rollback back to
	// it is a REAL repoint.
	headBody := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x","skill":{"name":"other","trigger":"trigger","steps":["step"]}}`
	if code, resp := acReq(t, h, "agent_packs/upsert", headBody, acID(), []auth.Scope{auth.ScopeAdmin}); code != http.StatusOK {
		t.Fatalf("seed head upsert status=%d body=%s", code, resp)
	}
	owner := packOwnerFor("t1", acAgent, "playbook")
	rollbackBody := fmt.Sprintf(`{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x","revision_id":%q}`, shadowRev)
	code, resp = acReqOwned(t, h, "rollback", rollbackBody, acID(), []auth.Scope{auth.ScopeAdmin}, owner)
	if code != http.StatusBadRequest {
		t.Fatalf("boot-owned rollback status = %d body=%s, want 400", code, resp)
	}
	if c := errCode(t, resp); c != protoerrors.CodeInvalidRequest {
		t.Fatalf("boot-owned rollback code = %q, want %q", c, protoerrors.CodeInvalidRequest)
	}
	if !strings.Contains(string(resp), "playbook") {
		t.Fatalf("boot-owned rollback message %s does not name the owned pack", resp)
	}
}

// TestAgentConfigHandler_AgentPacksCommit_BootOwnedTypedRefusal proves the
// governed commit of a boot-owned proposal is a typed 400 wire refusal
// through the full propose → commit flow.
func TestAgentConfigHandler_AgentPacksCommit_BootOwnedTypedRefusal(t *testing.T) {
	fixture := newPackSessionHandlerFixture(t)
	h := fixture.handler
	active, _, err := fixture.registry.Active(context.Background(), identity.Quadruple{Identity: *acID()}, acAgent, agentcfg.ConfigScopeAgent)
	if err != nil {
		t.Fatalf("load active base: %v", err)
	}
	proposeBody := fmt.Sprintf(`{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x","intent":"make a playbook","expected_content_hash":%q}`, active.ContentHash)
	code, resp := acReq(t, h, "agent_packs/propose", proposeBody, acID(), []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusOK {
		t.Fatalf("propose status=%d body=%s, want 200 (unowned request)", code, resp)
	}
	var proposed prototypes.AgentConfigAgentPacksProposeResponse
	if err := json.Unmarshal(resp, &proposed); err != nil {
		t.Fatalf("decode propose: %v body=%s", err, resp)
	}
	skillJSON, err := json.Marshal(proposed.Skill)
	if err != nil {
		t.Fatalf("encode proposed skill: %v", err)
	}
	commitBody := fmt.Sprintf(`{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x","skill":%s,"reviewed_hash":%q,"provenance":%q,"proposal_id":%q,"expected_content_hash":%q}`,
		skillJSON, proposed.Hash, proposed.Provenance, proposed.ProposalID, proposed.ExpectedContentHash)
	owner := packOwnerFor("t1", acAgent, "playbook")
	code, resp = acReqOwned(t, h, "agent_packs/commit", commitBody, acID(), []auth.Scope{auth.ScopeAdmin}, owner)
	if code != http.StatusBadRequest {
		t.Fatalf("boot-owned commit status = %d body=%s, want 400", code, resp)
	}
	if c := errCode(t, resp); c != protoerrors.CodeInvalidRequest {
		t.Fatalf("boot-owned commit code = %q, want %q", c, protoerrors.CodeInvalidRequest)
	}
	if !strings.Contains(string(resp), "playbook") {
		t.Fatalf("boot-owned commit message %s does not name the owned pack", resp)
	}
}
