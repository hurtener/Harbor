// Signed agent reach, end to end (Phase 232 / D-397; RFC §5.5, §6.16).
//
// Every request below crosses the real ES256 validator, auth middleware,
// served devstack mux, and production drivers for tasks, sessions, agent
// config, overlays, skills, and tools. The only added catalog entry is a real
// in-process tool registered through the shipped inproc driver.
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

const (
	arOtherAgent = "phase-232-other-agent"
	arEchoTool   = "phase_232_echo"
)

type agentReachStack struct {
	stack  *devstack.DevStack
	server *httptest.Server
	client *http.Client
}

func newAgentReachStack(t *testing.T) *agentReachStack {
	t.Helper()
	cfg := phase110bConfig(t)
	cfg.Skills.Driver = "localdb"
	cfg.Skills.DSN = filepath.Join(t.TempDir(), "skills.db")
	cfg.Skills.SessionPersonalCutover.Tenants = []config.SessionPersonalCutoverTenant{{
		TenantID: devstack.DefaultDevTenant, Epoch: "agent-reach-state-only", RosterDigest: "fixture", LegacyWritersDrained: true,
	}}
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		LLMConfigSnapshot: phase110bLLMSnapshot(cfg),
		SkipRunLoop:       true,
	})
	t.Cleanup(stack.Close)
	if stack.Handler == nil || stack.Validator == nil || stack.SigningKey == nil || stack.AgentReach == nil {
		t.Fatal("devstack omitted authenticated shared-reach assembly")
	}
	if err := inproc.RegisterFunc[struct{}, struct{}](stack.Catalog, arEchoTool,
		func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil }); err != nil {
		t.Fatalf("register real inproc tool: %v", err)
	}
	server := httptest.NewServer(stack.Handler)
	t.Cleanup(server.Close)
	return &agentReachStack{stack: stack, server: server, client: server.Client()}
}

func arIdentity(session string) identity.Identity {
	return identity.Identity{
		TenantID:  devstack.DefaultDevTenant,
		UserID:    devstack.DefaultDevUser,
		SessionID: session,
	}
}

func arScope(id identity.Identity) map[string]any {
	return map[string]any{"tenant": id.TenantID, "user": id.UserID, "session": id.SessionID}
}

// arToken signs an arbitrary claim shape with the exact private key the real
// devstack validator trusts. present=false omits agent_reach; present=true may
// carry an empty or malformed value so authentication behavior is observable.
func arToken(t *testing.T, s *agentReachStack, id identity.Identity, scopes []string, present bool, reach any) string {
	t.Helper()
	now := time.Now()
	claims := jwt.MapClaims{
		"iss":     "harbor-test",
		"sub":     id.UserID,
		"aud":     "harbor",
		"exp":     now.Add(time.Hour).Unix(),
		"nbf":     now.Add(-time.Minute).Unix(),
		"iat":     now.Unix(),
		"tenant":  id.TenantID,
		"user":    id.UserID,
		"session": id.SessionID,
		"scopes":  scopes,
	}
	if present {
		claims[auth.AgentReachClaim] = reach
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = devstack.DefaultKID
	signed, err := tok.SignedString(s.stack.SigningKey)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

func arPost(t *testing.T, s *agentReachStack, path, token string, body any) (int, []byte) {
	t.Helper()
	status, raw, err := arPostContext(context.Background(), s, path, token, body)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return status, raw
}

func arPostContext(ctx context.Context, s *agentReachStack, path, token string, body any) (int, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.server.URL+path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, fmt.Errorf("request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read: %w", err)
	}
	return resp.StatusCode, out, nil
}

func arAssertStatus(t *testing.T, got int, body []byte, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("status=%d, want %d, body=%s", got, want, body)
	}
}

func arAssertCode(t *testing.T, body []byte, want string) {
	t.Helper()
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode error envelope: %v (%s)", err, body)
	}
	if envelope.Code != want {
		t.Fatalf("code=%q, want %q, body=%s", envelope.Code, want, body)
	}
}

func arDecode[T any](t *testing.T, body []byte) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode response: %v (%s)", err, body)
	}
	return out
}

// arConfigSnapshot serializes every state family the thirteen config routes
// can mutate. Comparing snapshots around denied calls proves the reach gate
// ran before overlay, durable revision, and skill-store side effects.
func arConfigSnapshot(t *testing.T, s *agentReachStack, id identity.Identity, agentID string) string {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	quad := identity.Quadruple{Identity: id}
	overlay, overlaySet, err := s.stack.SessionOverlay.Get(ctx, quad, agentID)
	if err != nil {
		t.Fatalf("overlay snapshot: %v", err)
	}
	userRevision, userSet, err := s.stack.AgentConfig.Active(ctx, quad, agentID, agentcfg.ConfigScopeUser)
	if err != nil {
		t.Fatalf("user revision snapshot: %v", err)
	}
	skillRows, err := s.stack.Skills.List(ctx, quad, skills.ListFilter{})
	if err != nil {
		t.Fatalf("skills snapshot: %v", err)
	}
	raw, err := json.Marshal(struct {
		Overlay      any            `json:"overlay"`
		OverlaySet   bool           `json:"overlay_set"`
		UserRevision any            `json:"user_revision"`
		UserSet      bool           `json:"user_set"`
		Skills       []skills.Skill `json:"skills"`
	}{
		Overlay:      overlay,
		OverlaySet:   overlaySet,
		UserRevision: userRevision,
		UserSet:      userSet,
		Skills:       skillRows,
	})
	if err != nil {
		t.Fatalf("marshal state snapshot: %v", err)
	}
	return string(raw)
}

func arSeedAgent(t *testing.T, s *agentReachStack, id identity.Identity, agentID string) {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := s.stack.AgentConfig.SetRevision(ctx, identity.Quadruple{Identity: id}, agentID,
		agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("seed agent config: %v", err)
	}
}

// TestE2E_AgentReach_AuthenticatedMuxMatrix covers the acceptance matrix over
// one served mux: allowed, excluded, absent, empty, malformed, cross-tenant,
// explicit/default start, representative session/user/tools calls, and a
// no-side-effect assertion at both task/session and overlay storage edges.
func TestE2E_AgentReach_AuthenticatedMuxMatrix(t *testing.T) {
	s := newAgentReachStack(t)
	bootAgent := s.stack.AgentConfigID

	t.Run("start explicit/default and denied requests have no side effects", func(t *testing.T) {
		allowedDefaultID := arIdentity("p232-start-default")
		allowedDefault := arToken(t, s, allowedDefaultID, []string{"admin"}, true, []string{bootAgent})
		status, raw := arPost(t, s, "/v1/control/start", allowedDefault, map[string]any{
			"identity": arScope(allowedDefaultID), "query": "allowed default",
		})
		arAssertStatus(t, status, raw, http.StatusOK)

		allowedExplicitID := arIdentity("p232-start-explicit")
		allowedExplicit := arToken(t, s, allowedExplicitID, []string{"admin"}, true, []string{bootAgent})
		status, raw = arPost(t, s, "/v1/control/start", allowedExplicit, map[string]any{
			"identity": arScope(allowedExplicitID), "agent_id": bootAgent, "query": "allowed explicit",
		})
		arAssertStatus(t, status, raw, http.StatusOK)

		denyID := arIdentity("p232-start-denied")
		arSeedAgent(t, s, denyID, arOtherAgent)
		ctx, err := identity.With(context.Background(), denyID)
		if err != nil {
			t.Fatalf("identity.With: %v", err)
		}
		before, err := s.stack.Tasks.List(ctx, denyID, tasks.TaskFilter{})
		if err != nil {
			t.Fatalf("tasks before: %v", err)
		}
		if _, err := s.stack.Sessions.Get(ctx, denyID.SessionID); !errors.Is(err, sessions.ErrSessionNotFound) {
			t.Fatalf("session existed before denial: %v", err)
		}

		excluded := arToken(t, s, denyID, []string{"admin"}, true, []string{arOtherAgent})
		status, raw = arPost(t, s, "/v1/control/start", excluded, map[string]any{
			"identity": arScope(denyID), "agent_id": bootAgent, "query": "excluded",
		})
		arAssertStatus(t, status, raw, http.StatusForbidden)
		arAssertCode(t, raw, "scope_mismatch")

		missing := arToken(t, s, denyID, []string{"admin"}, false, nil)
		status, raw = arPost(t, s, "/v1/control/start", missing, map[string]any{
			"identity": arScope(denyID), "query": "missing reach",
		})
		arAssertStatus(t, status, raw, http.StatusForbidden)

		empty := arToken(t, s, denyID, []string{"admin"}, true, []string{})
		status, raw = arPost(t, s, "/v1/control/start", empty, map[string]any{
			"identity": arScope(denyID), "query": "empty reach",
		})
		arAssertStatus(t, status, raw, http.StatusForbidden)

		malformed := arToken(t, s, denyID, []string{"admin"}, true, []any{bootAgent, 7})
		status, raw = arPost(t, s, "/v1/control/start", malformed, map[string]any{
			"identity": arScope(denyID), "query": "malformed reach",
		})
		arAssertStatus(t, status, raw, http.StatusUnauthorized)

		after, err := s.stack.Tasks.List(ctx, denyID, tasks.TaskFilter{})
		if err != nil {
			t.Fatalf("tasks after: %v", err)
		}
		if len(before) != 0 || len(after) != 0 {
			t.Fatalf("denied starts created tasks: before=%d after=%d", len(before), len(after))
		}
		if _, err := s.stack.Sessions.Get(ctx, denyID.SessionID); !errors.Is(err, sessions.ErrSessionNotFound) {
			t.Fatalf("denied starts created a session: %v", err)
		}
	})

	t.Run("representative config tiers and cross-tenant body posture", func(t *testing.T) {
		id := arIdentity("p232-config-allowed")
		allScopes := []string{"admin", string(auth.ScopeAgentConfigUser)}
		allowed := arToken(t, s, id, allScopes, true, []string{bootAgent})

		status, raw := arPost(t, s, "/v1/agent_config/session/set_user_prompt", allowed, map[string]any{
			"identity": arScope(id), "agent_id": bootAgent, "user_prompt": "phase 232 allowed",
		})
		arAssertStatus(t, status, raw, http.StatusOK)

		status, raw = arPost(t, s, "/v1/agent_config/user/get", allowed, map[string]any{
			"identity": arScope(id), "agent_id": bootAgent,
		})
		arAssertStatus(t, status, raw, http.StatusOK)

		status, raw = arPost(t, s, "/v1/agent_config/user/skills/list", allowed, map[string]any{
			"identity": arScope(id), "agent_id": bootAgent,
		})
		arAssertStatus(t, status, raw, http.StatusOK)

		crossTenant := map[string]any{"tenant": "other-tenant", "user": id.UserID, "session": id.SessionID}
		status, raw = arPost(t, s, "/v1/agent_config/session/skills/list", allowed, map[string]any{
			"identity": crossTenant, "agent_id": bootAgent,
		})
		arAssertStatus(t, status, raw, http.StatusUnauthorized)
		arAssertCode(t, raw, "identity_required")

		denyID := arIdentity("p232-config-denied")
		ctx, err := identity.With(context.Background(), denyID)
		if err != nil {
			t.Fatalf("identity.With: %v", err)
		}
		quad := identity.Quadruple{Identity: denyID}
		if _, set, err := s.stack.SessionOverlay.Get(ctx, quad, bootAgent); err != nil || set {
			t.Fatalf("overlay before denial: set=%v err=%v", set, err)
		}
		missing := arToken(t, s, denyID, nil, false, nil)
		status, raw = arPost(t, s, "/v1/agent_config/session/set_user_prompt", missing, map[string]any{
			"identity": arScope(denyID), "agent_id": bootAgent, "user_prompt": "must-not-persist",
		})
		arAssertStatus(t, status, raw, http.StatusForbidden)
		if _, set, err := s.stack.SessionOverlay.Get(ctx, quad, bootAgent); err != nil || set {
			t.Fatalf("denied config call changed overlay: set=%v err=%v", set, err)
		}
	})

	t.Run("tools describe explicit projection is gated and omission is compatible", func(t *testing.T) {
		id := arIdentity("p232-tools")
		allowed := arToken(t, s, id, nil, true, []string{bootAgent})
		status, raw := arPost(t, s, "/v1/tools/describe", allowed, map[string]any{
			"identity": arScope(id), "id": arEchoTool, "agent_id": bootAgent,
		})
		arAssertStatus(t, status, raw, http.StatusOK)

		excluded := arToken(t, s, id, nil, true, []string{arOtherAgent})
		status, raw = arPost(t, s, "/v1/tools/describe", excluded, map[string]any{
			"identity": arScope(id), "id": arEchoTool, "agent_id": bootAgent,
		})
		arAssertStatus(t, status, raw, http.StatusForbidden)

		missingExplicit := arToken(t, s, id, nil, false, nil)
		status, raw = arPost(t, s, "/v1/tools/describe", missingExplicit, map[string]any{
			"identity": arScope(id), "id": arEchoTool, "agent_id": bootAgent,
		})
		arAssertStatus(t, status, raw, http.StatusForbidden)

		empty := arToken(t, s, id, nil, true, []string{})
		status, raw = arPost(t, s, "/v1/tools/describe", empty, map[string]any{
			"identity": arScope(id), "id": arEchoTool, "agent_id": bootAgent,
		})
		arAssertStatus(t, status, raw, http.StatusForbidden)

		status, raw = arPost(t, s, "/v1/tools/describe", missingExplicit, map[string]any{
			"identity": arScope(id), "id": arEchoTool,
		})
		arAssertStatus(t, status, raw, http.StatusOK)
	})

	t.Run("bearer-less carrier identity does not acquire signed reach", func(t *testing.T) {
		id := arIdentity("p232-carrier")
		body, err := json.Marshal(map[string]any{"identity": arScope(id), "agent_id": bootAgent})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		req, err := http.NewRequest(http.MethodPost, s.server.URL+"/v1/agent_config/session/skills/list", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Harbor-Tenant", id.TenantID)
		req.Header.Set("X-Harbor-User", id.UserID)
		req.Header.Set("X-Harbor-Session", id.SessionID)
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatalf("carrier request: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		arAssertStatus(t, resp.StatusCode, raw, http.StatusUnauthorized)
	})

	t.Run("without-validator carrier identity reaches the gate but has no authority", func(t *testing.T) {
		mux, err := transports.NewMux(s.stack.Surface, s.stack.Bus, transports.WithoutValidator())
		if err != nil {
			t.Fatalf("NewMux WithoutValidator: %v", err)
		}
		server := httptest.NewServer(mux)
		defer server.Close()

		id := arIdentity("p232-carrier-without-validator")
		ctx, err := identity.With(context.Background(), id)
		if err != nil {
			t.Fatalf("identity.With: %v", err)
		}
		before, err := s.stack.Tasks.List(ctx, id, tasks.TaskFilter{})
		if err != nil {
			t.Fatalf("tasks before carrier denial: %v", err)
		}
		if len(before) != 0 {
			t.Fatalf("carrier denial session started with %d tasks", len(before))
		}
		if _, err := s.stack.Sessions.Get(ctx, id.SessionID); !errors.Is(err, sessions.ErrSessionNotFound) {
			t.Fatalf("carrier denial session existed before call: %v", err)
		}

		body, err := json.Marshal(map[string]any{
			"identity": arScope(id), "query": "carrier identity is not signed reach",
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/control/start", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Harbor-Tenant", id.TenantID)
		req.Header.Set("X-Harbor-User", id.UserID)
		req.Header.Set("X-Harbor-Session", id.SessionID)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("carrier request: %v", err)
		}
		raw, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			t.Fatalf("read carrier refusal: %v", readErr)
		}
		arAssertStatus(t, resp.StatusCode, raw, http.StatusForbidden)
		arAssertCode(t, raw, "scope_mismatch")

		after, err := s.stack.Tasks.List(ctx, id, tasks.TaskFilter{})
		if err != nil {
			t.Fatalf("tasks after carrier denial: %v", err)
		}
		if len(after) != 0 {
			t.Fatalf("WithoutValidator carrier denial created %d tasks", len(after))
		}
		if _, err := s.stack.Sessions.Get(ctx, id.SessionID); !errors.Is(err, sessions.ErrSessionNotFound) {
			t.Fatalf("WithoutValidator carrier denial created a session: %v", err)
		}
	})
}

// TestE2E_AgentReach_ClosedAgentConfigCensus proves all thirteen enumerated
// routes cross the authenticated mux with a valid allowed control arm, then
// refuse excluded, absent, and empty reach without mutating any of the three
// backing state families. Malformed reach is authentication-wide and is pinned
// once in the start matrix above; repeating it thirteen times would only test
// the same validator exit before route dispatch.
func TestE2E_AgentReach_ClosedAgentConfigCensus(t *testing.T) {
	s := newAgentReachStack(t)
	id := arIdentity("p232-census")
	bootAgent := s.stack.AgentConfigID
	allScopes := []string{"admin", string(auth.ScopeAgentConfigUser)}
	setupToken := arToken(t, s, id, allScopes, true, []string{bootAgent})
	skill := func(name string) map[string]any {
		return map[string]any{
			"name": name, "trigger": "phase 232", "steps": []string{"verify reach"},
			"origin": "generated", "scope": "session",
		}
	}

	// Seed the two delete targets and two durable revisions required by the
	// allowed diff/rollback rows. These setup calls themselves traverse the
	// same authenticated mux and must succeed.
	for _, setup := range []struct {
		path string
		body map[string]any
	}{
		{path: "/v1/agent_config/session/skills/upsert", body: map[string]any{
			"identity": arScope(id), "agent_id": bootAgent, "skill": skill("session-seeded"),
		}},
		{path: "/v1/agent_config/user/skills/upsert", body: map[string]any{
			"identity": arScope(id), "agent_id": bootAgent, "skill": skill("user-seeded"),
		}},
	} {
		status, raw := arPost(t, s, setup.path, setupToken, setup.body)
		arAssertStatus(t, status, raw, http.StatusOK)
	}
	setUser := func(prompt string) prototypes.AgentConfigUserSetRevisionResponse {
		status, raw := arPost(t, s, "/v1/agent_config/user/set_revision", setupToken, map[string]any{
			"identity": arScope(id), "agent_id": bootAgent,
			"payload": map[string]any{"user_prompt": prompt},
		})
		arAssertStatus(t, status, raw, http.StatusOK)
		return arDecode[prototypes.AgentConfigUserSetRevisionResponse](t, raw)
	}
	first := setUser("phase 232 first")
	second := setUser("phase 232 second")

	type row struct {
		path   string
		scopes []string
		body   map[string]any
	}
	rows := []row{
		{path: "/v1/agent_config/session/set_user_prompt", body: map[string]any{"user_prompt": "allowed matrix"}},
		{path: "/v1/agent_config/session/set_source_disables", body: map[string]any{"disabled_servers": []string{"matrix-server"}}},
		{path: "/v1/agent_config/session/skills/list", body: map[string]any{}},
		{path: "/v1/agent_config/session/skills/upsert", body: map[string]any{"skill": skill("session-matrix-upsert")}},
		{path: "/v1/agent_config/session/skills/delete", body: map[string]any{"name": "session-seeded"}},
		{path: "/v1/agent_config/user/get", scopes: []string{string(auth.ScopeAgentConfigUser)}, body: map[string]any{}},
		{path: "/v1/agent_config/user/set_revision", scopes: []string{string(auth.ScopeAgentConfigUser)}, body: map[string]any{
			"payload": map[string]any{"user_prompt": "allowed matrix revision"},
		}},
		{path: "/v1/agent_config/user/list_revisions", scopes: []string{string(auth.ScopeAgentConfigUser)}, body: map[string]any{}},
		{path: "/v1/agent_config/user/diff", scopes: []string{string(auth.ScopeAgentConfigUser)}, body: map[string]any{
			"from_revision": first.Revision.RevisionID, "to_revision": second.Revision.RevisionID,
		}},
		{path: "/v1/agent_config/user/rollback", scopes: []string{string(auth.ScopeAgentConfigUser)}, body: map[string]any{
			"revision_id": first.Revision.RevisionID,
		}},
		{path: "/v1/agent_config/user/skills/list", body: map[string]any{}},
		{path: "/v1/agent_config/user/skills/upsert", body: map[string]any{"skill": skill("user-matrix-upsert")}},
		{path: "/v1/agent_config/user/skills/delete", body: map[string]any{"name": "user-seeded"}},
	}
	if len(rows) != 13 {
		t.Fatalf("closed census has %d rows, want 13", len(rows))
	}
	for _, tc := range rows {
		t.Run(tc.path, func(t *testing.T) {
			body := map[string]any{"identity": arScope(id), "agent_id": bootAgent}
			for key, value := range tc.body {
				body[key] = value
			}
			allowed := arToken(t, s, id, tc.scopes, true, []string{bootAgent})
			status, raw := arPost(t, s, tc.path, allowed, body)
			arAssertStatus(t, status, raw, http.StatusOK)

			before := arConfigSnapshot(t, s, id, bootAgent)
			modes := []struct {
				name    string
				present bool
				reach   any
			}{
				{name: "excluded", present: true, reach: []string{arOtherAgent}},
				{name: "absent"},
				{name: "empty", present: true, reach: []string{}},
			}
			for _, mode := range modes {
				t.Run(mode.name, func(t *testing.T) {
					token := arToken(t, s, id, tc.scopes, mode.present, mode.reach)
					got, deniedBody := arPost(t, s, tc.path, token, body)
					arAssertStatus(t, got, deniedBody, http.StatusForbidden)
					arAssertCode(t, deniedBody, "scope_mismatch")
				})
			}
			after := arConfigSnapshot(t, s, id, bootAgent)
			if after != before {
				t.Fatalf("denied calls mutated config state\nbefore=%s\nafter=%s", before, after)
			}
		})
	}
}

// TestE2E_AgentReach_SharedMuxConcurrentIsolationCancellationAndLeak drives
// N=120 mixed-authority calls through one authenticated mux under -race. Ten
// callers are independently cancelled; the other 110 retain their own reach
// decisions, and handler goroutines settle back to the warmed baseline.
func TestE2E_AgentReach_SharedMuxConcurrentIsolationCancellationAndLeak(t *testing.T) {
	s := newAgentReachStack(t)
	bootAgent := s.stack.AgentConfigID
	const n = 120
	tokens := make([]string, n)
	ids := make([]identity.Identity, n)
	for i := range n {
		ids[i] = arIdentity(fmt.Sprintf("p232-concurrent-%03d", i))
		reach := []string{bootAgent}
		if i%2 == 1 {
			reach = []string{arOtherAgent}
		}
		tokens[i] = arToken(t, s, ids[i], nil, true, reach)
	}

	// Warm the client/server connection pool before sampling the long-lived
	// server baseline, then close idle connections so only assembly goroutines
	// remain in the sample.
	status, raw := arPost(t, s, "/v1/agent_config/session/skills/list", tokens[0], map[string]any{
		"identity": arScope(ids[0]), "agent_id": bootAgent,
	})
	arAssertStatus(t, status, raw, http.StatusOK)
	s.client.CloseIdleConnections()
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			if i%12 == 0 {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				_, _, err := arPostContext(cancelled, s, "/v1/agent_config/session/skills/list", tokens[i], map[string]any{
					"identity": arScope(ids[i]), "agent_id": bootAgent,
				})
				if !errors.Is(err, context.Canceled) {
					errs <- fmt.Errorf("call %d cancellation: %w", i, err)
				}
				return
			}
			got, body, err := arPostContext(ctx, s, "/v1/agent_config/session/skills/list", tokens[i], map[string]any{
				"identity": arScope(ids[i]), "agent_id": bootAgent,
			})
			if err != nil {
				errs <- fmt.Errorf("call %d: %w", i, err)
				return
			}
			want := http.StatusOK
			if i%2 == 1 {
				want = http.StatusForbidden
			}
			if got != want {
				errs <- fmt.Errorf("call %d status=%d want=%d body=%s", i, got, want, body)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	s.client.CloseIdleConnections()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		if got := runtime.NumGoroutine(); got <= baseline+4 {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("goroutines did not settle: baseline=%d current=%d", baseline, runtime.NumGoroutine())
		}
	}
}

// Compile-time checks keep the test's body shapes tied to canonical public
// wire types even though the table sends maps to vary required zero values.
var (
	_ = prototypes.StartRequest{}
	_ = prototypes.AgentConfigSessionSetUserPromptRequest{}
	_ = prototypes.AgentConfigUserGetRequest{}
	_ = prototypes.ToolDescribeRequest{}
)
