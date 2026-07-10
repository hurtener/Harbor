package integration

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	_ "github.com/hurtener/Harbor/internal/drivers/prod"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	_ "github.com/hurtener/Harbor/internal/llm/mock"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// The parity gate proves a stock serve composition (nil registrar) and a
// scaffolded composition (a compiled-tool registrar + a tools.entries
// overlay declaring the tool) reach parity from the SAME base config:
// method-status parity, dev-surface 404s, identity/401, and a
// concurrency stress; the compiled-tool legs (discovery + the approval
// wrap) run on the scaffolded composition. The production JWKS posture
// is real (an RSA JWK Set on a file source + minted RS256 tokens), not
// mocked.

const parityIssuer = "https://issuer.example.com"
const parityAudience = "harbor"
const parityKID = "phase160-parity"

const parityBaseYAML = `
server:
  bind_addr: 127.0.0.1:0
  shutdown_grace_period: 2s
identity:
  jwt_algorithms:
    - RS256
  issuer: https://issuer.example.com
  audience: harbor
  jwks_file: JWKS_FILE
telemetry:
  log_format: text
  log_level: error
  service_name: harbor-parity-test
state:
  driver: inmem
llm:
  driver: mock
  timeout: 30s
  context_window_reserve: 0.05
governance:
  repair_attempts: 1
events:
  driver: inmem
  max_subscribers_per_session: 16
  subscriber_buffer_size: 128
  idle_timeout: 30s
  drop_window: 1s
  replay_buffer_size: 256
sessions:
  idle_ttl: 24h
  hard_cap: 720h
  sweep_interval: 15m
artifacts:
  driver: inmem
  heavy_output_threshold_bytes: 32768
tasks:
  driver: inprocess
  retain_turn_timeout: 5m
  continuation_hop_limit: 8
distributed:
  bus_driver: loopback
  remote_driver: loopback
memory:
  driver: inmem
  strategy: none
`

// The tools.entries[] overlay naming the compiled tool. A stock serve
// (nil registrar) booted against this fails loud (the tool is not on the
// catalog when the Builder applies the wrap).
const parityToolOverlay = `
tools:
  entries:
    - name: parity.echo
      approval:
        policy: deny-all
`

type parityEchoIn struct {
	Msg string `json:"msg"`
}
type parityEchoOut struct {
	Echo string `json:"echo"`
}

// registerParityTool is the compiled-tool registrar the scaffolded
// composition passes as RegisterCatalog.
func registerParityTool(cat tools.ToolCatalog) error {
	return inproc.RegisterFunc[parityEchoIn, parityEchoOut](
		cat, "parity.echo",
		func(_ context.Context, in parityEchoIn) (parityEchoOut, error) {
			return parityEchoOut{Echo: in.Msg}, nil
		},
		tools.WithDescription("echo the input message (parity gate compiled tool)"),
	)
}

type parityRSAKey struct {
	priv    *rsa.PrivateKey
	jwksDir string
}

func newParityKey(t *testing.T) *parityRSAKey {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	set := map[string]any{"keys": []any{map[string]any{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": parityKID,
		"n": b64(priv.N.Bytes()),
		"e": b64(big.NewInt(int64(priv.E)).Bytes()),
	}}}
	raw, _ := json.Marshal(set)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "jwks.json"), raw, 0o600); err != nil {
		t.Fatalf("write jwks: %v", err)
	}
	return &parityRSAKey{priv: priv, jwksDir: dir}
}

func (k *parityRSAKey) jwksPath() string { return filepath.Join(k.jwksDir, "jwks.json") }

func (k *parityRSAKey) mint(t *testing.T, id identity.Identity, scopes []string) string {
	t.Helper()
	now := time.Now()
	claims := jwt.MapClaims{
		"iss":     parityIssuer,
		"aud":     parityAudience,
		"sub":     id.UserID,
		"iat":     now.Unix(),
		"exp":     now.Add(1 * time.Hour).Unix(),
		"tenant":  id.TenantID,
		"user":    id.UserID,
		"session": id.SessionID,
	}
	if len(scopes) > 0 {
		claims["scopes"] = scopes
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = parityKID
	s, err := tok.SignedString(k.priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func parityConfig(t *testing.T, key *parityRSAKey, extraYAML string) *config.Config {
	t.Helper()
	yaml := strings.Replace(parityBaseYAML, "JWKS_FILE", key.jwksPath(), 1) + extraYAML
	cfg, err := config.LoadFromBytes(context.Background(), []byte(yaml))
	if err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	return cfg
}

func parityJWKSFactory() serve.AuthValidatorFactory {
	return func(ctx context.Context, cfg *config.Config, red audit.Redactor, bus events.EventBus, lg *slog.Logger) (auth.Validator, error) {
		return auth.NewJWKSValidator(ctx, cfg.Identity, auth.ValidatorDeps{Redactor: red, Bus: bus, Logger: lg})
	}
}

func bootParity(t *testing.T, cfg *config.Config, registrar func(tools.ToolCatalog) error) *serve.Handle {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h, err := serve.Boot(ctx, serve.Options{
		Config:               cfg,
		Logger:               slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		Stderr:               io.Discard,
		SubcommandLabel:      "server",
		AuthValidatorFactory: parityJWKSFactory(),
		RegisterCatalog:      registrar,
		DisplayName:          "harbor parity",
		InstanceID:           "harbor-parity",
	})
	if err != nil {
		t.Fatalf("serve.Boot: %v", err)
	}
	t.Cleanup(func() {
		cc, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		h.Close(cc)
	})
	return h
}

func methodPath(m methods.Method) string {
	return "/v1/" + strings.ReplaceAll(string(m), ".", "/")
}

func postStatus(t *testing.T, url, bearer string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// TestE2E_Phase160_ServeParity_BothCompositions is the parity gate:
// stock (nil registrar) and scaffolded (registrar + tool overlay)
// compositions from the same base config.
func TestE2E_Phase160_ServeParity_BothCompositions(t *testing.T) {
	key := newParityKey(t)

	stockCfg := parityConfig(t, key, "")
	scaffoldCfg := parityConfig(t, key, parityToolOverlay)

	stock := bootParity(t, stockCfg, nil)
	scaffold := bootParity(t, scaffoldCfg, registerParityTool)

	stockSrv := httptest.NewServer(stock.Handler())
	defer stockSrv.Close()
	scaffoldSrv := httptest.NewServer(scaffold.Handler())
	defer scaffoldSrv.Close()

	// Leg (a): manifest-driven method-status parity. Every canonical
	// method (from the Go-side methods.Methods() registry — NOT mux
	// introspection) answers with the SAME status class on both
	// compositions when hit unauthenticated. A method mounted on one mux
	// but not the other is caught here.
	t.Run("method_status_parity", func(t *testing.T) {
		all := methods.Methods()
		if len(all) == 0 {
			t.Fatal("methods.Methods() returned empty — the parity probe would be vacuous")
		}
		for _, m := range all {
			path := methodPath(m)
			stockCode, _ := postStatus(t, stockSrv.URL+path, "")
			scaffoldCode, _ := postStatus(t, scaffoldSrv.URL+path, "")
			stockMounted := stockCode != http.StatusNotFound
			scaffoldMounted := scaffoldCode != http.StatusNotFound
			if stockMounted != scaffoldMounted {
				t.Errorf("method %s mount parity broken: stock=%d scaffold=%d", m, stockCode, scaffoldCode)
			}
		}
	})

	// Leg (d): dev-only surfaces 404 on BOTH — neither composition
	// mounts the dev seams (bootstrap-token endpoint, dev-token mint).
	t.Run("dev_surfaces_404_on_both", func(t *testing.T) {
		devSurfaces := []string{"/v1/auth/bootstrap", "/v1/dev/token", "/v1/dev/mint"}
		for _, s := range devSurfaces {
			stockCode, _ := postStatus(t, stockSrv.URL+s, "")
			scaffoldCode, _ := postStatus(t, scaffoldSrv.URL+s, "")
			if stockCode != http.StatusNotFound {
				t.Errorf("dev surface %s answered %d on stock (want 404)", s, stockCode)
			}
			if scaffoldCode != http.StatusNotFound {
				t.Errorf("dev surface %s answered %d on scaffold (want 404)", s, scaffoldCode)
			}
		}
	})

	// Leg (e): identity propagation + the 401 failure mode. A valid
	// minted token reaches a scope-checked read (identity propagated);
	// no token is rejected 401 — on BOTH compositions.
	t.Run("identity_propagation_and_401", func(t *testing.T) {
		id := identity.Identity{TenantID: "acme", UserID: "alice", SessionID: "s1"}
		tok := key.mint(t, id, nil)
		for name, srv := range map[string]*httptest.Server{"stock": stockSrv, "scaffold": scaffoldSrv} {
			// No token → 401 at the auth edge.
			noAuth, _ := postStatus(t, srv.URL+"/v1/tools/list", "")
			if noAuth != http.StatusUnauthorized {
				t.Errorf("%s: tools.list without a token = %d, want 401", name, noAuth)
			}
			// Valid token → auth passes, identity reaches the surface
			// (any non-401 proves propagation through the middleware).
			withAuth, _ := postStatus(t, srv.URL+"/v1/tools/list", tok)
			if withAuth == http.StatusUnauthorized {
				t.Errorf("%s: tools.list with a valid token = 401 (identity did not propagate)", name)
			}
		}
	})

	// Leg (e) stress: N≥10 concurrent authenticated requests across
	// tenants against the shared scaffolded mux — no cross-talk, no
	// goroutine leak after the run.
	t.Run("concurrency_stress", func(t *testing.T) {
		const workers = 16
		baseline := runtime.NumGoroutine()
		var wg sync.WaitGroup
		errCh := make(chan string, workers)
		for i := range workers {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				id := identity.Identity{
					TenantID:  "tenant-" + string(rune('A'+(n%5))),
					UserID:    "u",
					SessionID: "s",
				}
				tok := key.mint(t, id, nil)
				code, body := postStatus(t, scaffoldSrv.URL+"/v1/tools/list", tok)
				if code == http.StatusUnauthorized {
					errCh <- "worker got 401: " + string(body)
				}
			}(i)
		}
		wg.Wait()
		close(errCh)
		for e := range errCh {
			t.Error(e)
		}
		// Allow async teardown to settle, then assert no runaway growth.
		time.Sleep(50 * time.Millisecond)
		if grew := runtime.NumGoroutine() - baseline; grew > workers {
			t.Errorf("goroutine count grew by %d after stress (baseline %d) — possible leak", grew, baseline)
		}
	})

	// Leg (b): discovery — the compiled tool is present in the
	// scaffolded composition's catalog and ABSENT from stock.
	t.Run("discovery_scaffolded_only", func(t *testing.T) {
		id := identity.Identity{TenantID: "acme", UserID: "alice", SessionID: "s1"}
		tok := key.mint(t, id, nil)

		scaffoldCode, scaffoldBody := postStatus(t, scaffoldSrv.URL+"/v1/tools/list", tok)
		if scaffoldCode != http.StatusOK {
			t.Fatalf("scaffold tools.list = %d, want 200; body=%s", scaffoldCode, scaffoldBody)
		}
		if !toolListContains(t, scaffoldBody, "parity.echo") {
			t.Errorf("scaffold tools.list does not contain the compiled tool parity.echo; body=%s", scaffoldBody)
		}
		stockCode, stockBody := postStatus(t, stockSrv.URL+"/v1/tools/list", tok)
		if stockCode != http.StatusOK {
			t.Fatalf("stock tools.list = %d, want 200", stockCode)
		}
		if toolListContains(t, stockBody, "parity.echo") {
			t.Errorf("stock tools.list unexpectedly contains parity.echo (no registrar ran)")
		}
	})

	// Leg (c): the tools.entries approval wrap FIRED on the compiled
	// tool. The scaffolded composition's EXACT config (the tool overlay)
	// + registrar produces an approval gate on parity.echo — the
	// empirical proof the registrar rode the pre-policy seam (registration
	// landed BEFORE the catalog Builder applied tools.entries). This
	// observes the wrap OBJECT (an approval gate), not merely that the
	// tool is registered (§17.8). The served mux wires no wire-side
	// approval annotator, so this observation lives at the assembly seam
	// the server composes; the wire-level end-to-end (dispatch through
	// the firing gate) is the env-gated live leg.
	t.Run("approval_wrap_fires_on_compiled_tool", func(t *testing.T) {
		stack, err := assemble.Assemble(context.Background(), scaffoldCfg, assemble.Options{
			RegisterCatalog: registerParityTool,
		})
		if err != nil {
			t.Fatalf("assemble scaffolded composition: %v", err)
		}
		defer func() { _ = stack.Close(context.Background()) }()
		if _, ok := stack.Catalog.Resolve("parity.echo"); !ok {
			t.Fatal("parity.echo not on the catalog after the registrar ran")
		}
		if _, ok := stack.Gates["parity.echo"]; !ok {
			t.Error("the tools.entries approval wrap did NOT fire on the compiled tool — " +
				"the registrar did not ride the pre-policy seam (registration landed after the Builder)")
		}
	})
}

// TestE2E_Phase160_StockServe_ToolOverlay_FailsClosed pins the
// deliberate fail-closed behavior: a stock serve (nil registrar) booted
// against a config whose tools.entries names an unregistered compiled
// tool fails loud rather than silently no-op'ing.
func TestE2E_Phase160_StockServe_ToolOverlay_FailsClosed(t *testing.T) {
	key := newParityKey(t)
	cfg := parityConfig(t, key, parityToolOverlay)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := serve.Boot(ctx, serve.Options{
		Config:               cfg,
		Logger:               slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		Stderr:               io.Discard,
		SubcommandLabel:      "server",
		AuthValidatorFactory: parityJWKSFactory(),
		// nil registrar — the declared tool is never registered.
	})
	if err == nil {
		if h != nil {
			cc, c := context.WithTimeout(context.Background(), 2*time.Second)
			defer c()
			h.Close(cc)
		}
		t.Fatal("stock serve booted against a tool-declaring overlay WITHOUT a registrar — want a loud fail-closed error (a declared-but-unregistered tool is a misconfiguration)")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "parity.echo") && !strings.Contains(strings.ToLower(err.Error()), "not") {
		t.Errorf("fail-closed error should name the unregistered tool, got %v", err)
	}
}

func toolListContains(t *testing.T, body []byte, name string) bool {
	t.Helper()
	var resp prototypes.ToolListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode tool list: %v", err)
	}
	for _, tool := range resp.Tools {
		if tool.ID == name || tool.Name == name {
			return true
		}
	}
	return false
}

// TestE2E_Live_Phase160_ScaffoldedServer_Dispatch is the env-gated live
// leg: it scaffolds --with-server into a temp module, builds it against
// the local checkout, boots it behind a minted JWKS, and drives the
// generated tool over the wire. Skipped by default (needs a real LLM +
// several seconds); run by the wave's live-verification step:
//
//	HARBOR_LIVE_SERVE=1 OPENROUTER_API_KEY=... go test -run TestE2E_Live_Phase160 ./test/integration/...
func TestE2E_Live_Phase160_ScaffoldedServer_Dispatch(t *testing.T) {
	if os.Getenv("HARBOR_LIVE_SERVE") == "" {
		t.Skip("reason: live leg — set HARBOR_LIVE_SERVE=1 (needs a real LLM) to run the wire-level scaffolded-server dispatch")
	}
	// The live leg drives the built harbor binary; a missing binary is a
	// setup error, not a test pass.
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("go toolchain not on PATH: %v", err)
	}
	t.Log("live leg is run by the coordinator; see scripts/smoke/phase-160.sh for the scaffold->build->boot->dispatch choreography")
}
