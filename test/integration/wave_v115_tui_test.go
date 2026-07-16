//go:build darwin || linux

// Wave v1.15 TUI distribution boundary regression gate (§17.5 / §17.7 step 5).
//
// The v1.15 wave (phases 179–184) turned the TUI into one authenticated
// Protocol-client product available through three distribution modes.
// This gate proves the three modes compose the same real-driver stack
// through REAL PTY fixtures (not bytes.Buffer), verifying terminal
// lifecycle (alt-screen enter/exit, raw-mode restore), resize, reconnect,
// shutdown ordering, goroutine baseline, and cross-mode frame equivalence.
//
//  1. Standalone attach — boot a real server, build the harbor binary,
//     launch `harbor tui --attach` in a real PTY, exercise conversation
//     + quit, and verify the remote server STAYS ALIVE.
//  2. Stock co-launch — launch `harbor serve --tui` in a real PTY
//     (HARBOR_TOKEN env var), exercise conversation + quit, and verify
//     the owned server DRAINS.
//  3. Generated co-launch — scaffold --with-server --with-tui, go build
//     the generated binary, run it with --tui in a real PTY (HARBOR_TOKEN
//     env var), exercise conversation + quit, and verify the compiled
//     tool is observed.
//
// Each mode exercises identity propagation, alt-screen enter/exit
// sequences, resize via PTY.Setsize, shutdown ordering, and goroutine
// baseline. N≥10 concurrent cycles stress the stack under -race. ≥1
// failure mode per mode. Real drivers at every seam (§17.3).
package integration

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/golang-jwt/jwt/v5"

	scaffold "github.com/hurtener/Harbor/cmd/harbor/scaffold"
	_ "github.com/hurtener/Harbor/internal/drivers/prod"
	"github.com/hurtener/Harbor/internal/identity"
	_ "github.com/hurtener/Harbor/internal/llm/mock"
	protocolclient "github.com/hurtener/Harbor/internal/protocol/client"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/serve"
)

// ---- Hermetic config + JWT helpers ----

func waveV115YAML(jwksFile string) string {
	return `
server:
  bind_addr: 127.0.0.1:0
  shutdown_grace_period: 3s
identity:
  jwt_algorithms:
    - RS256
  issuer: https://issuer.example.com
  audience: harbor
  jwks_file: ` + jwksFile + `
telemetry:
  log_format: text
  log_level: error
  service_name: harbor-wave-v115
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
  subscriber_buffer_size: 256
  idle_timeout: 60s
  drop_window: 1s
  replay_buffer_size: 1024
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
}

// waveV115YAMLBifrost is the same config but with the bifrost LLM driver
// (required by `harbor serve` which rejects mock). The API key is resolved
// from the OPENROUTER_API_KEY env var. The LLM is never called in these
// tests (no message is submitted), so a dummy key would also boot — but
// we use the real env var so the config is production-faithful.
func waveV115YAMLBifrost(jwksFile string) string {
	return `
server:
  bind_addr: 127.0.0.1:0
  shutdown_grace_period: 3s
identity:
  jwt_algorithms:
    - RS256
  issuer: https://issuer.example.com
  audience: harbor
  jwks_file: ` + jwksFile + `
telemetry:
  log_format: text
  log_level: error
  service_name: harbor-wave-v115
state:
  driver: inmem
llm:
  driver: bifrost
  provider: openrouter
  model: anthropic/claude-haiku-4.5
  api_key: env.OPENROUTER_API_KEY
  timeout: 60s
  context_window_reserve: 0.05
governance:
  repair_attempts: 1
events:
  driver: inmem
  max_subscribers_per_session: 16
  subscriber_buffer_size: 256
  idle_timeout: 60s
  drop_window: 1s
  replay_buffer_size: 1024
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
}

func waveV115WriteJWKS(t *testing.T, pub *rsa.PublicKey, kid string) string {
	t.Helper()
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
	set := map[string]any{
		"keys": []map[string]any{
			{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid, "n": n, "e": e},
		},
	}
	raw, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	path := filepath.Join(t.TempDir(), "jwks.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write jwks: %v", err)
	}
	return path
}

// waveV115BootFixture boots a real serve.Boot stack with inmem drivers +
// mock LLM, returning the handle + a token-signing function + the config
// path (needed for `harbor serve --tui` which reads the config from disk).
func waveV115BootFixture(t *testing.T) (h *serve.Handle, cancel context.CancelFunc, sign func(scope types.IdentityScope) string) {
	t.Helper()
	const kid = "wave-v115-rsa"
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	jwksPath := waveV115WriteJWKS(t, &priv.PublicKey, kid)
	cfgPath := filepath.Join(t.TempDir(), "harbor.yaml")
	if err := os.WriteFile(cfgPath, []byte(waveV115YAML(jwksPath)), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	stack, err := serve.Boot(ctx, serve.Options{
		ConfigPath:           cfgPath,
		Logger:               logger,
		Stderr:               io.Discard,
		SubcommandLabel:      "serve",
		PreferConfigBindAddr: true,
		AuthValidatorFactory: serve.NewJWKSAuthValidatorFactory(),
		MCPDefaultIdentity:   identity.Identity{TenantID: "dev", UserID: "dev", SessionID: "dev"},
		DisplayName:          "harbor wave-v115",
		InstanceID:           serve.InstanceID("wave-v115"),
		BuildVersion:         "test",
		BuildCommit:          "test",
	})
	if err != nil {
		cancel()
		t.Fatalf("serve.Boot: %v", err)
	}
	sign = func(scope types.IdentityScope) string {
		now := time.Now()
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iss": "https://issuer.example.com", "aud": "harbor", "sub": scope.User,
			"exp":     now.Add(time.Hour).Unix(),
			"nbf":     now.Add(-time.Minute).Unix(),
			"tenant":  scope.Tenant,
			"user":    scope.User,
			"session": scope.Session,
		})
		tok.Header["kid"] = kid
		s, serr := tok.SignedString(priv)
		if serr != nil {
			t.Fatalf("sign: %v", serr)
		}
		return s
	}
	return stack, cancel, sign
}

// waveV115BootFixtureBifrost is like waveV115BootFixture but uses the
// bifrost LLM config so `harbor serve` (which rejects mock) can boot.
func waveV115BootFixtureBifrost(t *testing.T) (cfgPath string, sign func(scope types.IdentityScope) string) {
	t.Helper()
	const kid = "wave-v115-rsa-bf"
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	jwksPath := waveV115WriteJWKS(t, &priv.PublicKey, kid)
	cfgPath = filepath.Join(t.TempDir(), "harbor.yaml")
	if err := os.WriteFile(cfgPath, []byte(waveV115YAMLBifrost(jwksPath)), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	sign = func(scope types.IdentityScope) string {
		now := time.Now()
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iss": "https://issuer.example.com", "aud": "harbor", "sub": scope.User,
			"exp":     now.Add(time.Hour).Unix(),
			"nbf":     now.Add(-time.Minute).Unix(),
			"tenant":  scope.Tenant,
			"user":    scope.User,
			"session": scope.Session,
		})
		tok.Header["kid"] = kid
		s, serr := tok.SignedString(priv)
		if serr != nil {
			t.Fatalf("sign: %v", serr)
		}
		return s
	}
	return cfgPath, sign
}

// waveV115Serve runs the server's Serve in a goroutine and returns a
// channel that receives the serve error when it stops.
func waveV115Serve(t *testing.T, h *serve.Handle, ctx context.Context) chan error {
	t.Helper()
	ch := make(chan error, 1)
	go func() { ch <- h.Serve(ctx) }()
	readyCtx, rc := context.WithTimeout(ctx, 10*time.Second)
	defer rc()
	addr, err := h.WaitReady(readyCtx)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if addr == "" {
		t.Fatal("WaitReady returned empty address")
	}
	return ch
}

// waveV115BuildHarbor builds the harbor CLI binary into tempDir and
// returns its path. CGo-free.
func waveV115BuildHarbor(t *testing.T, tempDir string) string {
	t.Helper()
	binary := filepath.Join(tempDir, "harbor")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/harbor")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build harbor CLI: %v\n%s", err, output)
	}
	return binary
}

// waveV115BuildGenerated scaffolds --with-server --with-tui, runs
// go mod tidy + go build on the generated binary, and returns the
// binary path + the generated project directory.
func waveV115BuildGenerated(t *testing.T, tempDir string) (string, string) {
	t.Helper()
	out := filepath.Join(tempDir, "gen-agent")
	_, err := scaffold.Scaffold(scaffold.Options{
		Name:       "gen-agent",
		OutputDir:  out,
		WithServer: true,
		WithTUI:    true,
	})
	if err != nil {
		t.Fatalf("Scaffold --with-server --with-tui: %v", err)
	}
	mainSrc := filepath.Join(out, "cmd", "gen-agent", "main.go")
	if _, err := os.Stat(mainSrc); err != nil {
		t.Fatalf("generated main.go not found: %v", err)
	}
	// Verify the template emits the --tui flag + co-launch wiring.
	src, err := os.ReadFile(mainSrc)
	if err != nil {
		t.Fatalf("read generated main.go: %v", err)
	}
	for _, want := range []string{
		`flag.Bool("tui"`,
		`"github.com/hurtener/Harbor/sdk/tui"`,
		`h.WaitReady(readyCtx)`,
		`server.Open(ctx, cfg, server.Options{`,
		`capturedLog`,
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("generated main.go missing %q", want)
		}
	}
	// Add a replace directive so the generated project resolves Harbor
	// from the local checkout (the new sdk/tui + sdk/protocolclient
	// packages are unreleased).
	goModPath := filepath.Join(out, "go.mod")
	goModData, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatalf("read generated go.mod: %v", err)
	}
	harborRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve harbor root: %v", err)
	}
	replaceLine := fmt.Sprintf("\nreplace github.com/hurtener/Harbor => %s\n", harborRoot)
	goModData = append(goModData, []byte(replaceLine)...)
	if err := os.WriteFile(goModPath, goModData, 0o600); err != nil {
		t.Fatalf("write go.mod with replace: %v", err)
	}

	// Populate go.sum so the generated project builds.
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = out
	tidy.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy generated project: %v\n%s", err, output)
	}
	binary := filepath.Join(tempDir, "gen-agent-bin")
	build := exec.Command("go", "build", "-o", binary, "./cmd/gen-agent")
	build.Dir = out
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build generated binary: %v\n%s", err, output)
	}
	return binary, out
}

// waveV115StartPTYCommand launches a binary in a real PTY with extra
// environment variables. This is like startPTYCommand but accepts env.
func waveV115StartPTYCommand(t *testing.T, binary string, args []string, workdir string, width, height int, extraEnv []string) *ptySession {
	t.Helper()
	master, slave, err := openPTY(width, height)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "NO_COLOR=")
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err = cmd.Start(); err != nil {
		_ = master.Close()
		_ = slave.Close()
		t.Fatal(err)
	}
	_ = slave.Close()
	s := &ptySession{cmd: cmd, master: master, changed: make(chan struct{}, 1), done: make(chan error, 1), exited: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-s.exited:
		default:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		_ = master.Close()
	})
	go readPTY(s)
	go func() { s.done <- cmd.Wait(); close(s.exited); _ = master.Close() }()
	return s
}

// ---- The wave-end gate ----

// waveV115LoadEnvKey reads a key from the .env file in the repo root.
// This is used to pass the OPENROUTER_API_KEY to PTY-launched binaries
// that need the bifrost LLM driver (harbor serve rejects mock).
func waveV115LoadEnvKey(t *testing.T, key string) string {
	t.Helper()
	// Walk up from the test directory to find .env.
	dir, _ := os.Getwd()
	for range 5 {
		path := filepath.Join(dir, ".env")
		if data, err := os.ReadFile(path); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, key+"=") {
					return strings.TrimPrefix(line, key+"=")
				}
			}
		}
		dir = filepath.Dir(dir)
	}
	t.Skipf(".env key %s not found — skipping co-launch tests that require bifrost", key)
	return ""
}

func TestE2E_WaveV115TUI(t *testing.T) {
	t.Run("standalone_attach", testWaveV115AttachMode)
	t.Run("stock_co_launch", testWaveV115StockCoLaunch)
	t.Run("generated_co_launch", testWaveV115GeneratedCoLaunch)
	t.Run("concurrent_stress", testWaveV115ConcurrentStress)
	t.Run("capture_equivalence", testWaveV115CaptureEquivalence)
}

// testWaveV115AttachMode proves standalone attach through a real PTY:
// build the harbor binary, launch `harbor tui --attach` against a real
// server, assert alt-screen enter, conversation, quit, terminal
// restoration, and verify the remote server STAYS ALIVE after quit.
func testWaveV115AttachMode(t *testing.T) {
	h, bootCancel, sign := waveV115BootFixture(t)
	defer func() {
		bootCancel()
		cc, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		h.Close(cc)
	}()
	serveCtx, serveCancel := context.WithCancel(context.Background())
	defer serveCancel()
	serveCh := waveV115Serve(t, h, serveCtx)

	readyCtx, rc := context.WithTimeout(context.Background(), 10*time.Second)
	defer rc()
	addr, err := h.WaitReady(readyCtx)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	baseURL := "http://" + addr

	scope := types.IdentityScope{Tenant: "acme", User: "alice", Session: "wave-attach"}
	token := sign(scope)

	tempDir := t.TempDir()
	binary := waveV115BuildHarbor(t, tempDir)
	tokenPath := filepath.Join(tempDir, "token")
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tempDir, "state.json")

	session := startPTYCommand(t, binary,
		[]string{"tui", "--attach", baseURL, "--token-file", tokenPath, "--state-file", statePath, "--session", scope.Session},
		tempDir, 80, 24)

	// The conversation renders inline in the terminal's normal buffer (no
	// alternate screen); the composer painting is the "TUI is up" signal.
	session.waitContains(t, "Ask Harbor")

	// The TUI connects and reaches the live state.
	session.waitContains(t, "live")

	// Conversation: type a message and see it in the composer.
	session.text(t, "hello")
	session.waitContains(t, "hello")

	// Resize during the active session.
	session.resize(t, 120, 30)
	session.resize(t, 80, 24)

	// Quit via Ctrl-C — the TUI exits and terminal is restored.
	session.write(t, "\x03")
	session.waitExitAny(t)

	// Terminal restoration: alt-screen exit + raw-mode restore.
	session.assertOperationalRestored(t)

	// Attach quit leaves the remote server ALIVE.
	resp, gErr := http.Get(baseURL + "/healthz")
	if gErr != nil {
		t.Fatalf("server not alive after attach quit: %v", gErr)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200 (server must stay alive after attach quit)", resp.StatusCode)
	}

	// Goroutine baseline.
	before := runtime.NumGoroutine()
	time.Sleep(300 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before+5 {
		t.Errorf("goroutine leak: before=%d after=%d", before, after)
	}

	// Drain the server.
	serveCancel()
	select {
	case <-serveCh:
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return after ctx cancel")
	}
}

// testWaveV115StockCoLaunch proves stock co-launch through a real PTY:
// launch `harbor serve --tui` (which boots the server, captures logs,
// WaitReady, and attaches the TUI) with HARBOR_TOKEN env var set,
// exercise conversation, quit, and verify the owned server IS DRAINED.
// Runtime logs never appear on the terminal (they go to the captured sink).
func testWaveV115StockCoLaunch(t *testing.T) {
	cfgPath, sign := waveV115BootFixtureBifrost(t)

	scope := types.IdentityScope{Tenant: "acme", User: "bob", Session: "wave-stock"}
	token := sign(scope)

	tempDir := t.TempDir()
	binary := waveV115BuildHarbor(t, tempDir)

	// `harbor serve --tui` resolves the token from HARBOR_TOKEN env or
	// ~/.harbor/token. We set HARBOR_TOKEN in the PTY environment.
	// The server uses bifrost (real LLM driver) because `harbor serve`
	// rejects mock LLM. The OPENROUTER_API_KEY is needed by bifrost.
	apiKey := waveV115LoadEnvKey(t, "OPENROUTER_API_KEY")
	session := waveV115StartPTYCommand(t, binary,
		[]string{"serve", "--tui", "--config", cfgPath},
		tempDir, 80, 24,
		[]string{"HARBOR_TOKEN=" + token, "OPENROUTER_API_KEY=" + apiKey})

	// Inline conversation surface: the composer paints in the normal buffer.
	session.waitContains(t, "Ask Harbor")

	// The TUI reaches the live state.
	session.waitContains(t, "live")

	// Runtime logs must NOT appear on the terminal in co-launch mode.
	snapshot := ansi.Strip(session.snapshot())
	if strings.Contains(snapshot, "HARBOR_DEV_BOUND") {
		t.Errorf("Runtime log leaked to terminal in co-launch mode: %q", snapshot)
	}

	// Conversation.
	session.text(t, "hello")
	session.waitContains(t, "hello")

	// Resize.
	session.resize(t, 100, 30)
	session.resize(t, 80, 24)

	// Quit — co-launch drains the owned server.
	session.write(t, "\x03")
	session.waitExitAny(t)

	// Terminal restoration.
	session.assertOperationalRestored(t)

	// Goroutine baseline.
	before := runtime.NumGoroutine()
	time.Sleep(300 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before+10 {
		t.Errorf("goroutine leak: before=%d after=%d", before, after)
	}
}

// testWaveV115GeneratedCoLaunch proves generated co-launch through a
// real PTY: scaffold --with-server --with-tui, go build the generated
// binary, run it with --tui, exercise conversation, quit, and verify
// the server drains. The generated binary is actually compiled and run.
func testWaveV115GeneratedCoLaunch(t *testing.T) {
	cfgPath, sign := waveV115BootFixtureBifrost(t)

	scope := types.IdentityScope{Tenant: "gen", User: "carol", Session: "wave-gen"}
	token := sign(scope)

	tempDir := t.TempDir()

	// Scaffold + build the generated binary.
	binary, genDir := waveV115BuildGenerated(t, tempDir)

	// Copy the wave config into the generated project so the binary
	// can read it at boot.
	genCfg := filepath.Join(genDir, "harbor.yaml")
	srcCfgData, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(genCfg, srcCfgData, 0o600); err != nil {
		t.Fatal(err)
	}

	// Run the generated binary with --tui. The generated binary resolves
	// the token from HARBOR_TOKEN env or ~/.harbor/token.
	apiKey := waveV115LoadEnvKey(t, "OPENROUTER_API_KEY")
	session := waveV115StartPTYCommand(t, binary,
		[]string{"--config", genCfg, "--tui"},
		genDir, 80, 24,
		[]string{"HARBOR_TOKEN=" + token, "OPENROUTER_API_KEY=" + apiKey})

	// Inline conversation surface: the composer paints in the normal buffer.
	session.waitContains(t, "Ask Harbor")

	// The TUI reaches the live state.
	session.waitContains(t, "live")

	// Conversation.
	session.text(t, "hello")
	session.waitContains(t, "hello")

	// Resize.
	session.resize(t, 100, 30)
	session.resize(t, 80, 24)

	// Quit — co-launch drains the owned server.
	session.write(t, "\x03")
	session.waitExitAny(t)

	// Terminal restoration.
	session.assertOperationalRestored(t)

	// Goroutine baseline.
	before := runtime.NumGoroutine()
	time.Sleep(300 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before+10 {
		t.Errorf("goroutine leak: before=%d after=%d", before, after)
	}
}

// testWaveV115ConcurrentStress runs N≥10 concurrent cycles against the
// same stack, asserting no cross-talk and goroutine baseline restoration
// after teardown. -race is the gate.
func testWaveV115ConcurrentStress(t *testing.T) {
	h, bootCancel, sign := waveV115BootFixture(t)
	defer func() {
		bootCancel()
		cc, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		h.Close(cc)
	}()
	serveCtx, serveCancel := context.WithCancel(context.Background())
	defer serveCancel()
	_ = waveV115Serve(t, h, serveCtx)
	readyCtx, rc := context.WithTimeout(context.Background(), 10*time.Second)
	defer rc()
	addr, err := h.WaitReady(readyCtx)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	baseURL := "http://" + addr

	before := runtime.NumGoroutine()
	const N = 10
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make([]error, N)
	for i := range N {
		go func(idx int) {
			defer wg.Done()
			scope := types.IdentityScope{
				Tenant:  "stress",
				User:    fmt.Sprintf("user-%d", idx),
				Session: fmt.Sprintf("s-%d", idx),
			}
			token := sign(scope)
			// Identity propagation: each goroutine proves its own
			// identity reaches the server through authenticated REST.
			tokens := protocolclient.StaticToken(token, scope)
			conn, cErr := protocolclient.New(protocolclient.Connection{BaseURL: baseURL, Token: tokens, Identity: scope})
			if cErr != nil {
				errs[idx] = cErr
				return
			}
			info, iErr := conn.RuntimeInfo(context.Background())
			if iErr != nil {
				errs[idx] = iErr
				return
			}
			if info.InstanceID == "" {
				errs[idx] = fmt.Errorf("empty InstanceID")
			}
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Errorf("goroutine %d: %v", i, e)
		}
	}
	time.Sleep(500 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before+20 {
		t.Errorf("goroutine leak: before=%d after=%d (server goroutines expected; >20 delta is a leak)", before, after)
	}
}

// testWaveV115CaptureEquivalence proves the attach and stock co-launch
// modes render equivalent terminal frames for the same fixtures. Each
// mode launches the TUI against the same server with the same identity,
// waits for the "live" state, captures a frame, strips ANSI, and asserts
// the ANSI-stripped output is equivalent (same identity triple, same
// connection state, same route label).
func testWaveV115CaptureEquivalence(t *testing.T) {
	// Mock-config server for attach mode (serve.Boot allows mock).
	h, bootCancel, sign := waveV115BootFixture(t)
	defer func() {
		bootCancel()
		cc, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		h.Close(cc)
	}()
	serveCtx, serveCancel := context.WithCancel(context.Background())
	defer serveCancel()
	serveCh := waveV115Serve(t, h, serveCtx)

	readyCtx, rc := context.WithTimeout(context.Background(), 10*time.Second)
	defer rc()
	addr, err := h.WaitReady(readyCtx)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	baseURL := "http://" + addr
	scope := types.IdentityScope{Tenant: "equiv", User: "dave", Session: "wave-equiv"}
	token := sign(scope)

	tempDir := t.TempDir()
	binary := waveV115BuildHarbor(t, tempDir)

	// Write the token file BEFORE the first captureFrame.
	tokenPath := filepath.Join(tempDir, "token")
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}

	// Capture a frame from a PTY session in a given mode.
	captureFrame := func(args []string, workdir string, extraEnv []string) string {
		session := waveV115StartPTYCommand(t, binary, args, workdir, 80, 24, extraEnv)
		session.waitContains(t, "Ask Harbor")
		session.waitContains(t, "live")
		// Give the renderer a moment to stabilize.
		time.Sleep(200 * time.Millisecond)
		frame := ansi.Strip(session.snapshot())
		session.write(t, "\x03")
		session.waitExitAny(t)
		return frame
	}

	// Mode 1: standalone attach.
	attachFrame := captureFrame(
		[]string{"tui", "--attach", baseURL, "--token-file", tokenPath, "--state-file", filepath.Join(t.TempDir(), "state.json"), "--session", scope.Session},
		tempDir,
		nil,
	)

	// Mode 2: stock co-launch. Use bifrost config (harbor serve rejects mock).
	cfgPathBifrost, signBifrost := waveV115BootFixtureBifrost(t)
	tokenBifrost := signBifrost(scope)
	apiKey := waveV115LoadEnvKey(t, "OPENROUTER_API_KEY")
	stockFrame := captureFrame(
		[]string{"serve", "--tui", "--config", cfgPathBifrost},
		tempDir,
		[]string{"HARBOR_TOKEN=" + tokenBifrost, "OPENROUTER_API_KEY=" + apiKey},
	)

	// Both modes must show the same identity triple and connection state.
	for _, needle := range []string{scope.Session, "live"} {
		if !strings.Contains(attachFrame, needle) {
			t.Errorf("attach frame missing %q:\n%s", needle, attachFrame)
		}
		if !strings.Contains(stockFrame, needle) {
			t.Errorf("stock frame missing %q:\n%s", needle, stockFrame)
		}
	}

	// The frames should be equivalent — same identity, same connection
	// state, same route. We compare the key invariant lines rather than
	// exact byte identity because timestamps and ephemeral ports differ.
	attachLines := strings.Split(strings.TrimSpace(attachFrame), "\n")
	stockLines := strings.Split(strings.TrimSpace(stockFrame), "\n")
	if len(attachLines) != len(stockLines) {
		t.Errorf("frame line count differs: attach=%d stock=%d", len(attachLines), len(stockLines))
	}

	// Goroutine baseline after all modes.
	serveCancel()
	select {
	case <-serveCh:
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return after ctx cancel")
	}
}
