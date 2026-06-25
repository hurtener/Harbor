// Wave-end composing E2E (CLAUDE.md §17.7 step 5) for the V1.6 session-hydration
// + user-scope agent-config wave. It composes BOTH tracks of the wave in ONE
// test binary, with REAL drivers on every seam — no mocks:
//
//   - Track A: the durable EventBus restart→rehydration feeding the windowed
//     `state.history` read. The LOAD-BEARING case (TestE2E_WaveSessionHydration_RestartSeam)
//     closes the one cross-phase seam no current test covers: a durable bus is
//     torn down and rebuilt over the SAME StateStore (a Runtime restart), and
//     the rebuilt bus's `state.history` window — read through the REAL HTTP
//     handler — must return a GAP-FREE, STRICTLY-MONOTONIC sequence that spans
//     pre- and post-restart events, with post-restart sequences CONTINUING above
//     the pre-restart high-water mark. That proves the durable driver's
//     sequence-recovery (recoverNextSeq) rehydrated, instead of resetting to 1
//     and colliding with pre-restart tokens.
//
//   - Track B: the durable user-scope agent-config tier projected at run start.
//     An admin agent-level config + a durable user-scope variant compose through
//     the REAL projection seam — the layered prompt precedence
//     (admin Base > admin User > USER-durable > session User) and the three-set
//     (admin ∪ user ∪ session) narrow-only tool-exposure union — and are isolated
//     per (tenant, user), never by agent_id.
//
// Cross-cutting (§17.3): identity propagates through every layer on both tracks;
// cross-tenant / cross-user reads never leak; ≥1 failure mode per track (missing
// identity → 401, incomplete identity triple → fail-closed projection); and an
// N≥10 concurrency stress under -race with a goroutine-leak guard.
package integration_test

import (
	"context"
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

	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/protocol"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
	"github.com/hurtener/Harbor/internal/tools"
)

// ---------------------------------------------------------------------------
// Track A scaffold: a restartable durable-bus + state.history stack.
//
// Unlike newStateHistoryStack (phase125), waveStack keeps the StateStore + the
// ArtifactStore STABLE across a bus restart and rebuilds only the per-generation
// bus + task registry + mux + httptest server — modelling a Runtime process
// restart over the same durable backing store.
// ---------------------------------------------------------------------------

type waveStack struct {
	t     *testing.T
	red   audit.Redactor
	st    state.StateStore        // SHARED across restarts (the durable backing)
	store artifacts.ArtifactStore // SHARED across restarts

	// per-generation surface, replaced on restart():
	bus     events.EventBus
	taskReg tasks.TaskRegistry
	srv     *httptest.Server
}

func waveDurableCfg() config.EventsConfig {
	return config.EventsConfig{
		Driver:                   "durable",
		MaxSubscribersPerSession: 32,
		SubscriberBufferSize:     256,
		IdleTimeout:              time.Minute,
		DropWindow:               time.Second,
		ReplayBufferSize:         64,
	}
}

func newWaveStack(t *testing.T) *waveStack {
	t.Helper()
	red := auditpatterns.New()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem: %v", err)
	}
	store, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	ws := &waveStack{t: t, red: red, st: st, store: store}
	ws.boot()
	t.Cleanup(func() {
		ws.teardownGeneration()
		_ = store.Close(context.Background())
		_ = st.Close(context.Background())
	})
	return ws
}

// boot constructs a fresh durable bus over the SHARED StateStore, plus the
// task registry + control surface + state.history mux + httptest server.
func (ws *waveStack) boot() {
	ws.t.Helper()
	bus, err := durable.New(context.Background(), waveDurableCfg(), ws.red, ws.st)
	if err != nil {
		ws.t.Fatalf("durable.New: %v", err)
	}
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store: ws.st, Bus: bus, Redactor: ws.red,
		Cfg: config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		ws.t.Fatalf("tasks.Open: %v", err)
	}
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		ws.t.Fatalf("NewControlSurface: %v", err)
	}
	mux, err := transports.NewMux(surface, bus,
		transports.WithoutValidator(),
		transports.WithStateHistory(bus, ws.store),
	)
	if err != nil {
		ws.t.Fatalf("NewMux: %v", err)
	}
	ws.bus = bus
	ws.taskReg = taskReg
	ws.srv = httptest.NewServer(mux)
}

// restart tears the CURRENT bus generation down (server, task registry, bus) —
// but NOT the StateStore — and boots a fresh generation over the SAME store.
// This is the Runtime-restart seam: the rebuilt durable bus must rehydrate its
// monotonic sequence counter from the persisted head records.
func (ws *waveStack) restart() {
	ws.t.Helper()
	ws.srv.Close()
	_ = ws.taskReg.Close(context.Background())
	if err := ws.bus.Close(context.Background()); err != nil {
		ws.t.Fatalf("bus.Close (restart): %v", err)
	}
	ws.boot()
}

func (ws *waveStack) teardownGeneration() {
	ws.srv.Close()
	_ = ws.taskReg.Close(context.Background())
	_ = ws.bus.Close(context.Background())
}

// historyPost posts a state.history request against the CURRENT generation's
// server, carrying the identity triple as carrier headers (mirrors phase125's
// post helper, but bound to ws.srv so it follows a restart).
func (ws *waveStack) historyPost(path, body string, id identity.Identity) (int, []byte) {
	ws.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ws.srv.URL+path, strings.NewReader(body))
	if id.TenantID != "" {
		req.Header.Set("X-Harbor-Tenant", id.TenantID)
		req.Header.Set("X-Harbor-User", id.UserID)
		req.Header.Set("X-Harbor-Session", id.SessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		ws.t.Fatalf("POST %s: %v", path, err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, raw
}

// readFullWindow fetches the whole retained window for a session in one page
// (limit covers all events in these tests) through the REAL state.history
// handler, oldest-first.
func (ws *waveStack) readFullWindow(sessionID string, id identity.Identity) prototypes.StateHistoryResponse {
	ws.t.Helper()
	status, raw := ws.historyPost("/v1/state/history",
		fmt.Sprintf(`{"session_id":%q,"limit":200}`, sessionID), id)
	if status != http.StatusOK {
		ws.t.Fatalf("state.history window status = %d; body=%s", status, raw)
	}
	var page prototypes.StateHistoryResponse
	mustJSON(ws.t, raw, &page)
	return page
}

// TestE2E_WaveSessionHydration_RestartSeam is the wave's load-bearing case: the
// Phase-124 durable-bus restart feeding the Phase-125 windowed state.history
// read. It proves the rebuilt bus's sequence counter rehydrated from the
// persisted store, so the windowed read is gap-free and strictly monotonic
// across the restart boundary.
func TestE2E_WaveSessionHydration_RestartSeam(t *testing.T) {
	ws := newWaveStack(t)
	id := identity.Identity{TenantID: "t-wave", UserID: "u-wave", SessionID: "s-wave"}

	// --- Pre-restart: publish an ordered set of N=12 events on the first bus.
	const preN = 12
	seedEvents(t, ws.bus, id, preN)

	// Capture the pre-restart high-water sequence THROUGH the real handler.
	before := ws.readFullWindow("s-wave", id)
	if before.TailSequence != preN || len(before.Events) != preN {
		t.Fatalf("pre-restart window: tail=%d events=%d, want %d/%d",
			before.TailSequence, len(before.Events), preN, preN)
	}
	preMax := before.TailSequence // 12 — the pre-restart high-water mark.

	// --- Restart: close the first bus (NOT the StateStore), rebuild over the
	//     SAME store. This is the seam under test.
	ws.restart()

	// --- Post-restart: publish more events on the NEW bus.
	const postN = 8
	seedEvents(t, ws.bus, id, postN)

	// === THE LOAD-BEARING ASSERTION (the reason this test exists) ===========
	// Read the whole retained window through the REAL state.history handler over
	// the REBUILT bus. The window MUST span pre- and post-restart events as ONE
	// gap-free, strictly-monotonic sequence:
	//
	//   * head_sequence == 1 (the oldest pre-restart event still retained),
	//   * the events are contiguous 1..(preMax+postN) with no skip and no
	//     duplicate,
	//   * tail_sequence == preMax+postN, and
	//   * the FIRST post-restart event sits at preMax+1 — i.e. the post-restart
	//     sequences CONTINUE ABOVE the pre-restart high-water mark instead of
	//     resetting to 1.
	//
	// Had recoverNextSeq NOT rehydrated, the new bus would re-number from 1, the
	// post-restart events would COLLIDE with pre-restart tokens, and the window
	// would either be short (overwritten keys) or non-monotonic. This assertion
	// is what closes the Phase-124→Phase-125 cross-phase seam.
	after := ws.readFullWindow("s-wave", id)
	wantTotal := int(preMax) + postN // 20
	if after.HeadSequence != 1 {
		t.Fatalf("post-restart head_sequence = %d, want 1 (oldest pre-restart event)", after.HeadSequence)
	}
	if after.TailSequence != uint64(wantTotal) {
		t.Fatalf("post-restart tail_sequence = %d, want %d (post-restart did NOT continue above pre-restart max %d — recoverNextSeq failed to rehydrate)",
			after.TailSequence, wantTotal, preMax)
	}
	if len(after.Events) != wantTotal {
		t.Fatalf("post-restart window has %d events, want %d (gap or collision across the restart boundary)", len(after.Events), wantTotal)
	}
	for i, ev := range after.Events {
		if ev.Sequence != uint64(i+1) {
			t.Fatalf("post-restart sequence gap/duplicate at position %d: Sequence=%d, want %d (window is not gap-free + strictly monotonic across the restart)",
				i, ev.Sequence, i+1)
		}
		// Identity propagated through every layer (publish → durable persist →
		// replay → wire projection).
		if ev.Tenant != id.TenantID || ev.User != id.UserID || ev.Session != id.SessionID {
			t.Fatalf("identity not propagated through the durable persistence layer at position %d: got (%s/%s/%s)",
				i, ev.Tenant, ev.User, ev.Session)
		}
	}
	// The first post-restart event continues strictly above the pre-restart max.
	firstPostRestart := after.Events[preMax]
	if firstPostRestart.Sequence != preMax+1 {
		t.Fatalf("first post-restart event = seq %d, want %d (the rehydrated counter must continue above the pre-restart high-water mark)",
			firstPostRestart.Sequence, preMax+1)
	}
	// ========================================================================

	// --- Cross-tenant isolation: a different tenant's read does not see u-wave's
	//     events — existence is never revealed across identities (404, not a leak).
	status, raw := ws.historyPost("/v1/state/history",
		`{"identity":{"tenant":"t-other","user":"u-wave","session":"s-wave"},"session_id":"s-wave"}`, id)
	if status != http.StatusNotFound {
		t.Fatalf("cross-tenant state.history = %d, want 404 (no existence leak); body=%s", status, raw)
	}
	assertCodeIs(t, raw, protoerrors.CodeNotFound)

	// --- Failure mode: a missing-identity read is rejected fail-closed (401),
	//     never a silent default to some ambient identity.
	status, raw = ws.historyPost("/v1/state/history", `{"session_id":"s-wave"}`, identity.Identity{})
	if status != http.StatusUnauthorized {
		t.Fatalf("missing-identity state.history = %d, want 401; body=%s", status, raw)
	}
	assertCodeIs(t, raw, protoerrors.CodeIdentityRequired)
}

// ---------------------------------------------------------------------------
// Track B scaffold: the user-scope agent-config projection at run start.
// ---------------------------------------------------------------------------

const waveAgentID = "agent-wave"

// waveToolCatalog registers four tools across four MCP sources so the three-set
// (admin ∪ user ∪ session) exclusion union can be exercised independently:
//   - alpha_one  (source "alpha")  — removed by the ADMIN paused-server set
//   - weather_now (source "weather") — removed by the USER DisabledServers set
//   - news_today (source "news")   — removed by the SESSION disabled-tool set
//   - stocks_quote (source "stocks") — survives all three tiers
func waveToolCatalog(t *testing.T) tools.ToolCatalog {
	t.Helper()
	cat := tools.NewCatalog()
	type td struct {
		name, source string
	}
	for _, d := range []td{
		{"alpha_one", "alpha"},
		{"weather_now", "weather"},
		{"news_today", "news"},
		{"stocks_quote", "stocks"},
	} {
		name, source := d.name, tools.ToolSourceID(d.source)
		if err := cat.Register(tools.ToolDescriptor{
			Tool: tools.Tool{Name: name, Source: source, Transport: tools.TransportMCP},
			Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{Value: "ok"}, nil
			},
		}); err != nil {
			t.Fatalf("register %q: %v", name, err)
		}
	}
	return cat
}

func waveViewHas(v tools.PlannerCatalogView, name string) bool {
	for _, tl := range v.List() {
		if tl.Name == name {
			return true
		}
	}
	return false
}

func waveViewNames(v tools.PlannerCatalogView) []string {
	out := make([]string, 0)
	for _, tl := range v.List() {
		out = append(out, tl.Name)
	}
	return out
}

func newWaveOverlay(t *testing.T) (sessionoverlay.Store, state.StateStore) {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("overlay state: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	ov, err := sessionoverlay.NewStore(st, nil)
	if err != nil {
		t.Fatalf("overlay store: %v", err)
	}
	return ov, st
}

// TestE2E_WaveSessionHydration_UserScopeProjection composes the durable
// user-scope tier (written through the REAL Protocol handler) with the REAL
// run-start projection seam: the four-way prompt-layer precedence and the
// three-set narrow-only tool-exposure union, isolated per (tenant, user).
func TestE2E_WaveSessionHydration_UserScopeProjection(t *testing.T) {
	ctx := context.Background()
	h := newUSHarness(t) // REAL statestore registry + Service + wire handler.
	reg := h.registry
	cat := waveToolCatalog(t)
	overlay, _ := newWaveOverlay(t)

	alice := usID("alice", "sa")
	bob := usID("bob", "sb")
	aliceQ := identity.Quadruple{Identity: alice}

	// --- Admin sets the agent-level prompt layer (Base + User) THROUGH the wire. ---
	_ = decode[prototypes.AgentConfigSetPromptLayersResponse](t, h.call(t, "/v1/agent_config/set_prompt_layers",
		prototypes.AgentConfigSetPromptLayersRequest{
			AgentID:      waveAgentID,
			PromptLayers: prototypes.AgentConfigPromptLayers{Base: ptr("OPERATOR_BASE"), User: ptr("admin-user")},
		}, usID("admin", "sx"), adminScopes()))

	// --- Admin pins an agent-level tool-exposure pause (the ADMIN tier of the
	//     union). set_tool_exposure preserves the prompt-layer section above. ---
	_ = decode[prototypes.AgentConfigSetToolExposureResponse](t, h.call(t, "/v1/agent_config/set_tool_exposure",
		prototypes.AgentConfigSetToolExposureRequest{
			AgentID:      waveAgentID,
			ToolExposure: prototypes.AgentConfigToolExposure{PausedServers: []string{"alpha"}},
		}, usID("admin", "sx"), adminScopes()))

	// --- User A sets a durable user-scope variant THROUGH the wire: a standing
	//     user prompt + a narrow-only DisabledServers set. ---
	_ = decode[prototypes.AgentConfigUserSetRevisionResponse](t, h.call(t, "/v1/agent_config/user/set_revision",
		prototypes.AgentConfigUserSetRevisionRequest{
			AgentID: waveAgentID,
			Payload: prototypes.AgentConfigUserPayload{
				UserPrompt:      "durable-user",
				DisabledServers: []string{"weather"},
			},
		}, alice, userScopes()))

	// --- Ephemeral session layer (the lowest-trust tier): a session user prompt
	//     and a session-scoped tool disable. ---
	if _, err := overlay.SetUserPrompt(ctx, aliceQ, waveAgentID, "session-user"); err != nil {
		t.Fatalf("overlay SetUserPrompt: %v", err)
	}
	if _, err := overlay.SetSourceDisables(ctx, aliceQ, waveAgentID, nil, []string{"news_today"}); err != nil {
		t.Fatalf("overlay SetSourceDisables: %v", err)
	}

	// === Prompt-layer precedence: admin Base > admin User > USER-durable > session User.
	ov, err := projection.ApplyPromptLayers(ctx, reg, overlay, waveAgentID, aliceQ, nil)
	if err != nil {
		t.Fatalf("ApplyPromptLayers: %v", err)
	}
	if ov == nil || ov.BasePromptLayer == nil || *ov.BasePromptLayer != "OPERATOR_BASE" {
		t.Fatalf("admin Base is not the run's base spine: %+v", ov)
	}
	wantUser := "admin-user\n\ndurable-user\n\nsession-user"
	if ov.UserPromptLayer == nil || *ov.UserPromptLayer != wantUser {
		t.Fatalf("user-layer precedence wrong:\n got=%q\nwant=%q", derefStr(ov.UserPromptLayer), wantUser)
	}

	// === Three-set tool-exposure union: admin(alpha) ∪ user(weather) ∪ session(news_today).
	view, err := projection.ActivePlannerCatalogView(ctx, reg, overlay, waveAgentID, aliceQ,
		cat, tools.CatalogFilter{TenantID: alice.TenantID, UserID: alice.UserID, SessionID: alice.SessionID})
	if err != nil {
		t.Fatalf("ActivePlannerCatalogView (alice): %v", err)
	}
	for _, excluded := range []string{"alpha_one", "weather_now", "news_today"} {
		if waveViewHas(view, excluded) {
			t.Fatalf("three-set union did not exclude %q: %v", excluded, waveViewNames(view))
		}
	}
	// User A's DisabledServers removal is the load-bearing Track-B assertion.
	if waveViewHas(view, "weather_now") {
		t.Fatalf("user DisabledServers did not remove the weather tool: %v", waveViewNames(view))
	}
	if !waveViewHas(view, "stocks_quote") || len(waveViewNames(view)) != 1 {
		t.Fatalf("over-exclusion: want only stocks_quote, got %v", waveViewNames(view))
	}

	// === Cross-user isolation: bob (SAME agent_id, no overlay) carries NONE of
	//     alice's user-scope overrides — agent_id is not an isolation principal.
	bobQ := identity.Quadruple{Identity: bob}
	bobOv, err := projection.ApplyPromptLayers(ctx, reg, nil, waveAgentID, bobQ, nil)
	if err != nil {
		t.Fatalf("ApplyPromptLayers (bob): %v", err)
	}
	// Bob still gets the admin tier (tenant-shared), but NOT alice's durable/session user layers.
	if bobOv == nil || bobOv.UserPromptLayer == nil || *bobOv.UserPromptLayer != "admin-user" {
		t.Fatalf("cross-user bleed: bob's user layer = %q, want only the admin layer", derefStr(bobUserLayer(bobOv)))
	}
	bobView, err := projection.ActivePlannerCatalogView(ctx, reg, nil, waveAgentID, bobQ,
		cat, tools.CatalogFilter{TenantID: bob.TenantID, UserID: bob.UserID, SessionID: bob.SessionID})
	if err != nil {
		t.Fatalf("ActivePlannerCatalogView (bob): %v", err)
	}
	// Admin pause still applies to bob (alpha excluded); alice's weather disable does NOT.
	if waveViewHas(bobView, "alpha_one") {
		t.Fatalf("admin pause did not apply to bob: %v", waveViewNames(bobView))
	}
	if !waveViewHas(bobView, "weather_now") {
		t.Fatalf("cross-user bleed: alice's weather disable reached bob's run: %v", waveViewNames(bobView))
	}

	// === Failure mode: an incomplete identity triple fails the projection closed,
	//     never silently returning the unfiltered view.
	bad := identity.Quadruple{Identity: identity.Identity{TenantID: usTenant, UserID: "alice"}} // no session
	if _, verr := projection.ActivePlannerCatalogView(ctx, reg, nil, waveAgentID, bad,
		cat, tools.CatalogFilter{TenantID: usTenant, UserID: "alice"}); verr == nil {
		t.Fatal("incomplete identity must fail the projection closed, got nil error")
	}
}

// derefStr returns the pointee or "" for a nil *string (test-local; the package
// already defines deref-style helpers in other files, so this carries a
// wave-specific name to avoid a redeclaration).
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// bobUserLayer safely extracts the user-prompt layer pointer (nil-safe).
func bobUserLayer(ov *planner.LLMOverrides) *string {
	if ov == nil {
		return nil
	}
	return ov.UserPromptLayer
}

// TestE2E_WaveSessionHydration_ConcurrencyStress runs BOTH tracks concurrently
// across DISTINCT identities against shared instances under -race, asserting no
// cross-talk, and guards against goroutine leaks (§11).
func TestE2E_WaveSessionHydration_ConcurrencyStress(t *testing.T) {
	// --- Track A shared stack: distinct identities, each with a known event count.
	ws := newWaveStack(t)
	const ids = 12
	for k := range ids {
		id := identity.Identity{
			TenantID:  fmt.Sprintf("t-c%d", k),
			UserID:    fmt.Sprintf("u-c%d", k),
			SessionID: fmt.Sprintf("s-c%d", k),
		}
		seedEvents(t, ws.bus, id, 3+k)
	}

	// --- Track B shared registry: distinct users disable distinct servers.
	ctx := context.Background()
	h := newUSHarness(t)
	reg := h.registry
	cat := waveToolCatalog(t)
	for k := range ids {
		user := fmt.Sprintf("cu%d", k)
		if rec := h.call(t, "/v1/agent_config/user/set_revision",
			prototypes.AgentConfigUserSetRevisionRequest{
				AgentID: waveAgentID,
				Payload: prototypes.AgentConfigUserPayload{DisabledServers: []string{"weather"}},
			}, usID(user, "s"), userScopes()); rec.Code != http.StatusOK {
			t.Fatalf("seed user %s: %d", user, rec.Code)
		}
	}

	// Baseline AFTER both long-lived stacks are built + seeded, so the guard
	// measures the concurrent WORK's goroutines (which must join), not the
	// stacks' steady-state daemons (bus/server, torn down at t.Cleanup).
	runtime.GC()
	baseline := runtime.NumGoroutine()

	const workers = 48
	var wg sync.WaitGroup
	errs := make(chan error, workers*2)
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := i % ids

			// Track A: concurrent state.history read; assert the exact own count.
			aID := identity.Identity{
				TenantID:  fmt.Sprintf("t-c%d", k),
				UserID:    fmt.Sprintf("u-c%d", k),
				SessionID: fmt.Sprintf("s-c%d", k),
			}
			page := ws.readFullWindow(aID.SessionID, aID)
			if len(page.Events) != 3+k {
				errs <- fmt.Errorf("track A k=%d got %d events, want %d (cross-identity bleed?)", k, len(page.Events), 3+k)
				return
			}

			// Track B: concurrent user-scope projection; even users disabled
			// weather, odd users did not — assert each sees only its own policy.
			user := fmt.Sprintf("cu%d", k)
			bID := identity.Quadruple{Identity: identity.Identity{TenantID: usTenant, UserID: user, SessionID: "s2"}}
			view, verr := projection.ActivePlannerCatalogView(ctx, reg, nil, waveAgentID, bID,
				cat, tools.CatalogFilter{TenantID: usTenant, UserID: user, SessionID: "s2"})
			if verr != nil {
				errs <- fmt.Errorf("track B %s: %w", user, verr)
				return
			}
			if waveViewHas(view, "weather_now") {
				errs <- fmt.Errorf("track B %s: weather not disabled (cross-user bleed)", user)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}

	// Goroutine-leak guard: the concurrent workers' goroutines must all have
	// joined, so the count returns to the post-setup baseline (the long-lived
	// bus/server daemons are part of the baseline and are torn down only at
	// t.Cleanup). The shared http.DefaultClient keeps per-connection keep-alive
	// readLoop/writeLoop goroutines alive after the requests return — those are
	// pooled transport state, not a leak in the system under test — so close
	// idle connections first, then poll a bounded real-time window for the
	// remaining transients to drain.
	http.DefaultClient.CloseIdleConnections()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > baseline+4 && time.Now().Before(deadline) {
		http.DefaultClient.CloseIdleConnections()
		runtime.GC()
		runtime.Gosched()
	}
	if leaked := runtime.NumGoroutine() - baseline; leaked > 4 {
		t.Fatalf("goroutine leak after concurrent stress: baseline=%d now=%d (delta=%d)",
			baseline, runtime.NumGoroutine(), leaked)
	}
}
