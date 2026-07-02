// Phase 149 — the HTTP-manifest boot loader (RFC §6.4 + §3.4; D-279).
//
// This test wires the REAL artifacts across the phase surface — no
// mocks at any seam (CLAUDE.md §17.3 #1):
//
//   - `assemble.Assemble` boots a real Stack (state/events/artifacts/
//     tasks/sessions all real in-mem drivers) from a *config.Config
//     declaring `tools.http_manifests`;
//   - the real `http.LoadManifest` / `http.RegisterManifest` loader
//     registers the manifest's tool on the real catalog;
//   - the round trip hits a real `httptest.Server` fixture;
//   - the OAuth-wrapped variant drives the real `tools/catalog`
//     `WrapWithOAuth` pre-check against the real `tokenexchange`
//     driver and the shipped Phase 142 RFC-8693 broker fixture
//     (`p142Broker` — reused verbatim per §17.8: the fixture already
//     derives from the real wire spec, so a second hand-rolled broker
//     would just be a second interpretation to drift from).
//
// It exercises: the tool resolving from the catalog with its
// `manifest:<name>` provenance stamp intact; an ordinary (non-OAuth)
// invoke round-tripping against the fixture server with the
// manifest's own static auth header, and failing loud when ctx
// carries no identity; the OAuth-wrapped variant pre-checking the
// broker (exchange observed broker-side, identity triple asserted in
// the exchange) before the HTTP round trip; two boot failure modes —
// a missing manifest file and an entry naming an unknown OAuth
// provider — both failing `Assemble` loudly, naming the file / config
// key; and a D-025 concurrent-reuse stress run (N=128) through ONE
// shared catalog.
//
// CLAUDE.md §17.1: this phase consumes Phase 27 (the HTTP manifest
// loader), Phase 64a (the catalog Builder + WrapWithOAuth), and Phase
// 110d (`assemble.Assemble`) — an integration test is mandatory.
// §17.3: real drivers + identity propagation + ≥2 failure modes + run
// under -race. `harbortest/devstack` boots the identical config shape
// with no devstack-side change because both the binary and devstack
// are thin wrappers over `assemble.Assemble` (D-196/D-197) — this
// test exercises that ONE wiring home directly.
package integration

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/catalog"
	httpdrv "github.com/hurtener/Harbor/internal/tools/drivers/http"
)

// p149Provider names the OAuth provider bound to the manifest tool in
// the OAuth-wrapped test.
const p149Provider = "weather-broker"

// p149WeatherServer is the fixture HTTP server the manifest's
// `url_template` points at (brief 03's "HTTP tool against
// httptest.Server" testing-strategy precedent).
type p149WeatherServer struct {
	server *httptest.Server

	mu       sync.Mutex
	calls    int
	lastCity string
	lastKey  string
}

func newP149WeatherServer(t *testing.T) *p149WeatherServer {
	t.Helper()
	s := &p149WeatherServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/weather", s.handle)
	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

func (s *p149WeatherServer) handle(w http.ResponseWriter, r *http.Request) {
	city := r.URL.Query().Get("city")
	key := r.Header.Get("X-API-Key")
	s.mu.Lock()
	s.calls++
	s.lastCity = city
	s.lastKey = key
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"temp_c": 21.5,
		"city":   city,
	})
}

func (s *p149WeatherServer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// p149WriteManifest writes a UTCP-style manifest declaring one
// `weather.lookup` HTTP tool whose `url_template` points at the
// fixture server and whose static auth secret comes from an
// `${ENV_VAR}` reference (t.Setenv'd here — a dummy value, §7 rule 2).
func p149WriteManifest(t *testing.T, weatherURL string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "weather.yaml")
	const envVar = "HARBOR_P149_WEATHER_API_KEY"
	t.Setenv(envVar, "dummy-weather-key-not-a-secret")
	content := fmt.Sprintf(`auth:
  weather_key:
    kind: api_key
    header: X-API-Key
    secret: ${%s}
tools:
  - name: weather.lookup
    method: GET
    url_template: %s/weather?city={{ .Args.city | urlquery }}
    description: Look up current weather by city name.
    auth_ref: weather_key
    args_schema: '{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}'
`, envVar, weatherURL)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

// p149BaseConfig returns a *config.Config that passes ValidateCore
// with NO LLM driver configured (Assemble's LLM band is skipped
// entirely when Driver is empty — dummy Provider/Model/APIKey satisfy
// the validator's non-mock-driver field requirements without ever
// being read) and the given manifest declared.
func p149BaseConfig(manifestPath string) *config.Config {
	cfg := config.Defaults()
	cfg.LLM.Driver = ""
	cfg.LLM.Provider = "openrouter"
	cfg.LLM.Model = "anthropic/claude-sonnet-4"
	cfg.LLM.APIKey = "sk-p149-dummy-not-a-secret"
	cfg.LLM.Timeout = 5 * time.Second
	cfg.Tools.HTTPManifests = []string{manifestPath}
	return cfg
}

// p149KEKEnv sets a fresh dummy 32-byte hex KEK env var (§7 rule 2 —
// never a real secret) and returns its name.
func p149KEKEnv(t *testing.T) string {
	t.Helper()
	kek := make([]byte, auth.KEKSizeBytes)
	for i := range kek {
		kek[i] = byte(i*11 + 5)
	}
	const envName = "HARBOR_P149_OAUTH_KEK"
	t.Setenv(envName, hex.EncodeToString(kek))
	return envName
}

// TestE2E_Phase149_HTTPManifestBoot_ResolveAndInvoke proves the boot
// loader registers the manifest tool with its provenance stamp intact
// and that an ordinary invoke round-trips against the fixture server
// through the manifest's own static auth. Identity is asserted
// mandatory (ctx with none fails loud).
func TestE2E_Phase149_HTTPManifestBoot_ResolveAndInvoke(t *testing.T) {
	weather := newP149WeatherServer(t)
	manifestPath := p149WriteManifest(t, weather.server.URL)

	cfg := p149BaseConfig(manifestPath)
	if err := cfg.ValidateCore(); err != nil {
		t.Fatalf("ValidateCore: %v", err)
	}

	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{SkipSteering: true})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	t.Cleanup(func() { _ = stack.Close(context.Background()) })

	desc, ok := stack.Catalog.Resolve("weather.lookup")
	if !ok {
		t.Fatal("weather.lookup did not register from the manifest")
	}
	if desc.Tool.Source != "manifest:weather.lookup" {
		t.Errorf("Tool.Source = %q, want manifest:weather.lookup (provenance)", desc.Tool.Source)
	}

	id := identity.Identity{TenantID: "tenant-1", UserID: "alice", SessionID: "s1"}
	ctx := p142Ctx(t, id)
	res, err := desc.Invoke(ctx, json.RawMessage(`{"city":"NYC"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	m, ok := res.Value.(map[string]any)
	if !ok || m["city"] != "NYC" {
		t.Fatalf("unexpected round-trip result: %+v", res.Value)
	}
	if got := weather.callCount(); got != 1 {
		t.Fatalf("fixture server calls = %d, want 1", got)
	}
	if weather.lastCity != "NYC" {
		t.Errorf("fixture saw city=%q, want NYC (args did not propagate)", weather.lastCity)
	}
	if weather.lastKey != "dummy-weather-key-not-a-secret" {
		t.Errorf("fixture saw X-API-Key=%q, want the manifest's static auth secret", weather.lastKey)
	}

	// Identity is mandatory (CLAUDE.md §6 rule 9): no identity on ctx
	// fails loud rather than silently invoking with an empty triple.
	if _, err := desc.Invoke(context.Background(), json.RawMessage(`{"city":"NYC"}`)); !errors.Is(err, httpdrv.ErrIdentityMissing) {
		t.Errorf("Invoke without identity: got %v, want wrapping http.ErrIdentityMissing", err)
	}
}

// TestE2E_Phase149_HTTPManifestBoot_OAuthWrapped_PreChecksBroker is
// the §13 primitive-with-consumer proof: a manifest-registered tool
// bound via `tools.entries[].oauth` to the D-271 `tokenexchange`
// driver pre-checks the REAL broker fixture (exchange observed
// broker-side, identity triple asserted in the exchange) before the
// HTTP round trip fires.
func TestE2E_Phase149_HTTPManifestBoot_OAuthWrapped_PreChecksBroker(t *testing.T) {
	weather := newP149WeatherServer(t)
	manifestPath := p149WriteManifest(t, weather.server.URL)
	broker := newP142Broker(t)

	cfg := p149BaseConfig(manifestPath)
	kekEnv := p149KEKEnv(t)
	cfg.Tools.OAuthTokenKEKEnv = kekEnv

	const clientIDEnv = "HARBOR_P149_CLIENT_ID"
	const clientSecretEnv = "HARBOR_P149_CLIENT_SECRET"
	t.Setenv(clientIDEnv, p142BrokerClient)
	t.Setenv(clientSecretEnv, p142BrokerSecret)

	cfg.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{{
		Name:            p149Provider,
		Driver:          "tokenexchange",
		ClientIDEnv:     clientIDEnv,
		ClientSecretEnv: clientSecretEnv,
		TokenURL:        broker.tokenURL(),
		Extra:           map[string]string{"audience": p142Audience},
	}}
	cfg.Tools.Entries = []config.ToolEntryConfig{{
		Name:  "weather.lookup",
		OAuth: &config.ToolOAuthConfig{Provider: p149Provider, BindingScope: "user"},
	}}
	if err := cfg.ValidateCore(); err != nil {
		t.Fatalf("ValidateCore: %v", err)
	}

	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{SkipSteering: true})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	t.Cleanup(func() { _ = stack.Close(context.Background()) })

	desc, ok := stack.Catalog.Resolve("weather.lookup")
	if !ok {
		t.Fatal("weather.lookup did not register from the manifest")
	}

	id := identity.Identity{TenantID: "tenant-1", UserID: "bob", SessionID: "s2"}
	sub := p142Subscribe(t, stack.Bus, id)
	ctx := p142Ctx(t, id)

	res, err := desc.Invoke(ctx, json.RawMessage(`{"city":"LA"}`))
	if err != nil {
		t.Fatalf("Invoke (oauth-wrapped): %v", err)
	}
	if got := broker.callCount(); got != 1 {
		t.Fatalf("expected 1 broker exchange, got %d", got)
	}
	ev := p142WaitEvent(t, sub, auth.EventTypeToolCredentialExchanged)
	payload := ev.Payload.(auth.ToolCredentialExchangedPayload)
	if payload.BindingScope != "user" {
		t.Errorf("payload.BindingScope = %q, want user", payload.BindingScope)
	}
	if ev.Identity.TenantID != "tenant-1" || ev.Identity.UserID != "bob" {
		t.Fatalf("exchange identity mismatch: %+v", ev.Identity)
	}
	m, ok := res.Value.(map[string]any)
	if !ok || m["city"] != "LA" {
		t.Fatalf("HTTP round trip result unexpected: %+v", res.Value)
	}
	if got := weather.callCount(); got != 1 {
		t.Fatalf("fixture server calls = %d, want exactly 1 (the pre-check must never itself call the weather API)", got)
	}
}

// TestE2E_Phase149_HTTPManifestBoot_MissingManifestFailsLoud —
// failure mode (a): a manifest that does not exist on disk fails
// `Assemble` loudly, naming the file and the config key. `ValidateCore`
// itself accepts the entry (the validator stays I/O-free; existence
// is boot's job).
func TestE2E_Phase149_HTTPManifestBoot_MissingManifestFailsLoud(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	cfg := p149BaseConfig(missing)
	if err := cfg.ValidateCore(); err != nil {
		t.Fatalf("ValidateCore unexpectedly rejected a not-yet-existing manifest path: %v", err)
	}

	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{SkipSteering: true})
	if stack != nil {
		_ = stack.Close(context.Background())
	}
	if err == nil {
		t.Fatal("Assemble accepted a missing manifest file")
	}
	if !strings.Contains(err.Error(), "tools.http_manifests[0]") {
		t.Errorf("err missing config key: %v", err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("err missing offending file path: %v", err)
	}
	if !errors.Is(err, httpdrv.ErrManifestInvalid) {
		t.Errorf("err = %v, want wrapping http.ErrManifestInvalid", err)
	}
}

// TestE2E_Phase149_HTTPManifestBoot_UnknownOAuthProviderFailsLoud —
// failure mode (b): an entry naming an OAuth provider that never got
// declared fails `Assemble` loudly through the existing catalog
// Builder path (`catalog.ErrUnknownOAuthProvider`). The provider name
// is set AFTER `ValidateCore` (which already cross-checks
// `entries[].oauth.provider` against `tools.oauth_providers[]` at
// config-validate time) to prove Assemble's OWN catalog-wiring seam
// fails loud independently — the shape a hand-built or post-load-
// mutated *config.Config can still reach Assemble in.
func TestE2E_Phase149_HTTPManifestBoot_UnknownOAuthProviderFailsLoud(t *testing.T) {
	weather := newP149WeatherServer(t)
	manifestPath := p149WriteManifest(t, weather.server.URL)

	cfg := p149BaseConfig(manifestPath)
	if err := cfg.ValidateCore(); err != nil {
		t.Fatalf("ValidateCore: %v", err)
	}
	cfg.Tools.Entries = []config.ToolEntryConfig{{
		Name:  "weather.lookup",
		OAuth: &config.ToolOAuthConfig{Provider: "does-not-exist", BindingScope: "user"},
	}}

	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{SkipSteering: true})
	if stack != nil {
		_ = stack.Close(context.Background())
	}
	if err == nil {
		t.Fatal("Assemble accepted an entry naming an unknown OAuth provider")
	}
	if !strings.Contains(err.Error(), "tools/catalog:") {
		t.Errorf("err missing stage prefix: %v", err)
	}
	if !errors.Is(err, catalog.ErrUnknownOAuthProvider) {
		t.Errorf("err = %v, want wrapping catalog.ErrUnknownOAuthProvider", err)
	}
}

// TestE2E_Phase149_HTTPManifestBoot_ConcurrentReuse is the D-025
// concurrent-reuse gate: N=128 concurrent invocations of ONE
// manifest-registered tool through a single shared catalog, asserting
// no cross-talk (each goroutine's own city argument round-trips
// intact) and that the goroutine baseline is restored after Close.
func TestE2E_Phase149_HTTPManifestBoot_ConcurrentReuse(t *testing.T) {
	weather := newP149WeatherServer(t)
	manifestPath := p149WriteManifest(t, weather.server.URL)

	cfg := p149BaseConfig(manifestPath)
	if err := cfg.ValidateCore(); err != nil {
		t.Fatalf("ValidateCore: %v", err)
	}

	baseline := runtime.NumGoroutine()
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{SkipSteering: true})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	desc, ok := stack.Catalog.Resolve("weather.lookup")
	if !ok {
		t.Fatal("weather.lookup did not register from the manifest")
	}

	const n = 128
	var wg sync.WaitGroup
	var failures atomic.Int64
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  "tenant-1",
				UserID:    fmt.Sprintf("user-%d", i%8),
				SessionID: fmt.Sprintf("session-%d", i),
			}
			ctx, idErr := identity.With(context.Background(), id)
			if idErr != nil {
				failures.Add(1)
				return
			}
			city := fmt.Sprintf("city-%d", i)
			res, invErr := desc.Invoke(ctx, json.RawMessage(fmt.Sprintf(`{"city":%q}`, city)))
			if invErr != nil {
				failures.Add(1)
				return
			}
			m, ok := res.Value.(map[string]any)
			if !ok || m["city"] != city {
				failures.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := failures.Load(); got != 0 {
		t.Fatalf("%d/%d concurrent invocations failed or observed cross-talk", got, n)
	}
	if got := weather.callCount(); got != n {
		t.Fatalf("fixture server saw %d calls, want %d", got, n)
	}

	if err := stack.Close(context.Background()); err != nil {
		t.Errorf("Close: %v", err)
	}
	// The HTTP driver's default client (no WithClient override in this
	// manifest) pools connections on http.DefaultTransport; 128 requests
	// leave idle keep-alive reader goroutines that only die on their own
	// idle timeout. Force them closed so the baseline check measures
	// runtime-owned goroutines, not net/http connection-pool churn.
	if tr, ok := http.DefaultTransport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && runtime.NumGoroutine() > baseline+2 {
		runtime.Gosched()
		time.Sleep(5 * time.Millisecond)
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 2 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
	}
}
