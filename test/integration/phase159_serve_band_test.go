package integration

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	_ "github.com/hurtener/Harbor/internal/drivers/prod"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	_ "github.com/hurtener/Harbor/internal/llm/mock"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/runtime/serve"
)

const serveBandTestYAML = `
server:
  bind_addr: 127.0.0.1:0
  shutdown_grace_period: 5s
identity:
  jwt_algorithms:
    - ES256
  issuer: https://issuer.example.com
  audience: harbor
  jwks_url: https://issuer.example.com/.well-known/jwks.json
telemetry:
  log_format: text
  log_level: error
  service_name: harbor-serveband-it
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

type serveBandKeySet struct {
	kid string
	pub *ecdsa.PublicKey
}

func (k *serveBandKeySet) KeyByID(kid string) (crypto.PublicKey, string, error) {
	if kid != k.kid {
		return nil, "", fmt.Errorf("kid %q not known", kid)
	}
	return k.pub, "ES256", nil
}

func writeServeBandCfg(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "harbor.yaml")
	if err := os.WriteFile(p, []byte(serveBandTestYAML), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	return p
}

// TestE2E_ServeBand_RealDrivers_IdentityPropagation boots the promoted serve
// band with real drivers behind an injected validator factory, drives
// /healthz + a canonical Protocol method over the wire, and proves a
// missing-identity request is rejected (the failure mode).
func TestE2E_ServeBand_RealDrivers_IdentityPropagation(t *testing.T) {
	cfgPath := writeServeBandCfg(t)
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	ks := &serveBandKeySet{kid: "serveband", pub: &priv.PublicKey}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := serve.Boot(ctx, serve.Options{
		ConfigPath:      cfgPath,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		Stderr:          io.Discard,
		SubcommandLabel: "serve",
		AuthValidatorFactory: func(_ context.Context, _ *config.Config, red audit.Redactor, bus events.EventBus, lg *slog.Logger) (auth.Validator, error) {
			return auth.NewValidator(ks, auth.WithRedactor(red), auth.WithEventBus(bus), auth.WithLogger(lg))
		},
		BuildLLMSnapshot: func(cfg *config.Config) (*llm.ConfigSnapshot, error) {
			snap := llm.SnapshotFromConfig(cfg.LLM, cfg.Artifacts)
			return &snap, nil
		},
		DisplayName:  "harbor serveband",
		InstanceID:   "harbor-serveband",
		BuildVersion: "test",
		BuildCommit:  "test",
	})
	if err != nil {
		t.Fatalf("serve.Boot: %v", err)
	}
	defer func() {
		cc, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		h.Close(cc)
	}()

	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	// /healthz answers 200.
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200", resp.StatusCode)
	}

	// A canonical Protocol method WITHOUT identity is rejected at the auth
	// edge (401) — identity is mandatory (the failure mode).
	code := postJSON(t, srv.URL+"/v1/agents/list", `{}`, "")
	if code != http.StatusUnauthorized {
		t.Fatalf("agents.list without a token = %d, want 401 (identity mandatory)", code)
	}
}

// TestE2E_ServeBand_CmdAndDevstack_SameSurfaceSet is the anti-drift assertion:
// the promoted serve.Boot handler (the cmd thin-caller) and the devstack
// thin-caller mount the SAME shared Protocol surface set. The surfaces the
// kit's hand-mirrored mux OMITTED (agents / governance / governance-key-rotate
// / auth-rotate) are now mounted on BOTH — closing the drift single-homing was
// meant to fix. A route that is mounted answers 401 unauthenticated; an
// unmounted route answers 404.
func TestE2E_ServeBand_CmdAndDevstack_SameSurfaceSet(t *testing.T) {
	cfgPath := writeServeBandCfg(t)
	cfg, err := config.Load(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	// The devstack thin-caller.
	kit := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
	defer kit.Close()
	kitSrv := httptest.NewServer(kit.Handler)
	defer kitSrv.Close()

	// The cmd thin-caller (promoted serve.Boot) — dev-composed so the
	// dev-only auth-rotate surface is present on this side too (parity with
	// the kit, which mints an ephemeral signer).
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	ks := &serveBandKeySet{kid: "serveband", pub: &priv.PublicKey}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := serve.Boot(ctx, serve.Options{
		ConfigPath:      cfgPath,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		Stderr:          io.Discard,
		SubcommandLabel: "dev",
		AuthValidatorFactory: func(_ context.Context, _ *config.Config, red audit.Redactor, bus events.EventBus, lg *slog.Logger) (auth.Validator, error) {
			return auth.NewValidator(ks, auth.WithRedactor(red), auth.WithEventBus(bus), auth.WithLogger(lg))
		},
		BuildAuthSurface: func(red audit.Redactor, bus events.EventBus, lg *slog.Logger) (*auth.RotateSurface, error) {
			return auth.NewRotateSurface(&serveBandIssuer{priv: priv}, red, auth.WithRotateBus(bus))
		},
		BuildLLMSnapshot: func(cfg *config.Config) (*llm.ConfigSnapshot, error) {
			snap := llm.SnapshotFromConfig(cfg.LLM, cfg.Artifacts)
			return &snap, nil
		},
		DisplayName:  "harbor serveband",
		InstanceID:   "harbor-serveband",
		BuildVersion: "test",
		BuildCommit:  "test",
	})
	if err != nil {
		t.Fatalf("serve.Boot: %v", err)
	}
	defer func() {
		cc, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		h.Close(cc)
	}()
	cmdSrv := httptest.NewServer(h.Handler())
	defer cmdSrv.Close()

	// The shared surface set — including the four surfaces the kit's mirror
	// previously omitted.
	sharedMethods := []string{
		"/v1/agents/list",                     // WithAgentsService
		"/v1/governance/get_tenant_overrides", // WithGovernanceService
		"/v1/governance/rotate_key",           // WithGovernanceKeyRotate
		"/v1/auth/rotate_token",               // WithAuthSurface
		"/v1/tools/describe",                  // WithToolsService
		"/v1/sessions/list",                   // WithSessionsService
	}
	for _, m := range sharedMethods {
		cmdCode := postJSON(t, cmdSrv.URL+m, `{}`, "")
		kitCode := postJSON(t, kitSrv.URL+m, `{}`, "")
		if cmdCode == http.StatusNotFound {
			t.Errorf("%s: NOT mounted on the cmd thin-caller (serve.Boot) — got 404", m)
		}
		if kitCode == http.StatusNotFound {
			t.Errorf("%s: NOT mounted on the devstack thin-caller — got 404 (the single-homing did not close the drift)", m)
		}
	}
}

// serveBandIssuer satisfies auth.TokenIssuer for the rotate surface. The
// anti-drift test only checks the route is MOUNTED (an unauthenticated request
// is rejected before IssueToken is reached), so a minimal issuer suffices.
type serveBandIssuer struct{ priv *ecdsa.PrivateKey }

func (i *serveBandIssuer) IssueToken(_ context.Context, id identity.Identity, _ []auth.Scope, now time.Time) (string, time.Time, error) {
	return "", now, fmt.Errorf("serveBandIssuer: not used in the anti-drift probe")
}

func postJSON(t *testing.T, url, body, bearer string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
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
	_ = resp.Body.Close()
	return resp.StatusCode
}
