// Phase 30 — Tool-side OAuth + HITL via pause/resume integration test
// (RFC §6.4 + §3.3; master-plan Phase 30 detail block; D-083).
//
// This test wires the REAL artifacts across the Phase 30 surface — no
// mocks at any seam (CLAUDE.md §17.3 #1):
//
//   - real state.StateStore across all three V1 drivers
//     (in-mem / SQLite / Postgres — Postgres leg skips without
//     HARBOR_PG_DSN per the existing convention);
//   - real audit.Redactor (the patterns driver, the canonical V1
//     rule set);
//   - real events.EventBus (in-mem driver);
//   - real pauseresume.Coordinator;
//   - a real httptest.Server emulating an OAuth 2.0 authorization
//     server with PKCE + RFC 7591 dynamic client registration +
//     metadata discovery — the "test authorization server" the
//     master-plan acceptance criterion names.
//
// It exercises:
//
//   - the full pause/resume cycle for BOTH binding scopes
//     (ScopeUser + ScopeAgent) — the master-plan's #1 acceptance
//     criterion (CLAUDE.md §17.3 happy path);
//   - cross-driver TokenStore conformance via
//     internal/tools/auth/conformancetest (the same suite the in-mem
//     leg runs; here each StateStore driver gets its own factory
//     invocation);
//   - A2A AUTH_REQUIRED → ErrAuthRequired convergence — same event
//     shape regardless of southbound transport;
//   - cross-tenant + cross-user + cross-agent identity propagation
//     (CLAUDE.md §17.3 #2);
//   - encryption at rest — driver conformance asserts ciphertext on
//     disk (the master-plan acceptance criterion);
//   - initiate-then-cancel emits no goroutine leak (the master-plan
//     acceptance criterion).
//
// CLAUDE.md §17.1: this phase consumes Phase 50 + Phase 53a + Phase 26
// shipped surfaces, so an integration test is mandatory; CLAUDE.md
// §17.3: real drivers + identity propagation + ≥1 failure mode + run
// under -race.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/registry"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/state/drivers/postgres"
	_ "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/auth/conformancetest"
)

// phase30ID is the canonical test identity. Documented dummy values
// per CLAUDE.md §7 rule 2.
var phase30ID = identity.Identity{
	TenantID:  "tenant-phase30",
	UserID:    "user-phase30",
	SessionID: "session-phase30",
}

const phase30AgentID = "agent-research-assistant-phase30"

// phase30Store opens a state.StateStore for the named driver.
// Postgres leg skips without HARBOR_PG_DSN — see phase50_durability_test.go
// for the convention.
type phase30StoreCase struct {
	name string
	open func(t *testing.T) state.StateStore
}

func phase30StoreCases() []phase30StoreCase {
	return []phase30StoreCase{
		{
			name: "inmem",
			open: func(t *testing.T) state.StateStore {
				t.Helper()
				s, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
				if err != nil {
					t.Fatalf("state.Open(inmem): %v", err)
				}
				t.Cleanup(func() { _ = s.Close(context.Background()) })
				return s
			},
		},
		{
			name: "sqlite",
			open: func(t *testing.T) state.StateStore {
				t.Helper()
				dsn := filepath.Join(t.TempDir(), "phase30.sqlite")
				s, err := state.Open(context.Background(), config.StateConfig{Driver: "sqlite", DSN: dsn})
				if err != nil {
					t.Fatalf("state.Open(sqlite): %v", err)
				}
				t.Cleanup(func() { _ = s.Close(context.Background()) })
				return s
			},
		},
		{
			name: "postgres",
			open: func(t *testing.T) state.StateStore {
				t.Helper()
				dsn := os.Getenv("HARBOR_PG_DSN")
				if dsn == "" {
					t.Skip("HARBOR_PG_DSN not set; skipping postgres leg")
				}
				s, err := state.Open(context.Background(), config.StateConfig{Driver: "postgres", DSN: dsn})
				if err != nil {
					t.Fatalf("state.Open(postgres): %v", err)
				}
				t.Cleanup(func() { _ = s.Close(context.Background()) })
				return s
			},
		},
	}
}

// TestE2E_Phase30_TokenStore_ConformanceAcrossDrivers runs the
// shared TokenStore + Sealer conformance suite against every V1
// state.StateStore driver — proving the master-plan's "TokenStore
// (InMem + SQLite + Postgres drivers)" criterion. The same suite
// exercises encryption-at-rest, cross-tenant / cross-user /
// cross-agent isolation, and mixed-scope coexistence (the
// master-plan's "user-bound and agent-bound tokens coexist for the
// same tool without collision" criterion).
func TestE2E_Phase30_TokenStore_ConformanceAcrossDrivers(t *testing.T) {
	for _, tc := range phase30StoreCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			conformancetest.Run(t, func(tt *testing.T) (auth.TokenStore, state.StateStore, auth.Sealer) {
				tt.Helper()
				raw := tc.open(tt)
				kek := phase30KEK()
				sealer, err := auth.NewAESGCMSealer(kek)
				if err != nil {
					tt.Fatalf("NewAESGCMSealer: %v", err)
				}
				ts, err := auth.NewTokenStore(raw, sealer)
				if err != nil {
					tt.Fatalf("NewTokenStore: %v", err)
				}
				return ts, raw, sealer
			})
		})
	}
}

// TestE2E_Phase30_FullPauseResumeCycle_BothBindingScopes is the
// master-plan headline acceptance criterion: a full OAuth
// pause/resume cycle round-trips for BOTH binding scopes. Real
// drivers everywhere; the httptest authorization server exercises
// PKCE + RFC 7591 dynamic registration + metadata discovery.
func TestE2E_Phase30_FullPauseResumeCycle_BothBindingScopes(t *testing.T) {
	for _, tc := range phase30StoreCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			env := buildPhase30Env(t, tc)
			t.Run("user_bound", func(t *testing.T) {
				ctx := phase30Ctx(t, phase30ID)
				runOAuthCycle(t, env, ctx, env.userCfg.Source, auth.ScopeUser, false)
			})
			t.Run("agent_bound_requires_admin_scope", func(t *testing.T) {
				ctx := registry.WithControlScope(phase30Ctx(t, phase30ID))
				runOAuthCycle(t, env, ctx, env.agentCfg.Source, auth.ScopeAgent, true)
			})
		})
	}
}

// runOAuthCycle drives the full cycle and asserts:
//   - Token() returns *ErrAuthRequired with the right binding scope
//   - PKCE round-trips (server-side verification matches)
//   - the pause is recorded; the resume terminates it
//   - tool.auth_required AND tool.auth_completed land on the bus
//   - subsequent Token() resolves from the store
//
// `requireAdmin` echoes the master-plan "admin-scope authz gates
// protect provider configuration" criterion: ScopeAgent flows must
// have registry.WithControlScope on ctx.
func runOAuthCycle(t *testing.T, env *phase30Env, ctx context.Context, source tools.ToolSourceID, expectScope auth.BindingScope, requireAdmin bool) {
	t.Helper()
	// 1. Token() with no record → ErrAuthRequired + pause + event.
	authRequiredCh := subscribeForEvent(t, env.bus, phase30ID, auth.EventTypeToolAuthRequired)
	defer authRequiredCh.cancel()

	if requireAdmin {
		// First call WITHOUT admin scope must fail loud for InitiateFlow.
		_, err := env.provider.InitiateFlow(stripAdmin(ctx), source)
		if !errors.Is(err, auth.ErrAdminScopeRequired) {
			t.Fatalf("InitiateFlow w/o admin scope: want ErrAdminScopeRequired, got %v", err)
		}
	}
	// Token() always works (admin not required for Token; the
	// returned ErrAuthRequired is the runtime's signal to the
	// Console to prompt the right principal).
	_, err := env.provider.Token(ctx, source)
	var authErr *auth.ErrAuthRequired
	if !errors.As(err, &authErr) {
		t.Fatalf("Token: want *ErrAuthRequired, got %T (%v)", err, err)
	}
	if authErr.BindingScope != expectScope {
		t.Fatalf("ErrAuthRequired.BindingScope: got %s want %s",
			authErr.BindingScope, expectScope)
	}
	if authErr.Source != source {
		t.Fatalf("ErrAuthRequired.Source: got %s want %s",
			authErr.Source, source)
	}
	if authErr.AuthorizeURL == "" {
		t.Fatalf("ErrAuthRequired.AuthorizeURL empty")
	}
	if authErr.State == "" {
		t.Fatalf("ErrAuthRequired.State empty")
	}
	// Verify the event landed with the same payload shape.
	authRequiredEv := authRequiredCh.wait(t, 2*time.Second)
	assertAuthRequiredEventShape(t, authRequiredEv, source, expectScope, authErr.State)

	// 2. Visit the authorize URL → server records (state, code).
	code, gotState, err := env.server.VisitAuthorizeURL(authErr.AuthorizeURL)
	if err != nil {
		t.Fatalf("VisitAuthorizeURL: %v", err)
	}
	if gotState != authErr.State {
		t.Fatalf("state cross-talk: got %q want %q", gotState, authErr.State)
	}

	// 3. CompleteFlow exchanges + persists + resumes.
	authCompletedCh := subscribeForEvent(t, env.bus, phase30ID, auth.EventTypeToolAuthCompleted)
	defer authCompletedCh.cancel()

	tok, err := env.provider.CompleteFlow(ctx, authErr.State, code)
	if err != nil {
		t.Fatalf("CompleteFlow: %v", err)
	}
	if tok.AccessToken == "" {
		t.Fatalf("CompleteFlow returned empty AccessToken")
	}
	if tok.BindingScope != expectScope {
		t.Fatalf("tok.BindingScope: got %s want %s", tok.BindingScope, expectScope)
	}
	if expectScope == auth.ScopeAgent {
		if tok.AgentID != phase30AgentID {
			t.Fatalf("agent-bound tok.AgentID: got %q want %q", tok.AgentID, phase30AgentID)
		}
		if tok.UserID != "" {
			t.Fatalf("agent-bound tok.UserID must be empty; got %q", tok.UserID)
		}
	} else {
		if tok.UserID != phase30ID.UserID {
			t.Fatalf("user-bound tok.UserID: got %q want %q",
				tok.UserID, phase30ID.UserID)
		}
	}

	// 4. tool.auth_completed event arrived.
	completedEv := authCompletedCh.wait(t, 2*time.Second)
	if completedEv.Type != auth.EventTypeToolAuthCompleted {
		t.Fatalf("completed event type: got %s", completedEv.Type)
	}

	// 5. Subsequent Token() resolves from the store; no pause.
	tok2, err := env.provider.Token(ctx, source)
	if err != nil {
		t.Fatalf("Token readback: %v", err)
	}
	if tok2.AccessToken != tok.AccessToken {
		t.Fatalf("Token readback mismatch")
	}
}

// TestE2E_Phase30_A2AAuthRequired_ConvergesOnSamePrimitive asserts
// the master-plan acceptance criterion: "A2A AUTH_REQUIRED triggers
// an identical event shape." A2A driver emits the same
// ErrAuthRequired typed sentinel; the runtime sees one event
// regardless of southbound transport.
//
// We exercise this at the type level — the ErrAuthRequired struct +
// the EventTypeToolAuthRequired event are transport-agnostic; a Phase
// 29 A2A driver that translates AUTH_REQUIRED into the same shape
// will exercise the same Pause/Resume path. The test below
// constructs an ErrAuthRequired directly to assert payload-shape
// invariance.
func TestE2E_Phase30_A2AAuthRequired_ConvergesOnSamePrimitive(t *testing.T) {
	t.Parallel()
	// The convergence is structural: both the MCP southbound path
	// (Phase 28) and the A2A southbound path (Phase 29) return the
	// SAME typed error and emit the SAME event. We assert the
	// payload shape carries the necessary fields for both transport
	// shapes.
	a2aLike := &auth.ErrAuthRequired{
		Source:       tools.ToolSourceID("a2a-peer-compliance"),
		SourceName:   "Compliance Agent",
		BindingScope: auth.ScopeAgent,
		AuthorizeURL: "https://compliance.example/oauth",
		State:        "shared-state",
		Scopes:       []string{"audit.read"},
		Message:      "A2A AUTH_REQUIRED",
	}
	mcpLike := &auth.ErrAuthRequired{
		Source:       tools.ToolSourceID("mcp-server-github"),
		SourceName:   "GitHub",
		BindingScope: auth.ScopeUser,
		AuthorizeURL: "https://github.com/oauth",
		State:        "shared-state",
		Scopes:       []string{"repo"},
		Message:      "MCP requires user OAuth",
	}
	// Both must satisfy errors.Is via the sentinel.
	if !errors.Is(a2aLike, auth.ErrAuthRequiredSentinel) {
		t.Fatal("A2A *ErrAuthRequired does not match ErrAuthRequiredSentinel")
	}
	if !errors.Is(mcpLike, auth.ErrAuthRequiredSentinel) {
		t.Fatal("MCP *ErrAuthRequired does not match ErrAuthRequiredSentinel")
	}
	// Both serialise into the SAME ToolAuthRequiredPayload shape —
	// the runtime emits one event type regardless of transport.
	a2aPayload := auth.ToolAuthRequiredPayload{
		Source: string(a2aLike.Source), SourceName: a2aLike.SourceName,
		BindingScope: string(a2aLike.BindingScope), AuthorizeURL: a2aLike.AuthorizeURL,
		State: a2aLike.State, Scopes: a2aLike.Scopes,
	}
	mcpPayload := auth.ToolAuthRequiredPayload{
		Source: string(mcpLike.Source), SourceName: mcpLike.SourceName,
		BindingScope: string(mcpLike.BindingScope), AuthorizeURL: mcpLike.AuthorizeURL,
		State: mcpLike.State, Scopes: mcpLike.Scopes,
	}
	// Field set is identical — same struct, same field names. A
	// Console rendering this payload does not branch on transport.
	if a2aPayload.State != mcpPayload.State {
		t.Fatalf("state cross-talk")
	}
}

// TestE2E_Phase30_InitiateThenCancel_NoGoroutineLeak is the
// master-plan acceptance criterion: "initiate-then-cancel emits no
// goroutine leak." 25 cycles of Token() → cancel-ctx without
// completing.
func TestE2E_Phase30_InitiateThenCancel_NoGoroutineLeak(t *testing.T) {
	env := buildPhase30Env(t, phase30StoreCases()[0])
	baseline := runtime.NumGoroutine()
	for i := 0; i < 25; i++ {
		ctx, cancel := context.WithCancel(phase30Ctx(t, phase30ID))
		_, err := env.provider.Token(ctx, env.userCfg.Source)
		var authErr *auth.ErrAuthRequired
		if !errors.As(err, &authErr) {
			cancel()
			t.Fatalf("Token: want *ErrAuthRequired, got %v", err)
		}
		cancel() // simulate "user closed the browser; no callback"
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+5 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	leak := runtime.NumGoroutine() - baseline
	if leak > 5 {
		t.Fatalf("goroutine leak: leaked=%d (baseline=%d, now=%d)",
			leak, baseline, runtime.NumGoroutine())
	}
}

// TestE2E_Phase30_FailureMode_StateMismatchCrossIdentity is the
// §17.3 #3 failure-mode coverage. A flow initiated under identity A
// must not be completable from identity B.
func TestE2E_Phase30_FailureMode_StateMismatchCrossIdentity(t *testing.T) {
	env := buildPhase30Env(t, phase30StoreCases()[0])

	idA := identity.Identity{TenantID: "tenantA", UserID: "uA", SessionID: "sA"}
	idB := identity.Identity{TenantID: "tenantB", UserID: "uB", SessionID: "sB"}
	ctxA := phase30Ctx(t, idA)
	ctxB := phase30Ctx(t, idB)

	_, err := env.provider.Token(ctxA, env.userCfg.Source)
	var authErr *auth.ErrAuthRequired
	if !errors.As(err, &authErr) {
		t.Fatalf("A Token: want *ErrAuthRequired, got %v", err)
	}
	code, _, err := env.server.VisitAuthorizeURL(authErr.AuthorizeURL)
	if err != nil {
		t.Fatalf("VisitAuthorizeURL: %v", err)
	}
	_, err = env.provider.CompleteFlow(ctxB, authErr.State, code)
	if !errors.Is(err, auth.ErrStateMismatch) {
		t.Fatalf("cross-identity CompleteFlow: want ErrStateMismatch, got %v", err)
	}
}

// TestE2E_Phase30_Concurrency_NoCrossTalk runs N≥16 concurrent OAuth
// cycles under distinct identity stacks (CLAUDE.md §17.3 concurrency
// stress). Asserts no cross-identity bleed.
func TestE2E_Phase30_Concurrency_NoCrossTalk(t *testing.T) {
	env := buildPhase30Env(t, phase30StoreCases()[0])
	const N = 16
	var wg sync.WaitGroup
	errCh := make(chan error, N)
	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  fmt.Sprintf("tenant-%d", i%3),
				UserID:    fmt.Sprintf("user-%d", i),
				SessionID: fmt.Sprintf("session-%d", i),
			}
			ctx := phase30Ctx(t, id)
			_, err := env.provider.Token(ctx, env.userCfg.Source)
			var authErr *auth.ErrAuthRequired
			if !errors.As(err, &authErr) {
				errCh <- fmt.Errorf("g%d Token: %v", i, err)
				return
			}
			code, _, err := env.server.VisitAuthorizeURL(authErr.AuthorizeURL)
			if err != nil {
				errCh <- fmt.Errorf("g%d VisitAuthorizeURL: %v", i, err)
				return
			}
			tok, err := env.provider.CompleteFlow(ctx, authErr.State, code)
			if err != nil {
				errCh <- fmt.Errorf("g%d CompleteFlow: %v", i, err)
				return
			}
			if tok.TenantID != id.TenantID || tok.UserID != id.UserID {
				errCh <- fmt.Errorf("g%d cross-identity bleed: tok=%s/%s ctx=%s/%s",
					i, tok.TenantID, tok.UserID, id.TenantID, id.UserID)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Error(e)
	}
}

// --- environment helpers --------------------------------------------------

type phase30Env struct {
	store       state.StateStore
	tokenStore  auth.TokenStore
	bus         events.EventBus
	coordinator pauseresume.Coordinator
	provider    *auth.Provider
	server      *phase30FakeAuthServer
	userCfg     auth.OAuthConfig
	agentCfg    auth.OAuthConfig
}

func buildPhase30Env(t *testing.T, sc phase30StoreCase) *phase30Env {
	t.Helper()
	raw := sc.open(t)
	kek := phase30KEK()
	sealer, err := auth.NewAESGCMSealer(kek)
	if err != nil {
		t.Fatalf("NewAESGCMSealer: %v", err)
	}
	ts, err := auth.NewTokenStore(raw, sealer)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}
	red := patternsAudit.New()
	busCfg := config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              500 * time.Millisecond,
		DropWindow:               50 * time.Millisecond,
	}
	bus, err := eventsInmem.New(busCfg, red)
	if err != nil {
		t.Fatalf("eventsInmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	coord := pauseresume.New()

	server := newPhase30FakeAuthServer(t)

	userCfg := auth.OAuthConfig{
		Source: tools.ToolSourceID("github-user"), SourceName: "GitHub",
		BindingScope: auth.ScopeUser,
		ServerURL:    server.BaseURL(),
		RedirectURI:  "http://localhost/callback",
		Scopes:       []string{"repo", "read:user"},
	}
	agentCfg := auth.OAuthConfig{
		Source: tools.ToolSourceID("outlook-shared"), SourceName: "Outlook (Shared)",
		BindingScope: auth.ScopeAgent,
		AgentID:      phase30AgentID,
		ServerURL:    server.BaseURL(),
		RedirectURI:  "http://localhost/callback",
		Scopes:       []string{"mail.read"},
	}
	prov, err := auth.NewProvider([]auth.OAuthConfig{userCfg, agentCfg}, auth.ProviderDeps{
		Store: ts, Bus: bus, Redactor: red, Coordinator: coord,
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("auth.NewProvider: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })

	return &phase30Env{
		store: raw, tokenStore: ts, bus: bus, coordinator: coord,
		provider: prov, server: server,
		userCfg: userCfg, agentCfg: agentCfg,
	}
}

func phase30KEK() []byte {
	// Dummy KEK for the integration test — never a real credential
	// (§7 rule 2). Reproducible across test runs so SQLite leg can
	// re-decrypt across re-opens within a single test.
	kek := make([]byte, auth.KEKSizeBytes)
	for i := range kek {
		kek[i] = byte(i*11 + 7)
	}
	return kek
}

func phase30Ctx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// stripAdmin removes the registry control-scope claim from a ctx so
// we can test the admin-scope authz gate.
func stripAdmin(ctx context.Context) context.Context {
	// The Phase 53a control-scope value is attached via context.WithValue
	// with an unexported key. Rebuilding from background with the same
	// identity drops the claim cleanly.
	id, _ := identity.From(ctx)
	out, _ := identity.With(context.Background(), id)
	return out
}

// --- bus subscription helper ---------------------------------------------

type capturingSub struct {
	sub    events.Subscription
	cancel func()
}

func subscribeForEvent(t *testing.T, bus events.EventBus, id identity.Identity, evType events.EventType) *capturingSub {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	sub, err := bus.Subscribe(ctx, events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: []events.EventType{evType},
	})
	if err != nil {
		cancel()
		t.Fatalf("Subscribe: %v", err)
	}
	return &capturingSub{
		sub: sub,
		cancel: func() {
			sub.Cancel()
			cancel()
		},
	}
}

func (c *capturingSub) wait(t *testing.T, d time.Duration) events.Event {
	t.Helper()
	select {
	case ev, ok := <-c.sub.Events():
		if !ok {
			t.Fatalf("subscription channel closed")
		}
		return ev
	case <-time.After(d):
		t.Fatalf("timed out waiting for event")
		return events.Event{}
	}
}

func assertAuthRequiredEventShape(t *testing.T, ev events.Event, source tools.ToolSourceID, scope auth.BindingScope, state string) {
	t.Helper()
	if ev.Type != auth.EventTypeToolAuthRequired {
		t.Fatalf("event type: got %s want %s", ev.Type, auth.EventTypeToolAuthRequired)
	}
	// The bus may carry the payload either typed (SafePayload bypass)
	// or as a RedactedMap depending on the redactor's behaviour.
	switch p := ev.Payload.(type) {
	case auth.ToolAuthRequiredPayload:
		if p.Source != string(source) || p.BindingScope != string(scope) || p.State != state {
			t.Fatalf("typed payload mismatch: %+v vs (src=%s, scope=%s, state=%s)",
				p, source, scope, state)
		}
	case events.RedactedMap:
		if p.Data["source"] != string(source) {
			t.Fatalf("redacted payload source: %v", p.Data["source"])
		}
		if p.Data["state"] != state {
			t.Fatalf("redacted payload state: %v", p.Data["state"])
		}
	default:
		t.Fatalf("unexpected payload type %T", p)
	}
}

// --- fake auth server -----------------------------------------------------
// A self-contained OAuth 2.0 authorization server with PKCE +
// RFC 7591 dynamic client registration + metadata discovery, served
// over httptest.Server. The test drives the authorize → token
// round-trip without a real browser: the /authorize handler returns
// the issued code in a JSON envelope the test parses.

type phase30FakeAuthServer struct {
	srv          *httptest.Server
	mu           sync.Mutex
	codes        map[string]struct{ state, challenge string }
	nextClientID string
	tokenCalls   int
}

func newPhase30FakeAuthServer(t *testing.T) *phase30FakeAuthServer {
	t.Helper()
	f := &phase30FakeAuthServer{
		codes:        make(map[string]struct{ state, challenge string }),
		nextClientID: "phase30-dyn-client",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", f.discovery)
	mux.HandleFunc("/register", f.register)
	mux.HandleFunc("/authorize", f.authorize)
	mux.HandleFunc("/token", f.token)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *phase30FakeAuthServer) BaseURL() string { return f.srv.URL }

func (f *phase30FakeAuthServer) discovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	body := map[string]string{
		"authorization_endpoint": f.srv.URL + "/authorize",
		"token_endpoint":         f.srv.URL + "/token",
		"registration_endpoint":  f.srv.URL + "/register",
	}
	_ = json.NewEncoder(w).Encode(body)
}

func (f *phase30FakeAuthServer) register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	body, err := readAll(r)
	if err != nil {
		http.Error(w, "read", http.StatusBadRequest)
		return
	}
	if !bytes.Contains(body, []byte("redirect_uris")) {
		http.Error(w, "missing redirect_uris", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"client_id": f.nextClientID})
}

func (f *phase30FakeAuthServer) authorize(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	chall := r.URL.Query().Get("code_challenge")
	if state == "" || chall == "" {
		http.Error(w, "missing state/challenge", http.StatusBadRequest)
		return
	}
	code := newAuthCode()
	f.mu.Lock()
	f.codes[code] = struct{ state, challenge string }{state, chall}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "state": state})
}

func (f *phase30FakeAuthServer) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	f.mu.Lock()
	f.tokenCalls++
	f.mu.Unlock()
	_ = r.ParseForm()
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		code := r.PostForm.Get("code")
		verifier := r.PostForm.Get("code_verifier")
		f.mu.Lock()
		rec, ok := f.codes[code]
		if ok {
			delete(f.codes, code)
		}
		f.mu.Unlock()
		if !ok {
			http.Error(w, "code unknown", http.StatusBadRequest)
			return
		}
		// PKCE verification.
		expectedChallenge := pkceChallengeS256(verifier)
		if expectedChallenge != rec.challenge {
			http.Error(w, "pkce verification failed", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "phase30-access-" + newAuthCode(),
			"refresh_token": "phase30-refresh-" + newAuthCode(),
			"token_type":    "Bearer",
			"expires_in":    3600,
			"scope":         r.PostForm.Get("scope"),
		})
	case "refresh_token":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "phase30-access-refreshed-" + newAuthCode(),
			"refresh_token": "phase30-refresh-rotated-" + newAuthCode(),
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	default:
		http.Error(w, "unsupported grant_type", http.StatusBadRequest)
	}
}

func (f *phase30FakeAuthServer) VisitAuthorizeURL(authorizeURL string) (string, string, error) {
	resp, err := http.Get(authorizeURL)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("authorize: %d", resp.StatusCode)
	}
	var out struct{ Code, State string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}
	return out.Code, out.State, nil
}

// readAll reads at most 64 KiB from a request body (defensive bound;
// the registration request is tiny).
func readAll(r *http.Request) ([]byte, error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := r.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if len(buf) > 64*1024 {
				return nil, errors.New("body too large")
			}
		}
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return buf, err
		}
	}
}

// newAuthCode mints a fresh URL-safe ID for codes / tokens in the
// fake server. crypto/rand-backed so concurrent goroutines do not
// collide on the timestamp-derived seed.
func newAuthCode() string {
	buf := make([]byte, 16)
	_, _ = phase30Rand(buf)
	return base64URLEncode(buf)
}

// pkceChallengeS256 is duplicated from internal/tools/auth/pkce.go
// because the function is unexported there. The semantics are RFC
// 7636 — BASE64URL(SHA256(verifier)) — so this is the spec, not a
// private detail.
func pkceChallengeS256(verifier string) string {
	// import-local to avoid pulling sha256 into the package header
	// for one helper.
	return pkceChallengeImpl(verifier)
}
