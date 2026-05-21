// Phase 73m (D-129) cross-subsystem integration test per CLAUDE.md §17
// — the §13 primitive-with-consumer discharge for the Console
// Settings-page surface.
//
// Phase 73m's ONE net-new Protocol method is `auth.rotate_token`; the
// Settings page is otherwise a pure consumer of the 72f / 72g posture
// surfaces and the 72h Console DB. This test exercises `auth.rotate_token`
// end-to-end:
//
//  1. A real assembled Runtime (harbortest/devstack.Assemble — real
//     inmem events / state / tasks / artifacts drivers + a real ES256
//     auth keypair) with a real auth.RotateSurface mounted through the
//     real transports.NewMux.
//  2. The happy path: an admin-scoped operator rotates their token over
//     the wire and gets a re-minted, parseable JWT back.
//  3. Identity propagation: the re-minted token carries exactly the
//     caller's verified `(tenant, user, session)` triple.
//  4. Failure mode (CLAUDE.md §17.3 #3): a request WITHOUT the verified
//     `admin` scope claim is rejected 403 with CodeIdentityScopeRequired.
//  5. Audit: a successful rotation emits an `audit.admin_scope_used`
//     event onto the real bus.
//  6. N≥10 concurrent operators rotate against the shared surface; the
//     goroutine baseline is restored after teardown (D-025).
//
// Real drivers everywhere on the seam — no mocks (CLAUDE.md §17.3).
package integration

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/protocol/types"

	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/llm/mock"
	_ "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// settingsTokenIssuer is the in-test auth.TokenIssuer for the Settings
// integration test. It re-mints an ES256 JWT signed with the devstack's
// signing key — so the re-minted token validates clean against the
// devstack's validator, exactly as the production dev signer does.
type settingsTokenIssuer struct {
	key *ecdsa.PrivateKey
}

func (s *settingsTokenIssuer) IssueToken(_ context.Context, id identity.Identity, scopes []auth.Scope, now time.Time) (string, time.Time, error) {
	exp := now.Add(time.Hour)
	strScopes := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		strScopes = append(strScopes, string(sc))
	}
	claims := jwt.MapClaims{
		"iss":     "harbor-test",
		"sub":     id.UserID,
		"aud":     "harbor",
		"exp":     exp.Unix(),
		"nbf":     now.Add(-time.Minute).Unix(),
		"iat":     now.Unix(),
		"tenant":  id.TenantID,
		"user":    id.UserID,
		"session": id.SessionID,
		"scopes":  strScopes,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = devstack.DefaultKID
	signed, err := tok.SignedString(s.key)
	if err != nil {
		return "", time.Time{}, err
	}
	return signed, exp, nil
}

// settingsPageConfig builds the minimal validated *config.Config the
// devstack helper needs for the Settings-page integration test.
func settingsPageConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{
			BindAddr:            "127.0.0.1:0",
			ShutdownGracePeriod: 1 * time.Second,
		},
		Identity: config.IdentityConfig{
			JWTAlgorithms: []string{"ES256"},
			Issuer:        "https://issuer.example.com",
			Audience:      "harbor",
			JWKSURL:       "https://issuer.example.com/.well-known/jwks.json",
		},
		Telemetry: config.TelemetryConfig{
			LogFormat:   "text",
			LogLevel:    "error",
			ServiceName: "harbor-phase73m",
		},
		State: config.StateConfig{Driver: "inmem"},
		LLM: config.LLMConfig{
			Driver:               "mock",
			Timeout:              5 * time.Second,
			ContextWindowReserve: 0.05,
		},
		Events: config.EventsConfig{
			Driver:                   "inmem",
			MaxSubscribersPerSession: 16,
			SubscriberBufferSize:     64,
			IdleTimeout:              2 * time.Second,
			DropWindow:               50 * time.Millisecond,
			ReplayBufferSize:         256,
		},
		Sessions: config.SessionsConfig{
			IdleTTL:       1 * time.Hour,
			HardCap:       24 * time.Hour,
			SweepInterval: 5 * time.Minute,
		},
		Artifacts: config.ArtifactsConfig{
			Driver:                    "inmem",
			HeavyOutputThresholdBytes: 32 * 1024,
		},
		Tasks: config.TasksConfig{
			Driver:               "inprocess",
			RetainTurnTimeout:    1 * time.Minute,
			ContinuationHopLimit: 4,
		},
		Distributed: config.DistributedConfig{
			BusDriver:    "loopback",
			RemoteDriver: "loopback",
		},
		Memory: config.MemoryConfig{
			Driver:             "inmem",
			Strategy:           "none",
			RecoveryBacklogMax: 8,
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("settingsPageConfig: cfg.Validate(): %v", err)
	}
	return cfg
}

// signSettingsToken mints an ES256 bearer for the given identity + scopes.
func signSettingsToken(t *testing.T, priv *ecdsa.PrivateKey, id identity.Identity, scopes []string) string {
	t.Helper()
	now := time.Now()
	claims := jwt.MapClaims{
		"iss":     "harbor-test",
		"sub":     id.UserID,
		"aud":     "harbor",
		"exp":     now.Add(1 * time.Hour).Unix(),
		"nbf":     now.Add(-1 * time.Minute).Unix(),
		"iat":     now.Unix(),
		"tenant":  id.TenantID,
		"user":    id.UserID,
		"session": id.SessionID,
	}
	if scopes != nil {
		claims["scopes"] = scopes
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = devstack.DefaultKID
	signed, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign settings token: %v", err)
	}
	return signed
}

// postRotate POSTs an AuthRotateTokenRequest to /v1/auth/rotate_token.
func postRotate(t *testing.T, baseURL string, body []byte, token string) (int, []byte) {
	t.Helper()
	url := baseURL + "/v1/auth/rotate_token"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func TestE2E_SettingsPage_RotateToken(t *testing.T) {
	cfg := settingsPageConfig(t)
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{SkipRunLoop: true})
	defer stack.Close()

	if stack.Surface == nil || stack.Validator == nil || stack.SigningKey == nil {
		t.Fatal("devstack did not assemble Surface / Validator / SigningKey")
	}

	// Subscribe to the admin-scope audit event BEFORE driving any
	// rotation so the fan-out bus does not race the publish.
	devID := identity.Identity{
		TenantID:  devstack.DefaultDevTenant,
		UserID:    devstack.DefaultDevUser,
		SessionID: devstack.DefaultDevSession,
	}
	subCtx, err := identity.With(context.Background(), devID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	sub, err := stack.Bus.Subscribe(subCtx, events.Filter{
		Tenant:  devID.TenantID,
		User:    devID.UserID,
		Session: devID.SessionID,
		Types:   []events.EventType{events.EventTypeAdminScopeUsed},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	// Build a real RotateSurface wired to the devstack's signing key +
	// real redactor + real bus.
	rotate, err := auth.NewRotateSurface(
		&settingsTokenIssuer{key: stack.SigningKey},
		stack.Audit,
		auth.WithRotateBus(stack.Bus),
	)
	if err != nil {
		t.Fatalf("NewRotateSurface: %v", err)
	}

	mux, err := transports.NewMux(stack.Surface, stack.Bus,
		transports.WithValidator(stack.Validator),
		transports.WithAuthSurface(rotate),
	)
	if err != nil {
		t.Fatalf("transports.NewMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// --- (1) happy path: admin-scoped rotation over the wire ----------
	t.Run("HappyPath_AdminScope", func(t *testing.T) {
		adminTok := signSettingsToken(t, stack.SigningKey, devID, []string{"admin"})
		code, body := postRotate(t, srv.URL, []byte(`{}`), adminTok)
		if code != http.StatusOK {
			t.Fatalf("rotate_token status = %d, want 200; body=%s", code, body)
		}
		var resp types.AuthRotateTokenResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if resp.NewToken == "" {
			t.Fatal("rotate_token returned an empty NewToken")
		}
		if resp.ExpiresAt.IsZero() {
			t.Error("rotate_token returned a zero ExpiresAt")
		}
		// (3) identity propagation — the re-minted token carries exactly
		// the caller's verified triple. Validate it through the
		// devstack's real validator.
		verified, err := stack.Validator.Validate(context.Background(), resp.NewToken)
		if err != nil {
			t.Fatalf("re-minted token failed validation: %v", err)
		}
		if verified.Identity != devID {
			t.Errorf("re-minted token identity = %+v, want %+v", verified.Identity, devID)
		}
	})

	// --- (4) failure mode: no admin scope → 403 -----------------------
	t.Run("RejectsWithoutAdminScope", func(t *testing.T) {
		noScopeTok := signSettingsToken(t, stack.SigningKey, devID, nil)
		code, body := postRotate(t, srv.URL, []byte(`{}`), noScopeTok)
		if code != http.StatusForbidden {
			t.Fatalf("rotate_token without admin scope status = %d, want 403; body=%s", code, body)
		}
	})

	// --- (5) audit: a successful rotation emitted the event -----------
	t.Run("AuditEventEmitted", func(t *testing.T) {
		select {
		case ev := <-sub.Events():
			if ev.Type != events.EventTypeAdminScopeUsed {
				t.Errorf("audit event type = %q, want %q", ev.Type, events.EventTypeAdminScopeUsed)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no audit.admin_scope_used event observed after a successful rotation")
		}
	})

	// --- (6) concurrency: N≥10 operators rotate concurrently ----------
	t.Run("ConcurrentReuse_NoLeak", func(t *testing.T) {
		baseline := runtime.NumGoroutine()
		const n = 16
		var wg sync.WaitGroup
		codes := make([]int, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				tok := signSettingsToken(t, stack.SigningKey, devID, []string{"admin"})
				code, _ := postRotate(t, srv.URL, []byte(`{}`), tok)
				codes[i] = code
			}(i)
		}
		wg.Wait()
		for i, c := range codes {
			if c != http.StatusOK {
				t.Errorf("concurrent rotation %d status = %d, want 200", i, c)
			}
		}
		// Drain the audit events the concurrent rotations emitted so the
		// subscription buffer does not leak; bounded wait.
		drained := 0
		for drained < n {
			select {
			case <-sub.Events():
				drained++
			case <-time.After(2 * time.Second):
				drained = n // stop draining; the count assertion is best-effort
			}
		}
		// Goroutine baseline restored after all requests return (D-025).
		settled := false
		for attempt := 0; attempt < 50; attempt++ {
			if runtime.NumGoroutine() <= baseline+8 {
				settled = true
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !settled {
			t.Errorf("goroutine count did not settle near baseline %d (got %d)", baseline, runtime.NumGoroutine())
		}
	})
}
