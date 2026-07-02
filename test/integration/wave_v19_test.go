// Wave-end composing E2E (CLAUDE.md §17.5 / §17.7 step 5) for the
// v1.9.0 "typed-output-and-brokered-credentials" wave — the band that
// ships run-level structured output (WithOutputSchema / D-272), the
// typed embed binding (RunTyped[T] / D-273), and the pull-based
// tokenexchange OAuth driver (D-271), with the D-274 envelope-counting
// + fail-loud-erasure integrity fixes riding alongside.
//
// This suite is IN ADDITION to each phase's own gate (the 142
// tokenexchange test, the 143 output-schema test, the 144 RunTyped
// test). It proves the COMBINED wave surface is alive together against
// ONE assembled production Stack per leg, with REAL drivers on every
// seam (CLAUDE.md §17.3 #1): the real assemble.Assemble stack (real
// inmem state / events / artifacts / tasks / memory drivers, the real
// react planner, the real catalog Builder applying WrapWithOAuth, the
// real dispatch executor + parallel executor), a real
// tokenexchange.OAuthProvider pointed at an httptest RFC-8693 broker
// (broker-side wire assertions per §17.8), and a scripted LLM DRIVER
// registered through the SAME registry path bifrost uses (so the full
// safety→corrections→downgrade→retry→governance chain composes for
// real). The only deliberate test seam is that scripted LLM driver
// (the mock-LLM's gated cousin) and the httptest broker fixture.
//
// The legs:
//
//  1. Composed happy path — a schema-constrained RunOnce whose planner
//     emits two native tool calls (→ react CallParallel through the
//     real catalog/executor), then the tokenexchange-wrapped tool
//     (broker grant + broker-side 8693 assertions), then a conforming
//     terminal answer. Asserts per-branch D-274 counting
//     (ToolCallsSeen == 3), the EXACT AnswerPayload bytes, exactly one
//     broker exchange, and a token-free tool.credential_exchanged.
//  2. Park/resume × output schema — a run parks on the unified pause
//     primitive while the broker withholds consent, the broker flips
//     to granting, the run resumes and its terminal answer still
//     validates onto AnswerPayload.
//  3. RunTyped nested-T under OutputMode: Tools — RunTyped[Outer] with
//     a nested struct field, a Tools-envelope (respond_with) scripted
//     response, proving derived-schema (additionalProperties:false at
//     EVERY level) × the respond_with unwrap path.
//  4. Failure modes — a broker 500 fails loud with auth.ErrExchangeFailed
//     and NO interactive fallback; schema exhaustion is
//     planner.ErrOutputInvalid, never silent text.
//  5. Identity propagation — distinct triples each drive a brokered
//     tool call (subject asserted broker-side) plus a corrective retry
//     whose llm.retry_with_feedback is observed on a triple-scoped
//     subscription.
//  6. Concurrency — N mixed RunOnce/RunTyped runs against ONE shared
//     Stack with distinct triples + goal-encoded bleed detection,
//     per-identity broker exchange counts, and a goroutine baseline
//     after Close (CLAUDE.md §17.3 stress requirement).
//
// LEG 2 DESIGN NOTE: the assembled ReAct planner does NOT convert a
// tool's *auth.ErrAuthRequired into a run-level pause — there is no
// tool-auth→run-pause bridge in the run loop (react itself never emits
// RequestPause). So leg 2 drives the RUN-level park with a deterministic
// PlannerOverride (the phase111b_oauth_completion_test.go:24-43 pattern),
// while the tokenexchange broker's consent→grant flip gates the RETRY
// tool dispatch — the schema-validated Finish payload is what proves
// "park/resume × schema" end to end.
//
// Runs under -race. No time.Sleep as a sync primitive — bounded
// eventually-style waits on real channels only.
//
// This file is `package integration_test` (matching the 143/144
// harnesses) so it reuses their in-package helpers (writeDevConfig,
// phase143Schema).
package integration_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	// Production driver aggregator — the §4.4 blank-import home the real
	// inmem state / events / artifacts / tasks / memory drivers register
	// through, so the assembled Stack resolves them.
	_ "github.com/hurtener/Harbor/internal/drivers/prod"

	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	stateInmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/auth/drivers/tokenexchange"
	"github.com/hurtener/Harbor/internal/tools/schema"
)

// Harness constants. The broker credentials are documented dummy
// fixtures (CLAUDE.md §7 rule 2) — never real values.
const (
	waveV19Provider     = "wave-v19-broker"
	waveV19Audience     = "https://graph.wave-v19.test"
	waveV19BrokerClient = "dummy-wave-v19-broker-client-not-a-secret"
	waveV19BrokerSecret = "dummy-wave-v19-broker-secret-not-a-secret"
	waveV19BrokeredTool = "send_report"
	waveV19ToolAlpha    = "lookup_alpha"
	waveV19ToolBeta     = "lookup_beta"

	// The RFC-8693 subject-token-type the tokenexchange driver presents
	// (a base64url JSON identity triple — the driver's Harbor-native
	// subject encoding). A driver wired to the wrong field fails the
	// broker-side assertion below.
	waveV19SubjectTokenType = "urn:harbor:oauth:token-type:identity-triple"
	waveV19GrantType        = "urn:ietf:params:oauth:grant-type:token-exchange"
)

// waveV19MarkerSchema is the concurrency leg's hand-authored run schema;
// waveV19Marker is the RunTyped[T] target whose DERIVED schema matches it.
const waveV19MarkerSchema = `{
	"type": "object",
	"required": ["marker"],
	"properties": {
		"marker": {"type": "string"},
		"ok": {"type": "boolean"}
	},
	"additionalProperties": false
}`

type waveV19Marker struct {
	Marker string `json:"marker"`
	OK     bool   `json:"ok,omitempty"`
}

// waveV19Inner / waveV19Outer are leg 3's nested RunTyped target: Outer
// carries a nested struct FIELD, so the derived schema must set
// additionalProperties:false at BOTH levels.
type waveV19Inner struct {
	Score float64 `json:"score"`
	Label string  `json:"label"`
}

type waveV19Outer struct {
	Title string       `json:"title"`
	Inner waveV19Inner `json:"inner"`
}

// ---------------------------------------------------------------------------
// RFC-8693 broker fixture (§17.8 spec-derived)
// ---------------------------------------------------------------------------

type waveV19Broker struct {
	server *httptest.Server

	mu       sync.Mutex
	calls    int
	subjects map[string]int // "<tenant>/<user>" → exchange count
	posture  atomic.Value   // "grant" | "consent" | "error500"
}

func newWaveV19Broker(t *testing.T) *waveV19Broker {
	t.Helper()
	b := &waveV19Broker{subjects: make(map[string]int)}
	b.posture.Store("grant")
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", b.handle)
	b.server = httptest.NewServer(mux)
	t.Cleanup(b.server.Close)
	return b
}

func (b *waveV19Broker) tokenURL() string    { return b.server.URL + "/oauth2/token" }
func (b *waveV19Broker) setPosture(p string) { b.posture.Store(p) }

func (b *waveV19Broker) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func (b *waveV19Broker) subjectCount(key string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.subjects[key]
}

func (b *waveV19Broker) handle(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	// §17.8: broker-side wire assertions. A driver wired to the wrong
	// 8693 field fails HERE, not in a self-consistent hand fixture.
	if r.Form.Get("grant_type") != waveV19GrantType {
		http.Error(w, "grant_type", http.StatusBadRequest)
		return
	}
	if r.Form.Get("subject_token_type") != waveV19SubjectTokenType {
		http.Error(w, "subject_token_type", http.StatusBadRequest)
		return
	}
	if r.Form.Get("audience") != waveV19Audience {
		http.Error(w, "audience", http.StatusBadRequest)
		return
	}
	if r.Form.Get("client_id") != waveV19BrokerClient || r.Form.Get("client_secret") != waveV19BrokerSecret {
		http.Error(w, "client auth", http.StatusUnauthorized)
		return
	}

	// Decode the subject triple so the issued token + per-subject count
	// carry the identity — a cross-tenant/user bleed is assertable here.
	var tenant, user string
	if raw, err := base64.RawURLEncoding.DecodeString(r.Form.Get("subject_token")); err == nil {
		var st struct {
			TenantID  string `json:"tenant_id"`
			UserID    string `json:"user_id"`
			SessionID string `json:"session_id"`
		}
		_ = json.Unmarshal(raw, &st)
		tenant, user = st.TenantID, st.UserID
	}

	b.mu.Lock()
	b.calls++
	b.subjects[tenant+"/"+user]++
	b.mu.Unlock()

	switch b.posture.Load().(string) {
	case "error500":
		http.Error(w, "broker down", http.StatusInternalServerError)
	case "consent":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":       "consent_required",
			"consent_url": "https://broker.wave-v19.test/consent",
		})
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "brokered-" + tenant + "-" + user,
			"token_type":   "Bearer",
			"expires_in":   3600,
			"scope":        r.Form.Get("scope"),
		})
	}
}

// ---------------------------------------------------------------------------
// TokenStore spy — brokered tokens are NEVER persisted (D-271)
// ---------------------------------------------------------------------------

type waveV19SpyStore struct {
	t     *testing.T
	inner auth.TokenStore
}

func (s *waveV19SpyStore) Get(ctx context.Context, scope auth.BindingScope, subjectID string, source tools.ToolSourceID) (auth.Token, bool, error) {
	return s.inner.Get(ctx, scope, subjectID, source)
}

func (s *waveV19SpyStore) Put(_ context.Context, _ auth.Token) error {
	s.t.Fatalf("TokenStore.Put called — brokered tokens must NEVER be persisted (D-271)")
	return nil
}

func (s *waveV19SpyStore) Delete(ctx context.Context, scope auth.BindingScope, subjectID string, source tools.ToolSourceID) error {
	return s.inner.Delete(ctx, scope, subjectID, source)
}

// ---------------------------------------------------------------------------
// Env: broker + tokenexchange provider (with its own real bus/coordinator)
// ---------------------------------------------------------------------------

// waveV19Env holds the broker + a pre-built tokenexchange provider. The
// provider owns its OWN real inmem bus + coordinator (assemble.Options
// has no injectable Bus/Coordinator seam), so the credential_exchanged /
// auth_required events fire on env.provBus — the test subscribes there;
// run-level events (pause, retry) ride the assembled stack.Bus.
type waveV19Env struct {
	broker   *waveV19Broker
	provider auth.OAuthProvider
	provBus  events.EventBus
	coord    pauseresume.Coordinator
}

func newWaveV19Env(t *testing.T) *waveV19Env {
	t.Helper()
	broker := newWaveV19Broker(t)
	red := patternsAudit.New()

	provBus, err := eventsInmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 32,
		SubscriberBufferSize:     128,
		IdleTimeout:              2 * time.Second,
		DropWindow:               50 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("eventsInmem.New: %v", err)
	}
	t.Cleanup(func() { _ = provBus.Close(context.Background()) })
	coord := pauseresume.New()

	// Real StateStore-backed TokenStore behind the spy (its Put must
	// never fire — the broker stays the single source of truth).
	raw, err := stateInmem.New(config.StateConfig{})
	if err != nil {
		t.Fatalf("state inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close(context.Background()) })
	kek := make([]byte, auth.KEKSizeBytes)
	for i := range kek {
		kek[i] = byte(i*7 + 3)
	}
	sealer, err := auth.NewAESGCMSealer(kek)
	if err != nil {
		t.Fatalf("NewAESGCMSealer: %v", err)
	}
	innerStore, err := auth.NewTokenStore(raw, sealer)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}
	store := &waveV19SpyStore{t: t, inner: innerStore}

	prov, err := tokenexchange.New(auth.ProviderConfig{
		Name:         waveV19Provider,
		ClientID:     waveV19BrokerClient,
		ClientSecret: waveV19BrokerSecret,
		Scopes:       []string{"Report.Send"},
		TokenURL:     broker.tokenURL(),
		Extra:        map[string]string{"audience": waveV19Audience},
	}, auth.FactoryDeps{
		Store: store, Bus: provBus, Redactor: red, Coordinator: coord,
		// DisableKeepAlives so the broker's httptest connections don't
		// linger as idle keep-alive goroutines past the run — the
		// concurrency leg's goroutine-baseline assertion is about the
		// assembled Stack joining its own goroutines, not HTTP pooling.
		HTTPClient: &http.Client{
			Timeout:   5 * time.Second,
			Transport: &http.Transport{DisableKeepAlives: true},
		},
	})
	if err != nil {
		t.Fatalf("tokenexchange.New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })

	return &waveV19Env{broker: broker, provider: prov, provBus: provBus, coord: coord}
}

// assemble builds a runnable production Stack: the scripted LLM driver
// (via LLMSnapshot, the SAME registry path bifrost uses) with the given
// OutputMode, the injected tokenexchange provider (name-match beats the
// cfg decl at assemble.go:783-791, so the cfg provider is NEVER
// constructed and the KEK env is never read), the base inproc tools
// pre-registered, and an optional PlannerOverride.
func (env *waveV19Env) assemble(t *testing.T, driverName string, mode llm.OutputMode, override planner.Planner) *assemble.Stack {
	t.Helper()
	cfg, err := config.Load(context.Background(), writeDevConfig(t))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.LLM.Model = "test/model"
	// Declare the tokenexchange provider so config cross-validation +
	// the entry's oauth.provider reference resolve. The env var NAMES are
	// only required to be non-empty for validation; the injected provider
	// (below) means this decl is dropped before construction, so the env
	// is never read.
	cfg.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{{
		Name:            waveV19Provider,
		Driver:          "tokenexchange",
		ClientIDEnv:     "WAVE_V19_BROKER_CLIENT",
		ClientSecretEnv: "WAVE_V19_BROKER_SECRET",
		TokenURL:        env.broker.tokenURL(),
		Extra:           map[string]string{"audience": waveV19Audience},
	}}
	cfg.Tools.OAuthTokenKEKEnv = "WAVE_V19_OAUTH_KEK"
	cfg.Tools.Entries = []config.ToolEntryConfig{{
		Name:  waveV19BrokeredTool,
		OAuth: &config.ToolOAuthConfig{Provider: waveV19Provider, BindingScope: "user"},
	}}

	opts := assemble.Options{
		LLMSnapshot: &llm.ConfigSnapshot{
			Driver:               driverName,
			Model:                "test/model",
			ContextWindowReserve: cfg.LLM.ContextWindowReserve,
			HeavyOutputThreshold: cfg.Artifacts.HeavyOutputThresholdBytes,
			ModelProfiles: map[string]llm.ModelProfile{
				"test/model": {
					ContextWindowTokens: 100000,
					TokenEstimator:      "chars_div_4",
					OutputMode:          mode,
					MaxRetries:          1,
				},
			},
		},
		OAuthProviders:   map[string]auth.OAuthProvider{waveV19Provider: env.provider},
		PreRegisterTools: waveV19BaseTools(),
		PlannerOverride:  override,
	}
	stack, err := assemble.Assemble(context.Background(), cfg, opts)
	if err != nil {
		if stack != nil {
			_ = stack.Close(context.Background())
		}
		t.Fatalf("assemble.Assemble: %v", err)
	}
	if stack.RunLoop == nil || stack.Planner == nil || stack.Catalog == nil {
		t.Fatalf("stack must have RunLoop + Planner + Catalog (RunLoop=%v Planner=%v Catalog=%v)",
			stack.RunLoop, stack.Planner, stack.Catalog)
	}
	return stack
}

// waveV19BaseTools returns the inproc descriptors the catalog Builder
// wraps: two plain tools (parallel-dispatch fodder) and the brokered
// tool (Source-keyed so the tokenexchange pre-check binds).
func waveV19BaseTools() []tools.ToolDescriptor {
	echo := func(name string) tools.ToolDescriptor {
		return tools.ToolDescriptor{
			Tool: tools.Tool{Name: name, Description: name, Loading: tools.LoadingAlways},
			Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{Value: map[string]any{"tool": name, "ok": true}}, nil
			},
		}
	}
	brokered := tools.ToolDescriptor{
		Tool: tools.Tool{
			Name:        waveV19BrokeredTool,
			Description: "send a report via the brokered API",
			Source:      tools.ToolSourceID(waveV19Provider),
			Loading:     tools.LoadingAlways,
		},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: map[string]any{"tool": waveV19BrokeredTool, "sent": true}}, nil
		},
	}
	return []tools.ToolDescriptor{echo(waveV19ToolAlpha), echo(waveV19ToolBeta), brokered}
}

// ---------------------------------------------------------------------------
// Scripted LLM driver (registered through the real llm registry)
// ---------------------------------------------------------------------------

type waveV19Driver struct {
	respond   func(n int, req llm.CompleteRequest) llm.CompleteResponse
	callCount atomic.Int64
}

func (d *waveV19Driver) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	n := int(d.callCount.Add(1))
	resp := d.respond(n, req)
	if req.Stream && req.OnContent != nil && resp.Content != "" {
		req.OnContent(resp.Content, false)
		req.OnContent("", true)
	}
	return resp, nil
}

func (d *waveV19Driver) Close(_ context.Context) error { return nil }

var waveV19DriverSeq atomic.Int64

func registerWaveV19Driver(respond func(n int, req llm.CompleteRequest) llm.CompleteResponse) (string, *waveV19Driver) {
	drv := &waveV19Driver{respond: respond}
	name := fmt.Sprintf("wave-v19-scripted-%d", waveV19DriverSeq.Add(1))
	llm.Register(name, func(_ llm.ConfigSnapshot, _ llm.Deps) (llm.Driver, error) {
		return drv, nil
	})
	return name, drv
}

func waveV19ToolCallResponse(id, name, args string) llm.CompleteResponse {
	return llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{
		{ID: id, Name: name, Args: json.RawMessage(args)},
	}}
}

func waveV19HasToolResult(req llm.CompleteRequest) bool {
	for _, m := range req.Messages {
		if m.Role == llm.RoleTool {
			return true
		}
	}
	return false
}

func waveV19AllText(req llm.CompleteRequest) string {
	var b strings.Builder
	for _, m := range req.Messages {
		if m.Content.Text != nil {
			b.WriteString(*m.Content.Text)
		}
		for _, p := range m.Content.Parts {
			if p.Type == llm.PartText {
				b.WriteString(p.Text)
			}
		}
	}
	return b.String()
}

func waveV19MarkerFor(i int) string { return fmt.Sprintf("WAVEV19_MARK_%d_END", i) }

func waveV19ExtractMarker(text string) string {
	const pre = "WAVEV19_MARK_"
	const suf = "_END"
	i := strings.Index(text, pre)
	if i < 0 {
		return ""
	}
	rest := text[i:]
	j := strings.Index(rest, suf)
	if j < 0 {
		return ""
	}
	return rest[:j+len(suf)]
}

// ---------------------------------------------------------------------------
// Event helpers
// ---------------------------------------------------------------------------

func waveV19Subscribe(t *testing.T, bus events.EventBus, id identity.Identity, types ...events.EventType) <-chan events.Event {
	t.Helper()
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID, Types: types,
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)
	return sub.Events()
}

func waveV19WaitEvent(t *testing.T, ch <-chan events.Event, want events.EventType) events.Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("subscription closed waiting for %q", want)
			}
			if ev.Type == want {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q", want)
		}
	}
}

func waveV19ID(user, session string) identity.Identity {
	return identity.Identity{TenantID: "wave-v19", UserID: user, SessionID: session}
}

// ---------------------------------------------------------------------------
// Leg 1 — composed happy path
// ---------------------------------------------------------------------------

func TestE2E_WaveV19_ComposedHappyPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newWaveV19Env(t)

	const wantPayload = `{"sentiment":"positive","confidence":0.9}`
	driver, _ := registerWaveV19Driver(func(n int, _ llm.CompleteRequest) llm.CompleteResponse {
		switch n {
		case 1:
			// Two native tool calls in one response → react CallParallel.
			return llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{
				{ID: "call_a", Name: waveV19ToolAlpha, Args: json.RawMessage(`{}`)},
				{ID: "call_b", Name: waveV19ToolBeta, Args: json.RawMessage(`{}`)},
			}}
		case 2:
			// The tokenexchange-wrapped tool → react CallTool.
			return waveV19ToolCallResponse("call_r", waveV19BrokeredTool, `{}`)
		default:
			// Native terminal turn: bare schema-conforming JSON.
			return llm.CompleteResponse{Content: wantPayload}
		}
	})
	stack := env.assemble(t, driver, llm.OutputModeNative, nil)
	defer func() { _ = stack.Close(ctx) }()

	id := waveV19ID("alice", "s-happy")
	credCh := waveV19Subscribe(t, env.provBus, id, auth.EventTypeToolCredentialExchanged)

	ans, err := stack.RunOnce(ctx, "classify the sentiment", id,
		assemble.WithOutputSchema(json.RawMessage(phase143Schema)))
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if ans.FinishReason != string(planner.FinishGoal) {
		t.Errorf("FinishReason = %q, want %q", ans.FinishReason, planner.FinishGoal)
	}
	// D-274 per-branch counting: CallParallel(2 branches) + CallTool(1) = 3.
	if ans.ToolCallsSeen != 3 {
		t.Errorf("ToolCallsSeen = %d, want 3 (2 parallel branches + 1 brokered call)", ans.ToolCallsSeen)
	}
	// Exact bytes — no lossy round-trip (D-272).
	if string(ans.AnswerPayload) != wantPayload {
		t.Errorf("AnswerPayload = %s, want the exact scripted bytes %s", ans.AnswerPayload, wantPayload)
	}
	// Exactly one broker exchange (only the brokered tool triggers it).
	if got := env.broker.callCount(); got != 1 {
		t.Errorf("broker exchanges = %d, want 1", got)
	}

	ev := waveV19WaitEvent(t, credCh, auth.EventTypeToolCredentialExchanged)
	p, ok := ev.Payload.(auth.ToolCredentialExchangedPayload)
	if !ok {
		t.Fatalf("credential_exchanged payload type = %T", ev.Payload)
	}
	if p.BindingScope != "user" {
		t.Errorf("credential_exchanged BindingScope = %q, want user", p.BindingScope)
	}
	if ev.Identity.UserID != "alice" || ev.Identity.TenantID != "wave-v19" {
		t.Errorf("credential_exchanged identity = %+v, want alice/wave-v19", ev.Identity)
	}
	// Zero token bytes in the event (D-271).
	if b, _ := json.Marshal(ev.Payload); strings.Contains(string(b), "brokered-wave-v19-alice") {
		t.Fatalf("TOKEN LEAK in credential_exchanged event: %s", b)
	}
}

// ---------------------------------------------------------------------------
// Leg 2 — park/resume × output schema
// ---------------------------------------------------------------------------

// waveV19ParkingPlanner is the deterministic PlannerOverride for leg 2
// (the phase111b pattern; see the file header's LEG 2 DESIGN NOTE):
// CallTool(brokered) → the broker's consent posture makes the pre-check
// fail auth_required → RequestPause parks the RUN → after the resume's
// injected context lands (and the broker has flipped to granting) →
// CallTool(brokered) retry succeeds → Finish with a schema-conforming
// payload the runtime edge validates onto AnswerPayload.
type waveV19ParkingPlanner struct{ payload any }

func (p *waveV19ParkingPlanner) Next(_ context.Context, rc planner.RunContext) (planner.Decision, error) {
	steps := 0
	if rc.Trajectory != nil {
		steps = len(rc.Trajectory.Steps)
	}
	injected := len(rc.Control.InjectedContext) > 0
	switch {
	case steps == 0:
		return planner.CallTool{Tool: waveV19BrokeredTool, Args: json.RawMessage(`{}`)}, nil
	case steps == 1 && !injected:
		return planner.RequestPause{
			Reason:  planner.PauseExternalEvent,
			Payload: map[string]any{"waiting_on": "tokenexchange broker consent"},
		}, nil
	case steps == 1 && injected:
		return planner.CallTool{Tool: waveV19BrokeredTool, Args: json.RawMessage(`{}`)}, nil
	default:
		return planner.Finish{Reason: planner.FinishGoal, Payload: p.payload}, nil
	}
}

func TestE2E_WaveV19_ParkResume_UnderOutputSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newWaveV19Env(t)
	env.broker.setPosture("consent") // withhold consent → pre-check parks

	// The LLM driver is unused by the override planner, but assemble
	// still resolves an LLM client.
	driver, _ := registerWaveV19Driver(func(int, llm.CompleteRequest) llm.CompleteResponse {
		return llm.CompleteResponse{Content: "unused"}
	})
	parker := &waveV19ParkingPlanner{payload: map[string]any{"sentiment": "neutral", "confidence": 0.5}}
	stack := env.assemble(t, driver, llm.OutputModeNative, parker)
	defer func() { _ = stack.Close(ctx) }()

	id := waveV19ID("carol", "s-park")
	const runID = "run-wave-v19-park"
	q := identity.Quadruple{Identity: id, RunID: runID}

	// Subscribe to the RUN-level pause on the assembled stack.Bus BEFORE
	// starting the run so the publish is never raced.
	pauseSub, err := stack.Bus.Subscribe(ctx, events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: []events.EventType{pauseresume.EventTypePauseRequested},
	})
	if err != nil {
		t.Fatalf("Bus.Subscribe(pause.requested): %v", err)
	}
	defer pauseSub.Cancel()

	type runResult struct {
		env planner.AnswerEnvelope
		err error
	}
	resCh := make(chan runResult, 1)
	go func() {
		a, e := stack.RunOnce(ctx, "brokered classify", id,
			assemble.WithRunID(runID), assemble.WithOutputSchema(json.RawMessage(phase143Schema)))
		resCh <- runResult{env: a, err: e}
	}()

	// The run parks: pause.requested lands with the run's identity.
	select {
	case ev, ok := <-pauseSub.Events():
		if !ok {
			t.Fatal("pause subscription closed before pause.requested")
		}
		if ev.Identity.Identity != id {
			t.Fatalf("pause.requested identity = %+v, want %+v", ev.Identity.Identity, id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the run to park (pause.requested)")
	}

	// Central consent completes → broker flips to granting.
	env.broker.setPosture("grant")

	// Resume the RUN via the steering inbox — the same path the Protocol
	// `resume` method drives. The INJECT_CONTEXT note is the operator's
	// "consent completed" marker the planner observes via RunContext.Control.
	inbox, err := stack.Steering.Lookup(q)
	if err != nil {
		t.Fatalf("steering.Registry.Lookup: %v", err)
	}
	if err := inbox.Enqueue(steering.ControlEvent{
		Type:         steering.ControlInjectContext,
		Identity:     q,
		CallerScope:  steering.ScopeOwnerUser,
		CallerTenant: q.TenantID,
		Payload:      map[string]any{"note": "broker consent completed"},
	}); err != nil {
		t.Fatalf("Enqueue(INJECT_CONTEXT): %v", err)
	}
	if err := inbox.Enqueue(steering.ControlEvent{
		Type:         steering.ControlResume,
		Identity:     q,
		CallerScope:  steering.ScopeOwnerUser,
		CallerTenant: q.TenantID,
		Payload:      map[string]any{"reason": "tokenexchange consent completed"},
	}); err != nil {
		t.Fatalf("Enqueue(RESUME): %v", err)
	}

	// The run re-enters, the retry dispatch succeeds against the now-
	// granting broker, and the terminal answer validates onto the envelope.
	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("RunOnce (park→resume): %v", res.err)
		}
		if res.env.FinishReason != string(planner.FinishGoal) {
			t.Errorf("FinishReason = %q, want %q", res.env.FinishReason, planner.FinishGoal)
		}
		if !strings.Contains(string(res.env.AnswerPayload), `"sentiment":"neutral"`) {
			t.Errorf("AnswerPayload = %s, want the conforming resumed payload", res.env.AnswerPayload)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the resumed run to finish")
	}

	// The broker saw the consent attempt AND the post-grant re-exchange.
	if got := env.broker.callCount(); got < 2 {
		t.Errorf("broker exchanges = %d, want ≥2 (consent decline + post-grant retry)", got)
	}
}

// ---------------------------------------------------------------------------
// Leg 3 — RunTyped nested-T under OutputMode: Tools
// ---------------------------------------------------------------------------

func TestE2E_WaveV19_RunTyped_NestedTools(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newWaveV19Env(t)

	// Prove the DERIVED schema sets additionalProperties:false at EVERY
	// level (the nested struct field too) — the property the Tools-mode
	// respond_with unwrap validates against.
	derived, err := schema.Derive(reflect.TypeOf(waveV19Outer{}))
	if err != nil {
		t.Fatalf("schema.Derive: %v", err)
	}
	rawSchema, err := json.Marshal(derived)
	if err != nil {
		t.Fatalf("marshal derived schema: %v", err)
	}
	if n := strings.Count(string(rawSchema), `"additionalProperties":false`); n < 2 {
		t.Fatalf("derived schema has %d additionalProperties:false, want ≥2 (top-level + nested inner): %s", n, rawSchema)
	}

	// Tools mode: the terminal turn is the respond_with envelope carrying
	// the schema-shaped object as `arguments`.
	const args = `{"title":"Q3 report","inner":{"score":0.9,"label":"good"}}`
	envelope := `{"name":"respond_with","arguments":` + args + `}`
	driver, _ := registerWaveV19Driver(func(int, llm.CompleteRequest) llm.CompleteResponse {
		return llm.CompleteResponse{Content: envelope}
	})
	stack := env.assemble(t, driver, llm.OutputModeTools, nil)
	defer func() { _ = stack.Close(ctx) }()

	id := waveV19ID("dave", "s-typed")
	out, ans, err := assemble.RunTyped[waveV19Outer](ctx, stack, "produce the quarterly report", id)
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if out.Title != "Q3 report" {
		t.Errorf("out.Title = %q, want %q", out.Title, "Q3 report")
	}
	if out.Inner.Score != 0.9 || out.Inner.Label != "good" {
		t.Errorf("out.Inner = %+v, want {0.9 good} (nested unwrap failed)", out.Inner)
	}
	if len(ans.AnswerPayload) == 0 {
		t.Error("expected a non-empty AnswerPayload on the envelope RunTyped returns alongside T")
	}
}

// ---------------------------------------------------------------------------
// Leg 4 — failure modes
// ---------------------------------------------------------------------------

func TestE2E_WaveV19_FailureModes(t *testing.T) {
	t.Parallel()

	// (a) Broker outage → auth.ErrExchangeFailed loud, NO interactive
	// fallback. Driven through the assembled catalog's wrapped descriptor
	// (real WrapWithOAuth + injected provider + broker).
	t.Run("BrokerOutage_FailsLoud", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		env := newWaveV19Env(t)
		env.broker.setPosture("error500")
		driver, _ := registerWaveV19Driver(func(int, llm.CompleteRequest) llm.CompleteResponse {
			return llm.CompleteResponse{Content: "unused"}
		})
		stack := env.assemble(t, driver, llm.OutputModeNative, nil)
		defer func() { _ = stack.Close(ctx) }()

		id := waveV19ID("erin", "s-outage")
		idCtx, err := identity.With(ctx, id)
		if err != nil {
			t.Fatalf("identity.With: %v", err)
		}
		desc, ok := stack.Catalog.Resolve(waveV19BrokeredTool)
		if !ok {
			t.Fatalf("brokered tool %q not registered on the assembled catalog", waveV19BrokeredTool)
		}
		_, err = desc.Invoke(idCtx, json.RawMessage(`{}`))
		if !errors.Is(err, auth.ErrExchangeFailed) {
			t.Fatalf("want auth.ErrExchangeFailed, got %v", err)
		}
		var authErr *auth.ErrAuthRequired
		if errors.As(err, &authErr) {
			t.Fatalf("broker outage must NOT surface *ErrAuthRequired (no interactive fallback): %v", err)
		}
		if strings.Contains(err.Error(), "brokered-") {
			t.Fatalf("error leaked token-shaped material: %v", err)
		}
	})

	// (b) Schema exhaustion → planner.ErrOutputInvalid, never silent text.
	t.Run("SchemaExhaustion_ErrOutputInvalid", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		env := newWaveV19Env(t)
		driver, _ := registerWaveV19Driver(func(int, llm.CompleteRequest) llm.CompleteResponse {
			return llm.CompleteResponse{Content: `{"sentiment":"elated"}`} // always wrong-enum
		})
		stack := env.assemble(t, driver, llm.OutputModeNative, nil)
		defer func() { _ = stack.Close(ctx) }()

		id := waveV19ID("frank", "s-exhaust")
		ans, err := stack.RunOnce(ctx, "classify the sentiment", id,
			assemble.WithOutputSchema(json.RawMessage(phase143Schema)))
		if !errors.Is(err, planner.ErrOutputInvalid) {
			t.Errorf("error = %v, want errors.Is planner.ErrOutputInvalid", err)
		}
		if ans.Answer != "" || ans.AnswerPayload != nil {
			t.Errorf("expected a zero envelope on failure (no silent text fallback), got %+v", ans)
		}
	})
}

// ---------------------------------------------------------------------------
// Leg 5 — identity propagation
// ---------------------------------------------------------------------------

func TestE2E_WaveV19_IdentityPropagation(t *testing.T) {
	t.Parallel()

	cases := []identity.Identity{
		{TenantID: "tenant-x", UserID: "xavier", SessionID: "sx"},
		{TenantID: "tenant-y", UserID: "yolanda", SessionID: "sy"},
	}
	for _, id := range cases {
		t.Run(id.TenantID, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newWaveV19Env(t)

			// Turn 1: brokered tool call (subject asserted broker-side).
			// Turn 2: schema-invalid → corrective retry → valid.
			driver, drv := registerWaveV19Driver(func(n int, _ llm.CompleteRequest) llm.CompleteResponse {
				switch n {
				case 1:
					return waveV19ToolCallResponse("call_r", waveV19BrokeredTool, `{}`)
				case 2:
					return llm.CompleteResponse{Content: `{"sentiment":"elated"}`} // wrong enum → retry
				default:
					return llm.CompleteResponse{Content: `{"sentiment":"positive"}`}
				}
			})
			stack := env.assemble(t, driver, llm.OutputModeNative, nil)
			defer func() { _ = stack.Close(ctx) }()

			// retry_with_feedback observed on a subscription scoped to THIS
			// call's exact triple — proves identity propagated through the
			// corrective-retry chain.
			subCtx, subCancel := context.WithCancel(ctx)
			defer subCancel()
			sub, err := stack.Bus.Subscribe(subCtx, events.Filter{
				Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
				Types: []events.EventType{llm.EventTypeRetryWithFeedback},
			})
			if err != nil {
				t.Fatalf("Bus.Subscribe: %v", err)
			}
			var retrySeen atomic.Bool
			drained := make(chan struct{})
			go func() {
				defer close(drained)
				for range sub.Events() {
					retrySeen.Store(true)
				}
			}()

			ans, err := stack.RunOnce(ctx, "classify via the brokered tool", id,
				assemble.WithOutputSchema(json.RawMessage(phase143Schema)))
			if err != nil {
				t.Fatalf("RunOnce: %v", err)
			}

			sub.Cancel()
			subCancel()
			select {
			case <-drained:
			case <-time.After(5 * time.Second):
			}

			if !retrySeen.Load() {
				t.Error("expected an llm.retry_with_feedback event scoped to this triple")
			}
			if drv.callCount.Load() < 3 {
				t.Errorf("driver calls = %d, want ≥3 (tool call + invalid + corrective)", drv.callCount.Load())
			}
			if !strings.Contains(string(ans.AnswerPayload), `"sentiment":"positive"`) {
				t.Errorf("AnswerPayload = %s, want the corrected positive answer", ans.AnswerPayload)
			}
			// Broker-side subject assertion: exactly this triple exchanged.
			key := id.TenantID + "/" + id.UserID
			if got := env.broker.subjectCount(key); got != 1 {
				t.Errorf("broker exchanges for %q = %d, want 1", key, got)
			}
			if got := env.broker.callCount(); got != 1 {
				t.Errorf("total broker exchanges = %d, want 1 (one brokered call, cached thereafter)", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Leg 6 — concurrency stress
// ---------------------------------------------------------------------------

// TestE2E_WaveV19_ConcurrencyStress drives N mixed RunOnce / RunTyped
// invocations against ONE shared Stack (CLAUDE.md §17.3 stress
// requirement), each with a distinct identity triple and a goal-encoded
// marker, and proves: no answer bleed (each run's payload carries its
// OWN marker and no other run's), per-identity broker isolation (each
// triple exchanges exactly once), no data race (the -race gate), and no
// goroutine leak after Close.
func TestE2E_WaveV19_ConcurrencyStress(t *testing.T) {
	ctx := context.Background()
	env := newWaveV19Env(t)
	baseline := goruntime.NumGoroutine()

	// Goal-based, stateless driver (concurrency-safe): turn 1 (no tool
	// result yet) → brokered tool call; terminal turn → echo the run's
	// own marker back into a schema-conforming payload.
	driver, _ := registerWaveV19Driver(func(_ int, req llm.CompleteRequest) llm.CompleteResponse {
		if !waveV19HasToolResult(req) {
			return waveV19ToolCallResponse("call_r", waveV19BrokeredTool, `{}`)
		}
		marker := waveV19ExtractMarker(waveV19AllText(req))
		return llm.CompleteResponse{Content: fmt.Sprintf(`{"marker":%q,"ok":true}`, marker)}
	})
	stack := env.assemble(t, driver, llm.OutputModeNative, nil)

	const n = 12 // ≥10 (CLAUDE.md §17.3)
	goalFor := func(i int) string {
		return fmt.Sprintf("wave-v19 concurrency goal %s", waveV19MarkerFor(i))
	}

	type result struct {
		idx     int
		payload string
		typed   bool
		err     error
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
			if i%2 == 0 {
				// RunTyped derives the schema from waveV19Marker.
				out, _, err := assemble.RunTyped[waveV19Marker](ctx, stack, goalFor(i), id)
				results[i] = result{idx: i, payload: out.Marker, typed: true, err: err}
				return
			}
			ans, err := stack.RunOnce(ctx, goalFor(i), id,
				assemble.WithOutputSchema(json.RawMessage(waveV19MarkerSchema)))
			results[i] = result{idx: i, payload: string(ans.AnswerPayload), typed: false, err: err}
		}(i)
	}
	wg.Wait()

	for _, r := range results {
		if r.err != nil {
			t.Errorf("run %d (typed=%v) failed: %v", r.idx, r.typed, r.err)
			continue
		}
		own := waveV19MarkerFor(r.idx)
		if !strings.Contains(r.payload, own) {
			t.Errorf("run %d: payload %q lacks its own marker %q — answer bleed", r.idx, r.payload, own)
		}
		for j := range n {
			if j != r.idx && strings.Contains(r.payload, waveV19MarkerFor(j)) {
				t.Errorf("run %d: payload contains run %d's marker — CROSS-RUN BLEED", r.idx, j)
			}
		}
	}

	// Per-identity broker isolation: each distinct triple exchanged once.
	for i := range n {
		key := fmt.Sprintf("t-%d/u-%d", i, i)
		if got := env.broker.subjectCount(key); got != 1 {
			t.Errorf("broker exchanges for %q = %d, want 1", key, got)
		}
	}

	if err := stack.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Goroutine baseline restored after teardown — the Stack's long-lived
	// goroutines are cancellable + joined on Close. (env.provBus/broker
	// live under t.Cleanup and are counted in the baseline.)
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
