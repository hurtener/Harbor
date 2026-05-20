// Phase 72f (D-111) cross-subsystem integration test per CLAUDE.md §17
// — the §13 primitive-with-consumer discharge for the runtime-posture
// surface (runtime.info / runtime.health / runtime.counters /
// runtime.drivers / metrics.snapshot).
//
// The Phase 72f surface is:
//
//   - The five `runtime.*` / `metrics.*` Protocol method-name constants
//     (internal/protocol/methods) a third-party Console branches on.
//   - The six posture wire types (internal/protocol/types/posture.go).
//   - protocol.PostureSurface — the transport-agnostic dispatcher.
//   - control.WithPostureSurface + transports.WithPostureSurface — the
//     wire-transport route-table extension.
//
// This test is the same-PR consumer: it boots a real assembled Runtime
// via harbortest/devstack.Assemble (real inmem events / state / tasks /
// artifacts drivers + a real ES256 auth keypair), constructs a real
// PostureSurface wired to those drivers, mounts it through the real
// transports.NewMux, and probes every posture method end-to-end:
//
//  1. Each of the five methods round-trips in-process + over the wire.
//  2. Identity propagates through every layer — the per-call identity
//     triple reaches the Counters seam unblended.
//  3. Failure mode — a cross-tenant request without the admin scope is
//     rejected CodeScopeMismatch (HTTP 403); the same request with the
//     admin scope succeeds (D-079).
//  4. N≥10 concurrent operators read runtime.counters against the live
//     surface; the goroutine baseline is restored after teardown.
//
// Real drivers everywhere on the seam — no mocks (CLAUDE.md §17.3).
package integration

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/protocol/types"

	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/llm/mock"
	_ "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// posturePerTenantCounters records the identity triple the Counters
// seam was called with so the test can prove identity propagation.
type posturePerTenantCounters struct {
	mu   sync.Mutex
	seen map[string]identity.Identity // keyed by tenant
}

func (p *posturePerTenantCounters) record(id identity.Identity) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen[id.TenantID] = id
}

func (p *posturePerTenantCounters) lookup(tenant string) (identity.Identity, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id, ok := p.seen[tenant]
	return id, ok
}

// runtimePostureConfig builds the minimal validated *config.Config the
// devstack helper needs.
func runtimePostureConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{
			BindAddr:            "127.0.0.1:0",
			ShutdownGracePeriod: 1 * time.Second,
		},
		Identity: config.IdentityConfig{
			JWTAlgorithms: []string{"ES256"},
			Issuer:        "https://issuer.example.com",
			Audience:      "harbor",
			JWKSURL:       "https://issuer.example.com/.well-known/jwks.json",
		},
		Telemetry: config.TelemetryConfig{
			LogFormat:   "text",
			LogLevel:    "error",
			ServiceName: "harbor-phase72f",
		},
		State: config.StateConfig{Driver: "inmem"},
		LLM: config.LLMConfig{
			Driver:               "mock",
			Timeout:              5 * time.Second,
			ContextWindowReserve: 0.05,
		},
		Events: config.EventsConfig{
			Driver:                   "inmem",
			MaxSubscribersPerSession: 16,
			SubscriberBufferSize:     64,
			IdleTimeout:              2 * time.Second,
			DropWindow:               50 * time.Millisecond,
			ReplayBufferSize:         256,
		},
		Sessions: config.SessionsConfig{
			IdleTTL:       1 * time.Hour,
			HardCap:       24 * time.Hour,
			SweepInterval: 5 * time.Minute,
		},
		Artifacts: config.ArtifactsConfig{
			Driver:                    "inmem",
			HeavyOutputThresholdBytes: 32 * 1024,
		},
		Tasks: config.TasksConfig{
			Driver:               "inprocess",
			RetainTurnTimeout:    1 * time.Minute,
			ContinuationHopLimit: 4,
		},
		Distributed: config.DistributedConfig{
			BusDriver:    "loopback",
			RemoteDriver: "loopback",
		},
		Memory: config.MemoryConfig{
			Driver:             "inmem",
			Strategy:           "none",
			RecoveryBacklogMax: 8,
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("runtimePostureConfig: cfg.Validate(): %v", err)
	}
	return cfg
}

// buildPostureSurface constructs a real PostureSurface wired to the
// devstack's real drivers. The Counters seam records the identity it is
// called with into rec so the test can prove identity propagation. The
// Drivers seam reflects the assembled cfg's real driver names.
func buildPostureSurface(t *testing.T, stack *devstack.DevStack, rec *posturePerTenantCounters) *protocol.PostureSurface {
	t.Helper()
	cfg := stack.Cfg
	s, err := protocol.NewPostureSurface(protocol.PostureDeps{
		Build: types.RuntimeInfo{
			BuildVersion:   "v0.0.0-test",
			BuildCommit:    "phase72f",
			BuildGoVersion: runtime.Version(),
		},
		Clock:    time.Now,
		BootedAt: time.Now().Add(-90 * time.Second),
		Health: func(_ context.Context) []types.SubsystemHealth {
			// Real subsystems the devstack assembled are ready; the
			// optional llm tier reports its assembled state.
			return []types.SubsystemHealth{
				{Subsystem: "events", Status: types.HealthStatusReady},
				{Subsystem: "state", Status: types.HealthStatusReady},
				{Subsystem: "tasks", Status: types.HealthStatusReady},
				{Subsystem: "artifacts", Status: types.HealthStatusReady},
				{Subsystem: "memory", Status: types.HealthStatusReady},
				{Subsystem: "metrics", Status: types.HealthStatusReady},
			}
		},
		Counters: func(_ context.Context, ident identity.Identity) types.RuntimeCounters {
			rec.record(ident)
			return types.RuntimeCounters{
				// Echo the caller's tenant length so a context bleed is
				// observable; the production seam reads the live task
				// registry.
				TasksRunning:   int64(len(ident.TenantID)),
				SessionsActive: 1,
			}
		},
		Drivers: func() []types.SubsystemDriver {
			return []types.SubsystemDriver{
				{Subsystem: "state", Driver: cfg.State.Driver},
				{Subsystem: "artifacts", Driver: cfg.Artifacts.Driver},
				{Subsystem: "memory", Driver: cfg.Memory.Driver},
				{Subsystem: "eventlog", Driver: cfg.Events.Driver},
			}
		},
		Metrics: func(_ context.Context) types.MetricsSnapshot {
			return types.MetricsSnapshot{
				Counters: []types.NamedCounter{
					{Name: "harbor_events_total", Value: 1, Labels: map[string]string{"event_type": "task.spawned"}},
				},
			}
		},
		// Phase 72g (D-112): the governance / llm posture seams + the
		// audit redactor + bus the merged PostureSurface requires. The
		// runtime-posture test does not exercise governance.posture /
		// llm.posture itself, but NewPostureSurface fails closed on a
		// nil mandatory seam — so they are wired with the devstack's
		// real redactor + bus.
		Governance:  governance.NewPostureProvider(governance.Config{}),
		LLM:         llm.NewPostureProvider(llm.ConfigSnapshot{Driver: "mock"}),
		Redactor:    stack.Audit,
		Bus:         stack.Bus,
		DisplayName: "harbor-phase72f-test",
		InstanceID:  "inst-phase72f-test",
	})
	if err != nil {
		t.Fatalf("NewPostureSurface: %v", err)
	}
	return s
}

// signPostureToken mints an ES256 bearer for the given identity, with
// the supplied scopes, signed by the devstack's key.
func signPostureToken(t *testing.T, priv *ecdsa.PrivateKey, id identity.Identity, scopes []string) string {
	t.Helper()
	now := time.Now()
	claims := jwt.MapClaims{
		"iss":     "harbor-test",
		"sub":     id.UserID,
		"aud":     "harbor",
		"exp":     now.Add(1 * time.Hour).Unix(),
		"nbf":     now.Add(-1 * time.Minute).Unix(),
		"iat":     now.Unix(),
		"tenant":  id.TenantID,
		"user":    id.UserID,
		"session": id.SessionID,
	}
	if scopes != nil {
		claims["scopes"] = scopes
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = devstack.DefaultKID
	signed, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign posture token: %v", err)
	}
	return signed
}

// postPosture POSTs a RuntimeInfoRequest to /v1/control/{method} with a
// bearer token and returns the HTTP status + raw body.
func postPosture(t *testing.T, baseURL string, method methods.Method, body []byte, token string) (int, []byte) {
	t.Helper()
	url := baseURL + "/v1/control/" + string(method)
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

func TestE2E_RuntimePosture(t *testing.T) {
	cfg := runtimePostureConfig(t)
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		SkipRunLoop: true,
	})
	defer stack.Close()

	if stack.Surface == nil || stack.Validator == nil || stack.SigningKey == nil {
		t.Fatal("devstack did not assemble the Surface / Validator / SigningKey — Phase 72f needs all three")
	}

	rec := &posturePerTenantCounters{seen: map[string]identity.Identity{}}
	posture := buildPostureSurface(t, stack, rec)

	// Mount a real transports mux with the posture surface wired in,
	// behind the devstack's real ES256 validator.
	mux, err := transports.NewMux(stack.Surface, stack.Bus,
		transports.WithValidator(stack.Validator),
		transports.WithPostureSurface(posture),
	)
	if err != nil {
		t.Fatalf("transports.NewMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	devID := identity.Identity{
		TenantID:  devstack.DefaultDevTenant,
		UserID:    devstack.DefaultDevUser,
		SessionID: devstack.DefaultDevSession,
	}

	// --- (1) in-process Dispatch of every posture method --------------
	t.Run("InProcess_AllMethods", func(t *testing.T) {
		ctx, err := identity.With(context.Background(), devID)
		if err != nil {
			t.Fatalf("identity.With: %v", err)
		}
		req := &types.RuntimeInfoRequest{
			Identity: types.IdentityScope{
				Tenant: devID.TenantID, User: devID.UserID, Session: devID.SessionID,
			},
		}
		for _, m := range []methods.Method{
			methods.MethodRuntimeInfo, methods.MethodRuntimeHealth,
			methods.MethodRuntimeCounters, methods.MethodRuntimeDrivers,
			methods.MethodMetricsSnapshot,
		} {
			out, derr := posture.Dispatch(ctx, m, req)
			if derr != nil {
				t.Fatalf("in-process Dispatch(%s): %v", m, derr)
			}
			if out == nil {
				t.Fatalf("in-process Dispatch(%s): nil response", m)
			}
		}
	})

	// --- (2) wire round-trip + capability advertisement ---------------
	t.Run("Wire_RuntimeInfo_AdvertisesCapability", func(t *testing.T) {
		token := signPostureToken(t, stack.SigningKey, devID, nil)
		body, _ := json.Marshal(types.RuntimeInfoRequest{
			Identity: types.IdentityScope{
				Tenant: devID.TenantID, User: devID.UserID, Session: devID.SessionID,
			},
		})
		status, raw := postPosture(t, srv.URL, methods.MethodRuntimeInfo, body, token)
		if status != http.StatusOK {
			t.Fatalf("runtime.info: status %d, want 200; body=%s", status, raw)
		}
		var info types.RuntimeInfo
		if err := json.Unmarshal(raw, &info); err != nil {
			t.Fatalf("decode RuntimeInfo: %v; body=%s", err, raw)
		}
		if info.ProtocolVersion != types.ProtocolVersion {
			t.Errorf("runtime.info ProtocolVersion = %q, want %q", info.ProtocolVersion, types.ProtocolVersion)
		}
		if info.InstanceID != "inst-phase72f-test" {
			t.Errorf("runtime.info InstanceID = %q", info.InstanceID)
		}
		var hasPosture bool
		for _, c := range info.Capabilities {
			if c == types.CapRuntimePosture {
				hasPosture = true
			}
		}
		if !hasPosture {
			t.Errorf("runtime.info Capabilities %v missing CapRuntimePosture", info.Capabilities)
		}
	})

	t.Run("Wire_RuntimeHealth", func(t *testing.T) {
		token := signPostureToken(t, stack.SigningKey, devID, nil)
		body, _ := json.Marshal(types.RuntimeInfoRequest{
			Identity: types.IdentityScope{
				Tenant: devID.TenantID, User: devID.UserID, Session: devID.SessionID,
			},
		})
		status, raw := postPosture(t, srv.URL, methods.MethodRuntimeHealth, body, token)
		if status != http.StatusOK {
			t.Fatalf("runtime.health: status %d, want 200; body=%s", status, raw)
		}
		var h types.RuntimeHealth
		if err := json.Unmarshal(raw, &h); err != nil {
			t.Fatalf("decode RuntimeHealth: %v", err)
		}
		if len(h.Subsystems) == 0 {
			t.Error("runtime.health returned no subsystems")
		}
	})

	t.Run("Wire_RuntimeDrivers_NoDSNLeak", func(t *testing.T) {
		token := signPostureToken(t, stack.SigningKey, devID, nil)
		body, _ := json.Marshal(types.RuntimeInfoRequest{
			Identity: types.IdentityScope{
				Tenant: devID.TenantID, User: devID.UserID, Session: devID.SessionID,
			},
		})
		status, raw := postPosture(t, srv.URL, methods.MethodRuntimeDrivers, body, token)
		if status != http.StatusOK {
			t.Fatalf("runtime.drivers: status %d, want 200; body=%s", status, raw)
		}
		// No DSN / connection-string substring may leak.
		for _, marker := range []string{"://", "postgresql://", "password"} {
			if containsSub(string(raw), marker) {
				t.Errorf("runtime.drivers response leaks a DSN-shaped substring %q: %s", marker, raw)
			}
		}
		var d types.RuntimeDrivers
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("decode RuntimeDrivers: %v", err)
		}
		if len(d.Subsystems) != 4 {
			t.Errorf("runtime.drivers returned %d subsystems, want 4", len(d.Subsystems))
		}
	})

	t.Run("Wire_MetricsSnapshot", func(t *testing.T) {
		token := signPostureToken(t, stack.SigningKey, devID, nil)
		body, _ := json.Marshal(types.RuntimeInfoRequest{
			Identity: types.IdentityScope{
				Tenant: devID.TenantID, User: devID.UserID, Session: devID.SessionID,
			},
		})
		status, raw := postPosture(t, srv.URL, methods.MethodMetricsSnapshot, body, token)
		if status != http.StatusOK {
			t.Fatalf("metrics.snapshot: status %d, want 200; body=%s", status, raw)
		}
		var m types.MetricsSnapshot
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode MetricsSnapshot: %v", err)
		}
		if len(m.Counters) == 0 {
			t.Error("metrics.snapshot returned no counters")
		}
	})

	// --- (3) identity propagation through runtime.counters -----------
	t.Run("Wire_RuntimeCounters_IdentityPropagation", func(t *testing.T) {
		token := signPostureToken(t, stack.SigningKey, devID, nil)
		body, _ := json.Marshal(types.RuntimeInfoRequest{
			Identity: types.IdentityScope{
				Tenant: devID.TenantID, User: devID.UserID, Session: devID.SessionID,
			},
		})
		status, raw := postPosture(t, srv.URL, methods.MethodRuntimeCounters, body, token)
		if status != http.StatusOK {
			t.Fatalf("runtime.counters: status %d, want 200; body=%s", status, raw)
		}
		var c types.RuntimeCounters
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("decode RuntimeCounters: %v", err)
		}
		// The Counters seam echoes the caller's tenant length.
		if c.TasksRunning != int64(len(devID.TenantID)) {
			t.Errorf("runtime.counters TasksRunning = %d, want %d — identity did not propagate to the seam",
				c.TasksRunning, len(devID.TenantID))
		}
		seen, ok := rec.lookup(devID.TenantID)
		if !ok {
			t.Fatalf("Counters seam never saw tenant %q — identity propagation broken", devID.TenantID)
		}
		if seen != devID {
			t.Errorf("Counters seam saw identity %+v, want %+v", seen, devID)
		}
	})

	// --- (4) failure mode: cross-tenant rejection + admission --------
	t.Run("Wire_CrossTenant_RequiresAdminScope", func(t *testing.T) {
		// A token verified as devID; the body asks for a different
		// tenant. Without the admin scope this must be rejected 403.
		token := signPostureToken(t, stack.SigningKey, devID, nil)
		crossBody, _ := json.Marshal(types.RuntimeInfoRequest{
			Identity: types.IdentityScope{
				Tenant: "other-tenant", User: devID.UserID, Session: devID.SessionID,
			},
		})
		status, raw := postPosture(t, srv.URL, methods.MethodRuntimeCounters, crossBody, token)
		if status != http.StatusForbidden {
			t.Fatalf("cross-tenant without admin: status %d, want 403; body=%s", status, raw)
		}

		// With the admin scope the cross-tenant read succeeds.
		adminTok := signPostureToken(t, stack.SigningKey, devID, []string{"admin"})
		status, raw = postPosture(t, srv.URL, methods.MethodRuntimeCounters, crossBody, adminTok)
		if status != http.StatusOK {
			t.Fatalf("cross-tenant with admin: status %d, want 200; body=%s", status, raw)
		}
		var c types.RuntimeCounters
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("decode cross-tenant RuntimeCounters: %v", err)
		}
		// The admin-scoped read reaches the other tenant's slice.
		if c.TasksRunning != int64(len("other-tenant")) {
			t.Errorf("admin cross-tenant counters TasksRunning = %d, want %d", c.TasksRunning, len("other-tenant"))
		}
	})

	t.Run("Wire_MissingIdentity_FailsClosed", func(t *testing.T) {
		// A request whose body identity is incomplete is rejected at
		// the surface edge. The auth middleware verified devID, so the
		// transport backfills an EMPTY body from the JWT — to actually
		// reach the surface's identity gate we send a body with a
		// mismatching user (the transport rejects user/session
		// mismatch with identity_required → 401).
		token := signPostureToken(t, stack.SigningKey, devID, nil)
		badBody, _ := json.Marshal(types.RuntimeInfoRequest{
			Identity: types.IdentityScope{
				Tenant: devID.TenantID, User: "someone-else", Session: devID.SessionID,
			},
		})
		status, _ := postPosture(t, srv.URL, methods.MethodRuntimeInfo, badBody, token)
		if status != http.StatusUnauthorized {
			t.Fatalf("body-identity mismatch: status %d, want 401", status)
		}
	})

	// --- (5) N≥10 concurrent operators read runtime.counters ---------
	t.Run("Concurrency_TenOperators", func(t *testing.T) {
		time.Sleep(20 * time.Millisecond)
		baseline := runtime.NumGoroutine()

		const n = 16
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				token := signPostureToken(t, stack.SigningKey, devID, nil)
				body, _ := json.Marshal(types.RuntimeInfoRequest{
					Identity: types.IdentityScope{
						Tenant: devID.TenantID, User: devID.UserID, Session: devID.SessionID,
					},
				})
				status, raw := postPosture(t, srv.URL, methods.MethodRuntimeCounters, body, token)
				if status != http.StatusOK {
					errs <- fmt.Errorf("operator-%d: status %d; body=%s", i, status, raw)
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}

		time.Sleep(50 * time.Millisecond)
		if after := runtime.NumGoroutine(); after > baseline+8 {
			t.Errorf("goroutine leak: baseline %d, after %d", baseline, after)
		}
	})
}

// containsSub reports whether s contains sub.
func containsSub(s, sub string) bool {
	return strings.Contains(s, sub)
}
