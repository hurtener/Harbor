// Helpers for the Phase 64 integration test. The dev cmd lives in
// `package main` (cmd/harbor) and cannot be imported from a _test.go
// outside that package — so this file mirrors the dev-stack assembly
// from cmd_dev.go, using the same registries the dev cmd reaches
// for. The duplication is acceptable per CLAUDE.md §17.2: integration
// tests live close to the seam they exercise; here the seam IS the
// composed runtime + protocol surface.

package integration_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/memory"
	memoryinmem "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
)

// phase64TestKeySet is the in-test auth.KeySet shape — kid → (pub, alg)
// — mirroring cmd/harbor's devKeySet.
type phase64TestKeySet struct {
	kid string
	pub *ecdsa.PublicKey
}

func (s *phase64TestKeySet) KeyByID(kid string) (crypto.PublicKey, string, error) {
	if kid != s.kid {
		return nil, "", fmt.Errorf("kid %q not known", kid)
	}
	return s.pub, "ES256", nil
}

// phase64TestStack is the bundle the integration test builds. Mirrors
// the production cmd/harbor `devStack` shape (handler + close fns).
type phase64TestStack struct {
	handler http.Handler
	closers []func(context.Context) error
}

// close runs every subsystem's Close in reverse dependency order.
func (s *phase64TestStack) close() {
	ctx := context.Background()
	for i := len(s.closers) - 1; i >= 0; i-- {
		_ = s.closers[i](ctx)
	}
}

// buildPhase64TestStack assembles a Phase-64-shaped dev stack against
// the test's dev config. The LLM driver is overridden to "mock" (the
// blank-import at the top of the test seats the registration) — the
// integration test thus exercises the SAME wiring path the dev cmd
// follows when HARBOR_DEV_ALLOW_MOCK=1 is set. Returns the stack
// plus a Bearer token signed by the in-test ES256 key.
//
// On any boot failure t.Fatal — every component is mandatory and a
// failure here means the test fixture has drifted from production.
func buildPhase64TestStack(t *testing.T) (*phase64TestStack, string) {
	t.Helper()
	cfgPath := writeDevConfig(t)
	cfg, err := config.Load(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	var closers []func(context.Context) error

	red := auditpatterns.New()

	bus, err := events.Open(context.Background(), cfg.Events, audit.Redactor(red))
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	closers = append(closers, bus.Close)

	stateStore, err := state.Open(context.Background(), cfg.State)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	closers = append(closers, stateStore.Close)

	artStore, err := artifacts.Open(context.Background(), cfg.Artifacts)
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	closers = append(closers, artStore.Close)

	// LLM client via the "mock" driver — Phase 64 routes through this
	// path when HARBOR_DEV_ALLOW_MOCK=1 fires. The mock is blank-
	// imported at the top of this test file.
	llmCfg := llm.ConfigSnapshot{
		Driver:               "mock",
		ContextWindowReserve: cfg.LLM.ContextWindowReserve,
		HeavyOutputThreshold: cfg.Artifacts.HeavyOutputThresholdBytes,
		ModelProfiles: map[string]llm.ModelProfile{
			"anthropic/claude-sonnet-4": {
				ContextWindowTokens: 200000,
				TokenEstimator:      "chars_div_4",
			},
		},
	}
	llmClient, err := llm.Open(context.Background(), llmCfg, llm.Deps{
		Artifacts: artStore,
		Bus:       bus,
	})
	if err != nil {
		t.Fatalf("llm.Open: %v", err)
	}
	closers = append(closers, llmClient.Close)

	// Memory store (strategy=none in the test fixture; the rolling-
	// summary path is exercised by the per-package summarizer test).
	memCfg := memory.ConfigSnapshot{
		Driver:             cfg.Memory.Driver,
		Strategy:           memory.Strategy(cfg.Memory.Strategy),
		BudgetTokens:       cfg.Memory.BudgetTokens,
		RecoveryBacklogMax: cfg.Memory.RecoveryBacklogMax,
	}
	memStore, err := memoryinmem.New(memCfg, memory.Deps{
		State: stateStore,
		Bus:   bus,
	}, memoryinmem.Options{})
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}
	closers = append(closers, memStore.Close)

	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    stateStore,
		Bus:      bus,
		Redactor: audit.Redactor(red),
		Cfg:      cfg.Tasks,
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	closers = append(closers, taskReg.Close)

	steeringReg := steering.NewRegistry()

	surface, err := protocol.NewControlSurface(taskReg, steeringReg)
	if err != nil {
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}

	// Mint an in-test ES256 keypair + a default-identity dev token.
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	keys := &phase64TestKeySet{kid: "harbor-test", pub: &priv.PublicKey}

	validator, err := auth.NewValidator(keys,
		auth.WithRedactor(audit.Redactor(red)),
	)
	if err != nil {
		t.Fatalf("auth.NewValidator: %v", err)
	}

	mux, err := transports.NewMux(surface, bus,
		transports.WithValidator(validator),
	)
	if err != nil {
		t.Fatalf("transports.NewMux: %v", err)
	}

	// Build the dev-shaped token: ES256-signed, kid=harbor-test, identity
	// triple (tenant=dev, user=dev, session=dev), admin scope.
	now := time.Now()
	tokObj := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss":     "harbor-test",
		"sub":     "dev",
		"aud":     "harbor",
		"exp":     now.Add(1 * time.Hour).Unix(),
		"nbf":     now.Add(-1 * time.Minute).Unix(),
		"iat":     now.Unix(),
		"tenant":  "dev",
		"user":    "dev",
		"session": "dev",
		"scopes":  []string{"admin", "console:fleet"},
	})
	tokObj.Header["kid"] = "harbor-test"
	token, err := tokObj.SignedString(priv)
	if err != nil {
		t.Fatalf("sign dev token: %v", err)
	}

	// Compose the same router shape cmd_dev.go does: /healthz +
	// /readyz on the surface root + /v1/* delegated to the protocol
	// mux. The integration test only hits /v1/*, but the /healthz
	// inclusion keeps the test routing symmetric to the production
	// dev cmd.
	router := http.NewServeMux()
	router.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","subcommand":"dev"}`))
	})
	router.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
	router.Handle("/v1/", mux)

	return &phase64TestStack{handler: router, closers: closers}, token
}
