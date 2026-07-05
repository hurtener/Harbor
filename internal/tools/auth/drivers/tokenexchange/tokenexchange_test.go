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
	"github.com/hurtener/Harbor/internal/tools/auth/credsource"
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
// posture is switchable (grant / consent_required / 5xx / gated slow
// grant) so the tests drive the park/resume/fail-loud/cancellation
// legs. Issued access tokens embed the SUBJECT TRIPLE's tenant + user
// so cross-tenant bleed is assertable broker-side.
type fakeBroker struct {
	server *httptest.Server

	mu         sync.Mutex
	tokenCalls int
	lastForm   url.Values
	perSubject int          // access-token suffix bump so each exchange is distinguishable
	posture    atomic.Value // string: "grant" | "consent" | "error500" | "slow"
	expiresIn  int
	consentURL string

	// started receives one signal per "slow"-posture request as it
	// arrives; gate blocks the response until closed. Both are set by
	// the test BEFORE switching posture to "slow".
	started chan struct{}
	gate    chan struct{}
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
	case "slow":
		// Signal arrival, then block until the test releases the gate;
		// respond as a normal grant afterwards.
		b.started <- struct{}{}
		<-b.gate
		b.writeGrant(w, r, n)
	default: // grant
		b.writeGrant(w, r, n)
	}
}

// writeGrant responds with a granted token embedding the subject
// triple's tenant + user (so cross-tenant bleed fails assertions).
func (b *fakeBroker) writeGrant(w http.ResponseWriter, r *http.Request, n int) {
	subj := decodeSubject(r.Form.Get("subject_token"))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":      "brokered-access-" + subj.TenantID + "-" + subj.UserID + "-" + itoa(n),
		"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
		"token_type":        "Bearer",
		"expires_in":        b.brokerExpiresIn(),
		"scope":             r.Form.Get("scope"),
	})
}

func (b *fakeBroker) brokerExpiresIn() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.expiresIn
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
	return mkProviderClock(t, broker, nil)
}

// mkProviderClock builds the provider with an optional controllable
// clock (nil → wall clock) — the TTL tests advance time instead of
// sleeping (CLAUDE.md §11).
func mkProviderClock(t *testing.T, broker *fakeBroker, clock func() time.Time) (auth.OAuthProvider, pauseresume.Coordinator, events.EventBus) {
	t.Helper()
	deps, coord, bus := mkDeps(t)
	if clock != nil {
		deps.Clock = clock
	}
	cfg := auth.ProviderConfig{
		Name:             tProviderName,
		CredentialSource: credsource.Static(tDummyBrokerClient, tDummyBrokerSecret),
		Scopes:           []string{"Mail.Read", "Calendars.Read"},
		TokenURL:         broker.tokenURL(),
		Extra:            map[string]string{"audience": "https://graph.microsoft.com"},
	}
	prov, err := tokenexchange.New(cfg, deps)
	if err != nil {
		t.Fatalf("tokenexchange.New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })
	return prov, coord, bus
}

// fakeClock is a mutex-guarded controllable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Now()} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// tokenPrefix is the broker-issued access-token prefix for an identity
// — embeds tenant + user so a cross-tenant bleed fails the assertion.
func tokenPrefix(id identity.Identity) string {
	return "brokered-access-" + id.TenantID + "-" + id.UserID + "-"
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
			Name:             tProviderName,
			CredentialSource: credsource.Static(tDummyBrokerClient, tDummyBrokerSecret),
			TokenURL:         broker.tokenURL(),
		}
	}
	deps, _, _ := mkDeps(t)

	cases := []struct {
		name   string
		mutate func(*auth.ProviderConfig)
		want   error
	}{
		// The credential resolves through the source seam, so an empty
		// client credential now surfaces at exchange time, not
		// construction (see TestToken_FailsLoud_EmptyResolvedCredential).
		// Construction fails loud on a NIL source.
		{"nil credential source", func(c *auth.ProviderConfig) { c.CredentialSource = nil }, tokenexchange.ErrMissingCredentialSource},
		{"missing token url", func(c *auth.ProviderConfig) { c.TokenURL = "" }, tokenexchange.ErrMissingTokenURL},
		{"bad cache ttl cap", func(c *auth.ProviderConfig) { c.Extra = map[string]string{"cache_ttl_cap": "not-a-duration"} }, tokenexchange.ErrBadCacheTTLCap},
		// A cap below the re-exchange floor is rejected loudly, never
		// silently clamped (fail-loud seam).
		{"sub-floor cache ttl cap", func(c *auth.ProviderConfig) { c.Extra = map[string]string{"cache_ttl_cap": "10s"} }, tokenexchange.ErrBadCacheTTLCap},
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

// TestToken_FailsLoud_EmptyResolvedCredential — the credential resolves
// through the source seam, so an empty client_id / client_secret now
// fails the EXCHANGE loud (wrapped ErrExchangeFailed + the typed
// ErrMissingClientID / ErrMissingClientSecret), never silently.
func TestToken_FailsLoud_EmptyResolvedCredential(t *testing.T) {
	t.Parallel()
	broker := newFakeBroker(t)
	deps, _, _ := mkDeps(t)
	cases := []struct {
		name string
		src  credsource.Source
		want error
	}{
		{"empty client id", credsource.Static("", tDummyBrokerSecret), tokenexchange.ErrMissingClientID},
		{"empty client secret", credsource.Static(tDummyBrokerClient, ""), tokenexchange.ErrMissingClientSecret},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := auth.ProviderConfig{
				Name:             tProviderName,
				CredentialSource: tc.src,
				TokenURL:         broker.tokenURL(),
			}
			prov, err := tokenexchange.New(cfg, deps)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = prov.Close(context.Background()) })
			_, err = prov.Token(mkCtx(t, aliceID()), "any")
			if !errors.Is(err, tc.want) {
				t.Fatalf("Token: want %v, got %v", tc.want, err)
			}
			if !errors.Is(err, auth.ErrExchangeFailed) {
				t.Fatalf("Token: want wrapped ErrExchangeFailed, got %v", err)
			}
		})
	}
}

func TestNew_FailsLoud_MissingDeps(t *testing.T) {
	t.Parallel()
	broker := newFakeBroker(t)
	cfg := auth.ProviderConfig{
		Name:             tProviderName,
		CredentialSource: credsource.Static(tDummyBrokerClient, tDummyBrokerSecret),
		TokenURL:         broker.tokenURL(),
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
	if !strings.HasPrefix(tok.AccessToken, tokenPrefix(aliceID())) {
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

	// Partial triples: identity.With is the only door onto a ctx and it
	// rejects every partial triple, so a partial triple can never reach
	// Token(). Pin that boundary here (plus the driver's own
	// identity.Validate defence for a hypothetical future ctx door):
	// each missing component fails identity.With, leaving the original
	// (identity-free) ctx — which Token() then fails closed on.
	partials := []struct {
		name string
		id   identity.Identity
	}{
		{"missing tenant", identity.Identity{UserID: tDummyUser, SessionID: tDummySession}},
		{"missing user", identity.Identity{TenantID: tDummyTenant, SessionID: tDummySession}},
		{"missing session", identity.Identity{TenantID: tDummyTenant, UserID: tDummyUser}},
	}
	for _, tc := range partials {
		t.Run(tc.name, func(t *testing.T) {
			ctx, err := identity.With(context.Background(), tc.id)
			if err == nil {
				t.Fatalf("identity.With accepted a partial triple: %+v", tc.id)
			}
			// The returned ctx carries no identity — Token fails closed.
			if _, terr := prov.Token(ctx, "any"); !errors.Is(terr, auth.ErrIdentityRequired) {
				t.Fatalf("Token (partial %s): want ErrIdentityRequired, got %v", tc.name, terr)
			}
		})
	}
}

// TestToken_NeverServedPastBrokerExpiry_ReexchangeRateFloored pins the
// TTL semantics: a token is NEVER served from cache past the
// broker-advertised expiry, and RE-EXCHANGE (not the serve window) is
// rate-floored at 30s — a broker advertising a shorter expiry than the
// floor fails loudly instead of spamming the bus.
func TestToken_NeverServedPastBrokerExpiry_ReexchangeRateFloored(t *testing.T) {
	t.Parallel()
	broker := newFakeBroker(t)
	broker.expiresIn = 1 // pathological near-zero broker expiry
	clock := newFakeClock()
	prov, _, _ := mkProviderClock(t, broker, clock.Now)
	ctx := mkCtx(t, aliceID())

	tok, err := prov.Token(ctx, "any")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	// Token.ExpiresAt is the BROKER validity — 1s, never inflated to
	// the floor (serving a stale token 29s past broker expiry was the
	// inverted posture this pins against).
	if got := tok.ExpiresAt.Sub(clock.Now()); got > 2*time.Second {
		t.Fatalf("Token.ExpiresAt inflated past broker expiry: %v", got)
	}
	// Within the broker validity: cache hit.
	if _, err := prov.Token(ctx, "any"); err != nil {
		t.Fatalf("Token (cache): %v", err)
	}
	if broker.calls() != 1 {
		t.Fatalf("expected cache hit within broker validity: %d broker calls", broker.calls())
	}

	// Past broker expiry but within the re-exchange floor: the stale
	// token is NOT served and the re-exchange is rate-floored →
	// fail-loud ErrExchangeFailed naming the floor.
	clock.Advance(2 * time.Second)
	_, err = prov.Token(ctx, "any")
	if !errors.Is(err, auth.ErrExchangeFailed) {
		t.Fatalf("want ErrExchangeFailed within re-exchange floor, got %v", err)
	}
	if !strings.Contains(err.Error(), "re-exchange floor") {
		t.Fatalf("error does not name the floor: %v", err)
	}
	if broker.calls() != 1 {
		t.Fatalf("rate floor must prevent a broker round-trip: %d calls", broker.calls())
	}

	// Past the floor: re-exchange proceeds.
	clock.Advance(30 * time.Second)
	if _, err := prov.Token(ctx, "any"); err != nil {
		t.Fatalf("Token after floor elapsed: %v", err)
	}
	if broker.calls() != 2 {
		t.Fatalf("expected re-exchange after floor: %d calls", broker.calls())
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
	broker.expiresIn = 0 // no broker expiry → the cap governs the serve horizon
	clock := newFakeClock()
	deps, _, _ := mkDeps(t)
	deps.Clock = clock.Now
	cfg := auth.ProviderConfig{
		Name:             tProviderName,
		CredentialSource: credsource.Static(tDummyBrokerClient, tDummyBrokerSecret),
		Scopes:           []string{"Mail.Read"}, TokenURL: broker.tokenURL(),
		Extra: map[string]string{"cache_ttl_cap": "45s"},
	}
	prov, err := tokenexchange.New(cfg, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })
	ctx := mkCtx(t, aliceID())
	tok, err := prov.Token(ctx, "any")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	// No broker expiry advertised → Token.ExpiresAt is zero (the cache
	// horizon is internal and capped, not reported as token validity).
	if !tok.ExpiresAt.IsZero() {
		t.Fatalf("Token.ExpiresAt should be zero when broker advertises no expiry, got %v", tok.ExpiresAt)
	}
	// audience defaults to the provider name when extra.audience is unset.
	if got := broker.form().Get("audience"); got != tProviderName {
		t.Fatalf("audience default: want %q, got %q", tProviderName, got)
	}
	// Within the 45s cap: cache hit.
	clock.Advance(40 * time.Second)
	if _, err := prov.Token(ctx, "any"); err != nil {
		t.Fatalf("Token (within cap): %v", err)
	}
	if broker.calls() != 1 {
		t.Fatalf("expected cache hit within cap: %d broker calls", broker.calls())
	}
	// Past the 45s cap: re-exchange.
	clock.Advance(10 * time.Second)
	if _, err := prov.Token(ctx, "any"); err != nil {
		t.Fatalf("Token (past cap): %v", err)
	}
	if broker.calls() != 2 {
		t.Fatalf("expected re-exchange past cap: %d broker calls", broker.calls())
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

// TestToken_CrossTenant_SameUserID_Isolated pins the §6 isolation
// contract on the cache / single-flight / revocation key: two TENANTS
// sharing a user ID never observe each other's brokered token — not
// from the broker, not from the cache, and not through Revoke.
func TestToken_CrossTenant_SameUserID_Isolated(t *testing.T) {
	t.Parallel()
	broker := newFakeBroker(t)
	prov, _, _ := mkProvider(t, broker)

	idA := identity.Identity{TenantID: "tenant-A", UserID: "alice", SessionID: "sA"}
	idB := identity.Identity{TenantID: "tenant-B", UserID: "alice", SessionID: "sB"}
	ctxA := mkCtx(t, idA)
	ctxB := mkCtx(t, idB)

	tokA, err := prov.Token(ctxA, "any")
	if err != nil {
		t.Fatalf("Token A: %v", err)
	}
	// Tenant-B same-user MUST be a cache miss → its own exchange, its
	// own token (asserted broker-side via the tenant-embedding token).
	tokB, err := prov.Token(ctxB, "any")
	if err != nil {
		t.Fatalf("Token B: %v", err)
	}
	if broker.calls() != 2 {
		t.Fatalf("cross-tenant cache bleed: %d broker calls (want 2)", broker.calls())
	}
	if tokA.AccessToken == tokB.AccessToken {
		t.Fatalf("cross-tenant token bleed: shared %q", tokA.AccessToken)
	}
	if !strings.HasPrefix(tokA.AccessToken, tokenPrefix(idA)) {
		t.Fatalf("tenant-A token wrong subject: %q", tokA.AccessToken)
	}
	if !strings.HasPrefix(tokB.AccessToken, tokenPrefix(idB)) {
		t.Fatalf("tenant-B token wrong subject: %q", tokB.AccessToken)
	}
	if tokA.TenantID != "tenant-A" || tokB.TenantID != "tenant-B" {
		t.Fatalf("tenant stamp bleed: A=%q B=%q", tokA.TenantID, tokB.TenantID)
	}

	// Each tenant now cache-hits its OWN entry.
	againA, err := prov.Token(ctxA, "any")
	if err != nil {
		t.Fatalf("Token A (cache): %v", err)
	}
	if againA.AccessToken != tokA.AccessToken || broker.calls() != 2 {
		t.Fatalf("tenant-A cache readback drift: %q (calls=%d)", againA.AccessToken, broker.calls())
	}

	// Revoke isolation: revoking tenant-A leaves tenant-B's cache
	// intact; tenant-A re-exchanges.
	if err := prov.Revoke(ctxA, "any"); err != nil {
		t.Fatalf("Revoke A: %v", err)
	}
	tokB2, err := prov.Token(ctxB, "any")
	if err != nil {
		t.Fatalf("Token B after A's revoke: %v", err)
	}
	if tokB2.AccessToken != tokB.AccessToken || broker.calls() != 2 {
		t.Fatalf("revoke crossed tenants: B token %q → %q (calls=%d)", tokB.AccessToken, tokB2.AccessToken, broker.calls())
	}
	if _, err := prov.Token(ctxA, "any"); err != nil {
		t.Fatalf("Token A after revoke: %v", err)
	}
	if broker.calls() != 3 {
		t.Fatalf("tenant-A should re-exchange after its revoke: %d calls", broker.calls())
	}
}

// TestSingleFlight_LeaderCancelDoesNotPoisonFollowers pins the D-025
// cancellation-cross-talk guarantee on the single-flight gate: the
// broker round-trip runs detached from the initiating caller, so
// cancelling the caller that STARTED the flight must not fail a
// follower collapsed onto the same flight.
func TestSingleFlight_LeaderCancelDoesNotPoisonFollowers(t *testing.T) {
	t.Parallel()
	broker := newFakeBroker(t)
	broker.started = make(chan struct{}, 2)
	broker.gate = make(chan struct{})
	broker.setPosture("slow")
	prov, _, _ := mkProvider(t, broker)
	id := aliceID()

	// Leader: starts the flight, then gets cancelled mid-exchange.
	leaderCtx, cancelLeader := context.WithCancel(mkCtx(t, id))
	leaderErr := make(chan error, 1)
	go func() {
		_, err := prov.Token(leaderCtx, "any")
		leaderErr <- err
	}()

	// Wait for the exchange to be in flight (flight registered + broker
	// reached), then start the follower — it collapses onto the flight.
	<-broker.started
	type res struct {
		tok auth.Token
		err error
	}
	followerRes := make(chan res, 1)
	go func() {
		tok, err := prov.Token(mkCtx(t, id), "any")
		followerRes <- res{tok, err}
	}()

	// Cancel the leader while the broker is still gated. The leader's
	// own frame errors with its ctx error...
	cancelLeader()
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader: want context.Canceled, got %v", err)
	}

	// ...and the follower, once the broker responds, still gets a real
	// token — never the leader's "context canceled".
	close(broker.gate)
	r := <-followerRes
	if r.err != nil {
		t.Fatalf("follower poisoned by leader cancellation: %v", r.err)
	}
	if !strings.HasPrefix(r.tok.AccessToken, tokenPrefix(id)) {
		t.Fatalf("follower token wrong subject: %q", r.tok.AccessToken)
	}
}

// TestRevoke_RacingInFlightExchange_NotRecached pins the revocation
// generation counter: a Revoke landing while an exchange is in flight
// must not be silently lost — the in-flight exchange declines to
// repopulate the cache, so the next Token() re-exchanges.
func TestRevoke_RacingInFlightExchange_NotRecached(t *testing.T) {
	t.Parallel()
	broker := newFakeBroker(t)
	broker.started = make(chan struct{}, 2)
	broker.gate = make(chan struct{})
	broker.setPosture("slow")
	prov, _, _ := mkProvider(t, broker)
	ctx := mkCtx(t, aliceID())

	tokCh := make(chan auth.Token, 1)
	errCh := make(chan error, 1)
	go func() {
		tok, err := prov.Token(ctx, "any")
		tokCh <- tok
		errCh <- err
	}()

	// Exchange in flight (gated at the broker) — Revoke now.
	<-broker.started
	if err := prov.Revoke(ctx, "any"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	// Release the broker; the in-flight caller still receives the
	// freshly minted token (the broker really issued it)...
	broker.setPosture("grant")
	close(broker.gate)
	tok := <-tokCh
	if err := <-errCh; err != nil {
		t.Fatalf("in-flight Token: %v", err)
	}
	if tok.AccessToken == "" {
		t.Fatalf("in-flight caller got no token")
	}
	if broker.calls() != 1 {
		t.Fatalf("setup drift: %d broker calls", broker.calls())
	}

	// ...but the revoked-mid-flight token was NOT cached: the next
	// Token() re-exchanges instead of serving it.
	tok2, err := prov.Token(ctx, "any")
	if err != nil {
		t.Fatalf("Token after racing revoke: %v", err)
	}
	if broker.calls() != 2 {
		t.Fatalf("revoke was silently lost: %d broker calls (want 2 — the post-revoke call must re-exchange)", broker.calls())
	}
	if tok2.AccessToken == tok.AccessToken {
		t.Fatalf("post-revoke Token served the revoked-mid-flight token %q", tok.AccessToken)
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
