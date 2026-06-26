// Cross-subsystem integration test per CLAUDE.md §17 — the §13
// primitive-with-consumer discharge for the agent-config Protocol
// capability (CapAgentConfig = "agent_config").
//
// The surface under test:
//
//   - The canonical capability constant types.CapAgentConfig and its
//     entry in canonicalCapabilities (the negotiable universe).
//   - PostureDeps.AgentConfigAvailable — the per-instance conditional
//     advertisement, wired from the same boolean that gates the
//     agent-config transport mount.
//   - runtime.info.capabilities advertising agent_config IFF the
//     agent-config surface is mounted.
//
// This test is the same-PR consumer: it boots a real assembled Runtime
// via harbortest/devstack.Assemble, builds a real PostureSurface, wires a
// real agentcfgprotocol.Service through the real transports.NewMux behind
// an httptest.Server, and proves:
//
//  1. With the agent-config service mounted AND AgentConfigAvailable=true,
//     runtime.info advertises agent_config and the /v1/agent_config/get
//     route is genuinely present (not 404).
//  2. Without the agent-config service AND AgentConfigAvailable=false,
//     runtime.info omits agent_config and the /v1/agent_config/get route
//     is genuinely absent (404) — "advertised iff mounted".
//  3. Failure mode — an incomplete-identity runtime.info is rejected 401
//     before any capability list is produced.
//
// Real drivers everywhere on the seam — no mocks (CLAUDE.md §17.3).
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	runtimeposture "github.com/hurtener/Harbor/internal/runtime/posture"
	"github.com/hurtener/Harbor/internal/skills"
	localdb "github.com/hurtener/Harbor/internal/skills/drivers/localdb"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// buildAgentConfigPosture constructs a real PostureSurface wired to the
// devstack's real drivers, with AgentConfigAvailable set per the caller —
// the per-instance advertisement under test.
func buildAgentConfigPosture(t *testing.T, stack *devstack.DevStack, agentConfigAvailable bool) *protocol.PostureSurface {
	t.Helper()
	cfg := stack.Cfg
	s, err := protocol.NewPostureSurface(protocol.PostureDeps{
		Build: types.RuntimeInfo{
			BuildVersion:   "v0.0.0-test",
			BuildCommit:    "agentcfg-cap",
			BuildGoVersion: runtime.Version(),
		},
		Clock:    time.Now,
		BootedAt: time.Now().Add(-30 * time.Second),
		Health: func(_ context.Context) []types.SubsystemHealth {
			return []types.SubsystemHealth{
				{Subsystem: "state", Status: types.HealthStatusReady},
			}
		},
		Counters: runtimeposture.CountersProvider(stack.Tasks, nil, stack.MCPRegistry),
		Drivers: func() []types.SubsystemDriver {
			return []types.SubsystemDriver{
				{Subsystem: "state", Driver: cfg.State.Driver},
			}
		},
		Metrics: func(_ context.Context) types.MetricsSnapshot {
			return types.MetricsSnapshot{}
		},
		Governance:           governance.NewPostureProvider(governance.Config{}),
		LLM:                  llm.NewPostureProvider(llm.ConfigSnapshot{Driver: "mock"}),
		Redactor:             stack.Audit,
		Bus:                  stack.Bus,
		DisplayName:          "harbor-agentcfg-cap-test",
		InstanceID:           "inst-agentcfg-cap-test",
		AgentConfigAvailable: agentConfigAvailable,
	})
	if err != nil {
		t.Fatalf("NewPostureSurface: %v", err)
	}
	return s
}

// buildAgentConfigService wires a real agentcfgprotocol.Service over a
// real StateStore-backed registry and a real localdb SkillStore — the
// production seam, no mocks.
func buildAgentConfigService(t *testing.T, stack *devstack.DevStack) *agentcfgprotocol.Service {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	skillStore, err := localdb.New(skills.ConfigSnapshot{Driver: "localdb", DSN: ":memory:"}, skills.Deps{Bus: stack.Bus})
	if err != nil {
		t.Fatalf("skills: %v", err)
	}
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: stack.Bus})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	svc, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithSkillStore(skillStore))
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = skillStore.Close(context.Background())
		_ = st.Close(context.Background())
	})
	return svc
}

func TestE2E_Phase128_AgentConfigCapability(t *testing.T) {
	cfg := runtimePostureConfig(t)
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{SkipRunLoop: true})
	defer stack.Close()

	if stack.Surface == nil || stack.Validator == nil || stack.SigningKey == nil {
		t.Fatal("devstack did not assemble the Surface / Validator / SigningKey")
	}

	devID := identity.Identity{
		TenantID:  devstack.DefaultDevTenant,
		UserID:    devstack.DefaultDevUser,
		SessionID: devstack.DefaultDevSession,
	}
	infoBody, _ := json.Marshal(types.RuntimeInfoRequest{
		Identity: types.IdentityScope{Tenant: devID.TenantID, User: devID.UserID, Session: devID.SessionID},
	})

	// --- (1) surface MOUNTED: advertised + route present --------------
	t.Run("SurfaceMounted_AdvertisesCapability", func(t *testing.T) {
		posture := buildAgentConfigPosture(t, stack, true)
		svc := buildAgentConfigService(t, stack)
		mux, err := transports.NewMux(stack.Surface, stack.Bus,
			transports.WithValidator(stack.Validator),
			transports.WithPostureSurface(posture),
			transports.WithAgentConfigService(svc),
		)
		if err != nil {
			t.Fatalf("transports.NewMux: %v", err)
		}
		srv := httptest.NewServer(mux)
		defer srv.Close()

		token := signPostureToken(t, stack.SigningKey, devID, nil)
		status, raw := postPosture(t, srv.URL, methods.MethodRuntimeInfo, infoBody, token)
		if status != http.StatusOK {
			t.Fatalf("runtime.info: status %d, want 200; body=%s", status, raw)
		}
		var info types.RuntimeInfo
		if err := json.Unmarshal(raw, &info); err != nil {
			t.Fatalf("decode RuntimeInfo: %v; body=%s", err, raw)
		}
		if !hasCapability(info.Capabilities, types.CapAgentConfig) {
			t.Errorf("runtime.info Capabilities %v missing agent_config when the surface is mounted", info.Capabilities)
		}

		// The /v1/agent_config/* route must genuinely be present — an
		// admin-scoped POST reaches the handler (not a 404 route-absent).
		adminTok := signPostureToken(t, stack.SigningKey, devID, []string{"admin"})
		acStatus, acRaw := postRaw(t, srv.URL+"/v1/agent_config/get",
			[]byte(`{"agent_id":"agent-int"}`), adminTok)
		if acStatus == http.StatusNotFound || acStatus == http.StatusNotImplemented {
			t.Fatalf("/v1/agent_config/get returned %d — route absent though the service is mounted; body=%s", acStatus, acRaw)
		}
	})

	// --- (2) surface NOT MOUNTED: omitted + route absent --------------
	t.Run("SurfaceAbsent_OmitsCapability", func(t *testing.T) {
		posture := buildAgentConfigPosture(t, stack, false)
		mux, err := transports.NewMux(stack.Surface, stack.Bus,
			transports.WithValidator(stack.Validator),
			transports.WithPostureSurface(posture),
		)
		if err != nil {
			t.Fatalf("transports.NewMux: %v", err)
		}
		srv := httptest.NewServer(mux)
		defer srv.Close()

		token := signPostureToken(t, stack.SigningKey, devID, nil)
		status, raw := postPosture(t, srv.URL, methods.MethodRuntimeInfo, infoBody, token)
		if status != http.StatusOK {
			t.Fatalf("runtime.info: status %d, want 200; body=%s", status, raw)
		}
		var info types.RuntimeInfo
		if err := json.Unmarshal(raw, &info); err != nil {
			t.Fatalf("decode RuntimeInfo: %v; body=%s", err, raw)
		}
		if hasCapability(info.Capabilities, types.CapAgentConfig) {
			t.Errorf("runtime.info Capabilities %v advertises agent_config though the surface is NOT mounted", info.Capabilities)
		}

		// The /v1/agent_config/* route must genuinely be absent (404).
		adminTok := signPostureToken(t, stack.SigningKey, devID, []string{"admin"})
		acStatus, _ := postRaw(t, srv.URL+"/v1/agent_config/get",
			[]byte(`{"agent_id":"agent-int"}`), adminTok)
		if acStatus != http.StatusNotFound {
			t.Errorf("/v1/agent_config/get returned %d, want 404 (route absent when the service is not mounted)", acStatus)
		}
	})

	// --- (3) failure mode: incomplete identity rejected 401 ----------
	t.Run("IncompleteIdentity_FailsClosed", func(t *testing.T) {
		posture := buildAgentConfigPosture(t, stack, true)
		mux, err := transports.NewMux(stack.Surface, stack.Bus,
			transports.WithValidator(stack.Validator),
			transports.WithPostureSurface(posture),
			transports.WithAgentConfigService(buildAgentConfigService(t, stack)),
		)
		if err != nil {
			t.Fatalf("transports.NewMux: %v", err)
		}
		srv := httptest.NewServer(mux)
		defer srv.Close()

		// A token verified as devID; the body claims a mismatching user —
		// the transport rejects the identity mismatch with 401 before any
		// capability list is produced.
		token := signPostureToken(t, stack.SigningKey, devID, nil)
		badBody, _ := json.Marshal(types.RuntimeInfoRequest{
			Identity: types.IdentityScope{Tenant: devID.TenantID, User: "someone-else", Session: devID.SessionID},
		})
		status, _ := postPosture(t, srv.URL, methods.MethodRuntimeInfo, badBody, token)
		if status != http.StatusUnauthorized {
			t.Fatalf("incomplete/mismatched identity runtime.info: status %d, want 401", status)
		}
	})
}

// hasCapability reports whether caps contains c.
func hasCapability(caps []types.Capability, c types.Capability) bool {
	for _, have := range caps {
		if have == c {
			return true
		}
	}
	return false
}

// postRaw POSTs a raw body to an absolute URL with a bearer token and
// returns the HTTP status + body. Used to probe route presence/absence
// directly (the agent-config routes are not posture methods).
func postRaw(t *testing.T, url string, body []byte, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}
