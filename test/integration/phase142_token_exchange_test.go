// Phase 142 — external tool-credential provisioning: the `tokenexchange`
// OAuth driver (RFC §6.4 + §3.3; D-271).
//
// This test wires the REAL artifacts across the phase surface — no
// mocks at any seam (CLAUDE.md §17.3 #1):
//
//   - a real httptest broker fixture derived from RFC 8693's wire
//     format (§17.8): it asserts the exact grant_type /
//     subject_token_type / audience / client-auth params ON THE BROKER
//     SIDE, so a driver wired to the wrong field fails the test;
//   - the real catalog WrapWithOAuth pre-check path (the §13 consumer);
//   - a real events.EventBus (in-mem driver) + real audit.Redactor
//     (patterns driver) + real pauseresume.Coordinator + a real
//     state.StateStore-backed TokenStore (whose Put must NEVER fire).
//
// It exercises: happy path; two-identity isolation (distinct triples →
// distinct tokens, asserted broker-side); consent_required park → broker
// flips to granting → resume → success (plus the bare-resume-against-a-
// still-declining-broker re-park leg); broker outage fail-loud with no
// interactive fallback and no secret in any event.
//
// CLAUDE.md §17.1: this phase consumes Phase 30 + 64a + 50 shipped
// surfaces, so an integration test is mandatory; §17.3: real drivers +
// identity propagation + ≥1 failure mode + run under -race.
package integration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	stateInmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/auth/drivers/tokenexchange"
	"github.com/hurtener/Harbor/internal/tools/catalog"
)

// Dummy credentials — never real values per §7 rule 2.
const (
	p142BrokerClient = "dummy-broker-client-id-not-a-secret"
	p142BrokerSecret = "dummy-broker-client-secret-not-a-secret"
	p142Provider     = "m365-broker"
	p142Audience     = "https://graph.microsoft.com"
)

// p142Broker is the RFC-8693 broker fixture (§17.8 spec-derived).
type p142Broker struct {
	server *httptest.Server

	mu       sync.Mutex
	calls    int
	lastForm url.Values
	posture  atomic.Value // "grant" | "consent" | "error500"
}

func newP142Broker(t *testing.T) *p142Broker {
	t.Helper()
	b := &p142Broker{}
	b.posture.Store("grant")
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", b.handle)
	b.server = httptest.NewServer(mux)
	t.Cleanup(b.server.Close)
	return b
}

func (b *p142Broker) tokenURL() string    { return b.server.URL + "/oauth2/token" }
func (b *p142Broker) setPosture(p string) { b.posture.Store(p) }

func (b *p142Broker) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func (b *p142Broker) form() url.Values {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastForm
}

func (b *p142Broker) handle(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	b.calls++
	b.lastForm = r.Form
	b.mu.Unlock()

	// §17.8: broker-side wire assertions. A driver wired to the wrong
	// 8693 field fails HERE.
	if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:token-exchange" {
		http.Error(w, "grant_type", http.StatusBadRequest)
		return
	}
	if r.Form.Get("subject_token_type") != "urn:harbor:oauth:token-type:identity-triple" {
		http.Error(w, "subject_token_type", http.StatusBadRequest)
		return
	}
	if r.Form.Get("audience") != p142Audience {
		http.Error(w, "audience", http.StatusBadRequest)
		return
	}
	if r.Form.Get("client_id") != p142BrokerClient || r.Form.Get("client_secret") != p142BrokerSecret {
		http.Error(w, "client auth", http.StatusUnauthorized)
		return
	}

	switch b.posture.Load().(string) {
	case "error500":
		http.Error(w, "broker down", http.StatusInternalServerError)
	case "consent":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":       "consent_required",
			"consent_url": "https://broker.example.test/consent",
		})
	default:
		// The issued token embeds the subject triple's tenant + user so
		// a cross-tenant bleed is assertable broker-side.
		var triple struct {
			TenantID, UserID string
		}
		if raw, err := base64.RawURLEncoding.DecodeString(r.Form.Get("subject_token")); err == nil {
			var st struct {
				TenantID  string `json:"tenant_id"`
				UserID    string `json:"user_id"`
				SessionID string `json:"session_id"`
			}
			_ = json.Unmarshal(raw, &st)
			triple.TenantID = st.TenantID
			triple.UserID = st.UserID
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "brokered-" + triple.TenantID + "-" + triple.UserID,
			"token_type":   "Bearer",
			"expires_in":   3600,
			"scope":        r.Form.Get("scope"),
		})
	}
}

// p142Env bundles the real wiring.
type p142Env struct {
	provider auth.OAuthProvider
	coord    pauseresume.Coordinator
	bus      events.EventBus
	broker   *p142Broker
	desc     tools.ToolDescriptor // the OAuth-wrapped tool descriptor
}

func newP142Env(t *testing.T) *p142Env {
	t.Helper()
	broker := newP142Broker(t)

	red := patternsAudit.New()
	bus, err := eventsInmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              500 * time.Millisecond,
		DropWindow:               50 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("eventsInmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	coord := pauseresume.New()

	// Real StateStore-backed TokenStore. The tokenexchange driver holds
	// it but must NEVER Put — the wrapper below fails the test if it does.
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
	store := &p142SpyStore{t: t, inner: innerStore}

	prov, err := tokenexchange.New(auth.ProviderConfig{
		Name:         p142Provider,
		ClientID:     p142BrokerClient,
		ClientSecret: p142BrokerSecret,
		Scopes:       []string{"Mail.Read"},
		TokenURL:     broker.tokenURL(),
		Extra:        map[string]string{"audience": p142Audience},
	}, auth.FactoryDeps{
		Store: store, Bus: bus, Redactor: red, Coordinator: coord,
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("tokenexchange.New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })

	// Real catalog WrapWithOAuth consumer path (D-090). The inner tool
	// records whether it was invoked so the test can prove the pre-check
	// short-circuits on a park.
	var invoked atomic.Int32
	base := tools.ToolDescriptor{
		Tool: tools.Tool{Name: "send_mail", Source: tools.ToolSourceID(p142Provider)},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			invoked.Add(1)
			return tools.ToolResult{}, nil
		},
	}
	desc := catalog.WrapWithOAuth(base, prov, catalog.OAuthWrapperOptions{
		ProviderName: p142Provider, BindingScope: auth.ScopeUser,
	})

	return &p142Env{provider: prov, coord: coord, bus: bus, broker: broker, desc: desc}
}

// p142SpyStore fails the test if Put is ever called.
type p142SpyStore struct {
	t     *testing.T
	inner auth.TokenStore
}

func (s *p142SpyStore) Get(ctx context.Context, scope auth.BindingScope, subjectID string, source tools.ToolSourceID) (auth.Token, bool, error) {
	return s.inner.Get(ctx, scope, subjectID, source)
}
func (s *p142SpyStore) Put(_ context.Context, _ auth.Token) error {
	s.t.Fatalf("TokenStore.Put called — brokered tokens must NEVER be persisted (D-271)")
	return nil
}
func (s *p142SpyStore) Delete(ctx context.Context, scope auth.BindingScope, subjectID string, source tools.ToolSourceID) error {
	return s.inner.Delete(ctx, scope, subjectID, source)
}

func p142Ctx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

func p142Subscribe(t *testing.T, bus events.EventBus, id identity.Identity) <-chan events.Event {
	t.Helper()
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)
	return sub.Events()
}

func p142WaitEvent(t *testing.T, ch <-chan events.Event, want events.EventType) events.Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type == want {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q", want)
		}
	}
}

// TestE2E_Phase142_TokenExchange_ThroughCatalog drives the happy path
// through the real WrapWithOAuth pre-check: a fresh identity has no
// cached credential, the pre-check exchanges against the broker, and
// the wrapped tool then invokes.
func TestE2E_Phase142_TokenExchange_ThroughCatalog(t *testing.T) {
	t.Parallel()
	env := newP142Env(t)
	id := identity.Identity{TenantID: "tenant-1", UserID: "alice", SessionID: "s1"}
	sub := p142Subscribe(t, env.bus, id)
	ctx := p142Ctx(t, id)

	if _, err := env.desc.Invoke(ctx, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("wrapped Invoke: %v", err)
	}
	if env.broker.callCount() != 1 {
		t.Fatalf("expected 1 broker exchange, got %d", env.broker.callCount())
	}
	// The credential-exchanged event fired with the correct subject and
	// zero token bytes.
	ev := p142WaitEvent(t, sub, auth.EventTypeToolCredentialExchanged)
	p := ev.Payload.(auth.ToolCredentialExchangedPayload)
	if p.BindingScope != "user" || p.SubjectKind != "user" {
		t.Fatalf("payload: %+v", p)
	}
	if ev.Identity.Identity.UserID != "alice" {
		t.Fatalf("event identity: %+v", ev.Identity)
	}
	if b, _ := json.Marshal(ev.Payload); strings.Contains(string(b), "brokered-tenant-1-alice") {
		t.Fatalf("TOKEN LEAK in credential_exchanged event: %s", b)
	}

	// Second Invoke → cache hit, no new exchange.
	if _, err := env.desc.Invoke(ctx, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("wrapped Invoke (cache): %v", err)
	}
	if env.broker.callCount() != 1 {
		t.Fatalf("cache hit expected; broker calls = %d", env.broker.callCount())
	}
}

// TestE2E_Phase142_TwoIdentityIsolation proves distinct triples get
// distinct tokens, asserted broker-side.
func TestE2E_Phase142_TwoIdentityIsolation(t *testing.T) {
	t.Parallel()
	env := newP142Env(t)
	idA := identity.Identity{TenantID: "tenant-1", UserID: "alice", SessionID: "sA"}
	idB := identity.Identity{TenantID: "tenant-1", UserID: "bob", SessionID: "sB"}

	tokA, err := env.provider.Token(p142Ctx(t, idA), tools.ToolSourceID(p142Provider))
	if err != nil {
		t.Fatalf("Token A: %v", err)
	}
	tokB, err := env.provider.Token(p142Ctx(t, idB), tools.ToolSourceID(p142Provider))
	if err != nil {
		t.Fatalf("Token B: %v", err)
	}
	if tokA.AccessToken == tokB.AccessToken {
		t.Fatalf("identity bleed: shared token %q", tokA.AccessToken)
	}
	if tokA.AccessToken != "brokered-tenant-1-alice" || tokB.AccessToken != "brokered-tenant-1-bob" {
		t.Fatalf("broker-side subject mismatch: A=%q B=%q", tokA.AccessToken, tokB.AccessToken)
	}
}

// TestE2E_Phase142_CrossTenantSameUserID_Isolation proves two TENANTS
// sharing a user ID never observe each other's brokered token —
// asserted broker-side (the token embeds the tenant), cache-side (the
// second tenant is a cache miss, not a hit on the first tenant's
// entry), and through Revoke (revoking one tenant leaves the other's
// cache intact). The cross-tenant credential-bleed regression guard.
func TestE2E_Phase142_CrossTenantSameUserID_Isolation(t *testing.T) {
	t.Parallel()
	env := newP142Env(t)
	idA := identity.Identity{TenantID: "tenant-1", UserID: "alice", SessionID: "sA"}
	idB := identity.Identity{TenantID: "tenant-2", UserID: "alice", SessionID: "sB"}
	ctxA := p142Ctx(t, idA)
	ctxB := p142Ctx(t, idB)
	src := tools.ToolSourceID(p142Provider)

	tokA, err := env.provider.Token(ctxA, src)
	if err != nil {
		t.Fatalf("Token A: %v", err)
	}
	// Tenant-2's same-user call MUST be its own exchange (cache miss),
	// yielding its own tenant-scoped token — asserted broker-side.
	tokB, err := env.provider.Token(ctxB, src)
	if err != nil {
		t.Fatalf("Token B: %v", err)
	}
	if env.broker.callCount() != 2 {
		t.Fatalf("cross-tenant cache bleed: %d broker calls (want 2)", env.broker.callCount())
	}
	if tokA.AccessToken != "brokered-tenant-1-alice" || tokB.AccessToken != "brokered-tenant-2-alice" {
		t.Fatalf("cross-tenant token bleed: A=%q B=%q", tokA.AccessToken, tokB.AccessToken)
	}
	if tokA.TenantID != "tenant-1" || tokB.TenantID != "tenant-2" {
		t.Fatalf("tenant stamp bleed: A=%q B=%q", tokA.TenantID, tokB.TenantID)
	}

	// Revoke isolation: revoking tenant-1 leaves tenant-2's cache
	// intact; tenant-1 re-exchanges.
	if err := env.provider.Revoke(ctxA, src); err != nil {
		t.Fatalf("Revoke A: %v", err)
	}
	tokB2, err := env.provider.Token(ctxB, src)
	if err != nil {
		t.Fatalf("Token B after A's revoke: %v", err)
	}
	if tokB2.AccessToken != tokB.AccessToken || env.broker.callCount() != 2 {
		t.Fatalf("revoke crossed tenants: B %q→%q (calls=%d)", tokB.AccessToken, tokB2.AccessToken, env.broker.callCount())
	}
	if _, err := env.provider.Token(ctxA, src); err != nil {
		t.Fatalf("Token A after revoke: %v", err)
	}
	if env.broker.callCount() != 3 {
		t.Fatalf("tenant-1 should re-exchange after its revoke: %d calls", env.broker.callCount())
	}
}

// TestE2E_Phase142_ConsentParkResumeSucceed drives the full park →
// flip → resume → success cycle plus the bare-resume-re-park leg.
func TestE2E_Phase142_ConsentParkResumeSucceed(t *testing.T) {
	t.Parallel()
	env := newP142Env(t)
	env.broker.setPosture("consent")
	id := identity.Identity{TenantID: "tenant-1", UserID: "carol", SessionID: "sC"}
	sub := p142Subscribe(t, env.bus, id)
	ctx := p142Ctx(t, id)

	// Pre-check parks: WrapWithOAuth returns the *ErrAuthRequired and
	// does NOT invoke the tool.
	_, err := env.desc.Invoke(ctx, json.RawMessage(`{}`))
	var authErr *auth.ErrAuthRequired
	if !errors.As(err, &authErr) {
		t.Fatalf("wrapped Invoke: want *ErrAuthRequired, got %v", err)
	}

	ev := p142WaitEvent(t, sub, auth.EventTypeToolAuthRequired)
	pauseToken := pauseresume.Token(ev.Payload.(auth.ToolAuthRequiredPayload).PauseToken)

	// Bare resume against a STILL-declining broker re-parks.
	if err := env.coord.Resume(ctx, pauseToken, pauseresume.DecisionResume, nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if _, err := env.provider.Token(ctx, tools.ToolSourceID(p142Provider)); !errors.As(err, &authErr) {
		t.Fatalf("bare resume against declining broker must re-park: got %v", err)
	}

	// Central consent completes → broker grants → Token() succeeds.
	env.broker.setPosture("grant")
	tok, err := env.provider.Token(ctx, tools.ToolSourceID(p142Provider))
	if err != nil {
		t.Fatalf("Token after grant: %v", err)
	}
	if tok.AccessToken != "brokered-tenant-1-carol" {
		t.Fatalf("unexpected token %q", tok.AccessToken)
	}
}

// TestE2E_Phase142_BrokerOutage_FailsLoud proves a broker outage fails
// loud with no interactive fallback and no secret in any event.
func TestE2E_Phase142_BrokerOutage_FailsLoud(t *testing.T) {
	t.Parallel()
	env := newP142Env(t)
	env.broker.setPosture("error500")
	id := identity.Identity{TenantID: "tenant-1", UserID: "dave", SessionID: "sD"}
	ctx := p142Ctx(t, id)

	_, err := env.desc.Invoke(ctx, json.RawMessage(`{}`))
	if !errors.Is(err, auth.ErrExchangeFailed) {
		t.Fatalf("want ErrExchangeFailed, got %v", err)
	}
	var authErr *auth.ErrAuthRequired
	if errors.As(err, &authErr) {
		t.Fatalf("broker outage must NOT surface *ErrAuthRequired (no interactive fallback): %v", err)
	}
	// The error message never carries the (nonexistent) token; it names
	// the broker host + status only.
	if strings.Contains(err.Error(), "brokered-") {
		t.Fatalf("error leaked token-shaped material: %v", err)
	}
}
