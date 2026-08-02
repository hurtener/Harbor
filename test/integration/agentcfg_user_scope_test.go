package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// This integration test wires the USER-scope durable config-variant tier (the
// middle tier of the authorization matrix) end-to-end with REAL drivers: the
// StateStore-backed registry, the in-memory bus, the real Protocol Service +
// wire handler, across three verified contexts (admin, user-scoped, plain
// session). It proves: an admin sets agent-level config; a user-scoped caller
// owns a durable variant (set→list→diff→rollback); user B cannot see or roll
// back user A's revision (cross-user isolation); a plain-session token is
// rejected on the user route (the scope gate); a verified user id equal to the
// reserved sentinel is rejected (defence-in-depth); a forged widening field is
// rejected at the wire; identity propagates through the edge. Runs under -race.

const (
	usTenant = "tenant-us"
	usAgent  = "agent-us"
)

type usHarness struct {
	handler    http.Handler
	registry   agentcfg.Registry
	state      state.StateStore
	bus        events.EventBus
	agentReach []string
}

func newUSHarness(t *testing.T) *usHarness {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 32, SubscriberBufferSize: 256,
		IdleTimeout: time.Minute, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	// The harness reach declares both agents. Activate their real agent-level
	// lifecycle once so the shared session-overlay projection remains fenced.
	activateFixtureAgent(t, reg, usID("fixture", "bootstrap"), usAgent)
	activateFixtureAgent(t, reg, usID("fixture", "bootstrap"), waveAgentID)
	svc, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithBus(bus))
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	h, err := stream.NewAgentConfigHandler(svc,
		stream.WithAgentConfigReachAuthorizer(auth.NewAgentReachAuthorizer()))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = st.Close(context.Background())
	})
	return &usHarness{handler: h, registry: reg, state: st, bus: bus, agentReach: []string{usAgent, waveAgentID}}
}

func (h *usHarness) call(t *testing.T, path string, body any, id identity.Identity, scopes []auth.Scope) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	req.Header.Set(stream.HeaderTenant, id.TenantID)
	req.Header.Set(stream.HeaderUser, id.UserID)
	req.Header.Set(stream.HeaderSession, id.SessionID)
	ctx := auth.WithScopes(req.Context(), scopes)
	ctx = auth.WithAgentReach(ctx, h.agentReach)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func (h *usHarness) rawCall(t *testing.T, path, body string, id identity.Identity, scopes []auth.Scope) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	req.Header.Set(stream.HeaderTenant, id.TenantID)
	req.Header.Set(stream.HeaderUser, id.UserID)
	req.Header.Set(stream.HeaderSession, id.SessionID)
	ctx := auth.WithScopes(req.Context(), scopes)
	ctx = auth.WithAgentReach(ctx, h.agentReach)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func usID(user, session string) identity.Identity {
	return identity.Identity{TenantID: usTenant, UserID: user, SessionID: session}
}

func userScopes() []auth.Scope { return []auth.Scope{auth.ScopeAgentConfigUser} }

func TestE2E_AgentConfig_UserScopeTier(t *testing.T) {
	h := newUSHarness(t)
	alice := usID("alice", "sa")
	bob := usID("bob", "sb")

	// --- Admin sets agent-level config (the chain the user tier must not touch). ---
	_ = decode[prototypes.AgentConfigSetPromptLayersResponse](t, h.call(t, "/v1/agent_config/set_prompt_layers", prototypes.AgentConfigSetPromptLayersRequest{
		AgentID: usAgent, PromptLayers: prototypes.AgentConfigPromptLayers{Base: ptr("OPERATOR_BASE")},
	}, usID("admin", "sx"), adminScopes()))

	// --- User A sets a durable variant (set → list → diff → rollback). ---
	set1 := decode[prototypes.AgentConfigUserSetRevisionResponse](t, h.call(t, "/v1/agent_config/user/set_revision", prototypes.AgentConfigUserSetRevisionRequest{
		AgentID: usAgent, Payload: prototypes.AgentConfigUserPayload{UserPrompt: "be terse", DisabledServers: []string{"weather"}},
	}, alice, userScopes()))
	set2 := decode[prototypes.AgentConfigUserSetRevisionResponse](t, h.call(t, "/v1/agent_config/user/set_revision", prototypes.AgentConfigUserSetRevisionRequest{
		AgentID: usAgent, Payload: prototypes.AgentConfigUserPayload{UserPrompt: "be terse and kind", DisabledServers: []string{"weather"}},
	}, alice, userScopes()))

	list := decode[prototypes.AgentConfigUserListRevisionsResponse](t, h.call(t, "/v1/agent_config/user/list_revisions", prototypes.AgentConfigUserListRevisionsRequest{
		AgentID: usAgent,
	}, alice, userScopes()))
	if len(list.Revisions) != 2 || list.Revisions[0].RevisionID != set2.Revision.RevisionID {
		t.Fatalf("user list wrong: n=%d", len(list.Revisions))
	}
	// The author anchor records the REAL (tenant, user).
	if list.Revisions[0].AuthorTenant != usTenant || list.Revisions[0].AuthorUser != "alice" {
		t.Fatalf("author anchor not the real (tenant, user): %+v", list.Revisions[0])
	}

	diff := decode[prototypes.AgentConfigUserDiffResponse](t, h.call(t, "/v1/agent_config/user/diff", prototypes.AgentConfigUserDiffRequest{
		AgentID: usAgent, FromRevision: set1.Revision.RevisionID, ToRevision: set2.Revision.RevisionID,
	}, alice, userScopes()))
	if !diff.Diff.PromptLayers.UserChanged {
		t.Fatalf("diff did not surface the user-layer change")
	}

	rb := decode[prototypes.AgentConfigUserRollbackResponse](t, h.call(t, "/v1/agent_config/user/rollback", prototypes.AgentConfigUserRollbackRequest{
		AgentID: usAgent, RevisionID: set1.Revision.RevisionID,
	}, alice, userScopes()))
	if rb.Revision.RevisionID != set1.Revision.RevisionID {
		t.Fatalf("rollback returned %q want %q", rb.Revision.RevisionID, set1.Revision.RevisionID)
	}

	// --- Cross-user isolation: bob's get is empty; bob's rollback of alice's rev is not-found. ---
	bGet := decode[prototypes.AgentConfigUserGetResponse](t, h.call(t, "/v1/agent_config/user/get", prototypes.AgentConfigUserGetRequest{
		AgentID: usAgent,
	}, bob, userScopes()))
	if bGet.Set {
		t.Fatalf("bob sees alice's durable variant")
	}
	bRB := h.call(t, "/v1/agent_config/user/rollback", prototypes.AgentConfigUserRollbackRequest{
		AgentID: usAgent, RevisionID: set1.Revision.RevisionID,
	}, bob, userScopes())
	if bRB.Code != http.StatusNotFound {
		t.Fatalf("bob rollback of alice's rev should be 404; got %d", bRB.Code)
	}

	// --- The agent-level chain is untouched by the user writes. ---
	adminGet := decode[prototypes.AgentConfigGetResponse](t, h.call(t, "/v1/agent_config/get", prototypes.AgentConfigGetRequest{
		AgentID: usAgent,
	}, usID("admin", "sx"), adminScopes()))
	if !adminGet.Set || adminGet.Revision.Payload.PromptLayers == nil || adminGet.Revision.Payload.PromptLayers.Base == nil || *adminGet.Revision.Payload.PromptLayers.Base != "OPERATOR_BASE" {
		t.Fatalf("agent chain disturbed by user writes: %+v", adminGet.Revision)
	}

	// --- Scope gate: a plain-session token (no scope) is rejected on a user route. ---
	noScope := h.call(t, "/v1/agent_config/user/set_revision", prototypes.AgentConfigUserSetRevisionRequest{
		AgentID: usAgent, Payload: prototypes.AgentConfigUserPayload{UserPrompt: "x"},
	}, alice, []auth.Scope{})
	if noScope.Code != http.StatusForbidden {
		t.Fatalf("no-scope user write should be 403; got %d", noScope.Code)
	}
	if c := errCode(t, noScope); c != protoerrors.CodeScopeMismatch {
		t.Fatalf("no-scope user route error = %s, want %s", c, protoerrors.CodeScopeMismatch)
	}

	// --- Admin token (no user scope) is rejected on a user route. ---
	adminOnUser := h.call(t, "/v1/agent_config/user/set_revision", prototypes.AgentConfigUserSetRevisionRequest{
		AgentID: usAgent, Payload: prototypes.AgentConfigUserPayload{UserPrompt: "x"},
	}, usID("admin", "sx"), adminScopes())
	if adminOnUser.Code != http.StatusForbidden {
		t.Fatalf("admin-only token on user route should be 403; got %d", adminOnUser.Code)
	}

	// --- Sentinel rejection: a verified user id equal to the reserved slot. ---
	sentinel := h.call(t, "/v1/agent_config/user/set_revision", prototypes.AgentConfigUserSetRevisionRequest{
		AgentID: usAgent, Payload: prototypes.AgentConfigUserPayload{UserPrompt: "x"},
	}, usID("__agentcfg__", "ss"), userScopes())
	if sentinel.Code != http.StatusForbidden {
		t.Fatalf("reserved-sentinel user write should be 403; got %d body=%s", sentinel.Code, sentinel.Body.String())
	}

	// --- Widening: a forged extra field in the payload is rejected at the wire. ---
	forged := `{"identity":{"tenant":"tenant-us","user":"alice","session":"sa"},"agent_id":"agent-us","payload":{"user_prompt":"x","base":"HIJACK","connections":{}}}`
	rec := h.rawCall(t, "/v1/agent_config/user/set_revision", forged, alice, userScopes())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("forged widening field should be rejected at the wire (400); got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestE2E_AgentConfig_UserScope_ConcurrentIsolation runs N concurrent distinct
// users against ONE shared service+registry, asserting each user's durable
// variant reflects its OWN content (no cross-user bleed), under -race.
func TestE2E_AgentConfig_UserScope_ConcurrentIsolation(t *testing.T) {
	h := newUSHarness(t)
	const n = 24
	var wg sync.WaitGroup
	errs := make(chan string, n*2)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user := "u-" + itoa(i)
			want := "prompt-" + itoa(i)
			id := usID(user, "s")
			if rec := h.call(t, "/v1/agent_config/user/set_revision", prototypes.AgentConfigUserSetRevisionRequest{
				AgentID: usAgent, Payload: prototypes.AgentConfigUserPayload{UserPrompt: want},
			}, id, userScopes()); rec.Code != http.StatusOK {
				errs <- "set failed for " + user
				return
			}
			get := decode[prototypes.AgentConfigUserGetResponse](t, h.call(t, "/v1/agent_config/user/get", prototypes.AgentConfigUserGetRequest{
				AgentID: usAgent,
			}, id, userScopes()))
			if !get.Set || get.Revision.Payload.PromptLayers == nil || get.Revision.Payload.PromptLayers.User == nil || *get.Revision.Payload.PromptLayers.User != want {
				errs <- "cross-user bleed for " + user
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
