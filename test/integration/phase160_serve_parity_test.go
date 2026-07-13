package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/config"
	_ "github.com/hurtener/Harbor/internal/drivers/prod"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	_ "github.com/hurtener/Harbor/internal/llm/mock"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	"github.com/hurtener/Harbor/internal/runtime/serve/external"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/approval"
	toolscatalog "github.com/hurtener/Harbor/internal/tools/catalog"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// The parity gate proves a stock serve composition (nil registrar, the
// serve subcommand's exact posture) and a scaffolded composition (booted
// through external.Open — the path an sdk/server binary ships: the
// production JWKS factory, PreferConfigBindAddr, the RegisterCatalog
// forward) reach parity from the SAME base config: method-status parity,
// dev-surface 404s, identity/401, and a concurrency stress; the
// compiled-tool legs (discovery, the scripted-LLM dispatch, the approval
// gate FIRING on dispatch) run on the scaffolded composition. The
// production JWKS posture is real (an RSA JWK Set on a file source +
// minted RS256 tokens), not mocked.

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

// The tools overlay the scaffolded composition boots against: a
// tools.entries[] wrap naming the compiled tool, plus a `built_in`
// declaration. A stock serve (nil registrar) booted against this fails
// loud (the compiled tool is not on the catalog when the Builder applies
// the wrap).
//
// The `built_in` entry is load-bearing (v1.13.1): built-ins are
// CONFIG-driven and the RUNTIME registers them at boot (with their
// backing stores). A registrar that ALSO registered them handed the
// catalog the same name twice and the scaffolded binary died with
// `duplicate tool name: clock.now` — so the two registration paths must
// coexist here, and BOTH tools must reach the served catalog.
const parityToolOverlay = `
tools:
  built_in:
    - clock.now
  entries:
    - name: parity.echo
      approval:
        policy: deny-all
`

// parityEchoResultPrefix is the compiled handler's fixture marker. It
// appears in tool OBSERVATIONS only (never in a goal/query), so its
// presence in a follow-up LLM prompt proves the dispatched result
// round-tripped through the executor into the trajectory.
const parityEchoResultPrefix = "PARITY_ECHO_RESULT:"

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
			return parityEchoOut{Echo: parityEchoResultPrefix + in.Msg}, nil
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
		"e": b64(bigEndianInt(priv.E)),
	}}}
	raw, _ := json.Marshal(set)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "jwks.json"), raw, 0o600); err != nil {
		t.Fatalf("write jwks: %v", err)
	}
	return &parityRSAKey{priv: priv, jwksDir: dir}
}

// bigEndianInt encodes a small positive int (the RSA public exponent)
// big-endian without leading zeros.
func bigEndianInt(v int) []byte {
	var b []byte
	for v > 0 {
		b = append([]byte{byte(v & 0xff)}, b...)
		v >>= 8
	}
	return b
}

func (k *parityRSAKey) jwksPath() string { return filepath.Join(k.jwksDir, "jwks.json") }

func (k *parityRSAKey) mint(t *testing.T, id identity.Identity) string {
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

// parityScriptedConfig builds the scripted-LLM variant: the same base
// posture (jwks_file identity, real inmem drivers) with the bifrost LLM
// driver pointed at the scripted OpenAI-compatible server — the same
// pattern the real-LLM trajectory tests use, so the run loop drives REAL
// tool calls in CI without a network provider.
func parityScriptedConfig(t *testing.T, key *parityRSAKey, llmServerURL, extraYAML string) *config.Config {
	t.Helper()
	const envKey = "HARBOR_TEST_160_FAKE_KEY"
	t.Setenv(envKey, "test-key-value")
	yaml := fmt.Sprintf(`
server:
  bind_addr: 127.0.0.1:0
  shutdown_grace_period: 2s
identity:
  jwt_algorithms:
    - RS256
  issuer: https://issuer.example.com
  audience: harbor
  jwks_file: %s
telemetry:
  log_format: text
  log_level: error
  service_name: harbor-parity-scripted
state:
  driver: inmem
llm:
  driver: bifrost
  provider: parity-fake
  model: %s
  timeout: 10s
  context_window_reserve: 0.05
  corrections:
    enabled: false
  custom_providers:
    - name: parity-fake
      base_url: %s
      api_key_env_var: %s
      models: [%s]
      timeout: 10s
      max_retries: 0
  model_profiles:
    %s:
      context_window_tokens: 8192
      token_estimator: chars_div_4
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
planner:
  driver: react
  max_steps: 4
`, key.jwksPath(), scriptedModel, llmServerURL, envKey, scriptedModel, scriptedModel) + extraYAML
	cfg, err := config.LoadFromBytes(context.Background(), []byte(yaml))
	if err != nil {
		t.Fatalf("LoadFromBytes(scripted): %v", err)
	}
	return cfg
}

// parityServer is one running composition (a live listener) the gate
// probes over real HTTP.
type parityServer struct {
	baseURL string
}

// waitParityBound polls until the composition's listener answers
// /healthz (bounded eventually-loop; §17.4 — a poll with a hard
// deadline, not a blind sleep).
func waitParityBound(t *testing.T, bindAddr func() string) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		addr := bindAddr()
		if addr != "" && !strings.HasSuffix(addr, ":0") {
			resp, err := http.Get("http://" + addr + "/healthz")
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return "http://" + addr
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("composition never bound + answered /healthz within the deadline")
	return ""
}

// bootStockServe boots the stock composition exactly as the serve
// subcommand does: serve.Boot + the shared production JWKS factory +
// PreferConfigBindAddr + a nil registrar, then a REAL listener.
func bootStockServe(t *testing.T, cfg *config.Config) *parityServer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h, err := serve.Boot(ctx, serve.Options{
		Config:               cfg,
		Stderr:               io.Discard,
		SubcommandLabel:      "serve",
		PreferConfigBindAddr: true,
		AuthValidatorFactory: serve.NewJWKSAuthValidatorFactory(),
		DisplayName:          "harbor parity stock",
		InstanceID:           "harbor-parity-stock",
	})
	if err != nil {
		cancel()
		t.Fatalf("serve.Boot (stock): %v", err)
	}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = h.Serve(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-serveDone:
		case <-time.After(10 * time.Second):
			t.Error("stock Serve did not return after cancel")
		}
		cc, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		h.Close(cc)
	})
	return &parityServer{baseURL: waitParityBound(t, h.BindAddr)}
}

// bootScaffoldedServer boots the scaffolded composition through
// external.Open — the EXACT path an sdk/server binary ships (production
// JWKS factory, PreferConfigBindAddr, the RegisterCatalog forward) — so
// a regression inside external.Open cannot escape the gate.
func bootScaffoldedServer(t *testing.T, cfg *config.Config, registrar func(tools.ToolCatalog) error) *parityServer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h, err := external.Open(ctx, cfg, "", registrar)
	if err != nil {
		cancel()
		t.Fatalf("external.Open (scaffolded): %v", err)
	}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = h.Serve(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-serveDone:
		case <-time.After(10 * time.Second):
			t.Error("scaffolded Serve did not return after cancel")
		}
		cc, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = h.Close(cc)
	})
	return &parityServer{baseURL: waitParityBound(t, h.BindAddr)}
}

func methodPath(m methods.Method) string {
	return "/v1/" + strings.ReplaceAll(string(m), ".", "/")
}

// postJSONStatus issues a POST with the given body/bearer and returns
// (status, body). Fatal on transport errors — call from the test
// goroutine only (workers use postJSONStatusErr).
func postJSONStatus(t *testing.T, client *http.Client, url, body, bearer string) (int, []byte) {
	t.Helper()
	code, respBody, err := postJSONStatusErr(client, url, body, bearer)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return code, respBody
}

// postJSONStatusErr is the goroutine-safe variant: transport errors are
// returned, never asserted (§17.4 — only the test goroutine fails).
func postJSONStatusErr(client *http.Client, url, body, bearer string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

// TestE2E_Phase160_ServeParity_BothCompositions is the parity gate:
// stock (serve.Boot, nil registrar) and scaffolded (external.Open,
// registrar + tool overlay) compositions from the same base config.
func TestE2E_Phase160_ServeParity_BothCompositions(t *testing.T) {
	key := newParityKey(t)

	stockCfg := parityConfig(t, key, "")
	scaffoldCfg := parityConfig(t, key, parityToolOverlay)

	stock := bootStockServe(t, stockCfg)
	scaffold := bootScaffoldedServer(t, scaffoldCfg, registerParityTool)
	client := &http.Client{Timeout: 10 * time.Second}
	t.Cleanup(client.CloseIdleConnections)

	// Leg (a): manifest-driven method-status parity. Every canonical
	// method (from the Go-side methods.Methods() registry — NOT mux
	// introspection) answers with the SAME status CLASS (code/100) on
	// both compositions when hit unauthenticated. A method mounted on
	// one mux but not the other — or answering a different class — is
	// caught here.
	t.Run("method_status_parity", func(t *testing.T) {
		all := methods.Methods()
		if len(all) == 0 {
			t.Fatal("methods.Methods() returned empty — the parity probe would be vacuous")
		}
		for _, m := range all {
			path := methodPath(m)
			stockCode, _ := postJSONStatus(t, client, stock.baseURL+path, `{}`, "")
			scaffoldCode, _ := postJSONStatus(t, client, scaffold.baseURL+path, `{}`, "")
			if stockCode/100 != scaffoldCode/100 {
				t.Errorf("method %s status-class parity broken: stock=%d scaffold=%d", m, stockCode, scaffoldCode)
			}
		}
	})

	// Leg (d): dev-only surfaces 404 on BOTH — neither composition
	// mounts the dev seams (bootstrap-token endpoint, dev-token mint).
	t.Run("dev_surfaces_404_on_both", func(t *testing.T) {
		devSurfaces := []string{"/v1/auth/bootstrap", "/v1/dev/token", "/v1/dev/mint"}
		for _, s := range devSurfaces {
			stockCode, _ := postJSONStatus(t, client, stock.baseURL+s, `{}`, "")
			scaffoldCode, _ := postJSONStatus(t, client, scaffold.baseURL+s, `{}`, "")
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
		tok := key.mint(t, id)
		for name, srv := range map[string]*parityServer{"stock": stock, "scaffold": scaffold} {
			noAuth, _ := postJSONStatus(t, client, srv.baseURL+"/v1/tools/list", `{}`, "")
			if noAuth != http.StatusUnauthorized {
				t.Errorf("%s: tools.list without a token = %d, want 401", name, noAuth)
			}
			withAuth, _ := postJSONStatus(t, client, srv.baseURL+"/v1/tools/list", `{}`, tok)
			if withAuth == http.StatusUnauthorized {
				t.Errorf("%s: tools.list with a valid token = 401 (identity did not propagate)", name)
			}
		}
	})

	// Leg (e) stress: N≥10 concurrent authenticated requests across
	// tenants against the shared scaffolded listener — no cross-talk;
	// the goroutine count returns to baseline (bounded eventually-poll
	// after closing the stress client's idle connections; §17.4).
	t.Run("concurrency_stress", func(t *testing.T) {
		const workers = 16
		stressClient := &http.Client{Timeout: 10 * time.Second}
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
				tok := key.mint(t, id)
				code, body, err := postJSONStatusErr(stressClient, scaffold.baseURL+"/v1/tools/list", `{}`, tok)
				if err != nil {
					errCh <- "worker transport error: " + err.Error()
					return
				}
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
		// Drain keep-alive connections, then poll the goroutine count
		// back toward baseline (tight bound: +2 slack for transient
		// runtime goroutines — never the worker count).
		stressClient.CloseIdleConnections()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if runtime.NumGoroutine() <= baseline+2 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if grew := runtime.NumGoroutine() - baseline; grew > 2 {
			t.Errorf("goroutine count still %d above baseline %d after the stress drained — leak", grew, baseline)
		}
	})

	// Leg (b) discovery: the compiled tool is present in the scaffolded
	// composition's catalog and ABSENT from stock. (Dispatch is the
	// scripted-LLM test below + the live leg.)
	t.Run("discovery_scaffolded_only", func(t *testing.T) {
		id := identity.Identity{TenantID: "acme", UserID: "alice", SessionID: "s1"}
		tok := key.mint(t, id)

		scaffoldCode, scaffoldBody := postJSONStatus(t, client, scaffold.baseURL+"/v1/tools/list", `{}`, tok)
		if scaffoldCode != http.StatusOK {
			t.Fatalf("scaffold tools.list = %d, want 200; body=%s", scaffoldCode, scaffoldBody)
		}
		if !toolListContains(t, scaffoldBody, "parity.echo") {
			t.Errorf("scaffold tools.list does not contain the compiled tool parity.echo; body=%s", scaffoldBody)
		}
		// The declared built-in reached the SAME catalog — registered by
		// the runtime from tools.built_in, not by the registrar. Both
		// registration paths coexist (the v1.13.1 duplicate-tool-name
		// boot death is the regression this pins).
		if !toolListContains(t, scaffoldBody, "clock.now") {
			t.Errorf("scaffold tools.list does not contain the declared built-in clock.now; the runtime's tools.built_in registration did not reach the served catalog; body=%s", scaffoldBody)
		}
		stockCode, stockBody := postJSONStatus(t, client, stock.baseURL+"/v1/tools/list", `{}`, tok)
		if stockCode != http.StatusOK {
			t.Fatalf("stock tools.list = %d, want 200", stockCode)
		}
		if toolListContains(t, stockBody, "parity.echo") {
			t.Errorf("stock tools.list unexpectedly contains parity.echo (no registrar ran)")
		}
	})

	// Leg (c) structural half: the tools.entries approval wrap is WIRED
	// on the compiled tool at the assembly seam the server composes (the
	// scaffolded composition's EXACT config + registrar produces a gate
	// object). The BEHAVIORAL half — the gate FIRING on a dispatched
	// call — is TestE2E_Phase160_ScriptedLLM_DispatchAndApprovalGate.
	t.Run("approval_wrap_wired_on_compiled_tool", func(t *testing.T) {
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
			t.Error("the tools.entries approval wrap did NOT wire on the compiled tool — " +
				"the registrar did not ride the pre-policy seam (registration landed after the Builder)")
		}
	})
}

// TestE2E_Phase160_StockServe_ToolOverlay_FailsClosed pins the
// deliberate fail-closed behavior: a stock serve (nil registrar) booted
// against a config whose tools.entries names an unregistered compiled
// tool fails loud with the catalog's ErrToolNotRegistered, naming the
// tool — never a silent no-op.
func TestE2E_Phase160_StockServe_ToolOverlay_FailsClosed(t *testing.T) {
	key := newParityKey(t)
	cfg := parityConfig(t, key, parityToolOverlay)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := serve.Boot(ctx, serve.Options{
		Config:               cfg,
		Stderr:               io.Discard,
		SubcommandLabel:      "serve",
		PreferConfigBindAddr: true,
		AuthValidatorFactory: serve.NewJWKSAuthValidatorFactory(),
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
	if !errors.Is(err, toolscatalog.ErrToolNotRegistered) {
		t.Errorf("fail-closed error is not catalog.ErrToolNotRegistered: %v", err)
	}
	if !strings.Contains(err.Error(), "parity.echo") {
		t.Errorf("fail-closed error should name the unregistered tool parity.echo, got %v", err)
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

// startParityTask POSTs control.start for the identity + query and
// returns the task id.
func startParityTask(t *testing.T, client *http.Client, baseURL, tok string, id identity.Identity, query string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"identity": map[string]any{
			"tenant": id.TenantID, "user": id.UserID, "session": id.SessionID,
		},
		"query": query,
	})
	code, resp := postJSONStatus(t, client, baseURL+"/v1/control/start", string(body), tok)
	if code != http.StatusOK {
		t.Fatalf("control.start = %d, want 200; body=%s", code, resp)
	}
	var started struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(resp, &started); err != nil || started.TaskID == "" {
		t.Fatalf("control.start response missing task_id: %s", resp)
	}
	return started.TaskID
}

// waitParityTaskComplete polls tasks.get (bounded eventually-loop) until
// the task reaches "complete" and returns the TaskDetail; a terminal
// failure or a timeout is fatal.
func waitParityTaskComplete(t *testing.T, client *http.Client, baseURL, tok string, id identity.Identity, taskID string, maxWait time.Duration) prototypes.TaskDetail {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"identity": map[string]any{
			"tenant": id.TenantID, "user": id.UserID, "session": id.SessionID,
		},
		"id": taskID,
	})
	deadline := time.Now().Add(maxWait)
	var last prototypes.TaskDetail
	for time.Now().Before(deadline) {
		code, resp := postJSONStatus(t, client, baseURL+"/v1/tasks/get", string(body), tok)
		if code == http.StatusOK {
			if err := json.Unmarshal(resp, &last); err == nil {
				switch last.Task.Status {
				case "complete":
					return last
				case "failed":
					t.Fatalf("task %s FAILED; detail=%s", taskID, resp)
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("task %s did not complete within %s (last status %q)", taskID, maxWait, last.Task.Status)
	return last
}

// TestE2E_Phase160_ScriptedLLM_DispatchAndApprovalGate is the CI leg of
// (b) dispatch + (c) wrap-fires: a scripted OpenAI-compatible LLM server
// (the deterministic real-bifrost pattern) drives the served run loop to
// CALL the RegisterCatalog-registered compiled tool.
//
// Subtest "dispatch": the un-gated composition (booted via external.Open
// — the sdk/server path) dispatches parity.echo through the catalog and
// the result round-trips: the terminal envelope carries the scripted
// answer with tool_calls_seen >= 1, and the compiled handler's fixture
// marker (present ONLY in tool observations) appears in the follow-up
// LLM prompt.
//
// Subtest "approval_gate_fires": the entries-wrapped variant proves the
// SERVED descriptor is the wrapped one: the same scripted tool-call
// dispatch trips the deny-all approval gate, observed as the
// tool.approval_requested event (the unified pause primitive firing) —
// not merely a gate object existing.
func TestE2E_Phase160_ScriptedLLM_DispatchAndApprovalGate(t *testing.T) {
	// NOT t.Parallel(): parityScriptedConfig calls t.Setenv.
	key := newParityKey(t)
	id := identity.Identity{TenantID: "acme", UserID: "alice", SessionID: "s1"}
	client := &http.Client{Timeout: 15 * time.Second}
	t.Cleanup(client.CloseIdleConnections)

	t.Run("dispatch", func(t *testing.T) {
		llm := newScriptedLLMServer(t,
			scriptedToolCallResponse("call_parity", "parity.echo", `{"msg":"round-trip probe"}`),
			scriptedFinishResponse("the compiled tool answered the probe"),
		)
		cfg := parityScriptedConfig(t, key, llm.URL(), "")
		srv := bootScaffoldedServer(t, cfg, registerParityTool)
		tok := key.mint(t, id)

		taskID := startParityTask(t, client, srv.baseURL, tok, id,
			"exercise the compiled parity tool")
		detail := waitParityTaskComplete(t, client, srv.baseURL, tok, id, taskID, 30*time.Second)

		// The terminal envelope (tasks.get result_inline) carries the
		// scripted answer + a non-zero tool_calls_seen — the dispatch
		// COUNTED, not just registered.
		if detail.ResultInline == "" {
			t.Fatal("tasks.get carries no result_inline for the completed task")
		}
		var envl struct {
			Answer        string `json:"answer"`
			ToolCallsSeen int    `json:"tool_calls_seen"`
		}
		if err := json.Unmarshal([]byte(detail.ResultInline), &envl); err != nil {
			t.Fatalf("result_inline is not an answer envelope: %v; raw=%s", err, detail.ResultInline)
		}
		if !strings.Contains(envl.Answer, "the compiled tool answered the probe") {
			t.Errorf("envelope answer = %q, want the scripted finish answer", envl.Answer)
		}
		if envl.ToolCallsSeen < 1 {
			t.Errorf("tool_calls_seen = %d, want >= 1 (the compiled tool was dispatched)", envl.ToolCallsSeen)
		}

		// The round-trip proof: the compiled handler's fixture marker
		// (emitted only in the tool OBSERVATION) reached the follow-up
		// LLM prompt.
		reqs := llm.Requests()
		if len(reqs) < 2 {
			t.Fatalf("scripted LLM saw %d requests, want >= 2 (CallTool + Finish)", len(reqs))
		}
		secondPrompt := flattenMessages(reqs[1].Messages)
		if !strings.Contains(secondPrompt, parityEchoResultPrefix) {
			t.Errorf("second LLM prompt does not carry the compiled handler's fixture marker %q — the dispatched result did not round-trip into the planner", parityEchoResultPrefix)
		}
	})

	t.Run("approval_gate_fires", func(t *testing.T) {
		// One scripted response only: the tool call. The deny-all gate
		// parks the invocation, so no second LLM turn happens before
		// teardown.
		llm := newScriptedLLMServer(t,
			scriptedToolCallResponse("call_parity_gated", "parity.echo", `{"msg":"gated probe"}`),
		)
		cfg := parityScriptedConfig(t, key, llm.URL(), parityToolOverlay)

		// Booted via serve.Boot with the SAME production posture
		// external.Open composes (shared prod JWKS factory +
		// PreferConfigBindAddr + registrar) — the internal handle is
		// used here because the gate-fires observation needs the bus.
		ctx, cancel := context.WithCancel(context.Background())
		h, err := serve.Boot(ctx, serve.Options{
			Config:               cfg,
			Stderr:               io.Discard,
			SubcommandLabel:      "serve",
			PreferConfigBindAddr: true,
			AuthValidatorFactory: serve.NewJWKSAuthValidatorFactory(),
			RegisterCatalog:      registerParityTool,
			DisplayName:          "harbor parity gated",
			InstanceID:           "harbor-parity-gated",
		})
		if err != nil {
			cancel()
			t.Fatalf("serve.Boot (gated): %v", err)
		}
		serveDone := make(chan struct{})
		go func() {
			defer close(serveDone)
			_ = h.Serve(ctx)
		}()
		t.Cleanup(func() {
			cancel()
			select {
			case <-serveDone:
			case <-time.After(10 * time.Second):
				t.Error("gated Serve did not return after cancel")
			}
			cc, c := context.WithTimeout(context.Background(), 5*time.Second)
			defer c()
			h.Close(cc)
		})
		baseURL := waitParityBound(t, h.BindAddr)

		// Subscribe for the approval event BEFORE starting the run.
		sub, err := h.Bus.Subscribe(ctx, events.Filter{
			Tenant:  id.TenantID,
			User:    id.UserID,
			Session: id.SessionID,
			Types:   []events.EventType{approval.EventTypeToolApprovalRequested},
		})
		if err != nil {
			t.Fatalf("bus.Subscribe: %v", err)
		}
		defer sub.Cancel()

		tok := key.mint(t, id)
		_ = startParityTask(t, client, baseURL, tok, id, "exercise the gated parity tool")

		// The gate FIRES: tool.approval_requested arrives for the
		// compiled tool with a pause token minted by the unified
		// pause/resume Coordinator — the behavioral proof the SERVED
		// descriptor is the entries-wrapped one.
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription closed before tool.approval_requested")
			}
			payload, pOK := ev.Payload.(approval.ToolApprovalRequestedPayload)
			if !pOK {
				t.Fatalf("unexpected payload type %T for tool.approval_requested", ev.Payload)
			}
			if payload.Tool != "parity.echo" {
				t.Errorf("approval requested for tool %q, want parity.echo", payload.Tool)
			}
			if payload.PauseToken == "" {
				t.Error("approval event carries no pause token — the gate did not ride the unified pause primitive")
			}
		case <-time.After(15 * time.Second):
			t.Fatal("timeout: the deny-all approval gate never FIRED on the dispatched compiled tool — the served descriptor is not the wrapped one")
		}
	})
}

// TestE2E_Live_Phase160_ScaffoldedServer_Dispatch is the env-gated live
// leg (the wire-level half of the CI/live split): the REAL external
// adopter path, end to end — scaffold --with-server as an external
// module, implement the generated tool stub, build against the local
// checkout, boot the subprocess behind a harbor-token-minted JWKS, and
// drive the generated tool through the wire with a REAL LLM. Skipped by
// default; run by the wave's live-verification step:
//
//	HARBOR_LIVE_SERVE=1 OPENROUTER_API_KEY=... go test -run TestE2E_Live_Phase160 -timeout 15m ./test/integration/...
func TestE2E_Live_Phase160_ScaffoldedServer_Dispatch(t *testing.T) {
	if os.Getenv("HARBOR_LIVE_SERVE") == "" {
		t.Skip("reason: live leg — set HARBOR_LIVE_SERVE=1 (needs a real LLM key) to run the wire-level scaffolded-server dispatch")
	}
	if os.Getenv("OPENROUTER_API_KEY") == "" {
		t.Fatal("HARBOR_LIVE_SERVE=1 but OPENROUTER_API_KEY is unset — the live leg needs a real provider key")
	}

	root := mcpCallRepoRoot(t)
	work := t.TempDir()
	// runCmd returns STDOUT only (harbor token mint writes the signed
	// token to stdout and nothing else; stderr may carry log lines that
	// must not contaminate a captured token). Stderr is folded into the
	// failure message.
	runCmd := func(dir string, name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s %s: %v\nstdout:\n%s\nstderr:\n%s", name, strings.Join(args, " "), err, stdout.String(), stderr.String())
		}
		return stdout.String()
	}

	// 1. Build the harbor CLI from the checkout under test.
	harborBin := filepath.Join(work, "harbor")
	runCmd(root, "go", "build", "-o", harborBin, "./cmd/harbor")

	// 2. keygen — the production JWKS local-dev loop.
	keysDir := filepath.Join(work, "keys")
	runCmd(work, harborBin, "token", "keygen", "--out", keysDir, "--alg", "ES256")

	// 3. The probe config: jwks_file posture + one custom tool + one
	// declared built-in (the runtime registers it from config; the
	// generated registrar must NOT, or the boot dies on a duplicate tool
	// name — the v1.13.1 regression) + a real provider (key read from
	// the environment by the server; never logged here).
	const liveIssuer = "https://issuer.live.local"
	const liveAudience = "phase-160-live"
	cfgYAML := fmt.Sprintf(`
server:
  bind_addr: 127.0.0.1:0
  shutdown_grace_period: 5s
identity:
  jwt_algorithms: [ES256]
  issuer: %s
  audience: %s
  jwks_file: %s
telemetry:
  log_format: json
  log_level: error
  service_name: phase-160-live
llm:
  driver: bifrost
  provider: openrouter
  model: anthropic/claude-haiku-4-5
  api_key: env.OPENROUTER_API_KEY
  timeout: 60s
  context_window_reserve: 0.05
  model_profiles:
    anthropic/claude-haiku-4-5:
      context_window_tokens: 200000
      token_estimator: chars_div_4
tools:
  built_in:
    - clock.now
  custom:
    - name: weather.lookup
      description: Look up current weather by city.
      input:
        city: string
      output:
        summary: string
`, liveIssuer, liveAudience, filepath.Join(keysDir, "jwks.json"))
	cfgPath := filepath.Join(work, "harbor.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	// 4. Scaffold --with-server as an external module.
	proj := filepath.Join(work, "live-agent")
	runCmd(work, harborBin, "scaffold", "--name", "live-agent",
		"--output", proj, "--with-server", "--from-config", cfgPath)

	// 5. The adopter step: implement the generated tool stub — the
	// handler returns a fixture string only the COMPILED tool can
	// produce.
	const liveFixture = "LIVE-PARITY-FIXTURE sunny, 21C"
	toolSrc := `package tools

import "context"

// WeatherLookupInput is the typed input shape for "weather.lookup".
type WeatherLookupInput struct {
	City string ` + "`json:\"city\"`" + `
}

// WeatherLookupOutput is the typed output shape for "weather.lookup".
type WeatherLookupOutput struct {
	Summary string ` + "`json:\"summary\"`" + `
}

// WeatherLookup is the "weather.lookup" tool's handler.
func WeatherLookup(_ context.Context, in WeatherLookupInput) (WeatherLookupOutput, error) {
	return WeatherLookupOutput{Summary: "` + liveFixture + ` in " + in.City}, nil
}
`
	if err := os.WriteFile(filepath.Join(proj, "tools", "weather_lookup.go"), []byte(toolSrc), 0o600); err != nil {
		t.Fatalf("implement tool stub: %v", err)
	}

	// 6. External build against the local checkout (the replace-directive
	// precedent).
	gomod := filepath.Join(proj, "go.mod")
	mod, err := os.ReadFile(gomod)
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	mod = append(mod, []byte(fmt.Sprintf("\nreplace github.com/hurtener/Harbor => %s\n", root))...)
	if err := os.WriteFile(gomod, mod, 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	runCmd(proj, "go", "mod", "tidy")
	srvBin := filepath.Join(work, "live-agent-bin")
	runCmd(proj, "go", "build", "-o", srvBin, "./cmd/live-agent")

	// 7. Boot the scaffolded server subprocess (the provider key is
	// inherited from the test environment; never printed).
	logPath := filepath.Join(work, "server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create server log: %v", err)
	}
	srvCmd := exec.Command(srvBin, "--config", cfgPath, "--bind", "127.0.0.1:0")
	srvCmd.Dir = work
	srvCmd.Stdout = logFile
	srvCmd.Stderr = logFile
	if err := srvCmd.Start(); err != nil {
		t.Fatalf("start scaffolded server: %v", err)
	}
	t.Cleanup(func() {
		_ = srvCmd.Process.Kill()
		_, _ = srvCmd.Process.Wait()
		_ = logFile.Close()
	})

	// Bounded poll for the bound address the server prints at listen.
	var baseURL string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		raw, _ := os.ReadFile(logPath)
		if i := bytes.Index(raw, []byte("HARBOR_DEV_BOUND=")); i >= 0 {
			rest := raw[i+len("HARBOR_DEV_BOUND="):]
			if j := bytes.IndexAny(rest, "\r\n"); j > 0 {
				baseURL = "http://" + string(rest[:j])
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if baseURL == "" {
		raw, _ := os.ReadFile(logPath)
		t.Fatalf("scaffolded server never bound; log:\n%s", raw)
	}

	// 8. Mint a token with the checkout's harbor CLI (the documented
	// local-dev loop) and drive the generated tool over the wire.
	tok := strings.TrimSpace(runCmd(work, harborBin, "token", "mint",
		"--key", filepath.Join(keysDir, "private.pem"),
		"--tenant", "acme", "--user", "alice", "--session", "s1",
		"--issuer", liveIssuer, "--audience", liveAudience))
	if tok == "" {
		t.Fatal("harbor token mint produced no token")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	id := identity.Identity{TenantID: "acme", UserID: "alice", SessionID: "s1"}

	taskID := startParityTask(t, client, baseURL, tok, id,
		"Use the weather.lookup tool to get the weather in Valparaiso, then repeat the tool's summary verbatim in your answer.")
	detail := waitParityTaskComplete(t, client, baseURL, tok, id, taskID, 3*time.Minute)

	var envl struct {
		Answer        string `json:"answer"`
		ToolCallsSeen int    `json:"tool_calls_seen"`
	}
	if err := json.Unmarshal([]byte(detail.ResultInline), &envl); err != nil {
		t.Fatalf("result_inline is not an answer envelope: %v; raw=%s", err, detail.ResultInline)
	}
	if !strings.Contains(envl.Answer, "LIVE-PARITY-FIXTURE") {
		t.Errorf("answer does not carry the compiled handler's fixture string; answer=%q", envl.Answer)
	}
	if envl.ToolCallsSeen < 1 {
		t.Errorf("tool_calls_seen = %d, want >= 1 (the generated tool was dispatched over the wire)", envl.ToolCallsSeen)
	}
}
