package credsource_test

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools/auth/credsource"
	"github.com/hurtener/Harbor/internal/tools/auth/credsource/credsourcetest"

	// Register the env + remote sources so the factory (credsource.Resolve)
	// resolves them — the §4.4 seam, exercised end to end.
	_ "github.com/hurtener/Harbor/internal/tools/auth/credsource/drivers/env"
	_ "github.com/hurtener/Harbor/internal/tools/auth/credsource/drivers/remote"
)

// Documented dummy fixtures — never real secrets (§7 rule 2).
const (
	cDummyClientID     = "dummy-client-id-not-a-secret"
	cDummyClientSecret = "dummy-client-secret-not-a-secret"
	cDummyServiceToken = "dummy-runtime-service-token-not-a-secret"
	cAuthTokenEnv      = "HARBOR_CREDSOURCE_TEST_SERVICE_TOKEN"
)

// minimalToolsConfig returns a valid config carrying exactly one OAuth
// provider, for the allowlist drift guard. It starts from config.Defaults()
// (a valid base) and sets the required LLM + identity fields.
func minimalToolsConfig() *config.Config {
	cfg := config.Defaults()
	cfg.LLM.Provider = "openrouter"
	cfg.LLM.Model = "anthropic/claude-sonnet-4"
	cfg.LLM.APIKey = "env.FAKE_TEST_KEY" // documented dummy fixture value
	cfg.Identity = config.IdentityConfig{
		JWTAlgorithms: []string{"ES256"},
		Issuer:        "https://issuer.example.com",
		Audience:      "harbor",
		JWKSURL:       "https://issuer.example.com/.well-known/jwks.json",
	}
	cfg.Tools.OAuthTokenKEKEnv = "HARBOR_OAUTH_TOKEN_KEK"
	cfg.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{{
		Name:            "gh",
		Driver:          "oauth2",
		ClientIDEnv:     "GH_CLIENT_ID",
		ClientSecretEnv: "GH_CLIENT_SECRET",
		AuthURL:         "https://github.com/login/oauth/authorize",
		TokenURL:        "https://github.com/login/oauth/access_token",
		RedirectURL:     "https://example.com/oauth/callback",
	}}
	return cfg
}

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

func mkID() identity.Identity {
	return identity.Identity{TenantID: "tenant-A", UserID: "user-1", SessionID: "session-x"}
}

func mkCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), mkID())
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

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

// mkRemoteSource builds a `remote` source through the factory, pointed at
// the given fixture, with the service token exported in the env.
func mkRemoteSource(t *testing.T, srv *credsourcetest.FixtureServer, clock func() time.Time) credsource.Source {
	t.Helper()
	t.Setenv(cAuthTokenEnv, cDummyServiceToken)
	red := mkRedactor()
	src, err := credsource.Resolve(credsource.SourceRemote, credsource.Config{
		ProviderName: "fixture-provider",
		Remote:       &credsource.RemoteConfig{URL: srv.URL(), AuthTokenEnv: cAuthTokenEnv},
		Clock:        clock,
		Bus:          mkBus(t, red),
		Redactor:     red,
	})
	if err != nil {
		t.Fatalf("build remote source: %v", err)
	}
	return src
}

// ---------------------------------------------------------------------------
// Shared conformance suite — both drivers pass (resolve + concurrent
// consistency). The TTL + single-flight-exactly-one-fetch specifics are
// meaningful only for the remote source (env has no TTL and no fetch to
// collapse) and are pinned in the remote-specific tests below.
// ---------------------------------------------------------------------------

func runSourceConformance(t *testing.T, newSource func(t *testing.T) credsource.Source, want credsource.ClientCredential) {
	t.Helper()

	t.Run("ValidateAtBoot", func(t *testing.T) {
		if err := newSource(t).ValidateAtBoot(context.Background()); err != nil {
			t.Fatalf("ValidateAtBoot: %v", err)
		}
	})

	t.Run("Resolve", func(t *testing.T) {
		got, err := newSource(t).Resolve(mkCtx(t))
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.ClientID != want.ClientID || got.ClientSecret != want.ClientSecret {
			t.Fatalf("Resolve = %s/%s, want %s/%s", got.ClientID, got.ClientSecret, want.ClientID, want.ClientSecret)
		}
	})

	t.Run("ConcurrentResolveConsistent", func(t *testing.T) {
		src := newSource(t)
		if err := src.ValidateAtBoot(context.Background()); err != nil {
			t.Fatalf("ValidateAtBoot: %v", err)
		}
		const n = 128
		var wg sync.WaitGroup
		errs := make(chan error, n)
		ctx := mkCtx(t)
		for range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got, err := src.Resolve(ctx)
				if err != nil {
					errs <- err
					return
				}
				if got.ClientID != want.ClientID || got.ClientSecret != want.ClientSecret {
					errs <- errors.New("credential mismatch under concurrency")
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent Resolve: %v", err)
		}
	})
}

func TestConformance_EnvSource(t *testing.T) {
	newSource := func(t *testing.T) credsource.Source {
		t.Setenv("HARBOR_CONF_ENV_CLIENT_ID", cDummyClientID)
		t.Setenv("HARBOR_CONF_ENV_CLIENT_SECRET", cDummyClientSecret)
		src, err := credsource.Resolve(credsource.SourceEnv, credsource.Config{
			ProviderName:    "conf-env",
			ClientIDEnv:     "HARBOR_CONF_ENV_CLIENT_ID",
			ClientSecretEnv: "HARBOR_CONF_ENV_CLIENT_SECRET",
		})
		if err != nil {
			t.Fatalf("build env source: %v", err)
		}
		return src
	}
	runSourceConformance(t, newSource, credsource.ClientCredential{ClientID: cDummyClientID, ClientSecret: cDummyClientSecret})
}

func TestConformance_RemoteSource(t *testing.T) {
	newSource := func(t *testing.T) credsource.Source {
		srv := credsourcetest.New(t, cDummyServiceToken, cDummyClientID, cDummyClientSecret)
		return mkRemoteSource(t, srv, nil)
	}
	runSourceConformance(t, newSource, credsource.ClientCredential{ClientID: cDummyClientID, ClientSecret: cDummyClientSecret})
}

// ---------------------------------------------------------------------------
// env-source fail-loud: today's boot behavior is preserved (byte-compatible
// messages naming the offending field).
// ---------------------------------------------------------------------------

func TestEnvSource_ValidateAtBoot_FailsLoud(t *testing.T) {
	// client_id env unset → the historical message shape.
	src, err := credsource.Resolve(credsource.SourceEnv, credsource.Config{
		ProviderName:    "gh",
		ProviderIndex:   2,
		ClientIDEnv:     "HARBOR_CONF_MISSING_CLIENT_ID",
		ClientSecretEnv: "HARBOR_CONF_MISSING_CLIENT_SECRET",
	})
	if err != nil {
		t.Fatalf("build env source: %v", err)
	}
	err = src.ValidateAtBoot(context.Background())
	if err == nil {
		t.Fatal("ValidateAtBoot with unset client_id env did not fail")
	}
	for _, want := range []string{`provider "gh"`, "oauth_providers[2]", `"HARBOR_CONF_MISSING_CLIENT_ID"`, "client_id_env"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("env fail-loud message missing %q: %v", want, err)
		}
	}
}

// ---------------------------------------------------------------------------
// remote-source specifics: TTL/expiry, single-flight, failure legs,
// no-secret-bytes, shape-only boot validation.
// ---------------------------------------------------------------------------

// fakeClock is a mutex-guarded controllable clock (no time.Sleep for
// synchronisation — §11).
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

func TestRemote_CacheHit_NoRefetch(t *testing.T) {
	srv := credsourcetest.New(t, cDummyServiceToken, cDummyClientID, cDummyClientSecret)
	src := mkRemoteSource(t, srv, nil)
	ctx := mkCtx(t)
	for i := range 5 {
		if _, err := src.Resolve(ctx); err != nil {
			t.Fatalf("Resolve #%d: %v", i, err)
		}
	}
	if srv.Hits() != 1 {
		t.Fatalf("expected exactly 1 fetch (rest served from cache), got %d", srv.Hits())
	}
}

func TestRemote_TTLExpiry_Refetches(t *testing.T) {
	srv := credsourcetest.New(t, cDummyServiceToken, cDummyClientID, cDummyClientSecret)
	srv.SetExpiresIn(60) // 60s broker expiry
	clock := newFakeClock()
	src := mkRemoteSource(t, srv, clock.Now)
	ctx := mkCtx(t)

	if _, err := src.Resolve(ctx); err != nil {
		t.Fatalf("Resolve #1: %v", err)
	}
	if srv.Hits() != 1 {
		t.Fatalf("hits after first resolve = %d, want 1", srv.Hits())
	}
	// Within TTL → cache hit.
	clock.Advance(30 * time.Second)
	if _, err := src.Resolve(ctx); err != nil {
		t.Fatalf("Resolve #2: %v", err)
	}
	if srv.Hits() != 1 {
		t.Fatalf("hits within TTL = %d, want 1 (cache hit)", srv.Hits())
	}
	// Past expiry → refetch, and the rotated credential is picked up.
	srv.Rotate("rotated-client-id", "rotated-secret")
	clock.Advance(90 * time.Second)
	got, err := src.Resolve(ctx)
	if err != nil {
		t.Fatalf("Resolve #3: %v", err)
	}
	if srv.Hits() != 2 {
		t.Fatalf("hits after expiry = %d, want 2 (refetch)", srv.Hits())
	}
	if got.ClientID != "rotated-client-id" || got.ClientSecret != "rotated-secret" {
		t.Fatalf("post-rotation credential = %s/%s, want rotated", got.ClientID, got.ClientSecret)
	}
}

// TestRemote_SingleFlight_N128 is the mandatory D-025 concurrent-reuse
// stress: N≥100 concurrent first-time Resolve calls against ONE source
// with a slow fixture collapse onto EXACTLY ONE fetch, race-free, with
// goroutines returning to baseline.
func TestRemote_SingleFlight_N128(t *testing.T) {
	// Slow the fixture so the burst piles up on one in-flight fetch.
	slow := credsourcetest.NewSlow(t, cDummyServiceToken, cDummyClientID, cDummyClientSecret, 40*time.Millisecond)
	src := mkRemoteSource(t, slow, nil)
	ctx := mkCtx(t)

	baseline := runtime.NumGoroutine()
	const n = 128
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cred, err := src.Resolve(ctx)
			if err != nil {
				errs <- err
				return
			}
			if cred.ClientID != cDummyClientID {
				errs <- errors.New("credential mismatch")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Resolve: %v", err)
	}
	if slow.Hits() != 1 {
		t.Fatalf("single-flight violated: %d fetches for %d concurrent misses, want 1", slow.Hits(), n)
	}
	// Goroutines settle back to baseline (bounded wait, no sleep-as-sync).
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if g := runtime.NumGoroutine(); g > baseline+4 {
		t.Fatalf("goroutine leak: baseline=%d, after=%d", baseline, g)
	}
}

func TestRemote_FailureLegs_FailLoudWithEvent(t *testing.T) {
	cases := []struct {
		name    string
		posture credsourcetest.Posture
	}{
		{"unreachable", credsourcetest.PostureDown},
		{"malformed", credsourcetest.PostureMalformed},
		{"unauthorized", credsourcetest.PostureUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := credsourcetest.New(t, cDummyServiceToken, cDummyClientID, cDummyClientSecret)
			t.Setenv(cAuthTokenEnv, cDummyServiceToken)
			red := mkRedactor()
			bus := mkBus(t, red)
			ch := subscribe(t, bus, mkID())
			src, err := credsource.Resolve(credsource.SourceRemote, credsource.Config{
				ProviderName: "fixture-provider",
				Remote:       &credsource.RemoteConfig{URL: srv.URL(), AuthTokenEnv: cAuthTokenEnv},
				Bus:          bus,
				Redactor:     red,
			})
			if err != nil {
				t.Fatalf("build remote source: %v", err)
			}
			srv.SetPosture(tc.posture)

			_, err = src.Resolve(mkCtx(t))
			if !errors.Is(err, credsource.ErrCredentialSourceUnavailable) {
				t.Fatalf("want ErrCredentialSourceUnavailable, got %v", err)
			}
			ev := waitEvent(t, ch, credsource.EventTypeProviderCredentialFetchFailed)
			p, ok := ev.Payload.(credsource.ProviderCredentialFetchFailedPayload)
			if !ok {
				t.Fatalf("failure event payload type = %T", ev.Payload)
			}
			if p.Provider != "fixture-provider" {
				t.Fatalf("failure event provider = %q", p.Provider)
			}
		})
	}
}

// TestRemote_NoSecretBytesInError pins that a failure error names the
// provider + endpoint but NEVER the fetched credential or the service
// token (§7).
func TestRemote_NoSecretBytesInError(t *testing.T) {
	srv := credsourcetest.New(t, cDummyServiceToken, cDummyClientID, cDummyClientSecret)
	src := mkRemoteSource(t, srv, nil)
	srv.SetPosture(credsourcetest.PostureDown)
	_, err := src.Resolve(mkCtx(t))
	if err == nil {
		t.Fatal("expected failure")
	}
	msg := err.Error()
	if !strings.Contains(msg, "fixture-provider") {
		t.Fatalf("error must name the provider: %v", err)
	}
	for _, secret := range []string{cDummyServiceToken, cDummyClientSecret} {
		if strings.Contains(msg, secret) {
			t.Fatalf("SECRET LEAK: error string contains a secret: %v", err)
		}
	}
}

// TestRemote_FetchedEvent_NoSecretBytes pins the success event carries the
// provider + endpoint host + expiry but no credential bytes.
func TestRemote_FetchedEvent_NoSecretBytes(t *testing.T) {
	srv := credsourcetest.New(t, cDummyServiceToken, cDummyClientID, cDummyClientSecret)
	t.Setenv(cAuthTokenEnv, cDummyServiceToken)
	red := mkRedactor()
	bus := mkBus(t, red)
	ch := subscribe(t, bus, mkID())
	src, err := credsource.Resolve(credsource.SourceRemote, credsource.Config{
		ProviderName: "fixture-provider",
		Remote:       &credsource.RemoteConfig{URL: srv.URL(), AuthTokenEnv: cAuthTokenEnv},
		Bus:          bus,
		Redactor:     red,
	})
	if err != nil {
		t.Fatalf("build remote source: %v", err)
	}
	if _, err := src.Resolve(mkCtx(t)); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	ev := waitEvent(t, ch, credsource.EventTypeProviderCredentialFetched)
	b, _ := json.Marshal(ev.Payload)
	for _, secret := range []string{cDummyServiceToken, cDummyClientSecret, cDummyClientID} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("SECRET LEAK: fetched event carries credential bytes: %s", b)
		}
	}
}

func TestRemote_UnsupportedFormatVersion_FailsLoud(t *testing.T) {
	srv := credsourcetest.New(t, cDummyServiceToken, cDummyClientID, cDummyClientSecret)
	src := mkRemoteSource(t, srv, nil)
	srv.SetPosture(credsourcetest.PostureBadVersion)
	_, err := src.Resolve(mkCtx(t))
	if !errors.Is(err, credsource.ErrCredentialSourceUnavailable) {
		t.Fatalf("want ErrCredentialSourceUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "format_version") {
		t.Fatalf("error must name the version mismatch: %v", err)
	}
}

func TestRemote_MissingServiceToken_FailsLoud(t *testing.T) {
	srv := credsourcetest.New(t, cDummyServiceToken, cDummyClientID, cDummyClientSecret)
	// Do NOT export the service token env var.
	red := mkRedactor()
	src, err := credsource.Resolve(credsource.SourceRemote, credsource.Config{
		ProviderName: "fixture-provider",
		Remote:       &credsource.RemoteConfig{URL: srv.URL(), AuthTokenEnv: "HARBOR_CREDSOURCE_UNSET_TOKEN"},
		Bus:          mkBus(t, red),
		Redactor:     red,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, err = src.Resolve(mkCtx(t))
	if !errors.Is(err, credsource.ErrCredentialSourceUnavailable) {
		t.Fatalf("want ErrCredentialSourceUnavailable, got %v", err)
	}
	if srv.Hits() != 0 {
		t.Fatalf("must not hit the coordinator with no service token, got %d hits", srv.Hits())
	}
}

func TestRemote_Resolve_CancelledCtx(t *testing.T) {
	srv := credsourcetest.New(t, cDummyServiceToken, cDummyClientID, cDummyClientSecret)
	src := mkRemoteSource(t, srv, nil)
	ctx, cancel := context.WithCancel(mkCtx(t))
	cancel()
	if _, err := src.Resolve(ctx); err == nil {
		t.Fatal("Resolve on a cancelled ctx returned nil error")
	}
}

func TestRemote_ValidateAtBoot_ShapeOnly_NoFetch(t *testing.T) {
	srv := credsourcetest.New(t, cDummyServiceToken, cDummyClientID, cDummyClientSecret)
	src := mkRemoteSource(t, srv, nil)
	if err := src.ValidateAtBoot(context.Background()); err != nil {
		t.Fatalf("ValidateAtBoot on a well-formed block: %v", err)
	}
	if srv.Hits() != 0 {
		t.Fatalf("ValidateAtBoot must NOT fetch, got %d hits", srv.Hits())
	}
}

// TestRemote_HTTPRequiresLoopback pins the TLS rule at ValidateAtBoot:
// plaintext http is rejected for non-loopback hosts (the fetch carries
// the runtime's service bearer token) and accepted for the loopback
// carve-out (the fixture / dev case).
func TestRemote_HTTPRequiresLoopback(t *testing.T) {
	red := mkRedactor()
	build := func(t *testing.T, rawURL string) credsource.Source {
		t.Helper()
		src, err := credsource.Resolve(credsource.SourceRemote, credsource.Config{
			ProviderName: "p",
			Remote:       &credsource.RemoteConfig{URL: rawURL, AuthTokenEnv: cAuthTokenEnv},
			Bus:          mkBus(t, red),
			Redactor:     red,
		})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		return src
	}

	// Non-loopback http → loud rejection naming the rule.
	err := build(t, "http://coordinator.example.com/broker-credential").ValidateAtBoot(context.Background())
	if err == nil {
		t.Fatal("ValidateAtBoot(non-loopback http) returned nil, want error")
	}
	if !strings.Contains(err.Error(), "must be https for non-loopback hosts") {
		t.Fatalf("error must name the TLS rule: %v", err)
	}

	// Loopback http → accepted (fixture / dev carve-out).
	for _, u := range []string{
		"http://127.0.0.1:9099/cred",
		"http://localhost:9099/cred",
		"http://[::1]:9099/cred",
	} {
		if err := build(t, u).ValidateAtBoot(context.Background()); err != nil {
			t.Fatalf("ValidateAtBoot(%s): %v (loopback http must be accepted)", u, err)
		}
	}
}

// TestRemote_Redirect_RefusedLoud pins that the credential fetch is
// terminal: a 302 from the endpoint is refused (never followed — a
// followed redirect could replay the service bearer to an unconfigured
// host), fails with the typed sentinel, and emits the failure event.
func TestRemote_Redirect_RefusedLoud(t *testing.T) {
	srv := credsourcetest.New(t, cDummyServiceToken, cDummyClientID, cDummyClientSecret)
	t.Setenv(cAuthTokenEnv, cDummyServiceToken)
	red := mkRedactor()
	bus := mkBus(t, red)
	ch := subscribe(t, bus, mkID())
	src, err := credsource.Resolve(credsource.SourceRemote, credsource.Config{
		ProviderName: "fixture-provider",
		Remote:       &credsource.RemoteConfig{URL: srv.URL(), AuthTokenEnv: cAuthTokenEnv},
		Bus:          bus,
		Redactor:     red,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	srv.SetPosture(credsourcetest.PostureRedirect)

	_, err = src.Resolve(mkCtx(t))
	if !errors.Is(err, credsource.ErrCredentialSourceUnavailable) {
		t.Fatalf("want ErrCredentialSourceUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("error must name the refused redirect: %v", err)
	}
	// Exactly one request reached the endpoint — the redirect was NOT
	// followed (following it would loop back and register a second hit).
	if h := srv.Hits(); h != 1 {
		t.Fatalf("endpoint hits = %d, want 1 (redirect must not be followed)", h)
	}
	ev := waitEvent(t, ch, credsource.EventTypeProviderCredentialFetchFailed)
	if p, ok := ev.Payload.(credsource.ProviderCredentialFetchFailedPayload); !ok || p.Provider != "fixture-provider" {
		t.Fatalf("failure event payload = %#v", ev.Payload)
	}
}

// TestRemote_SingleFlight_CancelOneWaiter_OthersResolve pins the
// positive path of the cancellation detach: N callers collapse onto one
// slow fetch, ONE caller's ctx is cancelled mid-flight, and every other
// caller still resolves (the flight runs on a context.WithoutCancel
// detach, so a cancelled initiator/waiter never poisons the shared
// round-trip). The doomed caller is launched FIRST so it plausibly
// initiates the flight — the strongest variant of the guarantee.
func TestRemote_SingleFlight_CancelOneWaiter_OthersResolve(t *testing.T) {
	slow := credsourcetest.NewSlow(t, cDummyServiceToken, cDummyClientID, cDummyClientSecret, 150*time.Millisecond)
	src := mkRemoteSource(t, slow, nil)
	baseCtx := mkCtx(t)

	const n = 32
	cancelCtx, cancel := context.WithCancel(baseCtx)

	var wg sync.WaitGroup
	results := make(chan error, n)
	cancelled := make(chan error, 1)
	started := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)
		_, err := src.Resolve(cancelCtx)
		cancelled <- err
	}()
	<-started

	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cred, err := src.Resolve(baseCtx)
			if err != nil {
				results <- err
				return
			}
			if cred.ClientID != cDummyClientID {
				results <- errors.New("credential mismatch")
				return
			}
			results <- nil
		}()
	}

	// Cancel the doomed caller while the slow fetch is still in flight.
	cancel()
	wg.Wait()
	close(results)

	// The cancelled caller observed its own cancellation...
	if err := <-cancelled; err == nil {
		t.Fatal("cancelled waiter resolved nil error, want ctx cancellation")
	}
	// ...and every other caller still resolved.
	for err := range results {
		if err != nil {
			t.Fatalf("surviving caller failed: %v (a cancelled waiter must not poison the flight)", err)
		}
	}
	// The detach held: the endpoint saw exactly one fetch — the doomed
	// caller's cancellation neither aborted the round-trip nor forced a
	// second one for the survivors.
	if h := slow.Hits(); h != 1 {
		t.Fatalf("endpoint hits = %d, want 1 (single flight, detach preserved)", h)
	}
}

func TestRemote_ValidateAtBoot_RejectsBadShape(t *testing.T) {
	red := mkRedactor()
	cases := []struct {
		name   string
		remote credsource.RemoteConfig
	}{
		{"empty url", credsource.RemoteConfig{URL: "", AuthTokenEnv: cAuthTokenEnv}},
		{"bad scheme", credsource.RemoteConfig{URL: "ftp://x/y", AuthTokenEnv: cAuthTokenEnv}},
		{"no host", credsource.RemoteConfig{URL: "https://", AuthTokenEnv: cAuthTokenEnv}},
		{"non-loopback http", credsource.RemoteConfig{URL: "http://coord.example.com/c", AuthTokenEnv: cAuthTokenEnv}},
		{"empty auth token env", credsource.RemoteConfig{URL: "https://coord.example.com/c", AuthTokenEnv: ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc := tc.remote
			src, err := credsource.Resolve(credsource.SourceRemote, credsource.Config{
				ProviderName: "p", Remote: &rc, Bus: mkBus(t, red), Redactor: red,
			})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if err := src.ValidateAtBoot(context.Background()); err == nil {
				t.Fatalf("ValidateAtBoot(%s) returned nil, want error", tc.name)
			}
		})
	}
}

// TestRegisteredSourcesMatchConfigAllowlist is the D-285 allowlist drift
// guard: every registered source name is accepted by the config
// validator, and an unknown one is rejected pre-boot. The
// `internal/config` package MUST NOT import credsource (§4.4), so the two
// surfaces are duplicated by design; this test catches the drift.
func TestRegisteredSourcesMatchConfigAllowlist(t *testing.T) {
	registered := credsource.RegisteredSources()
	found := map[string]bool{}
	for _, n := range registered {
		found[n] = true
	}
	if !found[credsource.SourceEnv] || !found[credsource.SourceRemote] {
		t.Fatalf("expected env + remote registered, got %v", registered)
	}

	// env accepted.
	cfg := minimalToolsConfig()
	cfg.Tools.OAuthProviders[0].CredentialSource = "env"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate credential_source=env: %v", err)
	}

	// remote accepted (tokenexchange driver).
	cfg = minimalToolsConfig()
	cfg.Tools.OAuthProviders[0].Driver = "tokenexchange"
	cfg.Tools.OAuthProviders[0].CredentialSource = "remote"
	cfg.Tools.OAuthProviders[0].ClientIDEnv = ""
	cfg.Tools.OAuthProviders[0].ClientSecretEnv = ""
	cfg.Tools.OAuthProviders[0].TokenURL = "https://broker.example.com/token"
	cfg.Tools.OAuthProviders[0].Remote = &config.ToolOAuthRemoteConfig{
		URL: "https://coord.example.com/cred", AuthTokenEnv: "HARBOR_SVC_TOKEN",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate credential_source=remote: %v", err)
	}

	// unknown rejected.
	cfg = minimalToolsConfig()
	cfg.Tools.OAuthProviders[0].CredentialSource = "no-such-source-xyz"
	if err := cfg.Validate(); err == nil {
		t.Fatal("validate credential_source=no-such-source-xyz returned nil, want error")
	}
}

// TestConfigValidate_RemoteURL_HTTPRequiresLoopback pins the config-
// validator leg of the TLS rule: a non-loopback http remote.url fails
// `harbor validate` loud naming the field; a loopback http one passes
// (the fixture / dev carve-out). Twin of the driver-side
// TestRemote_HTTPRequiresLoopback.
func TestConfigValidate_RemoteURL_HTTPRequiresLoopback(t *testing.T) {
	remoteCfg := func(rawURL string) *config.Config {
		cfg := minimalToolsConfig()
		cfg.Tools.OAuthProviders[0].Driver = "tokenexchange"
		cfg.Tools.OAuthProviders[0].CredentialSource = "remote"
		cfg.Tools.OAuthProviders[0].ClientIDEnv = ""
		cfg.Tools.OAuthProviders[0].ClientSecretEnv = ""
		cfg.Tools.OAuthProviders[0].TokenURL = "https://broker.example.com/token"
		cfg.Tools.OAuthProviders[0].Remote = &config.ToolOAuthRemoteConfig{
			URL: rawURL, AuthTokenEnv: "HARBOR_SVC_TOKEN",
		}
		return cfg
	}

	// Non-loopback http → rejected, naming the field + the rule.
	err := remoteCfg("http://coordinator.example.com/broker-credential").Validate()
	if err == nil {
		t.Fatal("Validate(non-loopback http remote.url) returned nil, want error")
	}
	if !strings.Contains(err.Error(), "remote.url") || !strings.Contains(err.Error(), "must be https for non-loopback hosts") {
		t.Fatalf("error must name the field and the TLS rule: %v", err)
	}

	// Loopback http → accepted.
	for _, u := range []string{
		"http://127.0.0.1:9099/cred",
		"http://localhost:9099/cred",
		"http://[::1]:9099/cred",
	} {
		if err := remoteCfg(u).Validate(); err != nil {
			t.Fatalf("Validate(%s): %v (loopback http must pass)", u, err)
		}
	}
}
