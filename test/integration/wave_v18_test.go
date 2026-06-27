// Wave-end composing E2E (CLAUDE.md §17.7 step 5) for the v1.8.0
// "Adopter-Path" wave — the band that makes all three advertised adopter
// paths work end-to-end. This suite is IN ADDITION to each wave phase's
// own per-phase gate (the assemble runonce tests, the 131c OIDC
// round-trip, the 131d harbor-token CLI tests, the 136 MCP agent-call
// test). It proves the COMBINED adopter surface is alive together, with
// real drivers on every seam (CLAUDE.md §17.3):
//
//   - EMBED — a real assembled Stack runs a goal through the production
//     one-call runner Stack.RunOnce (132 / D-265), plus a WithStream
//     variant asserting ordered streamed chunks arrive BEFORE the final
//     envelope (132-stream / D-266). Real production drivers everywhere;
//     the mock LLM is the explicit dev-only opt-in (D-089).
//
//   - PROTOCOL — BOTH on-ramps that close the v1.8.0 serve-attach cliff,
//     each against a real `harbor serve` subprocess whose JWKS verifier
//     is untouched (the §3 resolution):
//
//   - the BINDING IdP leg: a hermetic ES256/JWKS mock-OIDC issuer
//     (131c) → `harbor serve` pointed at the issuer JWKS → an OAuth2
//     client-credentials token → runtime.info OK + the granted scope
//     (decoded client-side from the JWT; the verifier authenticates
//     the signature, runtime.info echoes no scopes);
//
//   - the bring-your-own leg: `harbor token keygen` → serve's
//     `identity.jwks_file` pointed at the emitted jwks.json →
//     `harbor token mint` with matching --issuer/--audience →
//     runtime.info OK (131d / D-264);
//
//   - an asserted 401 on a mismatched-`iss` token (serve rejects).
//
//   - MCP TOOL DISPATCH — an in-process goal driving the runtime so the
//     planner invokes an MCP-sourced tool (`mcptest_echo`) THROUGH the
//     executor to a real stdio MCP subprocess, the echoed sentinel
//     proving the dispatch (136, §17.8 spec-derived fixture).
//
// The mandatory §17.3/§17.4 assertions:
//
//  1. Real drivers on every seam — no mock at any boundary except the
//     explicit dev-only mock LLM on the embed path and the scripted LLM
//     on the MCP path (both deliberate, gated test surfaces).
//  2. ≥1 failure mode per edge — a mismatched-iss token 401, a garbage
//     token 401, a missing-identity request 401; the D-220 invariant
//     (`harbor serve` mints nothing) re-asserted at the binary surface.
//  3. Identity propagation end-to-end — the (tenant,user,session) triple
//     flows through the embed run (answer is run-scoped), the Protocol
//     token (verified triple accepted, mismatch/absent rejected), and the
//     MCP dispatch (echoed result is impossible without the triple
//     reaching the MCP `_meta`; cross-identity Tasks.Get is rejected).
//  4. N≥10 concurrency stress — N concurrent RunOnce against ONE shared
//     Stack with distinct triples + per-run sinks: no answer bleed, no
//     cross-run chunk bleed, no goroutine leak after teardown.
//
// Runs under -race: `go test -race -run TestE2E_WaveV18 ./test/integration`.
//
// This file is `package integration` (not the external `integration_test`
// the wave_v17 precedent uses) so it can reuse the in-package mock-OIDC
// issuer (mockoidc_test.go), the `harbor serve` boot harness
// (oidc_serve_roundtrip_test.go), and the scripted-bifrost + MCP harness
// (phase83l_real_bifrost_test.go, mcp_agent_call_test.go) directly.
package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	// Production driver aggregator — the §4.4 blank-import home the recipe
	// tells a headless embedder to add. Pulls in the real state / memory /
	// artifact / events / tasks drivers the embed Stack resolves.
	_ "github.com/hurtener/Harbor/internal/drivers/prod"
	// Dev-only mock LLM (D-089): deliberately OUTSIDE the aggregator, an
	// explicit opt-in so the embed leg reaches FinishGoal deterministically.
	_ "github.com/hurtener/Harbor/internal/llm/mock"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/tasks"
)

const (
	// waveV18Issuer / waveV18Audience are the operator-chosen iss/aud the
	// harbor-token on-ramp mints with and `harbor serve` verifies against.
	waveV18Issuer   = "https://issuer.wavev18.test"
	waveV18Audience = "harbor"
	// waveV18EmbedGoal is echoed back into the mock LLM's answer, so an
	// answer carrying it proves the run was scoped to its own goal.
	waveV18EmbedGoal = "summarise the v1.8 adopter-path deployment status"
	// waveV18MCPSentinel is the token the MCP echo tool returns; it can
	// only appear in a step observation after a real dispatch to the
	// stdio subprocess.
	waveV18MCPSentinel = "HARBOR_WAVE_V18_MCP_DISPATCH_SENTINEL"
)

// TestE2E_WaveV18_AdopterPaths composes the three adopter paths on one
// suite and proves the combined v1.8.0 surface is alive together.
func TestE2E_WaveV18_AdopterPaths(t *testing.T) {
	// ===================================================================
	// EMBED leg (132 / D-265): the production one-call runner turns a goal
	// + identity into a terminal answer envelope, real drivers, mock LLM.
	// ===================================================================
	t.Run("Embed_RunOnce", func(t *testing.T) {
		ctx := context.Background()
		stack := waveV18EmbedStack(t)
		defer func() { _ = stack.Close(ctx) }()

		id := identity.Identity{TenantID: "acme", UserID: "u-embed", SessionID: "s-embed"}
		env, err := stack.RunOnce(ctx, waveV18EmbedGoal, id)
		if err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if env.FinishReason != string(planner.FinishGoal) {
			t.Errorf("FinishReason = %q, want %q", env.FinishReason, planner.FinishGoal)
		}
		if env.Answer == "" {
			t.Fatal("expected a non-empty answer on the golden embed path")
		}
		// Identity propagation: the mock LLM echoes the goal into the
		// answer, so a run-scoped answer carries ITS OWN goal.
		if !strings.Contains(env.Answer, waveV18EmbedGoal) {
			t.Errorf("answer %q does not carry the run's own goal — run not identity/goal-scoped", env.Answer)
		}
	})

	// ===================================================================
	// EMBED streaming leg (132-stream / D-266): a WithStream sink on the
	// SAME blocking RunOnce observes ordered chunks, and EVERY chunk is
	// delivered BEFORE RunOnce returns the envelope (deterministic — the
	// OnChunk seam fires synchronously on the run goroutine).
	// ===================================================================
	t.Run("Embed_WithStream_OrderedChunks", func(t *testing.T) {
		ctx := context.Background()
		stack := waveV18EmbedStack(t)
		defer func() { _ = stack.Close(ctx) }()

		var (
			mu        sync.Mutex
			returned  atomic.Bool
			lateEvent atomic.Bool
			tokens    strings.Builder
			anyEvent  bool
		)
		sink := func(e assemble.StreamEvent) {
			if returned.Load() {
				lateEvent.Store(true)
			}
			mu.Lock()
			anyEvent = true
			if e.Kind == assemble.StreamToken && !e.Reasoning {
				tokens.WriteString(e.Text)
			}
			mu.Unlock()
		}

		id := identity.Identity{TenantID: "acme", UserID: "u-stream", SessionID: "s-stream"}
		env, err := stack.RunOnce(ctx, waveV18EmbedGoal, id, assemble.WithStream(sink))
		returned.Store(true)
		if err != nil {
			t.Fatalf("RunOnce(WithStream): %v", err)
		}
		if lateEvent.Load() {
			t.Fatal("a StreamEvent arrived AFTER RunOnce returned — ordering contract broken (seam not synchronous)")
		}
		if env.FinishReason != string(planner.FinishGoal) {
			t.Fatalf("FinishReason = %q, want %q", env.FinishReason, planner.FinishGoal)
		}
		mu.Lock()
		got := tokens.String()
		sawEvent := anyEvent
		mu.Unlock()
		if !sawEvent {
			t.Fatal("WithStream sink observed no events — streaming did not fire")
		}
		// The assembled content tokens reconstruct this run's answer, so
		// they carry the run's own goal — proving the sink saw THIS run's
		// ordered chunks before the envelope.
		if !strings.Contains(got, waveV18EmbedGoal) {
			t.Errorf("assembled stream tokens %q do not contain the run's goal %q", got, waveV18EmbedGoal)
		}
	})

	// ===================================================================
	// PROTOCOL leg — both on-ramps against a real `harbor serve`.
	// ===================================================================
	t.Run("Protocol_OnRamps", func(t *testing.T) {
		harborBin := waveV18BuildHarbor(t)
		if !subcommandExists(t, harborBin, "serve") {
			t.Skip("harbor serve subcommand absent — pre-serve build")
		}

		// --- (a) the BINDING IdP on-ramp: mock-OIDC → serve → runtime.info.
		t.Run("OnRamp_OIDC_Binding", func(t *testing.T) {
			const (
				clientID = "harbor-wave-v18"
				// Documented dummy fixture secret — not a real credential.
				clientSecret = "wave-v18-test-secret"
				tenant       = "tenant-acme"
				user         = "svc-oidc"
				session      = "sess-oidc"
				wantScope    = "console:fleet"
			)
			issuer := startMockOIDC(t, mockOIDCConfig{
				audience:     waveV18Audience,
				clientID:     clientID,
				clientSecret: clientSecret,
				tenant:       tenant,
				user:         user,
				session:      session,
			})
			cfgPath := writeServeConfig(t, serveConfigParams{
				issuer:   issuer.issuer,
				audience: waveV18Audience,
				jwksURL:  issuer.jwksURL(),
			})
			baseURL := bootServe(t, harborBin, cfgPath)

			token := waveV18ClientCredentialsToken(t, issuer.tokenURL(), clientID, clientSecret, wantScope)
			// The IdP→client claim mapping: the granted scope rides in the
			// JWT (runtime.info echoes none).
			if scopes := waveV18DecodeScopes(t, token); !waveV18Contains(scopes, wantScope) {
				t.Fatalf("granted scopes %v do not carry %q — IdP claim mapping broken", scopes, wantScope)
			}
			status, info := waveV18RuntimeInfo(t, baseURL, token)
			if status != http.StatusOK {
				t.Fatalf("runtime.info over the OIDC token: status=%d, want 200", status)
			}
			if info.ProtocolVersion == "" || len(info.Capabilities) == 0 {
				t.Fatalf("runtime.info returned an empty posture: %+v", info)
			}
		})

		// --- (b) the bring-your-own on-ramp: harbor token → jwks_file →
		// serve → runtime.info, plus a mismatched-iss 401.
		t.Run("OnRamp_HarborToken_BringYourOwn", func(t *testing.T) {
			if !subcommandExists(t, harborBin, "token") {
				t.Skip("harbor token subcommand absent — pre-131d build")
			}
			keysDir := t.TempDir()
			jwksPath, privPath := waveV18Keygen(t, harborBin, keysDir)

			cfgPath := waveV18WriteServeConfigJWKSFile(t, waveV18Issuer, waveV18Audience, jwksPath)
			baseURL := bootServe(t, harborBin, cfgPath)

			// A matching-iss/aud token attaches.
			good := waveV18Mint(t, harborBin, privPath, waveV18Issuer, waveV18Audience, []string{"console:fleet"})
			status, info := waveV18RuntimeInfo(t, baseURL, good)
			if status != http.StatusOK {
				t.Fatalf("runtime.info over the harbor-token JWT: status=%d, want 200", status)
			}
			if info.ProtocolVersion == "" {
				t.Fatalf("runtime.info returned an empty posture over the self-issued token: %+v", info)
			}

			// FAILURE MODE: a token minted with the WRONG issuer is rejected
			// 401 — serve hard-rejects an iss mismatch.
			bad := waveV18Mint(t, harborBin, privPath, "https://evil.wavev18.test", waveV18Audience, nil)
			if badStatus, _ := waveV18RuntimeInfo(t, baseURL, bad); badStatus != http.StatusUnauthorized {
				t.Fatalf("mismatched-iss token: status=%d, want 401", badStatus)
			}
		})

		// --- (c) cross-cutting failure modes + the D-220 invariant.
		t.Run("FailureModes_And_D220", func(t *testing.T) {
			issuer := startMockOIDC(t, mockOIDCConfig{
				audience:     waveV18Audience,
				clientID:     "fm-client",
				clientSecret: "fm-secret",
				tenant:       "tenant-fm",
				user:         "u-fm",
				session:      "s-fm",
			})
			cfgPath := writeServeConfig(t, serveConfigParams{
				issuer:   issuer.issuer,
				audience: waveV18Audience,
				jwksURL:  issuer.jwksURL(),
			})
			baseURL := bootServe(t, harborBin, cfgPath)

			// A garbage bearer is rejected 401.
			if status, _ := waveV18RuntimeInfo(t, baseURL, "not-a-real-jwt"); status != http.StatusUnauthorized {
				t.Errorf("garbage token: status=%d, want 401", status)
			}
			// A request with NO identity (no bearer) is rejected 401 —
			// identity is mandatory at the Protocol edge (CLAUDE.md §6/§8).
			if status, _ := waveV18RuntimeInfo(t, baseURL, ""); status != http.StatusUnauthorized {
				t.Errorf("missing-identity request: status=%d, want 401", status)
			}

			// D-220 invariant: `harbor serve` mints nothing — re-asserted at
			// the binary surface (the smoke gates the same string).
			help := waveV18RunHarborCombined(t, harborBin, "serve", "--help")
			if !strings.Contains(help, "mints no token") {
				t.Errorf("`harbor serve --help` no longer advertises \"mints no token\" (D-220 invariant) — got:\n%s", help)
			}
		})
	})

	// ===================================================================
	// MCP TOOL DISPATCH leg (136): an in-process goal drives the planner to
	// invoke an MCP-sourced tool through the executor to a real stdio
	// subprocess; the echoed sentinel proves the dispatch (and that the
	// identity triple reached the MCP `_meta`).
	// ===================================================================
	t.Run("MCP_ToolDispatch", func(t *testing.T) {
		binPath := buildMCPEchoServer(t)
		server := newScriptedLLMServer(t,
			scriptedToolCallResponse("call_mcp_echo", "mcptest_echo",
				`{"message":"`+waveV18MCPSentinel+`"}`),
			scriptedFinishResponse("the echo tool returned the message"),
		)
		cfg := withMCPEchoServer(t, server.URL(), binPath)
		stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
		defer stack.Close()

		if stack.Tasks == nil || stack.RunLoopDriver == nil || stack.Catalog == nil {
			t.Fatal("devstack: Tasks / RunLoopDriver / Catalog nil — wiring broken")
		}
		if _, ok := stack.Catalog.Resolve("mcptest_echo"); !ok {
			t.Fatal("catalog: mcptest_echo not registered — MCP discovery did not reach the catalog")
		}

		devID := identity.Identity{
			TenantID:  devstack.DefaultDevTenant,
			UserID:    devstack.DefaultDevUser,
			SessionID: devstack.DefaultDevSession,
		}
		idCtx, err := identity.With(context.Background(), devID)
		if err != nil {
			t.Fatalf("identity.With: %v", err)
		}
		h, err := stack.Tasks.Spawn(idCtx, tasks.SpawnRequest{
			Identity: identity.Quadruple{Identity: devID},
			Kind:     tasks.KindForeground,
			Query:    "ask the echo tool to return '" + waveV18MCPSentinel + "'",
		})
		if err != nil {
			t.Fatalf("Tasks.Spawn: %v", err)
		}
		if status := waitForTaskTerminal(t, stack, idCtx, h.ID, 20*time.Second); status != tasks.StatusComplete {
			t.Fatalf("task terminal status = %s, want Complete", status)
		}

		traj := stack.RunLoopDriver.TrajectoryByTaskID(h.ID)
		if traj == nil {
			t.Fatal("RunLoopDriver.TrajectoryByTaskID returned nil")
		}
		var dispatched, obsHasSentinel bool
		for i := range traj.Steps {
			step := traj.Steps[i]
			if !jsonContains(t, step.Action, "mcptest_echo") {
				continue
			}
			dispatched = true
			// The sentinel in the OBSERVATION (the executor's result), not
			// the Action's input args, is the dispatch proof.
			obsHasSentinel = jsonContains(t, step.Observation, waveV18MCPSentinel) ||
				jsonContains(t, step.LLMObservation, waveV18MCPSentinel)
			break
		}
		if !dispatched {
			t.Fatalf("no trajectory step dispatched mcptest_echo (steps=%d)", len(traj.Steps))
		}
		if !obsHasSentinel {
			t.Fatal("mcptest_echo step carries no echoed sentinel in its observation — the call did not dispatch through the executor to the real MCP subprocess")
		}

		// Identity isolation: the run is visible to its own identity and
		// rejected for a foreign tenant.
		if _, err := stack.Tasks.Get(idCtx, h.ID); err != nil {
			t.Fatalf("Tasks.Get with the run's own identity: unexpected error %v", err)
		}
		intruderCtx, err := identity.With(context.Background(), identity.Identity{
			TenantID: "intruder", UserID: "intruder", SessionID: "intruder",
		})
		if err != nil {
			t.Fatalf("identity.With(intruder): %v", err)
		}
		if _, err := stack.Tasks.Get(intruderCtx, h.ID); !errors.Is(err, tasks.ErrNotFound) {
			t.Fatalf("Tasks.Get with a foreign identity: err = %v, want tasks.ErrNotFound", err)
		}
	})
}

// TestE2E_WaveV18_ConcurrencyStress drives N≥10 concurrent RunOnce
// invocations against ONE shared embed Stack (CLAUDE.md §17.4), each with
// a distinct identity triple and its own WithStream sink, then proves: no
// answer bleed (each run carries its own goal), no cross-run chunk bleed
// (each sink receives only its own run's chunks), no data race (the -race
// gate), and no goroutine leak after teardown. Not parallel:
// runtime.NumGoroutine is process-global.
func TestE2E_WaveV18_ConcurrencyStress(t *testing.T) {
	baseline := goruntime.NumGoroutine()
	ctx := context.Background()
	stack := waveV18EmbedStack(t)

	const n = 16 // ≥10 (CLAUDE.md §17.4)
	// Delimiter-wrapped per-run goals so no marker is a substring of
	// another ("g#1#" is not within "g#12#") — the no-bleed check needs it.
	goalFor := func(i int) string { return fmt.Sprintf("wave-v18 goal #%d#", i) }

	type result struct {
		idx    int
		env    planner.AnswerEnvelope
		err    error
		chunks string
	}
	results := make([]result, n)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  fmt.Sprintf("t-%d", i),
				UserID:    fmt.Sprintf("u-%d", i),
				SessionID: fmt.Sprintf("s-%d", i),
			}
			// Each run owns its sink — a Stack that leaked a shared sink
			// (a mutable field on the compiled artifact) would mix run j's
			// tokens in here.
			var sb strings.Builder
			sink := func(e assemble.StreamEvent) {
				if e.Kind == assemble.StreamToken && !e.Reasoning {
					sb.WriteString(e.Text)
				}
			}
			env, err := stack.RunOnce(ctx, goalFor(i), id,
				assemble.WithRunID(fmt.Sprintf("run-%d", i)), assemble.WithStream(sink))
			results[i] = result{idx: i, env: env, err: err, chunks: sb.String()}
		}(i)
	}
	wg.Wait()

	for _, r := range results {
		if r.err != nil {
			t.Errorf("run %d failed: %v", r.idx, r.err)
			continue
		}
		if r.env.FinishReason != string(planner.FinishGoal) {
			t.Errorf("run %d: FinishReason = %q, want %q", r.idx, r.env.FinishReason, planner.FinishGoal)
		}
		own := goalFor(r.idx)
		// No answer bleed: the run's answer carries its OWN goal.
		if !strings.Contains(r.env.Answer, own) {
			t.Errorf("run %d: answer %q lacks its own goal %q — context bleed", r.idx, r.env.Answer, own)
		}
		// No cross-run chunk bleed: own sink carries own goal and no other
		// run's goal marker.
		if !strings.Contains(r.chunks, own) {
			t.Errorf("run %d: own sink chunks %q lack its goal %q — chunks lost/misrouted", r.idx, r.chunks, own)
		}
		for j := range n {
			if j != r.idx && strings.Contains(r.chunks, goalFor(j)) {
				t.Errorf("run %d: sink chunks contain run %d's goal — CROSS-RUN CHUNK BLEED", r.idx, j)
			}
		}
	}

	if err := stack.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Goroutine baseline restored after teardown — the embed Stack's
	// long-lived goroutines are cancellable + joined on Close.
	const slack = 4
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		goruntime.GC()
		if goruntime.NumGoroutine() <= baseline+slack {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if delta := goruntime.NumGoroutine() - baseline; delta > slack {
		t.Errorf("goroutine count rose by %d after teardown (baseline=%d, after=%d) — join-on-shutdown contract violated",
			delta, baseline, goruntime.NumGoroutine())
	}
}

// ---------------------------------------------------------------------------
// Embed-leg helpers
// ---------------------------------------------------------------------------

// waveV18EmbedStack assembles a runnable production Stack backed by the
// dev-only mock LLM (D-089) so RunOnce reaches FinishGoal deterministically
// with zero external dependencies. Mirrors the assemble package's runnable
// fixture (mock LLM + a model profile) with real production drivers on
// every other seam.
func waveV18EmbedStack(t *testing.T) *assemble.Stack {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{BindAddr: "127.0.0.1:0", ShutdownGracePeriod: time.Second},
		Identity: config.IdentityConfig{
			JWTAlgorithms: []string{"ES256"},
			Issuer:        "https://issuer.example.com",
			Audience:      "harbor",
			JWKSURL:       "https://issuer.example.com/.well-known/jwks.json",
		},
		Telemetry: config.TelemetryConfig{LogFormat: "text", LogLevel: "error", ServiceName: "harbor-wave-v18-embed"},
		State:     config.StateConfig{Driver: "inmem"},
		LLM: config.LLMConfig{
			Driver:               "mock",
			Model:                "mock/echo",
			Timeout:              5 * time.Second,
			ContextWindowReserve: 0.05,
			ModelProfiles: map[string]config.LLMModelProfileConfig{
				"mock/echo": {ContextWindowTokens: 100000, TokenEstimator: "chars_div_4"},
			},
		},
		Governance: config.GovernanceConfig{RepairAttempts: 1},
		Events: config.EventsConfig{
			Driver:                   "inmem",
			MaxSubscribersPerSession: 16,
			SubscriberBufferSize:     128,
			IdleTimeout:              2 * time.Second,
			DropWindow:               50 * time.Millisecond,
			ReplayBufferSize:         512,
		},
		Sessions:    config.SessionsConfig{IdleTTL: time.Hour, HardCap: 24 * time.Hour, SweepInterval: 5 * time.Minute},
		Artifacts:   config.ArtifactsConfig{Driver: "inmem", HeavyOutputThresholdBytes: 32 * 1024},
		Tasks:       config.TasksConfig{Driver: "inprocess", RetainTurnTimeout: time.Minute, ContinuationHopLimit: 4},
		Distributed: config.DistributedConfig{BusDriver: "loopback", RemoteDriver: "loopback"},
		Memory:      config.MemoryConfig{Driver: "inmem", Strategy: "none", RecoveryBacklogMax: 8},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("embed cfg.Validate(): %v", err)
	}
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{})
	if err != nil {
		t.Fatalf("assemble.Assemble: %v", err)
	}
	if stack.RunLoop == nil || stack.Planner == nil {
		t.Fatalf("embed stack must have RunLoop + Planner (RunLoop=%v Planner=%v)", stack.RunLoop, stack.Planner)
	}
	return stack
}

// ---------------------------------------------------------------------------
// Protocol-leg helpers
// ---------------------------------------------------------------------------

// waveV18BuildHarbor compiles `bin/harbor` once (reusing the shared OIDC
// round-trip build helper, which also builds the worked example) and
// returns the harbor binary path.
func waveV18BuildHarbor(t *testing.T) string {
	t.Helper()
	harborBin, _ := buildOIDCBinaries(t)
	return harborBin
}

// waveV18RuntimeInfo POSTs runtime.info to a booted `harbor serve` with an
// empty identity body (the identity comes from the verified JWT, mirroring
// the worked OIDC client) and the bearer token. Returns the status and, on
// 200, the decoded RuntimeInfo.
func waveV18RuntimeInfo(t *testing.T, baseURL, token string) (int, prototypes.RuntimeInfo) {
	t.Helper()
	url := baseURL + "/v1/control/" + string(methods.MethodRuntimeInfo)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"identity":{}}`))
	if err != nil {
		t.Fatalf("new runtime.info request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST runtime.info: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var info prototypes.RuntimeInfo
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
			t.Fatalf("decode RuntimeInfo: %v", err)
		}
	}
	return resp.StatusCode, info
}

// waveV18ClientCredentialsToken runs the OAuth2 client-credentials grant
// against the mock issuer's token endpoint and returns the access token.
func waveV18ClientCredentialsToken(t *testing.T, tokenURL, clientID, clientSecret, scope string) string {
	t.Helper()
	form := "grant_type=client_credentials"
	if scope != "" {
		form += "&scope=" + scope
	}
	req, err := http.NewRequest(http.MethodPost, tokenURL, strings.NewReader(form))
	if err != nil {
		t.Fatalf("build token request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(clientID, clientSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("token endpoint: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token endpoint status=%d, want 200", resp.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	if tok.AccessToken == "" {
		t.Fatal("token endpoint returned no access_token")
	}
	return tok.AccessToken
}

// waveV18DecodeScopes pulls the `scopes` claim from a JWT payload without
// verifying the signature — a client-side read of the IdP claim mapping
// (the runtime is the authority that verifies the token).
func waveV18DecodeScopes(t *testing.T, token string) []string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token is not a three-segment JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode JWT payload: %v", err)
	}
	var claims struct {
		Scopes []string `json:"scopes"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("unmarshal JWT claims: %v", err)
	}
	return claims.Scopes
}

// waveV18Contains reports whether xs contains x.
func waveV18Contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// waveV18Keygen runs `harbor token keygen --out <dir> --json` and returns
// the emitted jwks.json + private.pem paths, parsed from the JSON output.
func waveV18Keygen(t *testing.T, harborBin, outDir string) (jwksPath, privPath string) {
	t.Helper()
	stdout := waveV18RunHarborStdout(t, harborBin, "token", "keygen", "--out", outDir, "--json")
	var res struct {
		PrivateKey string `json:"private_key"`
		JWKS       string `json:"jwks"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &res); err != nil {
		t.Fatalf("parse keygen --json %q: %v", stdout, err)
	}
	if res.JWKS == "" || res.PrivateKey == "" {
		t.Fatalf("keygen --json missing paths: %q", stdout)
	}
	return res.JWKS, res.PrivateKey
}

// waveV18Mint runs `harbor token mint --json` for an identity triple with
// the given iss/aud/scopes and returns the signed token.
func waveV18Mint(t *testing.T, harborBin, privPath, issuer, audience string, scopes []string) string {
	t.Helper()
	args := []string{
		"token", "mint", "--json", "--key", privPath,
		"--tenant", "acme", "--user", "alice", "--session", "sess-1",
		"--issuer", issuer, "--audience", audience,
	}
	if len(scopes) > 0 {
		args = append(args, "--scopes", strings.Join(scopes, ","))
	}
	stdout := waveV18RunHarborStdout(t, harborBin, args...)
	var res struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &res); err != nil {
		t.Fatalf("parse mint --json %q: %v", stdout, err)
	}
	if res.Token == "" {
		t.Fatalf("mint --json returned no token: %q", stdout)
	}
	return res.Token
}

// waveV18WriteServeConfigJWKSFile writes a production `harbor serve` config
// pointed at a local jwks.json (the bring-your-own on-ramp), mirroring the
// OIDC round-trip's config but swapping jwks_url for jwks_file. The LLM
// provider carries a literal dummy key so the driver constructs at boot
// without a real provider — runtime.info never invokes the LLM.
func waveV18WriteServeConfigJWKSFile(t *testing.T, issuer, audience, jwksFile string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := fmt.Sprintf(`server:
  bind_addr: 127.0.0.1:0
  shutdown_grace_period: 5s
identity:
  jwt_algorithms: [ES256, RS256]
  issuer: %q
  audience: %q
  jwks_file: %q
telemetry: {log_format: text, log_level: error, service_name: harbor-wave-v18-byo}
state: {driver: inmem}
llm:
  driver: bifrost
  provider: openrouter
  model: anthropic/claude-sonnet-4
  api_key: dummy-literal-key-not-used-by-runtime-info
  timeout: 60s
  context_window_reserve: 0.05
  model_profiles:
    anthropic/claude-sonnet-4:
      context_window_tokens: 200000
      token_estimator: chars_div_4
      json_schema_mode: native
      reasoning_effort: medium
governance: {repair_attempts: 1}
events:
  driver: inmem
  max_subscribers_per_session: 16
  subscriber_buffer_size: 256
  idle_timeout: 60s
  drop_window: 1s
  replay_buffer_size: 1024
sessions: {idle_ttl: 24h, hard_cap: 720h, sweep_interval: 15m}
artifacts: {driver: inmem, heavy_output_threshold_bytes: 32768}
tasks: {driver: inprocess, retain_turn_timeout: 5m, continuation_hop_limit: 8}
distributed: {bus_driver: loopback, remote_driver: loopback}
memory: {driver: inmem, strategy: none}
planner: {driver: react}
tools: {http_manifests: [], mcp_servers: []}
`, issuer, audience, jwksFile)
	path := dir + "/serve.yaml"
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write byo serve config: %v", err)
	}
	return path
}

// waveV18RunHarborStdout runs the harbor binary and returns its stdout,
// failing loud on a non-zero exit (stderr surfaced for diagnosis).
func waveV18RunHarborStdout(t *testing.T, harborBin string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, harborBin, args...)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("harbor %s: %v\nstderr:\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String()
}

// waveV18RunHarborCombined runs the harbor binary and returns its combined
// stdout+stderr (used for --help, which exits zero).
func waveV18RunHarborCombined(t *testing.T, harborBin string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, harborBin, args...)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, _ := cmd.CombinedOutput()
	return string(out)
}
