// Wave-end composing E2E (CLAUDE.md §17.7 step 5) for the v1.7
// "Protocol-edge hardening" band — the three already-merged phases:
//
//   - agent-config Protocol capability (runtime.info advertises
//     `agent_config` iff the agent-config control plane is mounted).
//   - the JWKS max-stale ceiling (fail-closed when the cached key snapshot
//     exceeds the ceiling and the fetcher is unreachable).
//   - the `sessions.delete` session-erasure surface (the three-store
//     cascade, own-session-only, refused on a RUNNING task).
//
// This suite is IN ADDITION to each phase's own per-phase integration test
// (`phase128_*`, `jwks_max_stale_*`, `phase130_*`), which remain the
// per-phase gates. It proves the COMBINED surface is alive together:
// ONE assembled stack with real drivers on every seam — a real control
// transport over an httptest.Server, real State / Memory / Artifact
// production drivers, a real sessions.Registry + CascadeEraser, the real
// agentcfgprotocol.Service, a real PostureSurface, and a real
// JWKS-backed auth.Validator with an INJECTED controllable clock — mounted
// on one transports.NewMux. No mocks at any seam (CLAUDE.md §17.3).
//
// The five mandatory combined-surface assertions (CLAUDE.md §17.7):
//
//  1. Both capabilities coexist — runtime.info over a verified triple
//     advertises BOTH agent_config (128) AND session_lifecycle (130).
//  2. Full erasure lifecycle (130) — open → state + turn + artifact +
//     completed task → sessions.delete → counts → inspect 404 / history
//     empty / artifacts empty / memory clean.
//  3. Per-edge failure modes — 129 fail-closed (jwks_stale 401 then
//     recover) and 130 fail-loud refusals (409 session_running +
//     401 identity_required, stores untouched).
//  4. Identity propagation end-to-end (cross-tenant isolation holds).
//  5. N≥10 concurrency stress — concurrent distinct-session deletes
//     interleaved with runtime.info + JWKS Validate; no cross-talk, no
//     data race, no goroutine leak after teardown.
//
// Runs under -race: `go test -race -run TestE2E_WaveV17 ./test/integration/...`.
package integration_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/memory"
	_ "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	runtimeposture "github.com/hurtener/Harbor/internal/runtime/posture"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/sessions"
	sessionsprotocol "github.com/hurtener/Harbor/internal/sessions/protocol"
	"github.com/hurtener/Harbor/internal/skills"
	localdb "github.com/hurtener/Harbor/internal/skills/drivers/localdb"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

const (
	waveV17Issuer   = "https://idp.wavev17.test"
	waveV17Audience = "harbor-runtime"
	waveV17KID      = "wavev17-rsa-1"
	waveV17MaxStale = 2 * time.Minute
	// waveV17RunningSession is the one session id the running-task probe
	// reports as RUNNING, so its erasure is refused with 409.
	waveV17RunningSession = "s-wavev17-running"
)

// waveV17Clock is a mutex-guarded controllable clock for the JWKS keyset.
// It is anchored at time.Now() at construction and only advances when the
// test calls advance() — so the keyset's notion of "now" (which drives
// snapshot staleness) is decoupled from real time, while the Validator's
// real-time exp/nbf checks pass with far-future token claims. No
// time.Sleep is used as a synchronisation primitive (CLAUDE.md §17.4).
type waveV17Clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *waveV17Clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *waveV17Clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// waveV17Assembly is the single shared stack the wave-end E2E composes —
// every field is a real production driver / surface, never a mock.
type waveV17Assembly struct {
	srvURL    string
	validator auth.Validator
	rsaPriv   *rsa.PrivateKey
	clk       *waveV17Clock
	jwksDown  *atomic.Bool

	store   state.StateStore
	mem     memory.MemoryStore
	arts    artifacts.ArtifactStore
	taskReg tasks.TaskRegistry
	reg     *sessions.Registry

	closeOnce sync.Once
	closers   []func()
}

// closeAll tears the assembly down in dependency order. Idempotent (guarded
// by sync.Once) so the goroutine-leak test can close explicitly before its
// NumGoroutine assertion while the registered t.Cleanup re-call is a no-op.
func (a *waveV17Assembly) closeAll() {
	a.closeOnce.Do(func() {
		for _, c := range a.closers {
			c()
		}
	})
}

// assembleWaveV17 builds the one shared stack — real drivers everywhere on
// the seam, a JWKS-backed Validator with an injected clock, the posture +
// sessions + agent-config surfaces all mounted on one mux. Registers
// teardown on t.Cleanup; exposes closeAll for the leak test's explicit
// teardown.
func assembleWaveV17(t *testing.T) *waveV17Assembly {
	t.Helper()
	ctx := context.Background()
	red := auditpatterns.New()

	// --- a real RSA keypair + a real JWKS endpoint with a flip-to-fail
	// switch (the IdP going unreachable: a 503 the keyset treats as a
	// refresh failure). ---
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa: %v", err)
	}
	var down atomic.Bool
	jwksBody := waveV17EncodeRSAJWKS(t, &priv.PublicKey, waveV17KID)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if down.Load() {
			http.Error(w, "idp unreachable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksBody)
	}))

	// --- real persistence drivers ---
	store, err := state.Open(ctx, config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	bus, err := durable.New(ctx, config.EventsConfig{
		Driver:                   "durable",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         256,
	}, red, store)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	mem, err := memory.Open(ctx, memory.ConfigSnapshot{
		Driver: "inmem", Strategy: memory.StrategyTruncation, BudgetTokens: 1000,
	}, memory.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	arts, err := artifacts.Open(ctx, config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	taskReg, err := tasks.Open(ctx, tasks.Dependencies{
		Store: store, Bus: bus, Redactor: audit.Redactor(red),
		Cfg: config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}

	// --- the real control surface (steering + tasks) ---
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		t.Fatalf("NewControlSurface: %v", err)
	}

	// --- the real sessions registry + the three-store CascadeEraser. The
	// running-task probe reports RUNNING only for the dedicated session, so
	// its erasure is refused (409). ---
	probe := func(_ context.Context, q identity.Quadruple) (bool, error) {
		return q.SessionID == waveV17RunningSession, nil
	}
	reg, err := sessions.New(store, config.SessionsConfig{}, bus,
		sessions.WithGCPolicy(sessions.GCPolicy{RunningProbe: probe}))
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: reg, State: store, Memory: mem, Artifacts: arts, Bus: bus, Redactor: red,
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}
	projector, err := sessionsprotocol.NewListerProjector(reg)
	if err != nil {
		t.Fatalf("NewListerProjector: %v", err)
	}
	sessionsSvc, err := sessionsprotocol.NewService(projector,
		sessionsprotocol.WithBus(bus),
		sessionsprotocol.WithRedactor(red),
		sessionsprotocol.WithEraser(eraser),
	)
	if err != nil {
		t.Fatalf("sessions/protocol.NewService: %v", err)
	}

	// --- the real agent-config control plane (StateStore-backed registry +
	// localdb SkillStore) ---
	acState, err := state.Open(ctx, config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("agentcfg state.Open: %v", err)
	}
	skillStore, err := localdb.New(skills.ConfigSnapshot{Driver: "localdb", DSN: ":memory:"}, skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("skills localdb.New: %v", err)
	}
	acReg, err := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: acState, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	agentCfgSvc, err := agentcfgprotocol.NewService(acReg, agentcfgprotocol.WithSkillStore(skillStore))
	if err != nil {
		t.Fatalf("agentcfgprotocol.NewService: %v", err)
	}

	// --- the real PostureSurface, with BOTH conditional flags wired true so
	// runtime.info honestly advertises agent_config AND session_lifecycle. ---
	posture, err := protocol.NewPostureSurface(protocol.PostureDeps{
		Build: prototypes.RuntimeInfo{
			BuildVersion:   "v0.0.0-test",
			BuildCommit:    "wave-v17",
			BuildGoVersion: runtime.Version(),
		},
		Clock:    time.Now,
		BootedAt: time.Now().Add(-30 * time.Second),
		Health: func(_ context.Context) []prototypes.SubsystemHealth {
			return []prototypes.SubsystemHealth{
				{Subsystem: "state", Status: prototypes.HealthStatusReady},
				{Subsystem: "events", Status: prototypes.HealthStatusReady},
				{Subsystem: "tasks", Status: prototypes.HealthStatusReady},
				{Subsystem: "artifacts", Status: prototypes.HealthStatusReady},
				{Subsystem: "memory", Status: prototypes.HealthStatusReady},
			}
		},
		Counters: runtimeposture.CountersProvider(taskReg, nil, nil),
		Drivers: func() []prototypes.SubsystemDriver {
			return []prototypes.SubsystemDriver{
				{Subsystem: "state", Driver: "inmem"},
				{Subsystem: "artifacts", Driver: "inmem"},
				{Subsystem: "memory", Driver: "inmem"},
				{Subsystem: "eventlog", Driver: "durable"},
			}
		},
		Metrics: func(_ context.Context) prototypes.MetricsSnapshot {
			return prototypes.MetricsSnapshot{}
		},
		Governance:                governance.NewPostureProvider(governance.Config{}),
		LLM:                       llm.NewPostureProvider(llm.ConfigSnapshot{Driver: "mock"}),
		Redactor:                  red,
		Bus:                       bus,
		DisplayName:               "harbor-wave-v17-test",
		InstanceID:                "inst-wave-v17-test",
		AgentConfigAvailable:      true,
		SessionLifecycleAvailable: true,
	})
	if err != nil {
		t.Fatalf("NewPostureSurface: %v", err)
	}

	// --- the one JWKS-backed Validator with the injected clock ---
	clk := &waveV17Clock{t: time.Now()}
	validator, err := auth.NewJWKSValidator(ctx, config.IdentityConfig{
		JWTAlgorithms: []string{"RS256"},
		Issuer:        waveV17Issuer,
		Audience:      waveV17Audience,
		JWKSURL:       jwks.URL,
		JWKSMaxStale:  waveV17MaxStale,
	}, auth.ValidatorDeps{Redactor: red, Bus: bus},
		auth.WithJWKSClock(clk.now),
		auth.WithJWKSMinRefreshInterval(time.Minute),
		auth.WithJWKSRefreshTTL(5*time.Minute),
	)
	if err != nil {
		t.Fatalf("NewJWKSValidator: %v", err)
	}

	// --- the one mux: posture + sessions + agent-config, all behind the
	// shared validator. ---
	mux, err := transports.NewMux(surface, bus,
		transports.WithValidator(validator),
		transports.WithPostureSurface(posture),
		transports.WithSessionsService(sessionsSvc),
		transports.WithAgentConfigService(agentCfgSvc),
	)
	if err != nil {
		t.Fatalf("transports.NewMux: %v", err)
	}
	srv := httptest.NewServer(mux)

	a := &waveV17Assembly{
		srvURL:    srv.URL,
		validator: validator,
		rsaPriv:   priv,
		clk:       clk,
		jwksDown:  &down,
		store:     store,
		mem:       mem,
		arts:      arts,
		taskReg:   taskReg,
		reg:       reg,
		closers: []func(){
			srv.Close,
			jwks.Close,
			func() { _ = reg.CloseRegistry(ctx) },
			func() { _ = acReg.Close(ctx) },
			func() { _ = skillStore.Close(ctx) },
			func() { _ = acState.Close(ctx) },
			func() { _ = taskReg.Close(ctx) },
			func() { _ = mem.Close(ctx) },
			func() { _ = arts.Close(ctx) },
			func() { _ = bus.Close(ctx) },
			func() { _ = store.Close(ctx) },
		},
	}
	t.Cleanup(a.closeAll)
	return a
}

// signToken mints an RS256 bearer for id (matching the JWKS public key),
// with far-future exp/nbf so the Validator's real-time token checks always
// pass — staleness is driven solely by the injected keyset clock.
func (a *waveV17Assembly) signToken(t *testing.T, id identity.Identity, scopes []string) string {
	t.Helper()
	now := time.Now()
	claims := jwt.MapClaims{
		"iss":     waveV17Issuer,
		"aud":     waveV17Audience,
		"sub":     id.UserID,
		"exp":     now.Add(72 * time.Hour).Unix(),
		"nbf":     now.Add(-time.Minute).Unix(),
		"iat":     now.Unix(),
		"tenant":  id.TenantID,
		"user":    id.UserID,
		"session": id.SessionID,
	}
	if scopes != nil {
		claims["scopes"] = scopes
	} else {
		claims["scopes"] = []string{}
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = waveV17KID
	signed, err := tok.SignedString(a.rsaPriv)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

// seedSession opens a session in the registry and seeds it with a turn + an
// artifact + a completed task, so the erasure cascade has real per-store
// data to remove. Returns the number of artifacts seeded (always 1).
func (a *waveV17Assembly) seedSession(t *testing.T, id identity.Identity) {
	t.Helper()
	ctx := context.Background()
	ictx, _ := identity.With(ctx, id)
	q := identity.Quadruple{Identity: id}

	if _, err := a.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("reg.Open %s: %v", id.SessionID, err)
	}
	if err := a.mem.AddTurn(ictx, q, memory.ConversationTurn{
		UserMessage: "secret question", AssistantResponse: "secret answer",
	}); err != nil {
		t.Fatalf("AddTurn %s: %v", id.SessionID, err)
	}
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	if _, err := a.arts.PutBytes(ctx, scope, []byte("private blob"), artifacts.PutOpts{Namespace: "test"}); err != nil {
		t.Fatalf("PutBytes %s: %v", id.SessionID, err)
	}
	// A real completed task in this session's scope (Spawn → Running →
	// Complete) — the kind-agnostic scope delete removes its state records.
	handle, err := a.taskReg.Spawn(ictx, tasks.SpawnRequest{
		Identity:    q,
		Kind:        tasks.KindForeground,
		Description: "wave-v17 completed task",
		Query:       "noop",
	})
	if err != nil {
		t.Fatalf("Spawn %s: %v", id.SessionID, err)
	}
	if err := a.taskReg.MarkRunning(ictx, handle.ID); err != nil {
		t.Fatalf("MarkRunning %s: %v", id.SessionID, err)
	}
	if err := a.taskReg.MarkComplete(ictx, handle.ID, tasks.TaskResult{Value: json.RawMessage(`{"ok":true}`)}); err != nil {
		t.Fatalf("MarkComplete %s: %v", id.SessionID, err)
	}
}

// artifactCount returns the number of artifacts in id's scope.
func (a *waveV17Assembly) artifactCount(t *testing.T, id identity.Identity) int {
	t.Helper()
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	refs, err := a.arts.List(context.Background(), scope)
	if err != nil {
		t.Fatalf("artifacts.List %s: %v", id.SessionID, err)
	}
	return len(refs)
}

// waveV17EncodeRSAJWKS encodes pub as a single-key JWK Set JSON, produced
// independently of the JWKS parser under test (math/big + base64url).
func waveV17EncodeRSAJWKS(t *testing.T, pub *rsa.PublicKey, kid string) []byte {
	t.Helper()
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	set := map[string]any{
		"keys": []any{
			map[string]any{
				"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid,
				"n": b64(pub.N.Bytes()),
				"e": b64(big.NewInt(int64(pub.E)).Bytes()),
			},
		},
	}
	raw, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	return raw
}

// waveV17HasCap reports whether caps contains c.
func waveV17HasCap(caps []prototypes.Capability, c prototypes.Capability) bool {
	for _, have := range caps {
		if have == c {
			return true
		}
	}
	return false
}

// waveV17PostControl POSTs a RuntimeInfoRequest to /v1/control/{method}.
func waveV17PostControl(t *testing.T, baseURL string, method methods.Method, body []byte, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/control/"+string(method), strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST control: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// waveV17RuntimeInfo fetches runtime.info for id and decodes the response.
func waveV17RuntimeInfo(t *testing.T, a *waveV17Assembly, id identity.Identity) (int, prototypes.RuntimeInfo) {
	t.Helper()
	body, _ := json.Marshal(prototypes.RuntimeInfoRequest{
		Identity: prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
	})
	status, raw := waveV17PostControl(t, a.srvURL, methods.MethodRuntimeInfo, body, a.signToken(t, id, nil))
	var info prototypes.RuntimeInfo
	if status == http.StatusOK {
		if err := json.Unmarshal(raw, &info); err != nil {
			t.Fatalf("decode RuntimeInfo: %v; body=%s", err, raw)
		}
	}
	return status, info
}

// deleteBody builds the sessions.delete request body for id.
func deleteBody(id identity.Identity) string {
	return fmt.Sprintf(`{"identity":{"tenant":%q,"user":%q,"session":%q}}`,
		id.TenantID, id.UserID, id.SessionID)
}

// TestE2E_WaveV17_CombinedSurface composes the band's three surfaces on one
// shared assembly and asserts the combined surface is alive together.
func TestE2E_WaveV17_CombinedSurface(t *testing.T) {
	a := assembleWaveV17(t)

	// ===================================================================
	// (1) Both capabilities coexist — runtime.info advertises agent_config
	//     (128) AND session_lifecycle (130) in one capabilities projection,
	//     and the canonical universe is internally consistent (7 caps).
	// ===================================================================
	t.Run("BothCapabilitiesCoexist", func(t *testing.T) {
		id := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-info"}
		status, info := waveV17RuntimeInfo(t, a, id)
		if status != http.StatusOK {
			t.Fatalf("runtime.info status=%d, want 200", status)
		}
		// ----- MANDATORY ASSERTION 1: both 128 + 130 capabilities coexist
		if !waveV17HasCap(info.Capabilities, prototypes.CapAgentConfig) ||
			!waveV17HasCap(info.Capabilities, prototypes.CapSessionLifecycle) {
			t.Fatalf("runtime.info Capabilities %v must advertise BOTH agent_config AND session_lifecycle", info.Capabilities)
		}
		// The conformance universe is internally consistent: every
		// advertised capability is canonical, and the canonical set is 7.
		for _, c := range info.Capabilities {
			if !prototypes.IsValidCapability(c) {
				t.Errorf("advertised capability %q is not in the canonical universe", c)
			}
		}
		if n := len(prototypes.Capabilities()); n != 7 {
			t.Errorf("canonical capability universe has %d entries, want 7", n)
		}
	})

	// ===================================================================
	// (2) Full erasure lifecycle (130) + (4) identity propagation /
	//     cross-tenant isolation.
	// ===================================================================
	t.Run("FullErasureLifecycle", func(t *testing.T) {
		ctx := context.Background()
		idErase := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-erase"}
		// A cross-tenant bystander that MUST survive the erasure — the
		// multi-isolation triple flows into the scoped stores, so erasing
		// tenant-A's session never touches tenant-B's data.
		idBystander := identity.Identity{TenantID: "tenant-B", UserID: "u-B", SessionID: "s-bystander"}

		a.seedSession(t, idErase)
		a.seedSession(t, idBystander)

		// sessions.delete over the verified triple.
		dstatus, dbody := postPhase130(t, a.srvURL, "delete", deleteBody(idErase), a.signToken(t, idErase, nil))
		if dstatus != http.StatusOK {
			t.Fatalf("delete: status=%d body=%s", dstatus, dbody)
		}
		var resp prototypes.SessionsDeleteResponse
		if err := json.Unmarshal(dbody, &resp); err != nil {
			t.Fatalf("decode delete response: %v; body=%s", err, dbody)
		}
		// ----- MANDATORY ASSERTION 2: deletion counts + full cascade.
		if !resp.Deleted || !resp.MemoryPurged || resp.SessionID != "s-erase" {
			t.Fatalf("delete response = %+v", resp)
		}
		if resp.ArtifactsDeleted != 1 {
			t.Errorf("ArtifactsDeleted = %d, want 1", resp.ArtifactsDeleted)
		}
		if resp.StateRecordsDeleted <= 0 {
			t.Errorf("StateRecordsDeleted = %d, want > 0", resp.StateRecordsDeleted)
		}

		// inspect → 404 not_found.
		istatus, ibody := postPhase130(t, a.srvURL, "inspect",
			`{"session_id":"s-erase",`+`"identity":{"tenant":"tenant-A","user":"u-A","session":"s-erase"}}`,
			a.signToken(t, idErase, nil))
		if istatus != http.StatusNotFound {
			t.Fatalf("post-erasure inspect: status=%d, want 404; body=%s", istatus, ibody)
		}
		// artifacts gone.
		if n := a.artifactCount(t, idErase); n != 0 {
			t.Errorf("artifacts survived erasure: %d", n)
		}
		// memory clean.
		patch, err := a.mem.GetLLMContext(ctx, identity.Quadruple{Identity: idErase})
		if err != nil {
			t.Fatalf("GetLLMContext: %v", err)
		}
		if len(patch.RecentTurns) != 0 {
			t.Errorf("memory survived erasure: %d turns", len(patch.RecentTurns))
		}
		// state.history empty: the erased triple's durable event stream + the
		// session.lifecycle record are gone.
		if _, err := a.store.Load(ctx, identity.Quadruple{Identity: idErase}, "events.durable.head"); !errors.Is(err, state.ErrNotFound) {
			t.Errorf("erased triple durable head survived (state.history non-empty): err=%v", err)
		}
		if _, err := a.store.Load(ctx, identity.Quadruple{Identity: idErase}, "session.lifecycle"); !errors.Is(err, state.ErrNotFound) {
			t.Errorf("session.lifecycle survived erasure: err=%v", err)
		}

		// ----- (4) identity propagation / cross-tenant isolation: the
		// tenant-B bystander is fully intact.
		if n := a.artifactCount(t, idBystander); n != 1 {
			t.Errorf("cross-tenant isolation broken: tenant-B artifacts = %d, want 1", n)
		}
		bpatch, err := a.mem.GetLLMContext(ctx, identity.Quadruple{Identity: idBystander})
		if err != nil {
			t.Fatalf("GetLLMContext bystander: %v", err)
		}
		if len(bpatch.RecentTurns) != 1 {
			t.Errorf("cross-tenant isolation broken: tenant-B turns = %d, want 1", len(bpatch.RecentTurns))
		}
		if _, err := a.store.Load(ctx, identity.Quadruple{Identity: idBystander}, "session.lifecycle"); err != nil {
			t.Errorf("cross-tenant isolation broken: tenant-B session.lifecycle erased: %v", err)
		}
	})

	// ===================================================================
	// (3) 130 fail-loud refusals — 409 session_running + 401
	//     identity_required, both with all stores untouched.
	// ===================================================================
	t.Run("ErasureRefusals_StoresUntouched", func(t *testing.T) {
		ctx := context.Background()

		// --- running-task → 409 session_running, stores untouched ---
		idRun := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: waveV17RunningSession}
		a.seedSession(t, idRun)
		rstatus, rbody := postPhase130(t, a.srvURL, "delete", deleteBody(idRun), a.signToken(t, idRun, nil))
		// ----- MANDATORY ASSERTION 3 (130 fail-loud): 409 + untouched.
		if rstatus != http.StatusConflict {
			t.Fatalf("running-task delete: status=%d, want 409; body=%s", rstatus, rbody)
		}
		var rerr protoerrors.Error
		if err := json.Unmarshal(rbody, &rerr); err != nil {
			t.Fatalf("decode 409 body: %v; body=%s", err, rbody)
		}
		if rerr.Code != protoerrors.CodeSessionRunning {
			t.Errorf("409 code = %q, want %q", rerr.Code, protoerrors.CodeSessionRunning)
		}
		if _, err := a.store.Load(ctx, identity.Quadruple{Identity: idRun}, "session.lifecycle"); err != nil {
			t.Errorf("running-refusal deleted the session record: %v", err)
		}
		if n := a.artifactCount(t, idRun); n != 1 {
			t.Errorf("running-refusal touched artifacts: %d, want 1", n)
		}

		// --- foreign-target → 401 identity_required, stores untouched.
		// Own-session-only: a valid tenant-A token but a body naming a
		// different tenant. Rejected at the transport identity gate before
		// the eraser runs (NO admin / cross-tenant erasure path). ---
		idForeign := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-foreign"}
		a.seedSession(t, idForeign)
		foreignBody := `{"identity":{"tenant":"tenant-OTHER","user":"u-A","session":"s-foreign"}}`
		fstatus, fbody := postPhase130(t, a.srvURL, "delete", foreignBody, a.signToken(t, idForeign, nil))
		if fstatus != http.StatusUnauthorized {
			t.Fatalf("foreign-target delete: status=%d, want 401; body=%s", fstatus, fbody)
		}
		var ferr protoerrors.Error
		if err := json.Unmarshal(fbody, &ferr); err != nil {
			t.Fatalf("decode foreign-target 401 body: %v; body=%s", err, fbody)
		}
		if ferr.Code != protoerrors.CodeIdentityRequired {
			t.Errorf("foreign-target code = %q, want %q", ferr.Code, protoerrors.CodeIdentityRequired)
		}
		// stores untouched: the attacker's own (tenant-A) session survives.
		if n := a.artifactCount(t, idForeign); n != 1 {
			t.Errorf("foreign-target refusal touched artifacts: %d, want 1", n)
		}
		if _, err := a.store.Load(ctx, identity.Quadruple{Identity: idForeign}, "session.lifecycle"); err != nil {
			t.Errorf("foreign-target refusal deleted the session record: %v", err)
		}
	})

	// ===================================================================
	// (3) 129 fail-closed — advance the injected clock past the max-stale
	//     ceiling with the JWKS fetcher down; a request that verified while
	//     fresh is now 401 jwks_stale; a successful refresh recovers it.
	//     Run LAST among the functional subtests and self-recovers, so the
	//     shared validator is left fresh.
	// ===================================================================
	t.Run("JWKSMaxStaleFailClosed", func(t *testing.T) {
		id := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-stale"}

		// Fresh — the same request verifies and reaches runtime.info.
		if status, _ := waveV17RuntimeInfo(t, a, id); status != http.StatusOK {
			t.Fatalf("pre-stale runtime.info status=%d, want 200", status)
		}

		// IdP down + clock past the 2m ceiling → 401 with the distinct
		// jwks_stale wire reason.
		a.jwksDown.Store(true)
		a.clk.advance(3 * time.Minute)
		body, _ := json.Marshal(prototypes.RuntimeInfoRequest{
			Identity: prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		})
		// ----- MANDATORY ASSERTION 3 (129 fail-closed): 401 jwks_stale.
		sstatus, sraw := waveV17PostControl(t, a.srvURL, methods.MethodRuntimeInfo, body, a.signToken(t, id, nil))
		if sstatus != http.StatusUnauthorized {
			t.Fatalf("stale runtime.info status=%d, want 401; body=%s", sstatus, sraw)
		}
		var serr protoerrors.Error
		_ = json.Unmarshal(sraw, &serr)
		if serr.Code != protoerrors.CodeAuthRejected {
			t.Errorf("stale error code = %q, want %q", serr.Code, protoerrors.CodeAuthRejected)
		}
		if !strings.Contains(serr.Message, "jwks_stale") {
			t.Errorf("stale message = %q, want it to carry the jwks_stale reason", serr.Message)
		}

		// IdP recovers + clock past the refresh window → staleness resets
		// and the same token verifies again.
		a.jwksDown.Store(false)
		a.clk.advance(2 * time.Minute)
		if status, _ := waveV17RuntimeInfo(t, a, id); status != http.StatusOK {
			t.Fatalf("recovered runtime.info status=%d, want 200", status)
		}
	})
}

// TestE2E_WaveV17_ConcurrencyStress drives N≥10 concurrent distinct-session
// deletes interleaved with runtime.info posts and direct JWKS Validate calls
// against ONE shared assembly, then proves no cross-talk, no data race
// (the -race gate), and no goroutine leak after teardown. Not parallel:
// runtime.NumGoroutine is process-global.
func TestE2E_WaveV17_ConcurrencyStress(t *testing.T) {
	baseline := runtime.NumGoroutine()

	a := assembleWaveV17(t)
	ctx := context.Background()

	const nSessions = 12 // ≥10 (CLAUDE.md §17.3)

	// Distinct stress sessions (all tenant-A/u-A; isolation key is the
	// session), each seeded with its own data, plus one control session
	// that is NEVER deleted — erasing any stress session must not touch it.
	stressIDs := make([]identity.Identity, nSessions)
	for i := range stressIDs {
		stressIDs[i] = identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: fmt.Sprintf("s-stress-%02d", i)}
		a.seedSession(t, stressIDs[i])
	}
	control := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-control"}
	a.seedSession(t, control)

	type delResult struct {
		status     int
		sessionID  string
		artDeleted int
	}
	results := make([]delResult, nSessions)

	var wg sync.WaitGroup

	// N concurrent deletes — each goroutine erases ONLY its own session.
	for i := range stressIDs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := stressIDs[i]
			status, raw := postPhase130(t, a.srvURL, "delete", deleteBody(id), a.signToken(t, id, nil))
			res := delResult{status: status}
			if status == http.StatusOK {
				var r prototypes.SessionsDeleteResponse
				if err := json.Unmarshal(raw, &r); err != nil {
					t.Errorf("[stress %d] decode delete: %v; body=%s", i, err, raw)
				}
				res.sessionID = r.SessionID
				res.artDeleted = r.ArtifactsDeleted
			}
			results[i] = res
		}(i)
	}

	// Interleave runtime.info posts (control session token).
	const infoWorkers = 8
	for w := 0; w < infoWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, info := waveV17RuntimeInfo(t, a, control)
			if status != http.StatusOK {
				t.Errorf("[stress info] status=%d, want 200", status)
				return
			}
			if !waveV17HasCap(info.Capabilities, prototypes.CapAgentConfig) ||
				!waveV17HasCap(info.Capabilities, prototypes.CapSessionLifecycle) {
				t.Errorf("[stress info] capabilities widened/narrowed: %v", info.Capabilities)
			}
		}()
	}

	// Interleave direct JWKS Validate calls — assert no identity bleed.
	const validateWorkers = 8
	for w := 0; w < validateWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok := a.signToken(t, control, []string{"admin"})
			v, err := a.validator.Validate(ctx, tok)
			if err != nil {
				t.Errorf("[stress validate] %v", err)
				return
			}
			if v.Identity.TenantID != "tenant-A" || v.Identity.UserID != "u-A" || v.Identity.SessionID != "s-control" {
				t.Errorf("[stress validate] identity bled: %+v", v.Identity)
			}
		}()
	}

	wg.Wait()

	// ----- MANDATORY ASSERTION 5: no cross-talk under concurrency.
	for i, res := range results {
		if res.status != http.StatusOK {
			t.Errorf("[stress %d] delete status=%d, want 200", i, res.status)
			continue
		}
		if res.sessionID != stressIDs[i].SessionID {
			t.Errorf("[stress %d] erased the wrong session: got %q, want %q", i, res.sessionID, stressIDs[i].SessionID)
		}
		if res.artDeleted != 1 {
			t.Errorf("[stress %d] ArtifactsDeleted=%d, want 1 (erasing one session must not reach another's artifacts)", i, res.artDeleted)
		}
		if n := a.artifactCount(t, stressIDs[i]); n != 0 {
			t.Errorf("[stress %d] artifacts survived erasure: %d", i, n)
		}
	}
	// The never-targeted control session is fully intact — erasing A never
	// touched B.
	if n := a.artifactCount(t, control); n != 1 {
		t.Errorf("control session disturbed by concurrent erasures: artifacts=%d, want 1", n)
	}
	if _, err := a.store.Load(ctx, identity.Quadruple{Identity: control}, "session.lifecycle"); err != nil {
		t.Errorf("control session.lifecycle disturbed by concurrent erasures: %v", err)
	}

	// Tear down the shared assembly, then assert no goroutine leak. The
	// long-lived components (the durable EventBus, the httptest servers)
	// are cancellable + joined on Close, so goroutines return to baseline.
	a.closeAll()

	const slack = 6
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && runtime.NumGoroutine() > baseline+slack {
		time.Sleep(20 * time.Millisecond)
	}
	if delta := runtime.NumGoroutine() - baseline; delta > slack {
		t.Errorf("goroutine count rose by %d after teardown (baseline=%d, after=%d) — join-on-shutdown contract violated",
			delta, baseline, runtime.NumGoroutine())
	}
}
