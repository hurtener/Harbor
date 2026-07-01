package tokenexchange_test

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

	"github.com/hurtener/Harbor/internal/audit"
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
)

// Dummy IDs / credentials — never real values per §7 rule 2.
const (
	tDummyTenant       = "tenant-A"
	tDummyUser         = "user-alice"
	tDummySession      = "session-001"
	tDummyBrokerClient = "dummy-broker-client-id-not-a-secret"
	tDummyBrokerSecret = "dummy-broker-client-secret-not-a-secret"
	tProviderName      = "m365-broker"
)

// --- fake broker ------------------------------------------------------

// fakeBroker is an httptest server emulating an RFC-8693 credential
// broker. It asserts the exact 8693 params ON THE BROKER SIDE (§17.8),
// so a driver wired to the wrong field fails the test. Its response
// posture is switchable (grant / consent_required / 5xx) so the tests
// drive the park/resume/fail-loud legs.
type fakeBroker struct {
	server *httptest.Server

	mu         sync.Mutex
	tokenCalls int
	lastForm   url.Values
	perSubject int          // access-token suffix bump so each exchange is distinguishable
	posture    atomic.Value // string: "grant" | "consent" | "error500"
	expiresIn  int
	consentURL string
}

func newFakeBroker(t *testing.T) *fakeBroker {
	t.Helper()
	b := &fakeBroker{
		expiresIn:  3600,
		consentURL: "https://broker.example.test/consent?u=alice",
	}
	b.posture.Store("grant")
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", b.handleToken)
	b.server = httptest.NewServer(mux)
	t.Cleanup(b.server.Close)
	return b
}

func (b *fakeBroker) tokenURL() string { return b.server.URL + "/oauth2/token" }

func (b *fakeBroker) setPosture(p string) { b.posture.Store(p) }

func (b *fakeBroker) calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tokenCalls
}

func (b *fakeBroker) form() url.Values {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastForm
}

func (b *fakeBroker) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	b.tokenCalls++
	b.lastForm = r.Form
	n := b.perSubject
	b.perSubject++
	b.mu.Unlock()

	// Broker-side param assertions — a driver wired to the wrong field
	// fails HERE (§17.8 spec-derived fixture).
	if got := r.Form.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:token-exchange" {
		http.Error(w, "grant_type="+got, http.StatusBadRequest)
		return
	}
	if got := r.Form.Get("subject_token_type"); got != "urn:harbor:oauth:token-type:identity-triple" {
		http.Error(w, "subject_token_type="+got, http.StatusBadRequest)
		return
	}
	if r.Form.Get("audience") == "" {
		http.Error(w, "missing audience", http.StatusBadRequest)
		return
	}
	if r.Form.Get("client_id") != tDummyBrokerClient || r.Form.Get("client_secret") != tDummyBrokerSecret {
		http.Error(w, "bad client auth", http.StatusUnauthorized)
		return
	}

	switch b.posture.Load().(string) {
	case "error500":
		http.Error(w, "broker down", http.StatusInternalServerError)
		return
	case "consent":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":             "consent_required",
			"error_description": "central consent needed",
			"consent_url":       b.consentURL,
		})
		return
	default: // grant
		// Decode the subject_token so the test can assert the triple
		// round-trips and derive a per-subject access token.
		subj := decodeSubject(r.Form.Get("subject_token"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":      "brokered-access-" + subj.UserID + "-" + itoa(n),
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
			"token_type":        "Bearer",
			"expires_in":        b.expiresIn,
			"scope":             r.Form.Get("scope"),
		})
	}
}

type subjectTriple struct {
	TenantID  string `json:"tenant_id"`
	UserID    string `json:"user_id"`
	SessionID string `json:"session_id"`
}

func decodeSubject(s string) subjectTriple {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return subjectTriple{}
	}
	var st subjectTriple
	_ = json.Unmarshal(raw, &st)
	return st
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	if neg {
		digits = "-" + digits
	}
	return digits
}

// --- deps -------------------------------------------------------------

func mkRedactor() audit.Redactor { return patternsAudit.New() }

func mkBus(t *testing.T, red audit.Redactor) events.EventBus {
	t.Helper()
	b, err := eventsInmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              500 * time.Millisecond,
		DropWindow:               50 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("events inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	return b
}

// putSpyStore wraps a real TokenStore but fails the test if Put is ever
// called — the acceptance criterion asserts brokered tokens are never
// persisted, by TEST not by review.
type putSpyStore struct {
	t     *testing.T
	inner auth.TokenStore
}

func (s *putSpyStore) Get(ctx context.Context, scope auth.BindingScope, subjectID string, source tools.ToolSourceID) (auth.Token, bool, error) {
	return s.inner.Get(ctx, scope, subjectID, source)
}

func (s *putSpyStore) Put(_ context.Context, _ auth.Token) error {
	s.t.Fatalf("TokenStore.Put was called — brokered tokens must NEVER be persisted (D-271)")
	return nil
}

func (s *putSpyStore) Delete(ctx context.Context, scope auth.BindingScope, subjectID string, source tools.ToolSourceID) error {
	return s.inner.Delete(ctx, scope, subjectID, source)
}

func mkSpyStore(t *testing.T) auth.TokenStore {
	t.Helper()
	raw, err := stateInmem.New(config.StateConfig{})
	if err != nil {
		t.Fatalf("state inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close(context.Background()) })
	kek := make([]byte, auth.KEKSizeBytes)
	for i := range kek {
		kek[i] = byte(i*13 + 5)
	}
	sealer, err := auth.NewAESGCMSealer(kek)
	if err != nil {
		t.Fatalf("NewAESGCMSealer: %v", err)
	}
	inner, err := auth.NewTokenStore(raw, sealer)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}
	return &putSpyStore{t: t, inner: inner}
}

func mkDeps(t *testing.T) (auth.FactoryDeps, pauseresume.Coordinator, events.EventBus) {
	t.Helper()
	red := mkRedactor()
	bus := mkBus(t, red)
	coord := pauseresume.New()
	return auth.FactoryDeps{
		Store:       mkSpyStore(t),
		Bus:         bus,
		Redactor:    red,
		Coordinator: coord,
		HTTPClient:  &http.Client{Timeout: 5 * time.Second},
		Clock:       time.Now,
	}, coord, bus
}

func mkProvider(t *testing.T, broker *fakeBroker) (auth.OAuthProvider, pauseresume.Coordinator, events.EventBus) {
	t.Helper()
	deps, coord, bus := mkDeps(t)
	cfg := auth.ProviderConfig{
		Name:         tProviderName,
		ClientID:     tDummyBrokerClient,
		ClientSecret: tDummyBrokerSecret,
		Scopes:       []string{"Mail.Read", "Calendars.Read"},
		TokenURL:     broker.tokenURL(),
		Extra:        map[string]string{"audience": "https://graph.microsoft.com"},
	}
	prov, err := tokenexchange.New(cfg, deps)
	if err != nil {
		t.Fatalf("tokenexchange.New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })
	return prov, coord, bus
}

func mkCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

func aliceID() identity.Identity {
	return identity.Identity{TenantID: tDummyTenant, UserID: tDummyUser, SessionID: tDummySession}
}

// --- tests ------------------------------------------------------------

func TestNew_FailsLoud_MissingConfig(t *testing.T) {
	t.Parallel()
	broker := newFakeBroker(t)
	base := func() auth.ProviderConfig {
		return auth.ProviderConfig{
			Name: tProviderName, ClientID: tDummyBrokerClient,
			ClientSecret: tDummyBrokerSecret, TokenURL: broker.tokenURL(),
		}
	}
	deps, _, _ := mkDeps(t)

	cases := []struct {
		name   string
		mutate func(*auth.ProviderConfig)
		want   error
	}{
		{"missing client id", func(c *auth.ProviderConfig) { c.ClientID = "" }, tokenexchange.ErrMissingClientID},
		{"missing client secret", func(c *auth.ProviderConfig) { c.ClientSecret = "" }, tokenexchange.ErrMissingClientSecret},
		{"missing token url", func(c *auth.ProviderConfig) { c.TokenURL = "" }, tokenexchange.ErrMissingTokenURL},
		{"bad cache ttl cap", func(c *auth.ProviderConfig) { c.Extra = map[string]string{"cache_ttl_cap": "not-a-duration"} }, tokenexchange.ErrBadCacheTTLCap},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(&cfg)
			_, err := tokenexchange.New(cfg, deps)
			if !errors.Is(err, tc.want) {
				t.Fatalf("New: want %v, got %v", tc.want, err)
			}
		})
	}
}

func TestNew_FailsLoud_MissingDeps(t *testing.T) {
	t.Parallel()
	broker := newFakeBroker(t)
	cfg := auth.ProviderConfig{
		Name: tProviderName, ClientID: tDummyBrokerClient,
		ClientSecret: tDummyBrokerSecret, TokenURL: broker.tokenURL(),
	}
	_, err := tokenexchange.New(cfg, auth.FactoryDeps{}) // all deps nil
	if !errors.Is(err, tokenexchange.ErrMissingDeps) {
		t.Fatalf("New: want ErrMissingDeps, got %v", err)
	}
}

func TestDriverRegistered(t *testing.T) {
	t.Parallel()
	found := false
	for _, n := range auth.RegisteredDrivers() {
		if n == tokenexchange.DriverName {
			found = true
		}
	}
	if !found {
		t.Fatalf("driver %q not registered: %v", tokenexchange.DriverName, auth.RegisteredDrivers())
	}
}

func TestToken_HappyPath_ExchangeAndCache(t *testing.T) {
	t.Parallel()
	broker := newFakeBroker(t)
	prov, _, bus := mkProvider(t, broker)
	ctx := mkCtx(t, aliceID())

	sub := subscribe(t, bus, aliceID())

	tok, err := prov.Token(ctx, tools.ToolSourceID("any"))
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if !strings.HasPrefix(tok.AccessToken, "brokered-access-"+tDummyUser) {
		t.Fatalf("unexpected access token %q", tok.AccessToken)
	}
	if tok.TenantID != tDummyTenant || tok.UserID != tDummyUser {
		t.Fatalf("identity not stamped on token: %+v", tok)
	}
	// broker-side: subject_token decoded to alice's triple.
	if st := decodeSubject(broker.form().Get("subject_token")); st.UserID != tDummyUser || st.TenantID != tDummyTenant || st.SessionID != tDummySession {
		t.Fatalf("broker saw wrong subject triple: %+v", st)
	}
	if got := broker.form().Get("audience"); got != "https://graph.microsoft.com" {
		t.Fatalf("audience override not applied: %q", got)
	}

	// Second call: served from cache — NO new broker request.
	tok2, err := prov.Token(ctx, tools.ToolSourceID("any"))
	if err != nil {
		t.Fatalf("Token (cache): %v", err)
	}
	if tok2.AccessToken != tok.AccessToken {
		t.Fatalf("cache miss: %q vs %q", tok2.AccessToken, tok.AccessToken)
	}
	if broker.calls() != 1 {
		t.Fatalf("expected exactly 1 broker call (cache hit on 2nd), got %d", broker.calls())
	}

	// Exactly one tool.credential_exchanged event, zero token bytes.
	ev := waitEvent(t, sub, auth.EventTypeToolCredentialExchanged)
	p, ok := ev.Payload.(auth.ToolCredentialExchangedPayload)
	if !ok {
		t.Fatalf("payload type %T", ev.Payload)
	}
	if p.BindingScope != "user" || p.SubjectKind != "user" {
		t.Fatalf("payload scope/kind: %+v", p)
	}
	if p.BrokerHost == "" {
		t.Fatalf("broker host missing from payload")
	}
	assertNoTokenBytes(t, ev.Payload, tok.AccessToken)
}

func TestToken_TwoIdentity_Isolation(t *testing.T) {
	t.Parallel()
	broker := newFakeBroker(t)
	prov, _, _ := mkProvider(t, broker)

	idA := identity.Identity{TenantID: tDummyTenant, UserID: "alice", SessionID: "s1"}
	idB := identity.Identity{TenantID: tDummyTenant, UserID: "bob", SessionID: "s2"}
	tokA, err := prov.Token(mkCtx(t, idA), "any")
	if err != nil {
		t.Fatalf("Token A: %v", err)
	}
	tokB, err := prov.Token(mkCtx(t, idB), "any")
	if err != nil {
		t.Fatalf("Token B: %v", err)
	}
	if tokA.AccessToken == tokB.AccessToken {
		t.Fatalf("identity bleed: A and B share a token %q", tokA.AccessToken)
	}
	if tokA.UserID != "alice" || tokB.UserID != "bob" {
		t.Fatalf("subject not carried: A=%q B=%q", tokA.UserID, tokB.UserID)
	}
}

func TestToken_BrokerOutage_FailsLoud(t *testing.T) {
	t.Parallel()
	broker := newFakeBroker(t)
	broker.setPosture("error500")
	prov, _, _ := mkProvider(t, broker)

	_, err := prov.Token(mkCtx(t, aliceID()), "any")
	if !errors.Is(err, auth.ErrExchangeFailed) {
		t.Fatalf("Token: want ErrExchangeFailed, got %v", err)
	}
	// Never a fallback to *ErrAuthRequired on a hard failure.
	var authErr *auth.ErrAuthRequired
	if errors.As(err, &authErr) {
		t.Fatalf("hard failure must NOT surface *ErrAuthRequired: %v", err)
	}
}

func TestToken_ConsentRequired_ParksTyped(t *testing.T) {
	t.Parallel()
	broker := newFakeBroker(t)
	broker.setPosture("consent")
	prov, coord, bus := mkProvider(t, broker)
	sub := subscribe(t, bus, aliceID())
	ctx := mkCtx(t, aliceID())

	_, err := prov.Token(ctx, "any")
	var authErr *auth.ErrAuthRequired
	if !errors.As(err, &authErr) {
		t.Fatalf("Token: want *ErrAuthRequired, got %v", err)
	}
	if authErr.AuthorizeURL != broker.consentURL {
		t.Fatalf("consent url not surfaced: %q", authErr.AuthorizeURL)
	}
	if !strings.Contains(authErr.Message, broker.server.URL[len("http://"):]) && !strings.Contains(authErr.Message, "broker") {
		t.Fatalf("message does not name broker host: %q", authErr.Message)
	}

	// A tool.auth_required event was emitted carrying the pause token.
	ev := waitEvent(t, sub, auth.EventTypeToolAuthRequired)
	arp := ev.Payload.(auth.ToolAuthRequiredPayload)
	pauseToken := pauseresume.Token(arp.PauseToken)
	if pauseToken == "" {
		t.Fatalf("no pause token on event")
	}

	// Bare resume against a STILL-declining broker → Token() re-parks.
	if err := coord.Resume(ctx, pauseToken, pauseresume.DecisionResume, nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	_, err = prov.Token(ctx, "any")
	if !errors.As(err, &authErr) {
		t.Fatalf("bare resume against declining broker must re-park: got %v", err)
	}

	// Broker flips to granting → resume → Token() succeeds.
	broker.setPosture("grant")
	tok, err := prov.Token(ctx, "any")
	if err != nil {
		t.Fatalf("Token after broker grant: %v", err)
	}
	if !strings.HasPrefix(tok.AccessToken, "brokered-access-") {
		t.Fatalf("unexpected token %q", tok.AccessToken)
	}
}

func TestInteractiveMethods_ReturnNonInteractive(t *testing.T) {
	t.Parallel()
	broker := newFakeBroker(t)
	prov, _, _ := mkProvider(t, broker)
	ctx := mkCtx(t, aliceID())

	if _, err := prov.InitiateFlow(ctx, "any"); !errors.Is(err, auth.ErrNonInteractive) {
		t.Fatalf("InitiateFlow: want ErrNonInteractive, got %v", err)
	}
	if _, err := prov.CompleteFlow(ctx, "state", "code"); !errors.Is(err, auth.ErrNonInteractive) {
		t.Fatalf("CompleteFlow: want ErrNonInteractive, got %v", err)
	}
	if err := prov.DenyFlow(ctx, "state", "reason"); !errors.Is(err, auth.ErrNonInteractive) {
		t.Fatalf("DenyFlow: want ErrNonInteractive, got %v", err)
	}
	if _, ok := prov.PendingFlow("state"); ok {
		t.Fatalf("PendingFlow must report no flow")
	}
}

func TestRevoke_ClearsCache_Idempotent(t *testing.T) {
	t.Parallel()
	broker := newFakeBroker(t)
	prov, _, _ := mkProvider(t, broker)
	ctx := mkCtx(t, aliceID())

	// Revoke with nothing cached → nil (idempotent).
	if err := prov.Revoke(ctx, "any"); err != nil {
		t.Fatalf("Revoke (empty): %v", err)
	}
	// Populate cache.
	if _, err := prov.Token(ctx, "any"); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if broker.calls() != 1 {
		t.Fatalf("expected 1 broker call, got %d", broker.calls())
	}
	// Revoke clears the cache → next Token re-exchanges.
	if err := prov.Revoke(ctx, "any"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := prov.Token(ctx, "any"); err != nil {
		t.Fatalf("Token after revoke: %v", err)
	}
	if broker.calls() != 2 {
		t.Fatalf("expected re-exchange after revoke (2 broker calls), got %d", broker.calls())
	}
}

func TestToken_MissingIdentity_FailsClosed(t *testing.T) {
	t.Parallel()
	broker := newFakeBroker(t)
	prov, _, _ := mkProvider(t, broker)

	// No identity at all → fail closed.
	if _, err := prov.Token(context.Background(), "any"); !errors.Is(err, auth.ErrIdentityRequired) {
		t.Fatalf("Token (no id): want ErrIdentityRequired, got %v", err)
	}
	// Revoke with no identity also fails closed.
	if err := prov.Revoke(context.Background(), "any"); !errors.Is(err, auth.ErrIdentityRequired) {
		t.Fatalf("Revoke (no id): want ErrIdentityRequired, got %v", err)
	}
}

func TestToken_TTLCapFloor(t *testing.T) {
	t.Parallel()
	broker := newFakeBroker(t)
	broker.expiresIn = 1 // pathological near-zero broker expiry
	prov, _, _ := mkProvider(t, broker)
	ctx := mkCtx(t, aliceID())

	tok, err := prov.Token(ctx, "any")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	// The cache expiry is floored at >=30s despite the 1s broker
	// expiry — guards against event/bus spam.
	if until := time.Until(tok.ExpiresAt); until < 25*time.Second {
		t.Fatalf("cache TTL not floored: expires in %v", until)
	}
	// Immediate re-call is a cache hit (no second broker request).
	if _, err := prov.Token(ctx, "any"); err != nil {
		t.Fatalf("Token (cache): %v", err)
	}
	if broker.calls() != 1 {
		t.Fatalf("floor should prevent re-exchange: %d broker calls", broker.calls())
	}
}

func TestProvider_Closed_FailsLoud(t *testing.T) {
	t.Parallel()
	broker := newFakeBroker(t)
	prov, _, _ := mkProvider(t, broker)
	ctx := mkCtx(t, aliceID())
	if err := prov.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close is idempotent.
	if err := prov.Close(context.Background()); err != nil {
		t.Fatalf("Close (2nd): %v", err)
	}
	if _, err := prov.Token(ctx, "any"); !errors.Is(err, auth.ErrProviderClosed) {
		t.Fatalf("Token after close: want ErrProviderClosed, got %v", err)
	}
	if err := prov.Revoke(ctx, "any"); !errors.Is(err, auth.ErrProviderClosed) {
		t.Fatalf("Revoke after close: want ErrProviderClosed, got %v", err)
	}
}

func TestToken_CtxCancelled(t *testing.T) {
	t.Parallel()
	broker := newFakeBroker(t)
	prov, _, _ := mkProvider(t, broker)
	ctx, cancel := context.WithCancel(mkCtx(t, aliceID()))
	cancel()
	if _, err := prov.Token(ctx, "any"); err == nil {
		t.Fatalf("Token with cancelled ctx: want error, got nil")
	}
	if err := prov.Revoke(ctx, "any"); err == nil {
		t.Fatalf("Revoke with cancelled ctx: want error, got nil")
	}
}

func TestNew_ValidCacheTTLCapParses(t *testing.T) {
	t.Parallel()
	broker := newFakeBroker(t)
	broker.expiresIn = 0 // no broker expiry → cap governs
	deps, _, _ := mkDeps(t)
	cfg := auth.ProviderConfig{
		Name: tProviderName, ClientID: tDummyBrokerClient, ClientSecret: tDummyBrokerSecret,
		Scopes: []string{"Mail.Read"}, TokenURL: broker.tokenURL(),
		Extra: map[string]string{"cache_ttl_cap": "45s"},
	}
	prov, err := tokenexchange.New(cfg, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })
	tok, err := prov.Token(mkCtx(t, aliceID()), "any")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	// Cap of 45s governs when the broker advertises no expiry.
	if until := time.Until(tok.ExpiresAt); until < 40*time.Second || until > 46*time.Second {
		t.Fatalf("cache_ttl_cap not applied: expires in %v", until)
	}
	// audience defaults to the provider name when extra.audience is unset.
	if got := broker.form().Get("audience"); got != tProviderName {
		t.Fatalf("audience default: want %q, got %q", tProviderName, got)
	}
}

func TestToken_ConsentRequired_NoConsentURL(t *testing.T) {
	t.Parallel()
	broker := newFakeBroker(t)
	broker.consentURL = "" // broker declines without supplying a consent URL
	broker.setPosture("consent")
	prov, _, _ := mkProvider(t, broker)
	_, err := prov.Token(mkCtx(t, aliceID()), "any")
	var authErr *auth.ErrAuthRequired
	if !errors.As(err, &authErr) {
		t.Fatalf("Token: want *ErrAuthRequired, got %v", err)
	}
	if authErr.AuthorizeURL != "" {
		t.Fatalf("expected empty AuthorizeURL when broker supplies none, got %q", authErr.AuthorizeURL)
	}
}

// --- event helpers ----------------------------------------------------

func subscribe(t *testing.T, bus events.EventBus, id identity.Identity) <-chan events.Event {
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

func waitEvent(t *testing.T, ch <-chan events.Event, want events.EventType) events.Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type == want {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for event %q", want)
		}
	}
}

func assertNoTokenBytes(t *testing.T, payload events.EventPayload, token string) {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if token != "" && strings.Contains(string(b), token) {
		t.Fatalf("TOKEN LEAK: event payload contains access token bytes: %s", b)
	}
}
