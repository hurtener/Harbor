package stream_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/runsnapshot"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	"github.com/hurtener/Harbor/internal/skills"
	localdb "github.com/hurtener/Harbor/internal/skills/drivers/localdb"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

type sessionHandlerFixture struct {
	handler   http.Handler
	state     state.StateStore
	registry  agentcfg.Registry
	mutations *stateMutationSpy
}

type stateMutationSpy struct {
	state.StateStore
	count atomic.Int64
}

func (s *stateMutationSpy) Save(ctx context.Context, record state.StateRecord) error {
	s.count.Add(1)
	return s.StateStore.Save(ctx, record)
}

func (s *stateMutationSpy) SaveIf(ctx context.Context, expectations []state.SlotExpectation, record state.StateRecord) error {
	s.count.Add(1)
	return s.StateStore.SaveIf(ctx, expectations, record)
}

func (s *stateMutationSpy) DeleteIf(ctx context.Context, expectation state.SlotExpectation) (bool, error) {
	s.count.Add(1)
	return s.StateStore.DeleteIf(ctx, expectation)
}

func (s *stateMutationSpy) Delete(ctx context.Context, id identity.Quadruple, kind string) error {
	s.count.Add(1)
	return s.StateStore.Delete(ctx, id, kind)
}

func (s *stateMutationSpy) DeleteScope(ctx context.Context, id identity.Identity) (int, error) {
	s.count.Add(1)
	return s.StateStore.DeleteScope(ctx, id)
}

func sessionHandler(t *testing.T) http.Handler {
	t.Helper()
	return newSessionHandlerFixture(t).handler
}

func newSessionHandlerFixture(t *testing.T) sessionHandlerFixture {
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
	svc, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithSkillStore(skStore),
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithSessionOverlay(ov),
		agentcfgprotocol.WithSessionPersonalSkillController(authority.Controller),
		agentcfgprotocol.WithRunSnapshotGate(runsnapshot.NewGate()),
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

func acReq(t *testing.T, h http.Handler, route, body string, id *identity.Identity, scopes []auth.Scope) (int, []byte) {
	return acReqReach(t, h, route, body, id, scopes, []string{acAgent})
}

func acReqReach(t *testing.T, h http.Handler, route, body string, id *identity.Identity, scopes []auth.Scope, reach []string) (int, []byte) {
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
		req = req.WithContext(auth.WithAgentReach(req.Context(), reach))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func acID() *identity.Identity {
	return &identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
}

const acAgent = "agent-x"

// TestAgentConfigHandler_AgentReachClosedCensus is the executable census for
// every Phase-232 agent-config data-plane verb. Each row supplies a body that
// decodes and reconciles before the excluded signed reach is refused, proving
// the shared gate sits before every service/storage call.
func TestAgentConfigHandler_AgentReachClosedCensus(t *testing.T) {
	identityJSON := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x"}`
	skill := `{"name":"s","trigger":"t","steps":["step"],"origin":"generated","scope":"session"}`
	cases := []struct {
		route  string
		body   string
		scopes []auth.Scope
	}{
		{"session/set_user_prompt", `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x","user_prompt":"x"}`, nil},
		{"session/set_source_disables", `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x","disabled_servers":["srv"]}`, nil},
		{"session/skills/list", identityJSON, nil},
		{"session/skills/upsert", `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x","skill":` + skill + `}`, nil},
		{"session/skills/delete", `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x","name":"s"}`, nil},
		{"user/get", identityJSON, []auth.Scope{auth.ScopeAgentConfigUser}},
		{"user/set_revision", `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x","payload":{}}`, []auth.Scope{auth.ScopeAgentConfigUser}},
		{"user/list_revisions", identityJSON, []auth.Scope{auth.ScopeAgentConfigUser}},
		{"user/diff", `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x","from_revision":"a","to_revision":"b"}`, []auth.Scope{auth.ScopeAgentConfigUser}},
		{"user/rollback", `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x","revision_id":"a"}`, []auth.Scope{auth.ScopeAgentConfigUser}},
		{"user/skills/list", identityJSON, nil},
		{"user/skills/upsert", `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x","skill":` + skill + `}`, nil},
		{"user/skills/delete", `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"agent-x","name":"s"}`, nil},
	}
	if len(cases) != 13 {
		t.Fatalf("agent-config reach census has %d routes, want 13", len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.route, func(t *testing.T) {
			h := sessionHandler(t)
			code, body := acReqReach(t, h, tc.route, tc.body, acID(), tc.scopes, []string{"other-agent"})
			if code != http.StatusForbidden {
				t.Fatalf("excluded reach status = %d body=%s", code, body)
			}
			if got := errCode(t, body); got != protoerrors.CodeScopeMismatch {
				t.Fatalf("excluded reach code = %q, want %q", got, protoerrors.CodeScopeMismatch)
			}
		})
	}
}

// TestAgentConfigHandler_SessionRoute_NonAdminAllowed proves a NON-admin
// (session-scoped) caller is permitted on a session-safe route (no admin
// scope claim required).
func TestAgentConfigHandler_SessionRoute_NonAdminAllowed(t *testing.T) {
	h := sessionHandler(t)
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"` + acAgent + `","user_prompt":"be brief"}`
	// No admin scope — an empty (but present) scope set.
	code, resp := acReq(t, h, "session/set_user_prompt", body, acID(), []auth.Scope{})
	if code != http.StatusOK {
		t.Fatalf("session route should allow a non-admin caller; got %d body=%s", code, resp)
	}
}

// TestAgentConfigHandler_AdminRoute_NonAdminRejected proves a NON-admin
// caller on an ADMIN route is rejected with CodeScopeMismatch (403) — the
// admin gate is unchanged.
func TestAgentConfigHandler_AdminRoute_NonAdminRejected(t *testing.T) {
	h := sessionHandler(t)
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"` + acAgent + `","prompt_layers":{"base":"x"}}`
	code, resp := acReq(t, h, "set_prompt_layers", body, acID(), []auth.Scope{}) // no admin
	if code != http.StatusForbidden {
		t.Fatalf("admin route should reject a non-admin caller; got %d body=%s", code, resp)
	}
	if c := errCode(t, resp); c != protoerrors.CodeScopeMismatch {
		t.Fatalf("admin route non-admin error code = %s, want %s", c, protoerrors.CodeScopeMismatch)
	}
}

// TestAgentConfigHandler_AdminRoute_AdminAllowed proves an admin caller still
// reaches an admin route (the gate is per-route, not blanket-removed).
func TestAgentConfigHandler_AdminRoute_AdminAllowed(t *testing.T) {
	h := sessionHandler(t)
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"` + acAgent + `","prompt_layers":{"base":"operator base"}}`
	code, resp := acReq(t, h, "set_prompt_layers", body, acID(), []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusOK {
		t.Fatalf("admin route should allow an admin caller; got %d body=%s", code, resp)
	}
}

// TestAgentConfigHandler_SessionRoute_IdentityRequired proves the session
// route still fails closed on a missing identity.
func TestAgentConfigHandler_SessionRoute_IdentityRequired(t *testing.T) {
	h := sessionHandler(t)
	body := `{"agent_id":"` + acAgent + `","user_prompt":"x"}`
	code, _ := acReq(t, h, "session/set_user_prompt", body, nil, []auth.Scope{})
	if code != http.StatusUnauthorized {
		t.Fatalf("missing identity should be 401; got %d", code)
	}
}

// TestAgentConfigHandler_SessionDisable_NonAdminAllowed covers the
// set_source_disables session-safe route for a non-admin caller.
func TestAgentConfigHandler_SessionDisable_NonAdminAllowed(t *testing.T) {
	h := sessionHandler(t)
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"` + acAgent + `","disabled_servers":["srvA"]}`
	code, resp := acReq(t, h, "session/set_source_disables", body, acID(), []auth.Scope{})
	if code != http.StatusOK {
		t.Fatalf("session disable route should allow a non-admin caller; got %d body=%s", code, resp)
	}
}
