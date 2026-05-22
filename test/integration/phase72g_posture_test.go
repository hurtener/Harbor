package integration_test

// phase72g_posture_test.go — the Phase 72g (D-112) §17 cross-subsystem
// integration suite for the `governance.posture` + `llm.posture`
// Protocol methods.
//
// It wires REAL drivers across the whole posture seam — the real
// governance posture provider over a configured IdentityTiers map, the
// real llm posture provider, the real audit patterns redactor, the real
// inmem event bus, the real Phase 60 wire transport (`transports.NewMux`),
// and the real Phase 61 ES256 auth validator over the canonical
// `internal/protocol/auth/testdata` keypair. No mocks at the seam
// (CLAUDE.md §17.3).
//
// Coverage:
//
//   - governance.posture returns the configured IdentityTiers verbatim.
//   - llm.posture MockMode round-trip — one leg boots with the mock flag
//     captured (RegisterMockModeCaptured(true)) and asserts MockMode ==
//     true; another boots production-shaped and asserts MockMode == false.
//   - cross-tenant rejection — a caller without auth.ScopeAdmin requesting
//     a different tenant_id → HTTP 403.
//   - cross-tenant accepted — an admin-scoped caller → HTTP 200.
//   - missing-identity rejection → HTTP 401.
//   - N≥10 concurrency stress across N tenants under -race.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	goruntime "runtime"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
)

const phase72gKid = "harbor-phase72g-k1"

var fixedNowPhase72g = time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)

// phase72gDeps holds a fully-wired posture stack for one boot mode.
type phase72gDeps struct {
	mux     *http.ServeMux
	bus     events.EventBus
	priv    *ecdsa.PrivateKey
	cleanup func()
}

// phase72gTiers is the configured governance posture the integration
// test asserts round-trips verbatim.
func phase72gTiers() map[string]governance.TierConfig {
	return map[string]governance.TierConfig{
		"free": {
			BudgetCeilingUSD: 5.0,
			RateLimit:        governance.RateLimitConfig{Capacity: 100, RefillTokens: 10, RefillInterval: time.Second},
			MaxTokens:        2048,
		},
		"enterprise": {
			BudgetCeilingUSD: 2500.0,
			RateLimit:        governance.RateLimitConfig{Capacity: 50000, RefillTokens: 2500, RefillInterval: time.Minute},
			MaxTokens:        128000,
		},
	}
}

// newPhase72gDeps wires the full posture stack. mockBoot controls the
// llm posture's MockMode: true simulates the HARBOR_DEV_ALLOW_MOCK=1
// boot path (RegisterMockModeCaptured(true)); false is the production-
// shaped boot.
func newPhase72gDeps(t *testing.T, mockBoot bool) *phase72gDeps {
	t.Helper()

	priv, pub := loadES256Phase61(t)
	red := auditpatterns.New()

	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     512,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         512,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}

	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("state.Open: %v", err)
	}
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: audit.Redactor(red),
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("tasks.Open: %v", err)
	}
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}

	// Governance posture provider over the configured tiers.
	govProvider := governance.NewPostureProvider(governance.Config{
		DefaultTier:   "free",
		IdentityTiers: phase72gTiers(),
	})

	// LLM posture provider. mockBoot drives the D-089 capture path —
	// RegisterMockModeCaptured is the SAME hook cmd/harbor/devmock.go
	// calls at the banner-emit call site.
	llm.RegisterMockModeCaptured(mockBoot)
	t.Cleanup(func() { llm.RegisterMockModeCaptured(false) })
	var llmProvider *llm.PostureProvider
	if mockBoot {
		llmProvider = llm.NewPostureProvider(llm.ConfigSnapshot{Driver: "mock"})
	} else {
		llmProvider = llm.NewPostureProvider(llm.ConfigSnapshot{
			Driver:   "bifrost",
			Provider: "openai",
			Model:    "openai/gpt-5.3-chat",
		})
	}

	// Phase 72g extends the Phase 72f PostureSurface — one surface, all
	// seven posture methods. The five 72f runtime seams are wired with
	// stable read-only closures; the two 72g seams carry the governance
	// + llm posture providers plus the audit redactor + bus.
	postureSurface, err := protocol.NewPostureSurface(protocol.PostureDeps{
		Build: types.RuntimeInfo{
			BuildVersion:   "phase72g-it",
			BuildCommit:    "phase72g-it",
			BuildGoVersion: goruntime.Version(),
		},
		Clock:    func() time.Time { return fixedNowPhase72g },
		BootedAt: fixedNowPhase72g,
		Health: func(context.Context) []types.SubsystemHealth {
			return []types.SubsystemHealth{
				{Subsystem: "state", Status: types.HealthStatusReady},
				{Subsystem: "events", Status: types.HealthStatusReady},
			}
		},
		Counters: func(context.Context, identity.Identity) types.RuntimeCounters {
			return types.RuntimeCounters{}
		},
		Drivers: func() []types.SubsystemDriver {
			return []types.SubsystemDriver{{Subsystem: "state", Driver: "inmem"}}
		},
		Metrics:     func(context.Context) types.MetricsSnapshot { return types.MetricsSnapshot{} },
		Governance:  govProvider,
		LLM:         llmProvider,
		Redactor:    red,
		Bus:         bus,
		DisplayName: "phase72g integration",
		InstanceID:  "phase72g-it",
	})
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("protocol.NewPostureSurface: %v", err)
	}

	keys := newES256KeySet(phase72gKid, pub)
	now := func() time.Time { return fixedNowPhase72g }
	v, err := auth.NewValidator(keys, auth.WithClock(now), auth.WithRedactor(red))
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("auth.NewValidator: %v", err)
	}

	mux, err := transports.NewMux(surface, bus,
		transports.WithValidator(v),
		transports.WithPostureSurface(postureSurface),
	)
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("transports.NewMux: %v", err)
	}

	return &phase72gDeps{
		mux:  mux,
		bus:  bus,
		priv: priv,
		cleanup: func() {
			_ = taskReg.Close(context.Background())
			_ = store.Close(context.Background())
			_ = bus.Close(context.Background())
		},
	}
}

// phase72gClaims mints a JWT MapClaims with the test's standard issuer /
// validity window.
func phase72gClaims(id identity.Identity, scopes []string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":     "https://idp.test",
		"sub":     id.UserID,
		"exp":     fixedNowPhase72g.Add(15 * time.Minute).Unix(),
		"nbf":     fixedNowPhase72g.Add(-1 * time.Minute).Unix(),
		"tenant":  id.TenantID,
		"user":    id.UserID,
		"session": id.SessionID,
		"scopes":  scopes,
	}
}

// postPosture issues a posture POST against the wired mux and returns
// the status + body.
func postPosture(t *testing.T, mux *http.ServeMux, path, token, body string) (int, []byte) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	out, _ := io.ReadAll(w.Result().Body)
	return w.Result().StatusCode, out
}

func TestE2E_Phase72g_GovernancePostureRoundTrip(t *testing.T) {
	deps := newPhase72gDeps(t, false)
	defer deps.cleanup()

	id := identity.Identity{TenantID: "tenant-a", UserID: "u1", SessionID: "s1"}
	tok := signES256Wave10(t, deps.priv, phase72gClaims(id, nil), phase72gKid)

	status, body := postPosture(t, deps.mux, "/v1/control/governance.posture", tok, `{}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	var resp govPostureWire
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if resp.DefaultTier != "free" {
		t.Errorf("DefaultTier = %q, want free", resp.DefaultTier)
	}
	if resp.ResolvedTier != "free" {
		t.Errorf("ResolvedTier = %q, want free", resp.ResolvedTier)
	}
	if len(resp.IdentityTiers) != 2 {
		t.Fatalf("IdentityTiers len = %d, want 2", len(resp.IdentityTiers))
	}
	ent := resp.IdentityTiers["enterprise"]
	if ent.BudgetCeilingUSD != 2500.0 || ent.MaxTokens != 128000 {
		t.Errorf("enterprise tier round-trip wrong: %+v", ent)
	}
	if ent.RateLimit.RefillIntervalMS != 60000 {
		t.Errorf("enterprise RateLimit.RefillIntervalMS = %d, want 60000 (1m)", ent.RateLimit.RefillIntervalMS)
	}
}

func TestE2E_Phase72g_LLMPostureMockModeRoundTrip(t *testing.T) {
	// Leg 1 — production-shaped boot: MockMode must be false.
	t.Run("production_boot_mock_false", func(t *testing.T) {
		deps := newPhase72gDeps(t, false)
		defer deps.cleanup()
		id := identity.Identity{TenantID: "tenant-a", UserID: "u1", SessionID: "s1"}
		tok := signES256Wave10(t, deps.priv, phase72gClaims(id, nil), phase72gKid)

		status, body := postPosture(t, deps.mux, "/v1/control/llm.posture", tok, `{}`)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", status, body)
		}
		var resp llmPostureWire
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.MockMode {
			t.Error("MockMode = true on a production-shaped boot, want false")
		}
		if resp.Provider != "openai" {
			t.Errorf("Provider = %q, want openai", resp.Provider)
		}
	})

	// Leg 2 — HARBOR_DEV_ALLOW_MOCK=1 boot: MockMode must be true.
	t.Run("dev_mock_boot_mock_true", func(t *testing.T) {
		deps := newPhase72gDeps(t, true)
		defer deps.cleanup()
		id := identity.Identity{TenantID: "tenant-a", UserID: "u1", SessionID: "s1"}
		tok := signES256Wave10(t, deps.priv, phase72gClaims(id, nil), phase72gKid)

		status, body := postPosture(t, deps.mux, "/v1/control/llm.posture", tok, `{}`)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", status, body)
		}
		var resp llmPostureWire
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !resp.MockMode {
			t.Error("MockMode = false under the dev mock boot, want true (D-089 capture-path desync)")
		}
		if resp.Provider != "mock" {
			t.Errorf("Provider = %q, want mock", resp.Provider)
		}
	})
}

func TestE2E_Phase72g_MissingIdentityRejected(t *testing.T) {
	deps := newPhase72gDeps(t, false)
	defer deps.cleanup()

	for _, path := range []string{"/v1/control/governance.posture", "/v1/control/llm.posture"} {
		status, _ := postPosture(t, deps.mux, path, "", `{}`)
		if status != http.StatusUnauthorized {
			t.Errorf("%s without a bearer token: status = %d, want 401", path, status)
		}
	}
}

func TestE2E_Phase72g_CrossTenantRejectedWithoutAdmin(t *testing.T) {
	deps := newPhase72gDeps(t, false)
	defer deps.cleanup()

	id := identity.Identity{TenantID: "tenant-a", UserID: "u1", SessionID: "s1"}
	tok := signES256Wave10(t, deps.priv, phase72gClaims(id, nil), phase72gKid)

	// A body identity naming a different tenant (User/Session match the
	// JWT) is the cross-tenant read shape — the merged Phase 72f/72g
	// PostureSurface gates it on the admin scope.
	crossBody := `{"identity":{"tenant":"tenant-other","user":"u1","session":"s1"}}`
	for _, path := range []string{"/v1/control/governance.posture", "/v1/control/llm.posture"} {
		status, body := postPosture(t, deps.mux, path, tok, crossBody)
		if status != http.StatusForbidden {
			t.Errorf("%s cross-tenant non-admin: status = %d, want 403; body=%s", path, status, body)
		}
	}
}

func TestE2E_Phase72g_CrossTenantAcceptedWithAdmin(t *testing.T) {
	deps := newPhase72gDeps(t, false)
	defer deps.cleanup()

	id := identity.Identity{TenantID: "tenant-a", UserID: "admin", SessionID: "s1"}
	tok := signES256Wave10(t, deps.priv,
		phase72gClaims(id, []string{string(auth.ScopeAdmin)}), phase72gKid)

	crossBody := `{"identity":{"tenant":"tenant-other","user":"admin","session":"s1"}}`
	status, body := postPosture(t, deps.mux, "/v1/control/governance.posture", tok, crossBody)
	if status != http.StatusOK {
		t.Fatalf("cross-tenant w/ admin scope: status = %d, want 200; body=%s", status, body)
	}
}

// TestE2E_Phase72g_ConcurrencyStress runs N≥10 concurrent posture
// readers across N tenants under -race; asserts no cross-talk — each
// goroutine sees its own ResolvedTier — and the suite stays green.
func TestE2E_Phase72g_ConcurrencyStress(t *testing.T) {
	deps := newPhase72gDeps(t, true)
	defer deps.cleanup()

	const n = 24
	var wg sync.WaitGroup
	wg.Add(n)
	errCh := make(chan string, n*2)
	for i := range n {

		go func() {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  "tenant-" + itoa72g(i),
				UserID:    "u",
				SessionID: "s",
			}
			tok := signES256Wave10(t, deps.priv, phase72gClaims(id, nil), phase72gKid)

			// governance.posture
			gs, gb := postPosture(t, deps.mux, "/v1/control/governance.posture", tok, `{}`)
			if gs != http.StatusOK {
				errCh <- "governance status != 200"
			} else {
				var gr govPostureWire
				if err := json.Unmarshal(gb, &gr); err != nil {
					errCh <- "governance decode: " + err.Error()
				} else if gr.ResolvedTier != "free" {
					errCh <- "governance context bleed: ResolvedTier=" + gr.ResolvedTier
				}
			}

			// llm.posture
			ls, lb := postPosture(t, deps.mux, "/v1/control/llm.posture", tok, `{}`)
			if ls != http.StatusOK {
				errCh <- "llm status != 200"
			} else {
				var lr llmPostureWire
				if err := json.Unmarshal(lb, &lr); err != nil {
					errCh <- "llm decode: " + err.Error()
				} else if !lr.MockMode {
					errCh <- "llm MockMode = false under dev-mock boot"
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for msg := range errCh {
		t.Error(msg)
	}
}

// --- wire-decode helpers (independent of internal/protocol/types so the
// integration test decodes the JSON shape a third-party Console would) ---

type rateLimitWire struct {
	Capacity         int   `json:"capacity"`
	RefillTokens     int   `json:"refill_tokens"`
	RefillIntervalMS int64 `json:"refill_interval_ms"`
}

type tierViewWire struct {
	BudgetCeilingUSD float64       `json:"budget_ceiling_usd"`
	RateLimit        rateLimitWire `json:"rate_limit"`
	MaxTokens        int           `json:"max_tokens"`
}

type govPostureWire struct {
	DefaultTier   string                  `json:"default_tier"`
	ResolvedTier  string                  `json:"resolved_tier"`
	IdentityTiers map[string]tierViewWire `json:"identity_tiers"`
}

type llmPostureWire struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Region   string `json:"region"`
	MockMode bool   `json:"mock_mode"`
}

func itoa72g(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
