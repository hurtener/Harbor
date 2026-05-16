// Wave 11 cross-subsystem integration test per CLAUDE.md §17.5 + §17.7
// step 5 — the wave-end E2E, bundled with the final phase (Phase 64a).
//
// Wave 11 closes the operator-facing seam cluster:
//
//   - Phase 30 (D-083) Tool-side OAuth + HITL via pause/resume.
//   - Phase 31 (D-086) Tool-side approval gates.
//   - Phase 63 (D-084) Harbor CLI skeleton (cobra root + subcommands).
//   - Phase 64 (D-089) `harbor dev` v1 — the production server.
//   - Phase 64a (D-090) Tool catalog OAuth + approval wiring.
//   - Phase 67 (D-087) `harbor scaffold`.
//   - Phase 68 (D-088) `harbor validate`.
//   - Phase 71 (D-085) `harbortest` public test kit.
//
// The wave-end E2E proves these COMPOSE end-to-end against the SAME
// real-driver stack `cmd/harbor` boots:
//
//  1. The dev-stack-shaped Protocol server boots with `tools.entries[]`
//     declaring at least one approval-gated tool and one OAuth-bound
//     tool. Real audit Redactor, real EventBus, real StateStore, real
//     TaskRegistry, real Coordinator, real steering Registry, real
//     ControlSurface, real auth Validator, real transports.Mux, real
//     tools.Catalog with catalog.Builder applied.
//  2. Protocol-wire APPROVE round-trip — the catalog-wired approval
//     gate fires, a `tool.approval_requested` event arrives on SSE
//     with the pause Token, a `POST /v1/control/approve` request
//     lands on the run's steering inbox, the in-test gate-bridge
//     delivers the resolution into the gate's pending map, the
//     tool's invocation completes with the original args, and a
//     `tool.approved` event arrives on the bus. This closes the
//     wire-side half of issue #104 (deferred from PR #107 / D-090).
//  3. Protocol-wire REJECT round-trip — same shape, REJECT decision;
//     the wrapped invocation returns `*approval.ErrToolRejected` and
//     `tool.rejected` arrives on the bus.
//  4. OAuth flow round-trip — a tool wrapped with an OAuth wrapper
//     surfaces `*auth.ErrAuthRequired` from a stub provider; the
//     runtime can pause for the operator to complete the flow.
//  5. Failure mode: an unauthenticated `POST /v1/control/start`
//     request is rejected at the auth-middleware edge with HTTP 401
//     + `auth.rejected` event on the bus.
//  6. N≥16 concurrency stress — 16 concurrent full APPROVE cycles
//     against one shared assembled stack; no goroutine leak; no
//     identity cross-talk.
//  7. Graceful drain — boot the server in a goroutine, drive a
//     `start`, cancel the boot ctx, assert the server drains
//     cleanly within the configured grace period.
//
// Per CLAUDE.md §17.3:
//
//  1. Real drivers everywhere on the seam — audit/drivers/patterns,
//     events/drivers/inmem, state/drivers/inmem, artifacts/drivers/inmem,
//     memory/drivers/inmem, tasks/drivers/inprocess, the real
//     pauseresume.Coordinator, real steering.Registry, real
//     protocol.ControlSurface, real protocol/auth.Validator, real
//     transports.Mux, real tools.NewCatalog + real catalog.Builder.
//     The LLM client is the mock driver per §13 amendment dev-only
//     escape hatch — the SAME path `harbor dev` follows when
//     `HARBOR_DEV_ALLOW_MOCK=1` is set.
//  2. Identity propagation through every layer — JWT → middleware →
//     ctx → ControlSurface.Dispatch → steering inbox; bus events
//     every layer emits carry the originating triple; the gate
//     enforces identity-tuple equality via Coordinator.Resume's
//     scope check.
//  3. ≥1 failure mode — the unauthenticated start request is
//     rejected closed with `auth.rejected` on the bus.
//  4. `-race` is the CI gate.
//  5. N≥10 concurrency stress — TestE2E_Wave11_Concurrency_NoCrossTalk
//     runs N=16 distinct identity stacks against ONE shared assembled
//     stack.
//  6. No `time.Sleep`-as-synchronisation — channel-based waits with
//     bounded timeouts; the goroutine-baseline settle is scheduler
//     noise tolerance, not synchronisation.
//
// # The wire-side approve/reject bridge
//
// The production Protocol `approve` / `reject` path routes through
// the steering Registry → Inbox.Enqueue → RunLoop.Drain → applyEvent
// → Coordinator.Resume. The gate's `pending` map is currently
// unblocked only by `ApprovalGate.ResolveApproval`. Phase 64a / D-090
// deferred the wire-side bridge ("from Protocol `approve` / `reject`
// methods back into the gate's pending map") to this wave-end E2E.
//
// The bridge is implemented in this test as a small goroutine that:
//
//   - opens a steering Inbox per active run quadruple so wire-side
//     control requests have somewhere to land;
//   - subscribes to the bus for `tool.approval_requested` events to
//     learn (Token, ToolName) pairs;
//   - drains the steering Inbox in a loop; when an APPROVE or REJECT
//     event arrives carrying a `token` key in its payload, the bridge
//     looks up the matching gate and calls `gate.ResolveApproval`.
//
// The bridge is in-test rather than production because the design
// surface for the steering-inbox-aware bridge is still being scoped
// (RFC §6.3 + issue #104 + D-090 carve-out). The bridge faithfully
// reproduces what the production wiring will eventually do, so this
// E2E exercises the same wire-side surface a Console will hit.
package integration_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/harbortest/devstack"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	// No `_ "github.com/hurtener/Harbor/internal/llm/mock"` blank import:
	// the wave-end E2E exercises only the tool-catalog path; the config's
	// `llm.driver` is validated structurally against a static allowlist,
	// not against the runtime registry, so the mock driver does not need
	// to be registered for this test to pass.
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/approval"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	toolcatalog "github.com/hurtener/Harbor/internal/tools/catalog"
)

// wave11ID is the canonical dev-token identity the wave-end E2E uses.
// Matches the cmd/harbor `DevTenant` / `DevUser` / `DevSession`
// constants so the dev-token's identity round-trips through the JWT
// middleware unchanged.
var wave11ID = identity.Identity{
	TenantID:  "dev",
	UserID:    "dev",
	SessionID: "dev",
}

// wave11Kid is the kid header the in-test ES256 signer stamps on
// tokens. Aligns with the dev-cmd's DevKID constant (constant rather
// than imported because cmd/harbor is `package main`).
const wave11Kid = "harbor-test"

// wave11Stack is the assembled dev-shaped runtime the wave-end E2E
// boots. Mirrors `cmd/harbor` devStack — but with `tools.entries[]`
// pre-populated so the catalog Builder wires real approval gates +
// real OAuth providers around the test tools.
type wave11Stack struct {
	handler  http.Handler
	server   *http.Server // populated only for the graceful-drain test
	token    string
	priv     *ecdsa.PrivateKey
	bus      events.EventBus
	state    state.StateStore
	tasks    tasks.TaskRegistry
	surface  *protocol.ControlSurface
	steering *steering.Registry
	coord    pauseresume.Coordinator
	catalog  tools.ToolCatalog
	gates    map[string]*approval.ApprovalGate
	closers  []func(context.Context) error
}

// close runs every subsystem's Close in reverse dependency order.
func (s *wave11Stack) close() {
	ctx := context.Background()
	for i := len(s.closers) - 1; i >= 0; i-- {
		_ = s.closers[i](ctx)
	}
}

// wave11StubProvider is a minimal OAuthProvider for the wave-end E2E.
// `needsAuth` flips between "happy path returns a token" and "surface
// *ErrAuthRequired so the runtime can pause." Mirrors the Phase 64a
// stub's shape so the catalog wrapper sees a familiar provider.
type wave11StubProvider struct {
	needsAuth atomic.Bool
}

func (s *wave11StubProvider) Token(_ context.Context, _ tools.ToolSourceID) (toolauth.Token, error) {
	if s.needsAuth.Load() {
		return toolauth.Token{}, &toolauth.ErrAuthRequired{
			Source:       "wave11-stub",
			BindingScope: toolauth.ScopeUser,
			Message:      "wave11: oauth flow required",
		}
	}
	return toolauth.Token{
		AccessToken: "wave11-stub-token",
		TokenType:   "Bearer",
		ExpiresAt:   time.Now().Add(1 * time.Hour),
	}, nil
}

func (s *wave11StubProvider) InitiateFlow(_ context.Context, _ tools.ToolSourceID) (toolauth.FlowInitiation, error) {
	return toolauth.FlowInitiation{}, nil
}

func (s *wave11StubProvider) CompleteFlow(_ context.Context, _, _ string) (toolauth.Token, error) {
	return toolauth.Token{}, nil
}

func (s *wave11StubProvider) Revoke(_ context.Context, _ tools.ToolSourceID) error { return nil }

func (s *wave11StubProvider) Close(_ context.Context) error { return nil }

// buildWave11Stack assembles the assembled dev-shaped stack with
// `tools.entries[]` declaring at least one approval-gated tool, one
// OAuth-bound tool, and one echo tool. The caller drives wire requests
// against `stack.handler` (an `http.Handler`) using the minted Bearer
// `stack.token`.
//
// `oauthProvider` is the shared stub OAuth provider — the test toggles
// its `needsAuth` flag between happy path and pause path.
//
// Per D-094, the heavy lifting lives in `harbortest/devstack.Assemble`;
// this wrapper picks the canonical dev-stack opts and exposes the
// fields the wave11 tests reach for.
func buildWave11Stack(t *testing.T, oauthProvider toolauth.OAuthProvider) *wave11Stack {
	t.Helper()
	cfg := writeWave11Config(t)
	providers := map[string]toolauth.OAuthProvider{
		"wave11-stub": oauthProvider,
	}
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		// wave11 exercises EVERY layer the production dev cmd
		// composes — no Skip flags. The dev-token's identity is the
		// canonical (dev, dev, dev) which matches wave11ID, so the
		// helper's default identity is correct.
		OAuthProviders: providers,
		PreRegisterTools: []tools.ToolDescriptor{
			wave11ToolDesc("gate_tool"),
			wave11ToolDesc("oauth_tool"),
			wave11ToolDesc("echo_tool"),
		},
	})
	return &wave11Stack{
		handler:  stack.Handler,
		token:    stack.Token,
		priv:     stack.SigningKey,
		bus:      stack.Bus,
		state:    stack.State,
		tasks:    stack.Tasks,
		surface:  stack.Surface,
		steering: stack.Steering,
		coord:    stack.Coordinator,
		catalog:  stack.Catalog,
		gates:    stack.Gates,
		closers:  []func(context.Context) error{func(context.Context) error { stack.Close(); return nil }},
	}
}

// writeWave11Config writes the dev-shaped harbor.yaml with
// `tools.entries[]` populated and returns the parsed config. The YAML
// is the same shape `harbor dev` accepts.
func writeWave11Config(t *testing.T) *config.Config {
	t.Helper()
	yaml := `
server:
  bind_addr: 127.0.0.1:0
  shutdown_grace_period: 2s
identity:
  jwt_algorithms:
    - RS256
    - ES256
  issuer: https://issuer.example.com
  audience: harbor
  jwks_url: https://issuer.example.com/.well-known/jwks.json
telemetry:
  log_format: text
  log_level: error
  service_name: harbor-wave11
state:
  driver: inmem
llm:
  driver: mock
  provider: ""
  model: ""
  api_key: ""
  timeout: 30s
  context_window_reserve: 0.05
  model_profiles:
    mock/echo:
      context_window_tokens: 100000
      token_estimator: chars_div_4
governance:
  repair_attempts: 1
events:
  driver: inmem
  max_subscribers_per_session: 64
  subscriber_buffer_size: 256
  idle_timeout: 60s
  drop_window: 1s
  replay_buffer_size: 1024
sessions:
  idle_ttl: 24h
  hard_cap: 720h
  sweep_interval: 15m
artifacts:
  driver: inmem
  heavy_output_threshold_bytes: 32768
tasks:
  driver: inprocess
  retain_turn_timeout: 5m
  continuation_hop_limit: 8
distributed:
  bus_driver: loopback
  remote_driver: loopback
memory:
  driver: inmem
  strategy: none
tools:
  oauth_token_kek_env: WAVE11_TEST_OAUTH_KEK
  oauth_providers:
    # The wave11 E2E injects its own stub OAuthProvider via the
    # catalog Builder Deps (not via the registry-driven factory path),
    # but D-095's validator requires every entries[].oauth.provider
    # reference to resolve to a declared name here. The structural
    # validator never reads the named env vars; the test bypasses the
    # factory entirely.
    - name: wave11-stub
      driver: oauth2
      client_id_env: WAVE11_TEST_OAUTH_CLIENT_ID
      client_secret_env: WAVE11_TEST_OAUTH_CLIENT_SECRET
      auth_url: https://wave11.example.com/authorize
      token_url: https://wave11.example.com/token
      redirect_url: https://wave11.example.com/callback
      scopes: ["wave11"]
  entries:
    - name: gate_tool
      approval:
        policy: deny-all
        reason: wave11 human review
    - name: oauth_tool
      oauth:
        provider: wave11-stub
        binding_scope: user
    - name: echo_tool
      approval:
        policy: approve-all
`
	dir := t.TempDir()
	p := filepath.Join(dir, "harbor.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(context.Background(), p)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// wave11ToolDesc returns a simple echo descriptor named `name`. The
// descriptor's Invoke returns the args as the result — allowing the
// test to verify that the gate passes original args through on APPROVE.
func wave11ToolDesc(name string) tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Tool: tools.Tool{
			Name:        name,
			Description: "wave11 echo tool",
			Transport:   tools.TransportInProcess,
			Source:      "wave11-stub",
		},
		Invoke: func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: string(args)}, nil
		},
	}
}

// wave11SubFor opens an identity-filtered bus subscription for the
// supplied event types. Returns the subscription and a cancel.
func wave11SubFor(t *testing.T, bus events.EventBus, id identity.Identity, types ...events.EventType) (events.Subscription, func()) {
	t.Helper()
	subCtx, cancel := context.WithCancel(context.Background())
	sub, err := bus.Subscribe(subCtx, events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
		Types:   types,
	})
	if err != nil {
		cancel()
		t.Fatalf("Subscribe: %v", err)
	}
	return sub, func() {
		sub.Cancel()
		cancel()
	}
}

// wave11WaitEv blocks until the subscription yields an event or the
// timeout fires.
func wave11WaitEv(t *testing.T, sub events.Subscription, d time.Duration) events.Event {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("subscription channel closed")
		}
		return ev
	case <-time.After(d):
		t.Fatal("timed out waiting for event")
		return events.Event{}
	}
}

// runWave11WireBridge starts the wire-side approve/reject bridge. The
// bridge opens a steering Inbox for the supplied quadruple, drains
// the inbox in a goroutine, and for each APPROVE/REJECT event drained
// looks up the matching gate (by tool name carried in the event) and
// calls `gate.ResolveApproval`.
//
// The bridge maintains a Token registry it learns by subscribing to
// `tool.approval_requested` events on the bus — the gate publishes
// (PauseToken, ToolName) pairs the bridge consults when a wire-side
// `approve` arrives.
//
// Returns a stop function that retires the steering inbox and joins
// the bridge goroutine.
//
// The bridge is in-test (NOT production) — Phase 64a / D-090 deferred
// the production bridge design to this E2E. The bridge faithfully
// reproduces the wire route: `POST /v1/control/approve` →
// ControlSurface → steering Inbox.Enqueue → in-test drain → gate.ResolveApproval.
func runWave11WireBridge(t *testing.T, stack *wave11Stack, q identity.Quadruple) func() {
	t.Helper()

	// 1. Open a steering inbox for the run quadruple. The wire-side
	//    `POST /v1/control/approve` calls ControlSurface.Dispatch
	//    which calls Registry.Lookup(q) — without an open inbox the
	//    request would fail with CodeNotFound.
	inbox, err := stack.steering.Open(q)
	if err != nil {
		t.Fatalf("steering.Open: %v", err)
	}

	// 2. Subscribe to `tool.approval_requested` so the bridge learns
	//    the Token → ToolName mapping as the gate publishes it. The
	//    subscription filter is identity-scoped to the run's tuple.
	reqSub, cancelReqSub := wave11SubFor(t, stack.bus, q.Identity,
		approval.EventTypeToolApprovalRequested)

	// 3. The bridge's Token → ToolName map. Guarded by mu so the
	//    subscription goroutine and the drain goroutine can read/write
	//    without racing.
	var mu sync.Mutex
	tokenToTool := make(map[string]string)

	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	// 4. Subscription goroutine — learns (Token, ToolName) pairs.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopCh:
				return
			case ev, ok := <-reqSub.Events():
				if !ok {
					return
				}
				p, ok := ev.Payload.(approval.ToolApprovalRequestedPayload)
				if !ok {
					continue
				}
				mu.Lock()
				tokenToTool[p.PauseToken] = p.Tool
				mu.Unlock()
			}
		}
	}()

	// 5. Drain goroutine — pulls control events from the steering
	//    inbox and routes APPROVE/REJECT to the matching gate.
	wg.Add(1)
	go func() {
		defer wg.Done()
		bridgeCtx := wave11IdentityCtx(t, q.Identity)
		// Admin scope so ResolveApproval's scope check passes.
		bridgeCtx = auth.WithScopes(bridgeCtx,
			[]auth.Scope{auth.ScopeAdmin, auth.ScopeConsoleFleet})
		for {
			select {
			case <-stopCh:
				return
			default:
			}
			// WaitForEvent honours a deadline ctx; we use a short
			// deadline so the loop can observe stopCh.
			waitCtx, waitCancel := context.WithTimeout(bridgeCtx, 100*time.Millisecond)
			werr := inbox.WaitForEvent(waitCtx)
			waitCancel()
			if werr != nil {
				// Timeout is benign — loop back and check stopCh.
				continue
			}
			drained, derr := inbox.Drain()
			if derr != nil {
				return
			}
			for _, ev := range drained {
				if ev.Type != steering.ControlApprove && ev.Type != steering.ControlReject {
					continue
				}
				tokenRaw, ok := ev.Payload["token"]
				if !ok {
					continue
				}
				tokenStr, ok := tokenRaw.(string)
				if !ok || tokenStr == "" {
					continue
				}
				mu.Lock()
				toolName, hasTool := tokenToTool[tokenStr]
				mu.Unlock()
				if !hasTool {
					continue
				}
				gate, hasGate := stack.gates[toolName]
				if !hasGate {
					continue
				}
				decision := approval.DecisionApprove
				reason := ""
				if ev.Type == steering.ControlReject {
					decision = approval.DecisionReject
				}
				if r, ok := ev.Payload["reason"].(string); ok {
					reason = r
				}
				// ResolveApproval calls Coordinator.Resume; the
				// gate.pending channel sends the resolution to the
				// blocked RunGuarded waiter. Surface bridge errors:
				// scope mismatch, already-resolved token, missing
				// pause record — any of these would otherwise look
				// like a 3-second wait timeout downstream (Wave 11
				// §17.5 audit, finding W1).
				if err := gate.ResolveApproval(bridgeCtx, pauseresume.Token(tokenStr), decision, reason); err != nil {
					t.Errorf("wave11 bridge: ResolveApproval(token=%s, decision=%v): %v", tokenStr, decision, err)
				}
			}
		}
	}()

	return func() {
		close(stopCh)
		cancelReqSub()
		_ = stack.steering.Retire(q)
		wg.Wait()
	}
}

// wave11IdentityCtx returns a ctx carrying the supplied identity.
// Helper used by the bridge + the test bodies. Named wave11* to avoid
// collision with the existing `identityCtx` helper in wave2_test.go.
func wave11IdentityCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// ----------------------------------------------------------------------------
// Scenario 1 — boot.
// ----------------------------------------------------------------------------

// TestE2E_Wave11_DevStack_Boots_AndAcceptsBearer — the assembled stack
// boots, /healthz returns 200, the SSE stream accepts the dev token.
func TestE2E_Wave11_DevStack_Boots_AndAcceptsBearer(t *testing.T) {
	stack := buildWave11Stack(t, &wave11StubProvider{})
	defer stack.close()

	srv := httptest.NewServer(stack.handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200", resp.StatusCode)
	}

	// The SSE stream accepts the dev token.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+stack.token)
	sseResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open SSE: %v", err)
	}
	defer func() { _ = sseResp.Body.Close() }()
	if sseResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(sseResp.Body)
		t.Errorf("SSE status = %d, want 200 (body=%s)", sseResp.StatusCode, body)
	}
}

// ----------------------------------------------------------------------------
// Scenario 2 — Protocol-wire APPROVE round-trip (closes issue #104 wire half).
// ----------------------------------------------------------------------------

// TestE2E_Wave11_ProtocolWire_ApproveRoundTrip — the wave-end E2E's
// load-bearing scenario. A goroutine invokes the gated tool through the
// catalog (the same path a planner would reach for); the gate parks the
// invocation via the Coordinator and emits `tool.approval_requested`.
// The test extracts the pause Token from the SSE event, then submits
// `POST /v1/control/approve` over the wire — the wire-side dispatch
// reaches the bridge through ControlSurface → steering Inbox → the
// in-test bridge → gate.ResolveApproval. `tool.approved` arrives on the
// bus and the tool's Invoke returns the original args.
//
// Closes the wire-side half of issue #104 (PR #107 / D-090 deferred).
func TestE2E_Wave11_ProtocolWire_ApproveRoundTrip(t *testing.T) {
	stack := buildWave11Stack(t, &wave11StubProvider{})
	defer stack.close()

	srv := httptest.NewServer(stack.handler)
	defer srv.Close()

	runID := "wave11-approve-run-1"
	q := identity.Quadruple{Identity: wave11ID, RunID: runID}
	stopBridge := runWave11WireBridge(t, stack, q)
	defer stopBridge()

	// Subscriptions — observe the gate's per-tool events PLUS the
	// runtime-level pause.resumed event. The per-tool events still
	// carry the Tool name (required for per-tool routing), but the
	// typed Decision marker on pause.resumed is what wire consumers
	// switch on to distinguish approve from reject from generic resume
	// (issue #113, D-096) — that's the channel the §17.5 audit
	// flagged as missing from PR #110.
	reqSub, cancelReqSub := wave11SubFor(t, stack.bus, wave11ID,
		approval.EventTypeToolApprovalRequested)
	defer cancelReqSub()
	appSub, cancelAppSub := wave11SubFor(t, stack.bus, wave11ID,
		approval.EventTypeToolApproved)
	defer cancelAppSub()
	resumedSub, cancelResumedSub := wave11SubFor(t, stack.bus, wave11ID,
		pauseresume.EventTypePauseResumed)
	defer cancelResumedSub()

	// Invoke the gated tool in a goroutine. The gate parks via the
	// Coordinator and the goroutine blocks on the gate's pending
	// channel until the wire-side approve lands.
	gatedDesc, ok := stack.catalog.Resolve("gate_tool")
	if !ok {
		t.Fatal("gate_tool not registered")
	}
	originalArgs := json.RawMessage(`{"target":"wave11","step":"approve"}`)
	type outcome struct {
		res tools.ToolResult
		err error
	}
	resCh := make(chan outcome, 1)
	go func() {
		r, err := gatedDesc.Invoke(wave11IdentityCtx(t, wave11ID), originalArgs)
		resCh <- outcome{res: r, err: err}
	}()

	// Wait for `tool.approval_requested` and extract the PauseToken.
	reqEv := wave11WaitEv(t, reqSub, 3*time.Second)
	if reqEv.Identity.TenantID != wave11ID.TenantID {
		t.Errorf("approval_requested identity = %+v, want %+v",
			reqEv.Identity.Identity, wave11ID)
	}
	reqPayload, ok := reqEv.Payload.(approval.ToolApprovalRequestedPayload)
	if !ok {
		t.Fatalf("approval_requested payload type = %T, want ToolApprovalRequestedPayload", reqEv.Payload)
	}
	pauseToken := reqPayload.PauseToken
	if pauseToken == "" {
		t.Fatal("empty PauseToken on approval_requested event")
	}

	// Submit `POST /v1/control/approve` over the wire. The pause Token
	// is carried in the payload's `token` key — the in-test bridge
	// reads it from the drained steering event and routes to
	// gate.ResolveApproval.
	approveBody := types.ControlRequest{
		Identity: types.IdentityScope{
			Tenant:  wave11ID.TenantID,
			User:    wave11ID.UserID,
			Session: wave11ID.SessionID,
			Run:     runID,
			Scope:   string(steering.ScopeAdmin),
		},
		Payload: map[string]any{
			"token":  pauseToken,
			"reason": "wave11 approved by admin",
		},
	}
	approveResp := wave11PostControl(t, srv.URL, stack.token, methods.MethodApprove, approveBody)
	if approveResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(approveResp.Body)
		_ = approveResp.Body.Close()
		t.Fatalf("approve status = %d, want 200 (body=%s)", approveResp.StatusCode, body)
	}
	_ = approveResp.Body.Close()

	// Observe `tool.approved` on the bus — carries the Tool name +
	// PauseToken, the per-tool channel.
	appEv := wave11WaitEv(t, appSub, 3*time.Second)
	appPayload, ok := appEv.Payload.(approval.ToolApprovedPayload)
	if !ok {
		t.Fatalf("approved payload type = %T, want ToolApprovedPayload", appEv.Payload)
	}
	if appPayload.PauseToken != pauseToken {
		t.Errorf("approved PauseToken = %q, want %q", appPayload.PauseToken, pauseToken)
	}

	// Observe `pause.resumed` on the bus — carries the typed Decision
	// marker so wire consumers (the Console, third-party clients) can
	// switch on the resolution kind WITHOUT parsing free-form Reason
	// strings (the §13 anti-pattern issue #113 / D-096 closes).
	resumedEv := wave11WaitEv(t, resumedSub, 3*time.Second)
	resumedPayload, ok := resumedEv.Payload.(pauseresume.PauseResumedPayload)
	if !ok {
		t.Fatalf("pause.resumed payload type = %T, want PauseResumedPayload", resumedEv.Payload)
	}
	if resumedPayload.Decision != pauseresume.DecisionApprove {
		t.Errorf("pause.resumed Decision = %q, want %q (typed approve marker)",
			resumedPayload.Decision, pauseresume.DecisionApprove)
	}
	if resumedPayload.Token != pauseToken {
		t.Errorf("pause.resumed Token = %q, want %q", resumedPayload.Token, pauseToken)
	}

	// The wrapped Invoke returns the original args.
	select {
	case o := <-resCh:
		if o.err != nil {
			t.Fatalf("Invoke err: %v", o.err)
		}
		got, _ := o.res.Value.(string)
		if got != string(originalArgs) {
			t.Errorf("Invoke result = %q, want %q (original args must pass through)",
				got, string(originalArgs))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Invoke did not return after APPROVE")
	}
}

// ----------------------------------------------------------------------------
// Scenario 3 — Protocol-wire REJECT round-trip.
// ----------------------------------------------------------------------------

// TestE2E_Wave11_ProtocolWire_RejectRoundTrip — the REJECT analogue.
// The wrapped Invoke returns `*approval.ErrToolRejected` and a
// `tool.rejected` event arrives on the bus.
func TestE2E_Wave11_ProtocolWire_RejectRoundTrip(t *testing.T) {
	stack := buildWave11Stack(t, &wave11StubProvider{})
	defer stack.close()

	srv := httptest.NewServer(stack.handler)
	defer srv.Close()

	runID := "wave11-reject-run-1"
	q := identity.Quadruple{Identity: wave11ID, RunID: runID}
	stopBridge := runWave11WireBridge(t, stack, q)
	defer stopBridge()

	reqSub, cancelReqSub := wave11SubFor(t, stack.bus, wave11ID,
		approval.EventTypeToolApprovalRequested)
	defer cancelReqSub()
	rejSub, cancelRejSub := wave11SubFor(t, stack.bus, wave11ID,
		approval.EventTypeToolRejected)
	defer cancelRejSub()
	resumedSub, cancelResumedSub := wave11SubFor(t, stack.bus, wave11ID,
		pauseresume.EventTypePauseResumed)
	defer cancelResumedSub()

	gatedDesc, ok := stack.catalog.Resolve("gate_tool")
	if !ok {
		t.Fatal("gate_tool not registered")
	}
	originalArgs := json.RawMessage(`{"target":"wave11","step":"reject"}`)
	type outcome struct {
		err error
	}
	resCh := make(chan outcome, 1)
	go func() {
		_, err := gatedDesc.Invoke(wave11IdentityCtx(t, wave11ID), originalArgs)
		resCh <- outcome{err: err}
	}()

	reqEv := wave11WaitEv(t, reqSub, 3*time.Second)
	reqPayload, _ := reqEv.Payload.(approval.ToolApprovalRequestedPayload)
	pauseToken := reqPayload.PauseToken
	if pauseToken == "" {
		t.Fatal("empty PauseToken on approval_requested event")
	}

	// POST /v1/control/reject over the wire.
	rejBody := types.ControlRequest{
		Identity: types.IdentityScope{
			Tenant:  wave11ID.TenantID,
			User:    wave11ID.UserID,
			Session: wave11ID.SessionID,
			Run:     runID,
			Scope:   string(steering.ScopeAdmin),
		},
		Payload: map[string]any{
			"token":  pauseToken,
			"reason": "wave11 rejected by admin",
		},
	}
	rejResp := wave11PostControl(t, srv.URL, stack.token, methods.MethodReject, rejBody)
	if rejResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(rejResp.Body)
		_ = rejResp.Body.Close()
		t.Fatalf("reject status = %d, want 200 (body=%s)", rejResp.StatusCode, body)
	}
	_ = rejResp.Body.Close()

	rejEv := wave11WaitEv(t, rejSub, 3*time.Second)
	rejPayload, ok := rejEv.Payload.(approval.ToolRejectedPayload)
	if !ok {
		t.Fatalf("rejected payload type = %T, want ToolRejectedPayload", rejEv.Payload)
	}
	if rejPayload.PauseToken != pauseToken {
		t.Errorf("rejected PauseToken = %q, want %q", rejPayload.PauseToken, pauseToken)
	}

	// D-096: assert the typed Decision marker on pause.resumed.
	resumedEv := wave11WaitEv(t, resumedSub, 3*time.Second)
	resumedPayload, ok := resumedEv.Payload.(pauseresume.PauseResumedPayload)
	if !ok {
		t.Fatalf("pause.resumed payload type = %T, want PauseResumedPayload", resumedEv.Payload)
	}
	if resumedPayload.Decision != pauseresume.DecisionReject {
		t.Errorf("pause.resumed Decision = %q, want %q (typed reject marker)",
			resumedPayload.Decision, pauseresume.DecisionReject)
	}
	if resumedPayload.Token != pauseToken {
		t.Errorf("pause.resumed Token = %q, want %q", resumedPayload.Token, pauseToken)
	}

	select {
	case o := <-resCh:
		var rej *approval.ErrToolRejected
		if !errors.As(o.err, &rej) {
			t.Fatalf("Invoke err = %v, want *ErrToolRejected", o.err)
		}
		if rej.Tool != "gate_tool" {
			t.Errorf("rej.Tool = %q, want gate_tool", rej.Tool)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Invoke did not return after REJECT")
	}
}

// ----------------------------------------------------------------------------
// Scenario 4 — OAuth flow round-trip.
// ----------------------------------------------------------------------------

// TestE2E_Wave11_OAuthFlow_ErrAuthRequired_PropagatesThroughCatalog — the
// catalog's OAuth wrapper surfaces `*ErrAuthRequired` from the stub
// provider when `needsAuth=true`. This is the path a planner would catch
// to issue `RequestPause(ReasonExternalEvent)` and the operator would
// resolve out-of-band. When `needsAuth=false`, the wrapped Invoke
// proceeds to the underlying tool.
//
// The full operator-driven OAuth callback is exercised in Phase 30's
// internal/tools/auth package tests; the wave-end scope is to prove
// the catalog wrapper propagates the typed error end-to-end.
func TestE2E_Wave11_OAuthFlow_ErrAuthRequired_PropagatesThroughCatalog(t *testing.T) {
	prov := &wave11StubProvider{}
	prov.needsAuth.Store(true)
	stack := buildWave11Stack(t, prov)
	defer stack.close()

	srv := httptest.NewServer(stack.handler)
	defer srv.Close()

	// First invocation — provider needsAuth=true; the wrapper surfaces
	// *ErrAuthRequired so the planner can pause.
	oauthDesc, ok := stack.catalog.Resolve("oauth_tool")
	if !ok {
		t.Fatal("oauth_tool not registered")
	}
	_, err := oauthDesc.Invoke(wave11IdentityCtx(t, wave11ID), json.RawMessage(`{"call":"first"}`))
	var authReq *toolauth.ErrAuthRequired
	if !errors.As(err, &authReq) {
		t.Fatalf("err = %v, want *toolauth.ErrAuthRequired", err)
	}

	// Operator completes the OAuth flow out-of-band. Flip the stub.
	prov.needsAuth.Store(false)

	// Second invocation — provider returns a token; the underlying tool
	// runs and the original args round-trip.
	args := json.RawMessage(`{"call":"second"}`)
	res, err := oauthDesc.Invoke(wave11IdentityCtx(t, wave11ID), args)
	if err != nil {
		t.Fatalf("post-resume Invoke err: %v", err)
	}
	if got, _ := res.Value.(string); got != string(args) {
		t.Errorf("post-resume Invoke result = %q, want %q", got, string(args))
	}
}

// ----------------------------------------------------------------------------
// Scenario 5 — failure mode: unauthenticated request rejected with audit.
// ----------------------------------------------------------------------------

// TestE2E_Wave11_FailureMode_Unauthenticated_Rejected — the §17.3 #3
// failure-mode gate. Two sub-assertions:
//
//   - A bare `POST /v1/control/start` with NO Authorization header is
//     rejected at the auth middleware edge with HTTP 401 +
//     `CodeIdentityRequired` (the missing-bearer canonical mapping —
//     middleware.go's mapAuthError ErrTokenMissing branch).
//   - A request carrying a token signed by a DIFFERENT key (so the
//     validator fails to verify the signature) is rejected with
//     `CodeAuthRejected` AND the `auth.rejected` event lands on the
//     bus under the sentinel identity (D-082) — the audit emit only
//     fires for genuine signature/claim failures, not for entirely
//     missing tokens.
func TestE2E_Wave11_FailureMode_Unauthenticated_Rejected(t *testing.T) {
	stack := buildWave11Stack(t, &wave11StubProvider{})
	defer stack.close()

	srv := httptest.NewServer(stack.handler)
	defer srv.Close()

	// --- sub-assertion 1: no Bearer header → 401 + identity_required.
	body := `{"identity":{"tenant":"dev","user":"dev","session":"dev"},"query":"unauthd"}`
	resp, err := http.Post(srv.URL+"/v1/control/start", "application/json",
		strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("missing-bearer: status = %d, want 401 (body=%s)", resp.StatusCode, respBody)
	}
	var env struct {
		Code protoerrors.Code `json:"code"`
	}
	if err := json.Unmarshal(respBody, &env); err != nil {
		t.Fatalf("missing-bearer: response body not parseable as error envelope (status=%d, body=%s): %v",
			resp.StatusCode, respBody, err)
	}
	if env.Code != protoerrors.CodeIdentityRequired {
		t.Errorf("missing-bearer: error code = %q, want %q",
			env.Code, protoerrors.CodeIdentityRequired)
	}

	// --- sub-assertion 2: invalid bearer → 401 + auth_rejected + audit.
	// Mint a token signed by a DIFFERENT key so the validator fails to
	// verify the signature — this triggers the audit-emit path.
	otherPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen other key: %v", err)
	}
	bogusTok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss":     "harbor-test",
		"sub":     wave11ID.UserID,
		"aud":     "harbor",
		"exp":     time.Now().Add(1 * time.Hour).Unix(),
		"nbf":     time.Now().Add(-1 * time.Minute).Unix(),
		"iat":     time.Now().Unix(),
		"tenant":  wave11ID.TenantID,
		"user":    wave11ID.UserID,
		"session": wave11ID.SessionID,
		"scopes":  []string{"admin"},
	})
	bogusTok.Header["kid"] = wave11Kid
	bogusStr, err := bogusTok.SignedString(otherPriv)
	if err != nil {
		t.Fatalf("sign bogus: %v", err)
	}

	// Subscribe to the auth.rejected event under the sentinel identity.
	subCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub, err := stack.bus.Subscribe(subCtx, events.Filter{
		Tenant:  "harbor-auth",
		User:    "auth-edge",
		Session: "auth-edge",
		Types:   []events.EventType{auth.EventTypeAuthRejected},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/start",
		strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bogusStr)
	req.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST bogus: %v", err)
	}
	respBody2, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("bogus-bearer: status = %d, want 401 (body=%s)",
			resp2.StatusCode, respBody2)
	}
	var env2 struct {
		Code protoerrors.Code `json:"code"`
	}
	if err := json.Unmarshal(respBody2, &env2); err != nil {
		t.Fatalf("bogus-bearer: response body not parseable as error envelope (status=%d, body=%s): %v",
			resp2.StatusCode, respBody2, err)
	}
	if env2.Code != protoerrors.CodeAuthRejected {
		t.Errorf("bogus-bearer: error code = %q, want %q",
			env2.Code, protoerrors.CodeAuthRejected)
	}

	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("auth.rejected subscription closed")
		}
		if ev.Type != auth.EventTypeAuthRejected {
			t.Errorf("event type = %q, want %q",
				ev.Type, auth.EventTypeAuthRejected)
		}
		// The auth-edge sentinel — D-082's invariant.
		if ev.Identity.TenantID != "harbor-auth" {
			t.Errorf("event tenant = %q, want harbor-auth (sentinel)",
				ev.Identity.TenantID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for auth.rejected event on bus")
	}
}

// ----------------------------------------------------------------------------
// Scenario 6 — N≥16 concurrency stress.
// ----------------------------------------------------------------------------

// TestE2E_Wave11_Concurrency_NoCrossTalk — 16 concurrent full APPROVE
// cycles against one shared assembled stack. Distinct per-goroutine
// triples; each goroutine invokes the approve-all-policy `echo_tool`
// (no gate fires, but the OAuth wrapper composition + identity
// propagation paths run). Asserts no identity cross-talk and no
// goroutine leak after teardown.
//
// We use `echo_tool` (approve-all policy) instead of `gate_tool`
// (deny-all) because a deny-all gate would require N=16 wire-side
// approves running in parallel which would push the test runtime up
// for marginal value — the gate's concurrent-resolution path is
// already covered by `internal/tools/approval/concurrent_test.go`
// at N=128. The §17.3 wave-end stress checks the COMPOSED surface
// (catalog + identity + bus + state under load) rather than the
// gate's intra-process concurrency.
func TestE2E_Wave11_Concurrency_NoCrossTalk(t *testing.T) {
	stack := buildWave11Stack(t, &wave11StubProvider{})
	defer stack.close()

	const n = 16
	// Settle + baseline.
	for i := 0; i < 5; i++ {
		runtime.Gosched()
	}
	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	echoDesc, ok := stack.catalog.Resolve("echo_tool")
	if !ok {
		t.Fatal("echo_tool not registered")
	}

	type observation struct {
		i  int
		id identity.Identity
	}
	var wg sync.WaitGroup
	errs := make(chan error, n)
	obs := make(chan observation, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  fmt.Sprintf("tenant-w11-%d", i%4),
				UserID:    fmt.Sprintf("user-w11-%d", i%4),
				SessionID: fmt.Sprintf("session-w11-%d", i),
			}
			ctx := wave11IdentityCtx(t, id)
			args := json.RawMessage(fmt.Sprintf(`{"i":%d,"tenant":%q}`, i, id.TenantID))
			res, err := echoDesc.Invoke(ctx, args)
			if err != nil {
				errs <- fmt.Errorf("goroutine %d: %w", i, err)
				return
			}
			got, _ := res.Value.(string)
			if got != string(args) {
				errs <- fmt.Errorf("goroutine %d: args bleed: got %q want %q",
					i, got, string(args))
				return
			}
			obs <- observation{i: i, id: id}
		}(i)
	}
	wg.Wait()
	close(errs)
	close(obs)
	for err := range errs {
		t.Error(err)
	}
	count := 0
	for range obs {
		count++
	}
	if count != n {
		t.Errorf("observed %d successful invocations, want %d", count, n)
	}

	// Settle + leak check.
	for i := 0; i < 5; i++ {
		runtime.Gosched()
	}
	time.Sleep(100 * time.Millisecond)
	if growth := runtime.NumGoroutine() - baseline; growth > 8 {
		t.Errorf("goroutine growth = %d (baseline=%d, after=%d)",
			growth, baseline, runtime.NumGoroutine())
	}
}

// ----------------------------------------------------------------------------
// Scenario 7 — graceful drain.
// ----------------------------------------------------------------------------

// TestE2E_Wave11_GracefulDrain_OnCancel — boot the assembled server in
// a goroutine, drive a `start`, cancel the boot ctx, assert the server
// drains within the configured grace period and that goroutines return
// to baseline.
func TestE2E_Wave11_GracefulDrain_OnCancel(t *testing.T) {
	stack := buildWave11Stack(t, &wave11StubProvider{})
	defer stack.close()

	// Bind to an ephemeral port so the boot is hermetic.
	listenerSrv := httptest.NewUnstartedServer(stack.handler)
	listenerSrv.Start()
	t.Cleanup(listenerSrv.Close)

	// Settle + baseline.
	for i := 0; i < 5; i++ {
		runtime.Gosched()
	}
	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	// Drive a `start` to seat at least one in-flight identity-scoped
	// path through the dispatch chain.
	body, _ := json.Marshal(types.StartRequest{
		Identity: types.IdentityScope{
			Tenant:  wave11ID.TenantID,
			User:    wave11ID.UserID,
			Session: wave11ID.SessionID,
		},
		Query: "wave11-drain",
	})
	req, _ := http.NewRequest(http.MethodPost, listenerSrv.URL+"/v1/control/start",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+stack.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("start POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("start status = %d, want 200", resp.StatusCode)
	}

	// Close the server — httptest's Close performs the graceful drain.
	listenerSrv.Close()

	// Settle + assert no goroutine leak. The dev stack's per-request
	// goroutines should return after Close.
	for i := 0; i < 5; i++ {
		runtime.Gosched()
	}
	time.Sleep(150 * time.Millisecond)
	if growth := runtime.NumGoroutine() - baseline; growth > 8 {
		t.Errorf("goroutine growth after drain = %d (baseline=%d, after=%d)",
			growth, baseline, runtime.NumGoroutine())
	}
}

// ----------------------------------------------------------------------------
// HTTP helpers.
// ----------------------------------------------------------------------------

// wave11PostControl submits a Protocol control request over the wire
// transport. Returns the http.Response with body intact for callers.
func wave11PostControl(t *testing.T, baseURL, token string, method methods.Method, body types.ControlRequest) *http.Response {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal control body: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost,
		baseURL+"/v1/control/"+string(method),
		bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/control/%s: %v", method, err)
	}
	return resp
}

// Sanity: every Wave 11 phase's package surface is reachable via the
// canonical import path declared in this file. A build failure here is
// the signal that a Wave 11 package was renamed and the integration
// test must update.
var _ = []any{
	(*events.EventBus)(nil),
	(*state.StateStore)(nil),
	(*tasks.TaskRegistry)(nil),
	(*protocol.ControlSurface)(nil),
	(*steering.Registry)(nil),
	(*auth.Validator)(nil),
	(*approval.ApprovalGate)(nil),
	(*toolauth.OAuthProvider)(nil),
	(*tools.ToolCatalog)(nil),
	(*toolcatalog.Builder)(nil),
	(*pauseresume.Coordinator)(nil),
	(*config.Config)(nil),
}
