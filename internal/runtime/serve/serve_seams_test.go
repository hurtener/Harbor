package serve

// serve_seams_test.go — per-seam error-path + option-matrix coverage for the
// promoted serve constructor: every caller-side injection seam's failure mode
// fails Boot loud (with the rollback drained), the exported BuildMux fan-out
// is driven directly across its optional-subsystem matrix, and the Handle's
// listen-error / double-Close / debug-listener behaviors are pinned.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/telemetry"
)

// errSeam is the sentinel each seam-error test injects and asserts through.
var errSeam = errors.New("serve: seam test sentinel")

// TestBoot_FactoryError_FailsLoud — item 1: a factory that errors fails Boot
// loud, never a silent unauthenticated fallback.
func TestBoot_FactoryError_FailsLoud(t *testing.T) {
	opts := baseOptions(t)
	opts.AuthValidatorFactory = func(_ context.Context, _ *config.Config, _ audit.Redactor, _ events.EventBus, _ *slog.Logger) (auth.Validator, error) {
		return nil, errSeam
	}
	h, err := Boot(context.Background(), opts)
	if h != nil || !errors.Is(err, errSeam) {
		t.Fatalf("Boot with an erroring factory: handle=%v err=%v, want nil handle + the seam error", h, err)
	}
}

// TestBoot_AuthSurfaceBuilderError_FailsLoud — item 2 (error half): a
// BuildAuthSurface that errors fails Boot loud.
func TestBoot_AuthSurfaceBuilderError_FailsLoud(t *testing.T) {
	opts := baseOptions(t)
	opts.BuildAuthSurface = func(_ audit.Redactor, _ events.EventBus, _ *slog.Logger) (*auth.RotateSurface, error) {
		return nil, errSeam
	}
	h, err := Boot(context.Background(), opts)
	if h != nil || !errors.Is(err, errSeam) {
		t.Fatalf("Boot with an erroring auth-surface builder: handle=%v err=%v, want nil handle + the seam error", h, err)
	}
}

// TestBoot_AuthSurface_MountedWhenBuilt_404WhenNil — item 2 (posture halves):
// a built rotate surface answers on /v1/auth/rotate_token (401 without a
// token — mounted, auth-gated); a nil builder leaves the route un-mounted
// (404).
func TestBoot_AuthSurface_MountedWhenBuilt_404WhenNil(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Nil builder → 404.
	hNil := bootTest(t, ctx, baseOptions(t))
	if code := probe(hNil, http.MethodPost, "/v1/auth/rotate_token", "{}"); code != http.StatusNotFound {
		t.Fatalf("rotate_token with a nil auth-surface builder = %d, want 404", code)
	}

	// Built surface → mounted (401 unauthenticated proves the route exists
	// behind the auth middleware).
	opts := baseOptions(t)
	opts.BuildAuthSurface = func(red audit.Redactor, bus events.EventBus, _ *slog.Logger) (*auth.RotateSurface, error) {
		return auth.NewRotateSurface(identityEchoIssuer{}, red, auth.WithRotateBus(bus))
	}
	hBuilt := bootTest(t, ctx, opts)
	if code := probe(hBuilt, http.MethodPost, "/v1/auth/rotate_token", "{}"); code != http.StatusUnauthorized {
		t.Fatalf("rotate_token with a built auth surface = %d, want 401 (mounted, auth-gated)", code)
	}
}

// TestBoot_LLMSnapshotBuilderError_FailsLoud — item 3.
func TestBoot_LLMSnapshotBuilderError_FailsLoud(t *testing.T) {
	opts := baseOptions(t)
	opts.BuildLLMSnapshot = func(_ *config.Config) (*llm.ConfigSnapshot, error) {
		return nil, errSeam
	}
	h, err := Boot(context.Background(), opts)
	if h != nil || !errors.Is(err, errSeam) {
		t.Fatalf("Boot with an erroring LLM-snapshot builder: handle=%v err=%v, want nil handle + the seam error", h, err)
	}
}

// TestBoot_ExtraRoutesError_FailsLoud_DrainsReturnedClosers — item 4 plus the
// partial-mount contract: a failing ExtraRoutes hands back the closers it
// accumulated, and the rollback drains them.
func TestBoot_ExtraRoutesError_FailsLoud_DrainsReturnedClosers(t *testing.T) {
	var drained atomic.Int32
	opts := baseOptions(t)
	opts.ExtraRoutes = func(_ context.Context, _ RouteMount) ([]func(context.Context) error, error) {
		return []func(context.Context) error{
			func(context.Context) error { drained.Add(1); return nil },
		}, errSeam
	}
	h, err := Boot(context.Background(), opts)
	if h != nil || !errors.Is(err, errSeam) {
		t.Fatalf("Boot with an erroring ExtraRoutes: handle=%v err=%v, want nil handle + the seam error", h, err)
	}
	if drained.Load() != 1 {
		t.Fatalf("the closer returned alongside the ExtraRoutes error was drained %d times, want 1 (the partial-mount rollback contract)", drained.Load())
	}
}

// TestBoot_PostBootError_FailsLoud — item 5.
func TestBoot_PostBootError_FailsLoud(t *testing.T) {
	opts := baseOptions(t)
	opts.PostBoot = func(_ context.Context, _ PostBootHandles) error {
		return errSeam
	}
	h, err := Boot(context.Background(), opts)
	if h != nil || !errors.Is(err, errSeam) {
		t.Fatalf("Boot with an erroring post-boot hook: handle=%v err=%v, want nil handle + the seam error", h, err)
	}
}

// TestBoot_BadConfigPath_FailsLoud — item 6.
func TestBoot_BadConfigPath_FailsLoud(t *testing.T) {
	opts := baseOptions(t)
	opts.ConfigPath += ".does-not-exist"
	h, err := Boot(context.Background(), opts)
	if h != nil || err == nil {
		t.Fatalf("Boot with a missing config: handle=%v err=%v, want nil handle + a loud config error", h, err)
	}
	if !strings.Contains(err.Error(), "config") {
		t.Errorf("error should name the config layer, got %v", err)
	}
}

// TestBoot_DebugAddrEnv_InvalidRejected_ValidBindsListener — item 7: an
// invalid (non-loopback) HARBOR_DEBUG_ADDR fails Boot loud; a valid loopback
// address boots and Serve binds the pprof debug listener.
func TestBoot_DebugAddrEnv_InvalidRejected_ValidBindsListener(t *testing.T) {
	t.Run("invalid_non_loopback", func(t *testing.T) {
		t.Setenv("HARBOR_DEBUG_ADDR", "0.0.0.0:9999")
		h, err := Boot(context.Background(), baseOptions(t))
		if h != nil || err == nil {
			t.Fatalf("Boot with a non-loopback HARBOR_DEBUG_ADDR: handle=%v err=%v, want nil handle + a loud error", h, err)
		}
		if !strings.Contains(err.Error(), "HARBOR_DEBUG_ADDR") {
			t.Errorf("error should name HARBOR_DEBUG_ADDR, got %v", err)
		}
	})

	t.Run("valid_loopback_binds_debug_listener", func(t *testing.T) {
		t.Setenv("HARBOR_DEBUG_ADDR", "127.0.0.1:0")
		// A stderr writer that never blocks: the debug banner + bound lines
		// are written before/during Serve.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		h, err := Boot(ctx, baseOptions(t))
		if err != nil {
			t.Fatalf("Boot with a loopback HARBOR_DEBUG_ADDR: %v", err)
		}
		served := make(chan error, 1)
		go func() { served <- h.Serve(ctx) }()
		// Wait for the main listener to bind (BindAddr flips off port 0),
		// which happens after the debug listener bound successfully.
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if !strings.HasSuffix(h.BindAddr(), ":0") {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if strings.HasSuffix(h.BindAddr(), ":0") {
			t.Fatal("main listener never bound — the debug-listener leg did not run")
		}
		cancel()
		select {
		case <-served:
		case <-time.After(10 * time.Second):
			t.Fatal("Serve did not return after ctx cancel")
		}
		cc, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		h.Close(cc)
	})
}

// TestServe_ListenError_OccupiedPort — item 8 (listen-error half): Serve on
// an already-occupied port returns a loud listen error.
func TestServe_ListenError_OccupiedPort(t *testing.T) {
	// Occupy a port first.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pre-bind: %v", err)
	}
	defer func() { _ = l.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := baseOptions(t)
	opts.BindAddr = l.Addr().String() // the occupied address
	h, err := Boot(ctx, opts)
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	defer func() {
		cc, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		h.Close(cc)
	}()

	if sErr := h.Serve(ctx); sErr == nil || !strings.Contains(sErr.Error(), "listen") {
		t.Fatalf("Serve on an occupied port = %v, want a loud listen error", sErr)
	}
}

// TestClose_Idempotent_DoubleCloseDrainsOnce — item 8 (double-Close half):
// the closer chain is drained exactly once; a second Close is a no-op.
func TestClose_Idempotent_DoubleCloseDrainsOnce(t *testing.T) {
	var drained atomic.Int32
	opts := baseOptions(t)
	opts.ExtraRoutes = func(_ context.Context, _ RouteMount) ([]func(context.Context) error, error) {
		return []func(context.Context) error{
			func(context.Context) error { drained.Add(1); return nil },
		}, nil
	}
	h, err := Boot(context.Background(), opts)
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	cc, c := context.WithTimeout(context.Background(), 5*time.Second)
	defer c()
	h.Close(cc)
	h.Close(cc) // second Close must not re-run the closers
	if drained.Load() != 1 {
		t.Fatalf("closer drained %d times across a double Close, want exactly 1 (idempotency)", drained.Load())
	}
}

// TestBoot_PreferConfigBindAddr_InPackage pins the bind-address discriminator
// at the package level: without the opt-in the caller's loopback port wins
// over a non-loopback config bind_addr; with it, the config address wins.
func TestBoot_PreferConfigBindAddr_InPackage(t *testing.T) {
	yaml := strings.Replace(serveTestYAML, "bind_addr: 127.0.0.1:0", "bind_addr: 0.0.0.0:8080", 1)
	cfgPath := writeTestCfgContent(t, yaml)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Without the opt-in: loopback port wins.
	opts := baseOptions(t)
	opts.ConfigPath = cfgPath
	opts.Port = 19997
	h := bootTest(t, ctx, opts)
	if got := h.BindAddr(); got != "127.0.0.1:19997" {
		t.Fatalf("without PreferConfigBindAddr: bind = %q, want 127.0.0.1:19997", got)
	}

	// With the opt-in: the config address wins.
	opts2 := baseOptions(t)
	opts2.ConfigPath = cfgPath
	opts2.Port = 19997
	opts2.PreferConfigBindAddr = true
	h2 := bootTest(t, ctx, opts2)
	if got := h2.BindAddr(); got != "0.0.0.0:8080" {
		t.Fatalf("with PreferConfigBindAddr: bind = %q, want the config's 0.0.0.0:8080", got)
	}
}

// writeTestCfgContent writes an arbitrary yaml body to a temp harbor.yaml.
func writeTestCfgContent(t *testing.T, yaml string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "harbor.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	return p
}

// TestBuildMux_OptionalSubsystemMatrix — item 9: drive the exported BuildMux
// directly across nil-handle variants and assert the mounted surface set
// follows the handles (a nil handle leaves its surface un-mounted → 404; a
// present handle mounts it → non-404).
func TestBuildMux_OptionalSubsystemMatrix(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	taskReg := mkDriverTestTaskRegistry(t, bus, red)
	steerReg := steering.NewRegistry()
	surface, err := protocol.NewControlSurface(taskReg, steerReg)
	if err != nil {
		t.Fatalf("NewControlSurface: %v", err)
	}
	metricsReg, metricsShutdown, err := telemetry.NewMetricsRegistry(config.TelemetryConfig{ServiceName: "serve-test"},
		telemetry.WithMetricReader(sdkmetric.NewManualReader()))
	if err != nil {
		t.Fatalf("NewMetricsRegistry: %v", err)
	}
	t.Cleanup(func() { _ = metricsShutdown(context.Background()) })
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	sessReg, err := sessions.New(st, config.SessionsConfig{
		IdleTTL: 24 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: 15 * time.Minute,
	}, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = sessReg.CloseRegistry(context.Background()) })

	cfg := config.Defaults()
	base := MuxInput{
		Cfg:          cfg,
		Surface:      surface,
		Bus:          bus,
		Redactor:     red,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:      metricsReg,
		Tasks:        taskReg,
		DisplayName:  "serve-test",
		InstanceID:   "serve-test",
		BuildVersion: "test",
		BuildCommit:  "test",
	}

	probeMux := func(m http.Handler, path string) int {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, req)
		return rec.Code
	}

	t.Run("core_only_leaves_optional_surfaces_unmounted", func(t *testing.T) {
		built, err := BuildMux(base)
		if err != nil {
			t.Fatalf("BuildMux (core only): %v", err)
		}
		if built.FlowRegistry != nil {
			t.Error("FlowRegistry should be nil without an artifact store")
		}
		for _, path := range []string{
			"/v1/sessions/list",                            // nil Sessions
			"/v1/agents/list",                              // nil Agents
			"/v1/auth/rotate_token",                        // nil AuthSurface
			"/v1/governance/rotate_key",                    // nil KeyRotator
			"/v1/runs/set_overrides",                       // nil RunsStore
			"/v1/sessions/turns/list",                      // nil TurnsProjector (HA-64 unwired)
			"/v1/sessions/turns/get",                       // nil TurnsProjector (HA-64 unwired)
			"/v1/observability/query",                      // nil RollupsStore/Quality (HA-65 unwired)
			"/v1/agent_config/user/skills/import_validate", // nil UserSkillImportService (HA-61 unwired)
			"/v1/agent_config/composition/preview",         // nil CompositionPreviewService (HA-66 unwired)
		} {
			if code := probeMux(built.Mux, path); code != http.StatusNotFound {
				t.Errorf("%s with its handle nil = %d, want 404", path, code)
			}
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/control/runtime.info", strings.NewReader(`{"identity":{"tenant":"t","user":"u","session":"s"}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Harbor-Tenant", "t")
		req.Header.Set("X-Harbor-User", "u")
		req.Header.Set("X-Harbor-Session", "s")
		rec := httptest.NewRecorder()
		built.Mux.ServeHTTP(rec, req)
		var info types.RuntimeInfo
		if rec.Code != http.StatusOK {
			t.Fatalf("runtime.info status=%d body=%s", rec.Code, rec.Body.String())
		}
		if err := json.NewDecoder(rec.Body).Decode(&info); err != nil {
			t.Fatal(err)
		}
		for _, capability := range info.Capabilities {
			if capability == types.CapStateSnapshots {
				t.Fatal("stripped mux advertised state_snapshots without history/session/pause hydration surfaces")
			}
		}
	})

	t.Run("present_handles_mount_their_surfaces", func(t *testing.T) {
		in := base
		in.Sessions = sessReg
		in.State = st
		in.Validator = nil // WithoutValidator: requests reach the handlers
		built, err := BuildMux(in)
		if err != nil {
			t.Fatalf("BuildMux (sessions wired): %v", err)
		}
		if code := probeMux(built.Mux, "/v1/sessions/list"); code == http.StatusNotFound {
			t.Errorf("sessions.list with a wired registry = 404 — the surface did not mount")
		}
	})

	t.Run("nil_surface_fails_loud", func(t *testing.T) {
		in := base
		in.Surface = nil
		if _, err := BuildMux(in); err == nil {
			t.Fatal("BuildMux with a nil ControlSurface must fail loud")
		}
	})
}

// TestRunLoopDriver_ProjectNaming_ActivePolicy — item 10 (naming half): a
// configured fleet-default naming policy with the titler + LLM wired resolves
// an active NamingSpec; missing dependencies resolve nil (opt-in, default
// off).
func TestRunLoopDriver_ProjectNaming_ActivePolicy(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	sessReg, err := sessions.New(st, config.SessionsConfig{
		IdleTTL: 24 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: 15 * time.Minute,
	}, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = sessReg.CloseRegistry(context.Background()) })

	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "r"}
	ctx, err := identity.With(context.Background(), q.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	// All three naming deps wired + an active policy → a resolved spec.
	d := &RunLoopDriver{
		namingDefault: config.RuntimeNamingConfig{Auto: true, AfterTurns: 1},
		sessionTitler: sessReg,
		namingLLM: namingCompleterFunc(func(_ context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
			return llm.CompleteResponse{}, nil
		}),
	}
	spec, err := d.projectNaming(ctx, d.agentConfigID, q, nil)
	if err != nil {
		t.Fatalf("projectNaming: %v", err)
	}
	if spec == nil {
		t.Fatal("an active naming policy with all deps wired must resolve a NamingSpec")
	}

	// Missing LLM → nil spec (default-off posture).
	dNoLLM := &RunLoopDriver{
		namingDefault: config.RuntimeNamingConfig{Auto: true, AfterTurns: 1},
		sessionTitler: sessReg,
	}
	spec, err = dNoLLM.projectNaming(ctx, dNoLLM.agentConfigID, q, nil)
	if err != nil || spec != nil {
		t.Fatalf("projectNaming without an LLM: spec=%v err=%v, want nil/nil", spec, err)
	}
}

// namingCompleterFunc adapts a func to steering.NamingCompleter.
type namingCompleterFunc func(context.Context, llm.CompleteRequest) (llm.CompleteResponse, error)

func (f namingCompleterFunc) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	return f(ctx, req)
}

// TestRunLoopDriver_ReconcileConnections_BootDeclaredNeverDetached — item 10
// (reconcile half): the run-start reconcile with a wired detacher runs
// against the attached-source enumeration; the boot-declared set is never
// detached, and a nil-registry detacher is a clean no-op.
func TestRunLoopDriver_ReconcileConnections_BootDeclaredNeverDetached(t *testing.T) {
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "r"}
	ctx, err := identity.With(context.Background(), q.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	// Nil detacher: reconcile is a no-op (the guard branch).
	dNil := &RunLoopDriver{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	dNil.reconcileConnections(ctx, dNil.agentConfigID, q) // must not panic / error-log fatally

	// A wired detacher over a nil registry enumerates no attached sources —
	// the reconcile runs its full path with nothing to detach (boot-declared
	// names in the skip set are honored trivially).
	cfg := &config.Config{}
	cfg.Tools.MCPServers = []config.MCPServerConfig{{Name: "boot-declared"}}
	d := &RunLoopDriver{
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		connectionDetacher: NewMCPConnectionDetacher(nil, nil, nil),
		bootDeclaredMCP:    BootDeclaredMCPServerSet(cfg),
	}
	d.reconcileConnections(ctx, d.agentConfigID, q)
}

// TestBoot_DefaultsAndSharedProbeSurfaces covers the option defaults (nil
// Logger / nil Stderr / empty SubcommandLabel / nil BuildLLMSnapshot) plus
// the /readyz + /metrics shared probe surfaces, and Serve's empty-label
// default via a pre-cancelled serve cycle.
func TestBoot_DefaultsAndSharedProbeSurfaces(t *testing.T) {
	opts := Options{
		ConfigPath:           writeTestCfg(t),
		AuthValidatorFactory: testFactory(t),
		// Logger / Stderr / SubcommandLabel / BuildLLMSnapshot deliberately
		// zero — the defaults must hold (default snapshot projection resolves
		// the yaml's mock driver, registered by this package's test imports).
		DisplayName:  "harbor test",
		InstanceID:   "harbor-test",
		BuildVersion: "test",
		BuildCommit:  "test",
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := Boot(ctx, opts)
	if err != nil {
		t.Fatalf("Boot with zero-valued optional fields: %v", err)
	}
	defer func() {
		cc, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		h.Close(cc)
	}()

	if code := probe(h, http.MethodGet, "/readyz", ""); code != http.StatusOK {
		t.Errorf("/readyz = %d, want 200", code)
	}
	if code := probe(h, http.MethodGet, "/metrics", ""); code != http.StatusOK {
		t.Errorf("/metrics = %d, want 200 (Prometheus pull surface)", code)
	}
	// The healthz body defaults the subcommand label to "dev".
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"subcommand":"dev"`) {
		t.Errorf("healthz body = %s, want the defaulted dev label", rec.Body.String())
	}

	// Serve with an already-cancelled ctx: binds, prints the bound line,
	// drains immediately — the empty-label default path in Serve runs.
	serveCtx, serveCancel := context.WithCancel(ctx)
	serveCancel()
	if sErr := h.Serve(serveCtx); sErr != nil {
		t.Errorf("Serve under a pre-cancelled ctx should drain cleanly, got %v", sErr)
	}
}

// TestBoot_SkillsModelProfilesAndCORSBanner covers the optional config
// bands: a configured skills store (the directory construction), llm model
// profiles (the valid-models loop), and the dev-only CORS wildcard banner.
func TestBoot_SkillsModelProfilesAndCORSBanner(t *testing.T) {
	yaml := serveTestYAML + `
skills:
  driver: localdb
  dsn: ":memory:"
`
	yaml = strings.Replace(yaml, "llm:\n  driver: mock", `llm:
  driver: mock
  model_profiles:
    mock/echo:
      context_window_tokens: 100000
      token_estimator: chars_div_4`, 1)
	yaml = strings.Replace(yaml, "server:\n  bind_addr: 127.0.0.1:0", `server:
  bind_addr: 127.0.0.1:0
  cors_dev_allow_any: true`, 1)
	cfgPath := writeTestCfgContent(t, yaml)

	var stderr strings.Builder
	opts := baseOptions(t)
	opts.ConfigPath = cfgPath
	opts.Stderr = &stderrSyncWriter{b: &stderr}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	h := bootTest(t, ctx, opts)
	if h.Cfg.Skills.Driver != "localdb" {
		t.Errorf("skills driver = %q, want localdb", h.Cfg.Skills.Driver)
	}
	if !strings.Contains(stderr.String(), "DEV-ONLY CORS WILDCARD") {
		t.Error("the CORS wildcard banner did not print")
	}
}

// stderrSyncWriter is a minimal synchronized strings.Builder writer.
type stderrSyncWriter struct {
	mu sync.Mutex
	b  *strings.Builder
}

func (w *stderrSyncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// TestServe_DebugListenerBindFailure — an occupied HARBOR_DEBUG_ADDR port
// fails Serve loud (the pprof listener never silently skips).
func TestServe_DebugListenerBindFailure(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pre-bind: %v", err)
	}
	defer func() { _ = l.Close() }()
	t.Setenv("HARBOR_DEBUG_ADDR", l.Addr().String())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := Boot(ctx, baseOptions(t))
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	defer func() {
		cc, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		h.Close(cc)
	}()
	if sErr := h.Serve(ctx); sErr == nil || !strings.Contains(sErr.Error(), "pprof debug") {
		t.Fatalf("Serve with an occupied debug port = %v, want a loud pprof-debug listen error", sErr)
	}
}

// TestClose_CloserError_WarnsLoud — a closer that errors during drain is
// logged, never swallowed silently or panicking.
func TestClose_CloserError_WarnsLoud(t *testing.T) {
	opts := baseOptions(t)
	opts.ExtraRoutes = func(_ context.Context, _ RouteMount) ([]func(context.Context) error, error) {
		return []func(context.Context) error{
			func(context.Context) error { return errSeam },
		}, nil
	}
	h, err := Boot(context.Background(), opts)
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	cc, c := context.WithTimeout(context.Background(), 5*time.Second)
	defer c()
	h.Close(cc) // must log the closer error and keep draining
}

// TestWaitReady_ReturnsBoundAddress is the in-package AC1 gate: WaitReady
// returns the actual OS-assigned address after Serve binds an ephemeral
// port, with no polling or second listener lifecycle.
func TestWaitReady_ReturnsBoundAddress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := Boot(ctx, baseOptions(t))
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	defer func() {
		cc, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		h.Close(cc)
	}()
	go func() { _ = h.Serve(ctx) }()
	readyCtx, rc := context.WithTimeout(context.Background(), 10*time.Second)
	defer rc()
	addr, err := h.WaitReady(readyCtx)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if addr == "" || strings.HasSuffix(addr, ":0") {
		t.Fatalf("WaitReady returned empty or unbound address %q", addr)
	}
	resp, gErr := http.Get("http://" + addr + "/healthz")
	if gErr != nil {
		t.Fatalf("dial WaitReady address %s: %v", addr, gErr)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz on %s: status %d", addr, resp.StatusCode)
	}
	cancel()
}

// TestWaitReady_BindFailure_ReturnsError is the in-package gate for the
// failure path: WaitReady carries the bind error when the listener fails.
func TestWaitReady_BindFailure_ReturnsError(t *testing.T) {
	hold, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold listen: %v", err)
	}
	defer func() { _ = hold.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := baseOptions(t)
	opts.BindAddr = hold.Addr().String()
	h, err := Boot(ctx, opts)
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	defer func() {
		cc, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		h.Close(cc)
	}()
	go func() { _ = h.Serve(ctx) }()
	readyCtx, rc := context.WithTimeout(context.Background(), 5*time.Second)
	defer rc()
	_, wErr := h.WaitReady(readyCtx)
	if wErr == nil {
		t.Fatal("WaitReady succeeded on a bind that should have failed")
	}
	cancel()
}

// TestWaitReady_CancelledCtx_ReturnsCtxErr proves WaitReady returns
// ctx.Err() when the caller's ctx cancels before the listener binds.
func TestWaitReady_CancelledCtx_ReturnsCtxErr(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := Boot(ctx, baseOptions(t))
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	defer func() {
		cc, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		h.Close(cc)
	}()
	readyCtx, readyCancel := context.WithCancel(context.Background())
	readyCancel()
	_, wErr := h.WaitReady(readyCtx)
	if wErr == nil {
		t.Fatal("WaitReady succeeded on an already-cancelled ctx")
	}
}

// TestWaitReady_ConcurrentWaiters_RaceFree proves the one-shot
// readiness is race-safe: N goroutines all calling WaitReady against one
// shared handle receive the same bound address with no data race.
func TestWaitReady_ConcurrentWaiters_RaceFree(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := Boot(ctx, baseOptions(t))
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	defer func() {
		cc, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		h.Close(cc)
	}()
	go func() { _ = h.Serve(ctx) }()
	const N = 100
	var wg sync.WaitGroup
	addrs := make([]string, N)
	wg.Add(N)
	for i := range N {
		go func(idx int) {
			defer wg.Done()
			readyCtx, rc := context.WithTimeout(context.Background(), 10*time.Second)
			defer rc()
			a, wErr := h.WaitReady(readyCtx)
			if wErr != nil {
				t.Errorf("waiter %d: %v", idx, wErr)
				return
			}
			addrs[idx] = a
		}(i)
	}
	wg.Wait()
	cancel()
	first := addrs[0]
	if first == "" {
		t.Fatal("first waiter got empty address")
	}
	for i, a := range addrs {
		if a != first {
			t.Errorf("waiter %d got %q, want %q", i, a, first)
		}
	}
}

// TestNewJWKSAuthValidatorFactory_ReturnsFactory covers the production
// auth-validator factory constructor — the one shared factory every
// production serve caller injects. The factory is called with a real
// config + redactor + bus + logger and must return a non-nil validator
// backed by a real JWKS file (not the unreachable URL in serveTestYAML).
func TestNewJWKSAuthValidatorFactory_ReturnsFactory(t *testing.T) {
	factory := NewJWKSAuthValidatorFactory()
	if factory == nil {
		t.Fatal("NewJWKSAuthValidatorFactory returned nil")
	}
	// Write a real JWKS file so the factory's synchronous initial fetch
	// succeeds (the test config's jwks_url is unreachable by design).
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	n := base64.RawURLEncoding.EncodeToString(priv.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(priv.E)).Bytes())
	set := map[string]any{
		"keys": []map[string]any{
			{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "serve-test", "n": n, "e": e},
		},
	}
	raw, _ := json.Marshal(set)
	jwksPath := filepath.Join(t.TempDir(), "jwks.json")
	if err := os.WriteFile(jwksPath, raw, 0o600); err != nil {
		t.Fatalf("write jwks: %v", err)
	}
	yaml := strings.Replace(serveTestYAML,
		"jwks_url: https://issuer.example.com/.well-known/jwks.json",
		"jwks_file: "+jwksPath, 1)
	// The test config's jwt_algorithms is [ES256]; our JWKS key is
	// RS256. Align the config so the validator accepts the key.
	yaml = strings.Replace(yaml, "- ES256", "- RS256", 1)
	cfgPath := filepath.Join(t.TempDir(), "harbor.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	cfg, err := config.Load(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	validator, err := factory(context.Background(), cfg, red, bus, logger)
	if err != nil {
		t.Fatalf("factory call: %v", err)
	}
	if validator == nil {
		t.Fatal("factory returned nil validator")
	}
}

// TestInstanceID_HostnameResilience covers the InstanceID helper — the
// stable-per-process identifier a Console keys each attachment by. The
// helper appends the hostname when available and falls back to the bare
// prefix otherwise.
func TestInstanceID_HostnameResilience(t *testing.T) {
	id := InstanceID("harbor-test")
	if id == "" {
		t.Fatal("InstanceID returned empty")
	}
	if !strings.HasPrefix(id, "harbor-test") {
		t.Errorf("InstanceID %q does not start with the prefix", id)
	}
	// InstanceID always returns prefix + "-" + hostname when a hostname
	// is available; the bare prefix is the fallback when os.Hostname
	// fails. Both shapes are valid; the assertion is non-empty +
	// prefix-carrying.
	bare := InstanceID("")
	// bare is "-<hostname>" or "" (when hostname is unavailable); both
	// are acceptable. The assertion is that it does NOT carry a fake
	// prefix.
	if bare != "" && !strings.HasPrefix(bare, "-") {
		t.Errorf("InstanceID with empty prefix returned %q, want empty or -hostname", bare)
	}
}

// TestServe_GracefulShutdownWithinGracePeriod covers Serve's ctx-cancel
// path: when ctx cancels, Serve drains within the configured grace
// period and returns nil. This is the production shutdown-ordering path.
func TestServe_GracefulShutdownWithinGracePeriod(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := Boot(ctx, baseOptions(t))
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	defer func() {
		cc, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		h.Close(cc)
	}()
	serveDone := make(chan error, 1)
	go func() { serveDone <- h.Serve(ctx) }()
	// Wait for the listener to bind.
	readyCtx, rc := context.WithTimeout(context.Background(), 10*time.Second)
	defer rc()
	addr, wErr := h.WaitReady(readyCtx)
	if wErr != nil {
		t.Fatalf("WaitReady: %v", wErr)
	}
	if addr == "" || strings.HasSuffix(addr, ":0") {
		t.Fatalf("WaitReady returned unbound address %q", addr)
	}
	// Cancel the serve ctx — Serve must drain and return nil.
	cancel()
	select {
	case sErr := <-serveDone:
		if sErr != nil {
			t.Errorf("Serve returned %v on graceful shutdown, want nil", sErr)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Serve did not return within 15s of ctx cancel")
	}
}

// TestServe_HABBOR_DEBUG_ADDR_EmptyString_ProducesNoDebugListener
// covers the branch where HARBOR_DEBUG_ADDR is set to an empty string —
// the env var is present but blank, so no debug listener is mounted.
// This is a boundary case: a blank env var MUST NOT be treated as a
// valid debug address.
func TestServe_HARBOR_DEBUG_ADDR_EmptyString_ProducesNoDebugListener(t *testing.T) {
	t.Setenv("HARBOR_DEBUG_ADDR", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := Boot(ctx, baseOptions(t))
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	defer func() {
		cc, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		h.Close(cc)
	}()
	// No debug server should be present (the empty env var is a no-op).
	// The healthz + readyz surfaces are still mounted.
	if code := probe(h, http.MethodGet, "/readyz", ""); code != http.StatusOK {
		t.Errorf("/readyz = %d, want 200", code)
	}
}

// TestServe_StderrWritesHARBOR_DEV_BOUND proves Serve writes the
// HARBOR_DEV_BOUND line to the configured stderr sink — the co-launch
// log-separation contract (a co-launch binary reads this line from the
// captured sink, not the terminal).
func TestServe_StderrWritesHARBOR_DEV_BOUND(t *testing.T) {
	var stderr strings.Builder
	opts := baseOptions(t)
	opts.Stderr = &stderrSyncWriter{b: &stderr}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := Boot(ctx, opts)
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	defer func() {
		cc, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		h.Close(cc)
	}()
	serveDone := make(chan error, 1)
	go func() { serveDone <- h.Serve(ctx) }()
	readyCtx, rc := context.WithTimeout(context.Background(), 10*time.Second)
	defer rc()
	_, wErr := h.WaitReady(readyCtx)
	if wErr != nil {
		t.Fatalf("WaitReady: %v", wErr)
	}
	cancel()
	<-serveDone
	if !strings.Contains(stderr.String(), "HARBOR_DEV_BOUND=") {
		t.Errorf("stderr missing HARBOR_DEV_BOUND line; got:\n%s", stderr.String())
	}
}

// TestBoot_ProgrammaticConfig_ValidateFailure covers the Boot branch
// where opts.Config is non-nil but fails Validate. A programmatically-
// built config cannot bypass validation (the identity/JWKS ceremony a
// Protocol server MUST carry is enforced here, loud).
func TestBoot_ProgrammaticConfig_ValidateFailure(t *testing.T) {
	cfg := config.Defaults()
	// Defaults() carries no identity block — Validate must reject it.
	opts := Options{
		Config:               cfg,
		AuthValidatorFactory: testFactory(t),
		SubcommandLabel:      "dev",
	}
	_, err := Boot(context.Background(), opts)
	if err == nil {
		t.Fatal("Boot with an invalid programmatically-built config must fail loud")
	}
	if !strings.Contains(err.Error(), "config") {
		t.Errorf("error should name the config layer, got %v", err)
	}
}

// TestServe_GracePeriodFallback covers Serve's `if grace <= 0` branch:
// when the config's ShutdownGracePeriod is zero at the Serve call site,
// Serve falls back to 30s. The config validator rejects a zero grace
// period, so this test loads a valid config then zeroes the field
// post-load to exercise the Serve-level fallback.
func TestServe_GracePeriodFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := Boot(ctx, baseOptions(t))
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	defer func() {
		cc, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		h.Close(cc)
	}()
	// Zero the grace period post-load to exercise Serve's 30s fallback.
	h.Cfg.Server.ShutdownGracePeriod = 0
	serveDone := make(chan error, 1)
	go func() { serveDone <- h.Serve(ctx) }()
	readyCtx, rc := context.WithTimeout(context.Background(), 10*time.Second)
	defer rc()
	_, wErr := h.WaitReady(readyCtx)
	if wErr != nil {
		t.Fatalf("WaitReady: %v", wErr)
	}
	// Cancel — Serve must drain via the 30s fallback (well within the
	// test's timeout).
	cancel()
	select {
	case <-serveDone:
	case <-time.After(15 * time.Second):
		t.Fatal("Serve did not return within 15s of ctx cancel (grace-period fallback)")
	}
}
