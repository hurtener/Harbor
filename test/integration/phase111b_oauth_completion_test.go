// Phase 111b — Tool-OAuth completion leg (D-199; RFC §6.4 + §3.3 +
// §6.3; docs/plans/phase-111b-tool-oauth-completion.md).
//
// The FULL choreography, real artifacts on every seam (§17.3 — no
// mocks at any boundary):
//
//   - real `tools.NewCatalog` + the Phase 64a `catalog.Builder`
//     applying an OAuth entry (`WrapWithOAuth` in the Invoke path),
//   - the REAL Phase 30 `*auth.Provider` (PKCE + RFC 7591 dynamic
//     registration + metadata discovery against an httptest
//     authorization server whose /authorize 302-REDIRECTS the
//     simulated browser onto the mounted callback handler),
//   - the REAL `auth.CallbackHandler` mounted headless on its own
//     mux + httptest.Server (the plan's headless-mountable leg),
//   - real `pauseresume.New(WithBus)` Coordinator, real in-mem
//     events bus, real patterns redactor, real in-mem StateStore +
//     TokenStore (AES-GCM at rest),
//   - real `deterministic.DeterministicPlanner` driving CallTool →
//     RequestPause(ExternalEvent) → CallTool → Finish,
//   - real `steering.Registry` + `steering.RunLoop` + the REAL
//     production `dispatch.NewToolExecutor` (D-194).
//
// The choreography proven end-to-end:
//
//	planner dispatches the OAuth-gated tool
//	  → the wrapper's Token() parks a pause (tool.auth_required +
//	    pause.requested on the bus)
//	  → the planner parks the RUN (RequestPause{ExternalEvent})
//	  → the simulated user visits the authorize URL; the
//	    authorization server 302-redirects the browser onto the
//	    mounted auth.CallbackHandler
//	  → CompleteFlow exchanges (state, code), persists the token,
//	    and resumes the OAuth pause (pause.resumed{Decision: resume}
//	    + tool.auth_completed on the bus)
//	  → the run re-enters (the steering RESUME — the same inbox path
//	    the Protocol edge's resume method drives; see the recipe's
//	    honesty note: the run-level resume is operator-steered at V1)
//	  → the re-dispatched tool invocation SUCCEEDS using the
//	    freshly-minted token (the tool body reads it via
//	    provider.Token, exactly as the HTTP/MCP drivers compose
//	    bearer headers) — the "bare Resume re-parks immediately"
//	    trap is the regression this guards.
//
// Identity propagation is asserted on the tool.auth_required /
// pause.resumed payloads and on the minted token's binding; the
// failure modes cover the expired flow (410 + pause still parked)
// and the replayed callback (200 without a second token exchange).
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/deterministic"
	"github.com/hurtener/Harbor/internal/runtime/dispatch"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	toolcatalog "github.com/hurtener/Harbor/internal/tools/catalog"
)

// phase111bID is the canonical documented dummy identity (§7 rule 2).
var phase111bID = identity.Identity{
	TenantID:  "tenant-phase111b",
	UserID:    "user-phase111b",
	SessionID: "session-phase111b",
}

func mustPhase111bFlowStore(t *testing.T, store state.StateStore, sealer toolauth.Sealer) toolauth.FlowStore {
	t.Helper()
	flows, err := toolauth.NewFlowStore(store, sealer)
	if err != nil {
		t.Fatalf("NewFlowStore: %v", err)
	}
	return flows
}

const (
	phase111bTool     = "phase111b_gated_fetch"
	phase111bProvider = "github"
	phase111bTimeout  = 15 * time.Second
)

// phase111bEnv bundles the wired real artifacts.
type phase111bEnv struct {
	cat      tools.ToolCatalog
	bus      events.EventBus
	coord    pauseresume.Coordinator
	provider toolauth.OAuthProvider
	steerReg *steering.Registry
	runLoop  *steering.RunLoop
	executor steering.ToolExecutor
	authSrv  *phase111bAuthServer
	cbSrv    *httptest.Server // the headless-mounted callback handler
	source   tools.ToolSourceID
	// toolRuns counts post-wrapper tool-body executions.
	toolRuns *atomic.Int64
	// seenToken records the bearer the tool body fetched via
	// provider.Token — the "tool invocation succeeds USING the token"
	// probe (the same read an HTTP/MCP driver does to compose the
	// Authorization header).
	seenToken *atomic.Value
	// seenIdentity records the identity the tool body read from ctx.
	seenIdentity *atomic.Value
}

func buildPhase111bEnv(t *testing.T) *phase111bEnv {
	t.Helper()
	red := auditpatterns.New()
	bus, err := eventsInmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     128,
		IdleTimeout:              2 * time.Second,
		DropWindow:               50 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("eventsInmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	// WithBus is what makes pause.requested / pause.resumed land on
	// the canonical stream (Wave 11.5 §17.5 F1) — the acceptance
	// criteria assert both.
	coord := pauseresume.New(pauseresume.WithBus(bus))

	stateStore, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = stateStore.Close(context.Background()) })

	kek := phase111bKEK()
	sealer, err := toolauth.NewAESGCMSealer(kek)
	if err != nil {
		t.Fatalf("NewAESGCMSealer: %v", err)
	}
	tokenStore, err := toolauth.NewTokenStore(stateStore, sealer)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	// The headless callback mount: a plain mux + httptest.Server —
	// no Protocol server, no dev server, no cmd/harbor. The server
	// starts first (the provider's RedirectURI needs its URL); the
	// handler is registered on the mux below, once the provider map
	// is built (the handler snapshots the map at construction —
	// D-025 immutability).
	cbMux := http.NewServeMux()
	cbSrv := httptest.NewServer(cbMux)
	t.Cleanup(cbSrv.Close)
	redirectURI := cbSrv.URL + toolauth.CallbackPath

	authSrv := newPhase111bAuthServer(t)

	source := tools.ToolSourceID("phase111b-source")
	oauthCfg := toolauth.OAuthConfig{
		Source:       source,
		SourceName:   "Phase111b Upstream",
		BindingScope: toolauth.ScopeUser,
		ServerURL:    authSrv.BaseURL(),
		RedirectURI:  redirectURI,
		Scopes:       []string{"repo"},
	}
	prov, err := toolauth.NewProvider([]toolauth.OAuthConfig{oauthCfg}, toolauth.ProviderDeps{
		Store: tokenStore, Flows: mustPhase111bFlowStore(t, stateStore, sealer), Bus: bus, Redactor: red, Coordinator: coord,
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("auth.NewProvider: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })
	providers := map[string]toolauth.OAuthProvider{phase111bProvider: prov}
	cbMux.Handle(toolauth.CallbackRoutePattern, toolauth.CallbackHandler(providers))

	// Real catalog + the Phase 64a Builder applying the OAuth entry —
	// the SAME production wiring path `assemble.Assemble` drives.
	cat := tools.NewCatalog()
	toolRuns := &atomic.Int64{}
	seenToken := &atomic.Value{}
	seenIdentity := &atomic.Value{}
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{
			Name:        phase111bTool,
			Description: "fetch behind a tool-OAuth attachment (phase 111b E2E)",
			Transport:   tools.TransportInProcess,
			Source:      source,
		},
		Invoke: func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			toolRuns.Add(1)
			if id, ok := identity.From(ctx); ok {
				seenIdentity.Store(id)
			}
			// The driver-shaped read: fetch the bearer to compose the
			// upstream request — proves the invocation USES the
			// freshly-minted token.
			tok, terr := prov.Token(ctx, source)
			if terr != nil {
				return tools.ToolResult{}, fmt.Errorf("phase111b tool: token fetch: %w", terr)
			}
			seenToken.Store(tok.AccessToken)
			return tools.ToolResult{Value: map[string]any{"fetched": string(args)}}, nil
		},
	}); err != nil {
		t.Fatalf("catalog.Register: %v", err)
	}
	b := toolcatalog.New([]config.ToolEntryConfig{
		{
			Name: phase111bTool,
			OAuth: &config.ToolOAuthConfig{
				Provider:     phase111bProvider,
				BindingScope: string(toolauth.ScopeUser),
			},
		},
	}, toolcatalog.Deps{
		Catalog:        cat,
		Coordinator:    coord,
		Bus:            bus,
		Redactor:       red,
		OAuthProviders: providers,
	})
	if err := b.Apply(context.Background()); err != nil {
		t.Fatalf("catalog Builder.Apply: %v", err)
	}

	steerReg := steering.NewRegistry()
	rl, err := steering.NewRunLoop(steerReg, coord, steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}

	artStore, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	t.Cleanup(func() { _ = artStore.Close(context.Background()) })
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    stateStore,
		Bus:      bus,
		Redactor: red,
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	t.Cleanup(func() { _ = taskReg.Close(context.Background()) })

	return &phase111bEnv{
		cat:          cat,
		bus:          bus,
		coord:        coord,
		provider:     prov,
		steerReg:     steerReg,
		runLoop:      rl,
		executor:     dispatch.NewToolExecutor(cat, artStore, taskReg),
		authSrv:      authSrv,
		cbSrv:        cbSrv,
		source:       source,
		toolRuns:     toolRuns,
		seenToken:    seenToken,
		seenIdentity: seenIdentity,
	}
}

func phase111bKEK() []byte {
	// Dummy KEK — documented non-secret fixture (§7 rule 2).
	kek := make([]byte, toolauth.KEKSizeBytes)
	for i := range kek {
		kek[i] = byte(i*13 + 5)
	}
	return kek
}

// phase111bPlanner builds the REAL deterministic planner driving the
// choreography:
//
//	step A: CallTool (trajectory empty) — fails auth_required
//	step B: RequestPause{ExternalEvent} (one errored step, no
//	        injected context yet) — parks the RUN
//	step C: CallTool again (the resume's INJECT_CONTEXT marker is
//	        visible) — succeeds with the minted token
//	step D: Finish (two trajectory steps)
func phase111bPlanner(t *testing.T) *deterministic.DeterministicPlanner {
	t.Helper()
	steps := func(rc planner.RunContext) int {
		if rc.Trajectory == nil {
			return 0
		}
		return len(rc.Trajectory.Steps)
	}
	injected := func(rc planner.RunContext) bool {
		return len(rc.Control.InjectedContext) > 0
	}
	p, err := deterministic.NewDeterministicPlanner(deterministic.WithSteps(
		&deterministic.CallToolStep{
			Tool: phase111bTool,
			ArgsBuilder: func(planner.RunContext) (json.RawMessage, error) {
				return json.RawMessage(`{"q":"phase111b"}`), nil
			},
			When: func(rc planner.RunContext) bool { return steps(rc) == 0 },
		},
		&deterministic.PauseStep{
			Reason: planner.PauseExternalEvent,
			PayloadBuilder: func(planner.RunContext) (map[string]any, error) {
				return map[string]any{"waiting_on": "tool-oauth completion"}, nil
			},
			When: func(rc planner.RunContext) bool { return steps(rc) == 1 && !injected(rc) },
		},
		&deterministic.CallToolStep{
			Tool: phase111bTool,
			ArgsBuilder: func(planner.RunContext) (json.RawMessage, error) {
				return json.RawMessage(`{"q":"phase111b-retry"}`), nil
			},
			When: func(rc planner.RunContext) bool { return steps(rc) == 1 && injected(rc) },
		},
		&deterministic.FinishStep{
			Reason: planner.FinishGoal,
			When:   func(rc planner.RunContext) bool { return steps(rc) >= 2 },
		},
	))
	if err != nil {
		t.Fatalf("deterministic.NewDeterministicPlanner: %v", err)
	}
	return p
}

// phase111bSubscribe opens an identity-filtered subscription BEFORE
// the producing call so the publish is never raced.
func phase111bSubscribe(t *testing.T, bus events.EventBus, id identity.Identity, types ...events.EventType) (events.Subscription, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	sub, err := bus.Subscribe(ctx, events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: types,
	})
	if err != nil {
		cancel()
		t.Fatalf("Subscribe: %v", err)
	}
	return sub, func() { sub.Cancel(); cancel() }
}

func phase111bWait(t *testing.T, sub events.Subscription, what string) events.Event {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatalf("subscription closed waiting for %s", what)
		}
		return ev
	case <-time.After(phase111bTimeout):
		t.Fatalf("timeout waiting for %s", what)
		return events.Event{}
	}
}

// phase111bBrowse simulates the user's browser visiting the authorize
// URL: the authorization server records the grant and 302-redirects
// onto the mounted callback handler; the client follows the redirect
// (the default http.Client behaviour). Returns the FINAL response's
// status code and URL — the callback handler's own answer (the body
// is drained and closed here; no caller needs it).
func phase111bBrowse(t *testing.T, authorizeURL string) (int, *url.URL) {
	t.Helper()
	resp, err := http.Get(authorizeURL) //nolint:noctx // test browser simulation against a local httptest URL with a bounded server-side timeout
	if err != nil {
		t.Fatalf("browse authorize URL: %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain browse response: %v", err)
	}
	return resp.StatusCode, resp.Request.URL
}

// TestE2E_Phase111b_FullOAuthChoreography is the acceptance headline:
// gated tool → pause → authorize → redirect → CompleteFlow → resume →
// re-entry → the tool succeeds using the minted token.
func TestE2E_Phase111b_FullOAuthChoreography(t *testing.T) {
	env := buildPhase111bEnv(t)
	id := phase111bID

	authReqSub, cancelAuthReq := phase111bSubscribe(t, env.bus, id, toolauth.EventTypeToolAuthRequired)
	defer cancelAuthReq()
	authDoneSub, cancelAuthDone := phase111bSubscribe(t, env.bus, id, toolauth.EventTypeToolAuthCompleted)
	defer cancelAuthDone()
	pauseReqSub, cancelPauseReq := phase111bSubscribe(t, env.bus, id, pauseresume.EventTypePauseRequested)
	defer cancelPauseReq()
	pauseResSub, cancelPauseRes := phase111bSubscribe(t, env.bus, id, pauseresume.EventTypePauseResumed)
	defer cancelPauseRes()

	q := identity.Quadruple{Identity: id, RunID: "run-phase111b-choreography"}
	spec := steering.RunSpec{
		Planner: phase111bPlanner(t),
		Base: planner.RunContext{
			Quadruple:  q,
			Goal:       "exercise the phase 111b OAuth completion choreography",
			Trajectory: &planner.Trajectory{},
			Catalog:    tools.NewPlannerView(env.cat, tools.CatalogFilter{TenantID: q.TenantID, UserID: q.UserID, SessionID: q.SessionID}),
		},
		MaxSteps:     10,
		ToolExecutor: env.executor,
	}

	type runResult struct {
		fin planner.Finish
		err error
	}
	resCh := make(chan runResult, 1)
	go func() {
		fin, err := env.runLoop.Run(context.Background(), spec)
		resCh <- runResult{fin: fin, err: err}
	}()

	// 1. The gated dispatch parks: tool.auth_required + pause.requested
	//    land on the bus with the run's identity (§17.3 #2).
	authReqEv := phase111bWait(t, authReqSub, "tool.auth_required")
	if authReqEv.Identity.Identity != id {
		t.Fatalf("tool.auth_required identity = %+v, want %+v", authReqEv.Identity.Identity, id)
	}
	authReq, ok := authReqEv.Payload.(toolauth.ToolAuthRequiredPayload)
	if !ok {
		t.Fatalf("tool.auth_required payload type = %T", authReqEv.Payload)
	}
	if authReq.Source != string(env.source) || authReq.AuthorizeURL == "" || authReq.State == "" {
		t.Fatalf("tool.auth_required payload incomplete: %+v", authReq)
	}
	oauthPauseReqEv := phase111bWait(t, pauseReqSub, "pause.requested (the OAuth pause)")
	if oauthPauseReqEv.Identity.Identity != id {
		t.Fatalf("OAuth pause.requested identity = %+v, want %+v", oauthPauseReqEv.Identity.Identity, id)
	}
	oauthPause, ok := oauthPauseReqEv.Payload.(pauseresume.PauseRequestedPayload)
	if !ok {
		t.Fatalf("OAuth pause.requested payload type = %T", oauthPauseReqEv.Payload)
	}
	if oauthPause.Token != authReq.PauseToken {
		t.Fatalf("OAuth pause.requested token %q != tool.auth_required pause token %q", oauthPause.Token, authReq.PauseToken)
	}

	// Token returns ErrAuthRequired after creating its own pause, so the
	// RunLoop records the failed tool step and then installs a SECOND pause for
	// the planner's ExternalEvent decision. Wait for that run-level token before
	// completing OAuth: otherwise a fast callback can be followed by RESUME
	// while the RunLoop has not yet installed its outstanding pause.
	runPauseReqEv := phase111bWait(t, pauseReqSub, "pause.requested (the planner run pause)")
	if runPauseReqEv.Identity != q {
		t.Fatalf("planner pause.requested identity = %+v, want %+v", runPauseReqEv.Identity, q)
	}
	runPause, ok := runPauseReqEv.Payload.(pauseresume.PauseRequestedPayload)
	if !ok {
		t.Fatalf("planner pause.requested payload type = %T", runPauseReqEv.Payload)
	}
	if runPause.Token == "" || runPause.Token == oauthPause.Token {
		t.Fatalf("planner pause token %q, OAuth pause token %q: want distinct non-empty tokens", runPause.Token, oauthPause.Token)
	}

	// The tool body never ran pre-callback.
	if got := env.toolRuns.Load(); got != 0 {
		t.Fatalf("tool body executions pre-callback = %d, want 0", got)
	}

	// 2. The simulated user authorizes; the authorization server
	//    302-redirects the browser onto the mounted callback handler.
	status, landedOn := phase111bBrowse(t, authReq.AuthorizeURL)
	if status != http.StatusOK {
		t.Fatalf("callback handler answered %d, want 200", status)
	}
	if landedOn.Path != toolauth.CallbackPath {
		t.Fatalf("redirect landed on %q, want %q", landedOn.Path, toolauth.CallbackPath)
	}

	// 3. CompleteFlow persisted the token + resumed the OAuth pause:
	//    tool.auth_completed and pause.resumed{Decision: resume} land.
	doneEv := phase111bWait(t, authDoneSub, "tool.auth_completed")
	done, ok := doneEv.Payload.(toolauth.ToolAuthCompletedPayload)
	if !ok {
		t.Fatalf("tool.auth_completed payload type = %T", doneEv.Payload)
	}
	if done.State != authReq.State {
		t.Fatalf("tool.auth_completed state = %q, want %q", done.State, authReq.State)
	}
	resEv := phase111bWait(t, pauseResSub, "pause.resumed")
	if resEv.Identity.Identity != id {
		t.Fatalf("pause.resumed identity = %+v, want %+v", resEv.Identity.Identity, id)
	}
	resPayload, ok := resEv.Payload.(pauseresume.PauseResumedPayload)
	if !ok {
		t.Fatalf("pause.resumed payload type = %T", resEv.Payload)
	}
	if resPayload.Decision != pauseresume.DecisionResume {
		t.Fatalf("pause.resumed Decision = %q, want %q (D-096)", resPayload.Decision, pauseresume.DecisionResume)
	}
	if resPayload.Token != done.PauseToken {
		t.Fatalf("pause.resumed token %q != tool.auth_completed pause token %q", resPayload.Token, done.PauseToken)
	}

	// 4. CompleteFlow resumed the OAuth pause (oauthPause.Token), not the
	//    planner run pause (runPause.Token). The two pauses are deliberately
	//    distinct coordination layers. With the run pause already installed
	//    above, this explicit control event releases exactly the RunLoop's
	//    outstanding ExternalEvent park after the token is durably persisted.
	inbox, err := env.steerReg.Lookup(q)
	if err != nil {
		t.Fatalf("steering.Registry.Lookup: %v", err)
	}
	if err := inbox.Enqueue(steering.ControlEvent{
		Type:         steering.ControlInjectContext,
		Identity:     q,
		CallerScope:  steering.ScopeOwnerUser,
		CallerTenant: q.TenantID,
		Payload:      map[string]any{"note": "oauth completed; token persisted"},
	}); err != nil {
		t.Fatalf("Enqueue(INJECT_CONTEXT): %v", err)
	}
	if err := inbox.Enqueue(steering.ControlEvent{
		Type:         steering.ControlResume,
		Identity:     q,
		CallerScope:  steering.ScopeOwnerUser,
		CallerTenant: q.TenantID,
		Payload:      map[string]any{"reason": "tool-oauth completed"},
	}); err != nil {
		t.Fatalf("Enqueue(RESUME): %v", err)
	}
	// 5. The run re-enters and finishes: the re-dispatched tool
	//    invocation succeeded USING the minted token.
	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("Run: %v", res.err)
		}
		if res.fin.Reason != planner.FinishGoal {
			t.Fatalf("Finish.Reason = %q, want %q", res.fin.Reason, planner.FinishGoal)
		}
	case <-time.After(phase111bTimeout):
		t.Fatal("run did not finish after the OAuth completion — re-entry is broken")
	}
	if got := env.toolRuns.Load(); got != 1 {
		t.Fatalf("tool body executions = %d, want 1", got)
	}
	seenTok, _ := env.seenToken.Load().(string)
	if seenTok == "" || seenTok != env.authSrv.LastIssuedAccessToken() {
		t.Fatalf("tool used token %q, want the server-minted %q",
			seenTok, env.authSrv.LastIssuedAccessToken())
	}
	seenID, _ := env.seenIdentity.Load().(identity.Identity)
	if seenID != id {
		t.Fatalf("tool saw identity %+v, want %+v", seenID, id)
	}
	// The re-entered step's observation carries the tool result, not
	// an error — the "bare Resume re-parks immediately" trap did NOT
	// fire (the callback persisted the token BEFORE the run resumed).
	steps := spec.Base.Trajectory.Steps
	if len(steps) != 2 {
		t.Fatalf("trajectory steps = %d, want 2", len(steps))
	}
	if obs, ok := steps[0].Observation.(map[string]any); !ok || obs["error"] == nil {
		t.Fatalf("step 0 observation = %#v, want the auth_required error payload", steps[0].Observation)
	}
	obs, ok := steps[1].Observation.(map[string]any)
	if !ok || obs["error"] != nil {
		t.Fatalf("step 1 observation = %#v, want a successful tool result", steps[1].Observation)
	}
	if obs["fetched"] != `{"q":"phase111b-retry"}` {
		t.Fatalf("step 1 observation = %#v, want the retried args echoed", obs)
	}
}

// TestE2E_Phase111b_ReplayedCallback_IdempotentSuccess verifies that an
// ambiguous browser retry receives success from the bounded completion
// tombstone without re-exchanging the one-time authorization code.
func TestE2E_Phase111b_ReplayedCallback_IdempotentSuccess(t *testing.T) {
	env := buildPhase111bEnv(t)
	ctx := phase111bCtx(t, phase111bID)

	_, terr := env.provider.Token(ctx, env.source)
	var authErr *toolauth.ErrAuthRequired
	if !errors.As(terr, &authErr) {
		t.Fatalf("Token: want *ErrAuthRequired, got %v", terr)
	}
	status, landedOn := phase111bBrowse(t, authErr.AuthorizeURL)
	if status != http.StatusOK {
		t.Fatalf("first callback status = %d, want 200", status)
	}
	if got := env.authSrv.TokenExchangeCount(); got != 1 {
		t.Fatalf("token exchanges after first callback = %d, want 1", got)
	}
	// Replay the EXACT redirect target the browser landed on.
	replayStatus, _ := phase111bBrowse(t, landedOn.String())
	if replayStatus != http.StatusOK {
		t.Fatalf("replayed callback status = %d, want 200", replayStatus)
	}
	if got := env.authSrv.TokenExchangeCount(); got != 1 {
		t.Fatalf("token exchanges after replay = %d, want 1", got)
	}
}

// TestE2E_Phase111b_ExpiredFlow_Gone_PauseStillParked — the callback
// arriving after FlowTTL is 410 and the OAuth pause stays parked
// (cleanup is the sweeper's / operator's job, not the stale
// redirect's).
func TestE2E_Phase111b_ExpiredFlow_Gone_PauseStillParked(t *testing.T) {
	red := auditpatterns.New()
	bus, err := eventsInmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              time.Second,
		DropWindow:               50 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("eventsInmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	coord := pauseresume.New(pauseresume.WithBus(bus))
	stateStore, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = stateStore.Close(context.Background()) })
	sealer, err := toolauth.NewAESGCMSealer(phase111bKEK())
	if err != nil {
		t.Fatalf("NewAESGCMSealer: %v", err)
	}
	tokenStore, err := toolauth.NewTokenStore(stateStore, sealer)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	var clockMu sync.Mutex
	now := time.Now()
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}

	authSrv := newPhase111bAuthServer(t)
	cbMux := http.NewServeMux()
	cbSrv := httptest.NewServer(cbMux)
	t.Cleanup(cbSrv.Close)

	source := tools.ToolSourceID("phase111b-expiry-source")
	prov, err := toolauth.NewProvider([]toolauth.OAuthConfig{{
		Source:       source,
		BindingScope: toolauth.ScopeUser,
		ServerURL:    authSrv.BaseURL(),
		RedirectURI:  cbSrv.URL + toolauth.CallbackPath,
	}}, toolauth.ProviderDeps{
		Store: tokenStore, Flows: mustPhase111bFlowStore(t, stateStore, sealer), Bus: bus, Redactor: red, Coordinator: coord,
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		Clock:      clock,
		FlowTTL:    time.Minute,
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })
	cbMux.Handle(toolauth.CallbackRoutePattern,
		toolauth.CallbackHandler(map[string]toolauth.OAuthProvider{"expiry": prov}))

	id := phase111bID
	ctx := phase111bCtx(t, id)
	sub, cancelSub := phase111bSubscribe(t, bus, id, toolauth.EventTypeToolAuthRequired)
	defer cancelSub()

	_, terr := prov.Token(ctx, source)
	var authErr *toolauth.ErrAuthRequired
	if !errors.As(terr, &authErr) {
		t.Fatalf("Token: want *ErrAuthRequired, got %v", terr)
	}
	pauseToken := phase111bWait(t, sub, "tool.auth_required").
		Payload.(toolauth.ToolAuthRequiredPayload).PauseToken

	// The flow expires before the user completes.
	clockMu.Lock()
	now = now.Add(2 * time.Minute)
	clockMu.Unlock()

	status, _ := phase111bBrowse(t, authErr.AuthorizeURL)
	if status != http.StatusGone {
		t.Fatalf("expired callback status = %d, want 410", status)
	}
	// The pause record is STILL parked.
	st, err := coord.Status(ctx, pauseresume.Token(pauseToken))
	if err != nil {
		t.Fatalf("coordinator.Status: %v", err)
	}
	if st.State != pauseresume.StatusPaused {
		t.Fatalf("expired flow's pause state = %q, want paused", st.State)
	}
}

func phase111bCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// --- the test authorization server -----------------------------------------
// A self-contained OAuth 2.0 authorization server (PKCE + RFC 7591
// dynamic registration + metadata discovery) whose /authorize handler
// 302-REDIRECTS the browser onto the flow's redirect_uri — the leg
// that makes "the redirect hits the handler" literal.

type phase111bAuthServer struct {
	srv   *httptest.Server
	mu    sync.Mutex
	codes map[string]struct {
		state     string
		challenge string
	}
	lastAccess     string
	tokenExchanges int
}

func newPhase111bAuthServer(t *testing.T) *phase111bAuthServer {
	t.Helper()
	f := &phase111bAuthServer{
		codes: make(map[string]struct {
			state     string
			challenge string
		}),
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

func (f *phase111bAuthServer) BaseURL() string { return f.srv.URL }

// LastIssuedAccessToken returns the most recently minted access token
// (the value the tool body must end up using).
func (f *phase111bAuthServer) LastIssuedAccessToken() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastAccess
}

// TokenExchangeCount returns the number of calls that reached the token
// endpoint, including rejected calls. A replay handled by Harbor's completion
// tombstone must leave this unchanged.
func (f *phase111bAuthServer) TokenExchangeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenExchanges
}

func (f *phase111bAuthServer) discovery(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]string{
		"authorization_endpoint": f.srv.URL + "/authorize",
		"token_endpoint":         f.srv.URL + "/token",
		"registration_endpoint":  f.srv.URL + "/register",
	})
}

func (f *phase111bAuthServer) register(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"client_id": "phase111b-dyn-client"})
}

// authorize simulates the user clicking "Allow": it mints a code and
// 302-redirects the browser to redirect_uri?code=...&state=....
func (f *phase111bAuthServer) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state, chall, redirect := q.Get("state"), q.Get("code_challenge"), q.Get("redirect_uri")
	if state == "" || chall == "" || redirect == "" {
		http.Error(w, "missing state/challenge/redirect_uri", http.StatusBadRequest)
		return
	}
	code := "code-" + state[:8]
	f.mu.Lock()
	f.codes[code] = struct {
		state     string
		challenge string
	}{state: state, challenge: chall}
	f.mu.Unlock()
	u, err := url.Parse(redirect)
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	rq := u.Query()
	rq.Set("code", code)
	rq.Set("state", state)
	u.RawQuery = rq.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (f *phase111bAuthServer) token(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.tokenExchanges++
	f.mu.Unlock()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "parse", http.StatusBadRequest)
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		http.Error(w, "unsupported grant_type", http.StatusBadRequest)
		return
	}
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
	if pkceChallengeImpl(verifier) != rec.challenge {
		http.Error(w, "pkce verification failed", http.StatusBadRequest)
		return
	}
	access := "phase111b-access-" + newAuthCode()
	f.mu.Lock()
	f.lastAccess = access
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  access,
		"refresh_token": "phase111b-refresh-" + newAuthCode(),
		"token_type":    "Bearer",
		"expires_in":    3600,
	})
}
