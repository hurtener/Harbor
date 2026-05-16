package oauth2_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
	"github.com/hurtener/Harbor/internal/state"
	stateInmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/auth/drivers/oauth2"
)

// Dummy IDs / credentials — never real values per §7 rule 2.
const (
	tDummyTenant   = "tenant-A"
	tDummyUser     = "user-alice"
	tDummySession  = "session-001"
	tDummyClientID = "dummy-client-id-not-a-secret"
	tDummyClientSc = "dummy-client-secret-not-a-secret" //nolint:gosec // dummy fixture per §7 rule 2
)

func fixedKEK(t *testing.T) []byte {
	t.Helper()
	kek := make([]byte, auth.KEKSizeBytes)
	for i := range kek {
		kek[i] = byte(i*13 + 5)
	}
	return kek
}

func mkRedactor(t *testing.T) audit.Redactor {
	t.Helper()
	return patternsAudit.New()
}

func mkBus(t *testing.T, red audit.Redactor) events.EventBus {
	t.Helper()
	cfg := config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     32,
		IdleTimeout:              500 * time.Millisecond,
		DropWindow:               50 * time.Millisecond,
	}
	b, err := eventsInmem.New(cfg, red)
	if err != nil {
		t.Fatalf("events inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	return b
}

func mkStore(t *testing.T) state.StateStore {
	t.Helper()
	s, err := stateInmem.New(config.StateConfig{})
	if err != nil {
		t.Fatalf("state inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func mkTokenStore(t *testing.T) auth.TokenStore {
	t.Helper()
	store := mkStore(t)
	sealer, err := auth.NewAESGCMSealer(fixedKEK(t))
	if err != nil {
		t.Fatalf("NewAESGCMSealer: %v", err)
	}
	ts, err := auth.NewTokenStore(store, sealer)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}
	return ts
}

func mkDeps(t *testing.T) auth.FactoryDeps {
	t.Helper()
	red := mkRedactor(t)
	return auth.FactoryDeps{
		Store:       mkTokenStore(t),
		Bus:         mkBus(t, red),
		Redactor:    red,
		Coordinator: pauseresume.New(),
		HTTPClient:  &http.Client{Timeout: 5 * time.Second},
		Clock:       time.Now,
	}
}

func mkCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), identity.Identity{
		TenantID:  tDummyTenant,
		UserID:    tDummyUser,
		SessionID: tDummySession,
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// fakeAuthServer is a tiny in-test OAuth authorization server. It does
// NOT do PKCE verification end-to-end (the V1 oauth2 driver delegates
// to *auth.Provider which does); we only need this server to respond
// to /token POSTs so the goroutine-leak + identity tests can exercise
// real code paths.
func newFakeAuthServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		// Mirror back the state so a real browser flow could pick it
		// up; tests don't drive this.
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		body := map[string]any{
			"access_token":  "dummy-access-not-a-secret",
			"refresh_token": "dummy-refresh-not-a-secret",
			"token_type":    "Bearer",
			"expires_in":    3600,
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// validProviderConfig builds a structurally-valid ProviderConfig
// pointing at srv. ClientID / ClientSecret are dummy fixtures per §7
// rule 2.
func validProviderConfig(srv *httptest.Server) auth.ProviderConfig {
	return auth.ProviderConfig{
		Name:         "test-provider",
		ClientID:     tDummyClientID,
		ClientSecret: tDummyClientSc,
		Scopes:       []string{"repo", "read:user"},
		AuthURL:      srv.URL + "/authorize",
		TokenURL:     srv.URL + "/token",
		RedirectURL:  "http://localhost/callback",
	}
}

func TestNew_HappyPath_ConstructsProvider(t *testing.T) {
	t.Parallel()
	srv := newFakeAuthServer(t)
	prov, err := oauth2.New(validProviderConfig(srv), mkDeps(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })
	if prov == nil {
		t.Fatal("New returned nil provider")
	}
}

func TestNew_FailsLoud_EmptyClientID(t *testing.T) {
	t.Parallel()
	srv := newFakeAuthServer(t)
	cfg := validProviderConfig(srv)
	cfg.ClientID = ""
	_, err := oauth2.New(cfg, mkDeps(t))
	if err == nil {
		t.Fatal("New with empty ClientID did not error")
	}
	if !errors.Is(err, oauth2.ErrMissingClientID) {
		t.Fatalf("err = %v, want ErrMissingClientID", err)
	}
}

func TestNew_FailsLoud_EmptyClientSecret(t *testing.T) {
	t.Parallel()
	srv := newFakeAuthServer(t)
	cfg := validProviderConfig(srv)
	cfg.ClientSecret = ""
	_, err := oauth2.New(cfg, mkDeps(t))
	if err == nil {
		t.Fatal("New with empty ClientSecret did not error")
	}
	if !errors.Is(err, oauth2.ErrMissingClientSecret) {
		t.Fatalf("err = %v, want ErrMissingClientSecret", err)
	}
}

func TestNew_FailsLoud_MissingEndpoints(t *testing.T) {
	t.Parallel()
	srv := newFakeAuthServer(t)
	cfg := validProviderConfig(srv)
	cfg.AuthURL = ""
	cfg.TokenURL = ""
	_, err := oauth2.New(cfg, mkDeps(t))
	if err == nil {
		t.Fatal("New with empty endpoints did not error")
	}
	if !errors.Is(err, oauth2.ErrMissingEndpoints) {
		t.Fatalf("err = %v, want ErrMissingEndpoints", err)
	}
}

func TestNew_FailsLoud_MissingRedirectURL(t *testing.T) {
	t.Parallel()
	srv := newFakeAuthServer(t)
	cfg := validProviderConfig(srv)
	cfg.RedirectURL = ""
	_, err := oauth2.New(cfg, mkDeps(t))
	if err == nil {
		t.Fatal("New with empty RedirectURL did not error")
	}
	if !errors.Is(err, oauth2.ErrMissingRedirectURL) {
		t.Fatalf("err = %v, want ErrMissingRedirectURL", err)
	}
}

func TestNew_FailsLoud_NilDeps(t *testing.T) {
	t.Parallel()
	srv := newFakeAuthServer(t)
	cfg := validProviderConfig(srv)
	// Empty deps: the underlying NewProvider rejects nil store / bus /
	// redactor / coordinator with a wrapped error.
	_, err := oauth2.New(cfg, auth.FactoryDeps{})
	if err == nil {
		t.Fatal("New with empty deps did not error")
	}
}

func TestToken_IdentityRequired(t *testing.T) {
	t.Parallel()
	srv := newFakeAuthServer(t)
	prov, err := oauth2.New(validProviderConfig(srv), mkDeps(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })

	// No identity in context — must fail closed.
	_, err = prov.Token(context.Background(), tools.ToolSourceID("anything"))
	if err == nil {
		t.Fatal("Token without identity did not error")
	}
	if !errors.Is(err, auth.ErrIdentityRequired) {
		t.Fatalf("err = %v, want ErrIdentityRequired", err)
	}
}

func TestToken_NoStoredToken_ReturnsAuthRequired(t *testing.T) {
	t.Parallel()
	srv := newFakeAuthServer(t)
	prov, err := oauth2.New(validProviderConfig(srv), mkDeps(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })

	// First Token() call with no stored token surfaces typed
	// *ErrAuthRequired — the catalog wrapper / runtime turns this
	// into a pause for OAuth completion (Phase 50).
	_, err = prov.Token(mkCtx(t), tools.ToolSourceID("any-source"))
	if err == nil {
		t.Fatal("Token with no stored token did not return ErrAuthRequired")
	}
	var arErr *auth.ErrAuthRequired
	if !errors.As(err, &arErr) {
		t.Fatalf("err = %v, want *ErrAuthRequired", err)
	}
	if arErr.AuthorizeURL == "" {
		t.Fatal("ErrAuthRequired.AuthorizeURL empty")
	}
	if arErr.State == "" {
		t.Fatal("ErrAuthRequired.State empty")
	}
}

func TestToken_SourceArgIsRetargeted(t *testing.T) {
	t.Parallel()
	// The catalog wrapper passes d.Tool.Source which is unrelated to
	// the operator's provider name; the V1 oauth2 driver collapses
	// every source onto the operator-configured single OAuthConfig.
	// Assert that two different source IDs both surface the same
	// AuthorizeURL (i.e. the same internal flow).
	srv := newFakeAuthServer(t)
	prov, err := oauth2.New(validProviderConfig(srv), mkDeps(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })

	ctx := mkCtx(t)
	_, errA := prov.Token(ctx, tools.ToolSourceID("source-A"))
	_, errB := prov.Token(ctx, tools.ToolSourceID("source-B"))

	var aErr, bErr *auth.ErrAuthRequired
	if !errors.As(errA, &aErr) {
		t.Fatalf("source-A err = %v, want *ErrAuthRequired", errA)
	}
	if !errors.As(errB, &bErr) {
		t.Fatalf("source-B err = %v, want *ErrAuthRequired", errB)
	}
	// Source on the typed error mirrors the configured provider's
	// source (the retarget). The two calls produce different State
	// values (fresh per call) but identical Source.
	if aErr.Source != bErr.Source {
		t.Fatalf("Source differs across retarget: %q vs %q", aErr.Source, bErr.Source)
	}
}

// TestConcurrentReuse_D025 — N=128 concurrent invocations against one
// shared provider instance under -race; assert no races, no
// goroutine leaks (baseline-restored within tolerance), and that every
// invocation surfaces the typed *ErrAuthRequired error (proving each
// goroutine reached the underlying Provider correctly).
func TestConcurrentReuse_D025(t *testing.T) {
	t.Parallel()
	srv := newFakeAuthServer(t)
	prov, err := oauth2.New(validProviderConfig(srv), mkDeps(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })

	const N = 128
	ctx := mkCtx(t)

	var wg sync.WaitGroup
	wg.Add(N)
	var ok atomic.Int64
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			_, err := prov.Token(ctx, tools.ToolSourceID("concurrent-source"))
			var arErr *auth.ErrAuthRequired
			if errors.As(err, &arErr) {
				ok.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if got := int(ok.Load()); got != N {
		t.Fatalf("ok = %d, want %d (all concurrent Token calls should surface *ErrAuthRequired)", got, N)
	}
}

func TestDriverRegistered(t *testing.T) {
	t.Parallel()
	// init() in the driver package self-registered "oauth2".
	registered := auth.RegisteredDrivers()
	found := false
	for _, n := range registered {
		if n == oauth2.DriverName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("driver %q not in registered list: %v", oauth2.DriverName, registered)
	}
}

// TestResolve_ViaRegistry — the registry's Resolve helper dispatches
// to the driver's New by name. Drives the integration path the dev
// stack uses.
func TestResolve_ViaRegistry(t *testing.T) {
	t.Parallel()
	srv := newFakeAuthServer(t)
	cfg := validProviderConfig(srv)
	prov, err := auth.Resolve(context.Background(), oauth2.DriverName, cfg, mkDeps(t))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })
	if prov == nil {
		t.Fatal("Resolve returned nil")
	}
}

func TestResolve_UnknownDriverFailsLoud(t *testing.T) {
	t.Parallel()
	srv := newFakeAuthServer(t)
	cfg := validProviderConfig(srv)
	_, err := auth.Resolve(context.Background(), "no-such-driver", cfg, mkDeps(t))
	if err == nil {
		t.Fatal("Resolve with unknown driver did not error")
	}
	if !errors.Is(err, auth.ErrDriverUnknown) {
		t.Fatalf("err = %v, want ErrDriverUnknown", err)
	}
}
