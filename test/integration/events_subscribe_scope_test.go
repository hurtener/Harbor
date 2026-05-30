// Phase 72 / D-105 cross-subsystem integration test per CLAUDE.md §17
// — the §13 primitive-with-consumer discharge for the canonical
// `events.subscribe` method-name + the `CodeIdentityScopeRequired`
// wire code.
//
// The Phase 72 surface is:
//
//   - methods.MethodEventsSubscribe = "events.subscribe" — the
//     canonical Protocol method-name constant a third-party Console
//     branches on.
//   - errors.CodeIdentityScopeRequired = "identity_scope_required" —
//     the canonical wire-rejection code returned at the SSE edge when
//     a Subscribe request's scope set is insufficient for the
//     requested fan-in. HTTP 403.
//   - The Phase 60 `GET /v1/events` SSE route + the Phase 61
//     auth.Middleware are the wire-transport binding; this test
//     exercises both end-to-end.
//
// Scope-degradation regression suite — six scenarios:
//
//  1. Triple-scoped happy-path subscribe (tenant A only sees tenant
//     A events; tenant B's publish never reaches the stream).
//  2. Cross-tenant subscribe `?admin=1` without the scope claim →
//     403 + identity_scope_required.
//  3. Cross-tenant subscribe `?admin=1` with `auth.ScopeAdmin` → 200,
//     the admin subscriber sees BOTH tenants' events, and the bus
//     emits `audit.admin_scope_used`.
//  4. Cross-tenant subscribe `?admin=1` with `auth.ScopeConsoleFleet`
//     → same as 3 (both scopes satisfy the gate).
//  5. Expired token surfaces as `auth_rejected` (401) at the Phase 61
//     middleware — Phase 72's scope code is NOT reached.
//  6. Dropped-middleware shape: a request reaching the Handler
//     without going through auth (no scope set on ctx) AND asking for
//     `?admin=1` is rejected from cross-tenant fan-in by default —
//     the Phase 60 trust-based posture has never honoured `?admin=1`
//     since Phase 61.
//
// Real drivers everywhere on the seam — no mocks (CLAUDE.md §17.3):
//
//   - real events/drivers/inmem bus,
//   - real protocol/auth.Middleware over the real ES256 testdata
//     keypair,
//   - real internal/protocol/transports.NewMux assembling the wire
//     surface with auth.
//
// Multi-isolation surface: every scenario asserts the
// (tenant, user, session) triple flows through the wire layer to the
// bus filter; cross-tenant subscribers without the scope claim are
// REJECTED, not silently degraded to a triple-scoped view (the
// failure mode the §17.6 "fix what the integration test finds" rule
// keeps closing).
package integration_test

import (
	"bufio"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// fixedNowPhase72 — deterministic clock the integration test shares
// with the validator so exp/nbf claims are reproducible.
var fixedNowPhase72 = time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)

// phase72Deps holds the wired wire surface — REAL drivers across the
// seam, no mocks (CLAUDE.md §17.3).
type phase72Deps struct {
	mux     http.Handler
	bus     events.EventBus
	priv    *ecdsa.PrivateKey
	cleanup func()
}

func newPhase72Deps(t *testing.T) *phase72Deps {
	t.Helper()

	priv, pub := loadES256Phase72(t)

	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         128,
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

	keys := newES256KeySetPhase72("k1", pub)
	now := func() time.Time { return fixedNowPhase72 }
	v, err := auth.NewValidator(keys, auth.WithClock(now), auth.WithRedactor(red))
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("auth.NewValidator: %v", err)
	}
	mux, err := transports.NewMux(surface, bus,
		transports.WithKeepalive(50*time.Millisecond),
		transports.WithValidator(v),
	)
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("transports.NewMux: %v", err)
	}

	return &phase72Deps{
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

// es256KeySetPhase72 is the integration test's KeySet — single-key
// map.
type es256KeySetPhase72 struct {
	kid string
	pub crypto.PublicKey
}

func newES256KeySetPhase72(kid string, pub crypto.PublicKey) *es256KeySetPhase72 {
	return &es256KeySetPhase72{kid: kid, pub: pub}
}

func (s *es256KeySetPhase72) KeyByID(kid string) (crypto.PublicKey, string, error) {
	if kid != s.kid {
		return nil, "", fmt.Errorf("kid %q not in key set", kid)
	}
	return s.pub, "ES256", nil
}

func loadES256Phase72(t *testing.T) (*ecdsa.PrivateKey, *ecdsa.PublicKey) {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs repo root: %v", err)
	}
	priv := readPEMBlockPhase72(t, filepath.Join(repoRoot, "internal/protocol/auth/testdata/es256_private.pem"))
	pub := readPEMBlockPhase72(t, filepath.Join(repoRoot, "internal/protocol/auth/testdata/es256_public.pem"))
	ecPriv, err := x509.ParseECPrivateKey(priv)
	if err != nil {
		k, perr := x509.ParsePKCS8PrivateKey(priv)
		if perr != nil {
			t.Fatalf("parse ES256 private: EC=%v PKCS8=%v", err, perr)
		}
		var ok bool
		ecPriv, ok = k.(*ecdsa.PrivateKey)
		if !ok {
			t.Fatalf("PKCS8 key is not *ecdsa.PrivateKey")
		}
	}
	pubAny, err := x509.ParsePKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("parse ES256 public: %v", err)
	}
	ecPub, ok := pubAny.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("public key is not *ecdsa.PublicKey")
	}
	return ecPriv, ecPub
}

func readPEMBlockPhase72(t *testing.T, abs string) []byte {
	t.Helper()
	raw, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read %q: %v", abs, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatalf("no PEM block in %q", abs)
	}
	return block.Bytes
}

// phase72Kid is the key ID every signES256Phase72 token carries.
const phase72Kid = "k1"

func signES256Phase72(t *testing.T, priv *ecdsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = phase72Kid
	signed, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign ES256: %v", err)
	}
	return signed
}

func validClaimsPhase72(tenant, user, session string, scopes []string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":     "https://idp.test",
		"sub":     user,
		"exp":     fixedNowPhase72.Add(15 * time.Minute).Unix(),
		"nbf":     fixedNowPhase72.Add(-1 * time.Minute).Unix(),
		"tenant":  tenant,
		"user":    user,
		"session": session,
		"scopes":  scopes,
	}
}

// expiredClaimsPhase72 mints claims whose `exp` is in the past
// relative to the fixed clock — the Phase 61 validator rejects them
// as `token_expired` → `auth_rejected` (401).
func expiredClaimsPhase72(tenant, user, session string) jwt.MapClaims {
	c := validClaimsPhase72(tenant, user, session, nil)
	c["exp"] = fixedNowPhase72.Add(-1 * time.Hour).Unix()
	return c
}

// publishCancelledPhase72 publishes a `runtime.run_cancelled` event
// for the supplied identity. Used to drive cross-tenant assertions.
func publishCancelledPhase72(t *testing.T, bus events.EventBus, id identity.Identity, runID string) {
	t.Helper()
	err := bus.Publish(context.Background(), events.Event{
		Type:     events.EventTypeRuntimeRunCancelled,
		Identity: identity.Quadruple{Identity: id, RunID: runID},
		Payload:  events.RunCancelledPayload{RunID: runID, CancelledAt: time.Now().UnixNano()},
	})
	if err != nil {
		t.Fatalf("bus.Publish: %v", err)
	}
}

// TestE2E_Phase72_TripleScoped_NoCrossTenantLeak — scenario 1:
// triple-scoped subscribe sees its own tenant's events and NONE from
// another tenant on the same bus.
func TestE2E_Phase72_TripleScoped_NoCrossTenantLeak(t *testing.T) {
	deps := newPhase72Deps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	tenantA := identity.Identity{TenantID: "t-A", UserID: "u-A", SessionID: "s-A"}
	tenantB := identity.Identity{TenantID: "t-B", UserID: "u-B", SessionID: "s-B"}
	tokA := signES256Phase72(t, deps.priv, validClaimsPhase72(tenantA.TenantID, tenantA.UserID, tenantA.SessionID, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+tokA)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open SSE: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Publish one for OUR tenant + one for the FOREIGN tenant.
	publishCancelledPhase72(t, deps.bus, tenantA, "r-A")
	publishCancelledPhase72(t, deps.bus, tenantB, "r-B-leak-probe")

	sc := bufio.NewScanner(resp.Body)
	deadline := time.Now().Add(1500 * time.Millisecond)
	sawMine := false
	for sc.Scan() && time.Now().Before(deadline) {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if strings.Contains(data, "r-B-leak-probe") {
			t.Fatal("cross-tenant event leaked onto a triple-scoped stream — multi-isolation broken")
		}
		if strings.Contains(data, "r-A") {
			sawMine = true
		}
	}
	if !sawMine {
		t.Error("triple-scoped subscriber did not receive its own tenant's event")
	}
}

// TestE2E_Phase72_CrossTenant_WithoutScope_Rejected403 — scenario 2:
// `?admin=1` from a JWT lacking the scope claim → 403 +
// identity_scope_required Code in the body.
func TestE2E_Phase72_CrossTenant_WithoutScope_Rejected403(t *testing.T) {
	deps := newPhase72Deps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	tenantA := identity.Identity{TenantID: "t-A", UserID: "u-A", SessionID: "s-A"}
	tok := signES256Phase72(t, deps.priv, validClaimsPhase72(tenantA.TenantID, tenantA.UserID, tenantA.SessionID, nil))

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/events?admin=1", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open admin SSE: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json (typed Protocol error envelope)", ct)
	}
	body := readBodyPhase72(t, resp)
	if !strings.Contains(body, `"code":"identity_scope_required"`) {
		t.Errorf("body missing identity_scope_required code: %q", body)
	}
}

// TestE2E_Phase72_CrossTenant_WithScopeAdmin_Accepted — scenario 3:
// `?admin=1` with the `admin` scope opens the cross-tenant fan-in.
// The subscriber sees events from BOTH tenants, and the audit emit
// (`audit.admin_scope_used`) lands on the stream.
func TestE2E_Phase72_CrossTenant_WithScopeAdmin_Accepted(t *testing.T) {
	runCrossTenantAcceptCase(t, "admin")
}

// TestE2E_Phase72_CrossTenant_WithScopeConsoleFleet_Accepted —
// scenario 4: `?admin=1` with the `console:fleet` scope works the
// same way as `admin` (D-079: both scopes satisfy the gate).
func TestE2E_Phase72_CrossTenant_WithScopeConsoleFleet_Accepted(t *testing.T) {
	runCrossTenantAcceptCase(t, "console:fleet")
}

func runCrossTenantAcceptCase(t *testing.T, scope string) {
	t.Helper()
	deps := newPhase72Deps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	adminID := identity.Identity{TenantID: "t-admin", UserID: "u-admin", SessionID: "s-admin"}
	tenantA := identity.Identity{TenantID: "t-A", UserID: "u-A", SessionID: "s-A"}
	tenantB := identity.Identity{TenantID: "t-B", UserID: "u-B", SessionID: "s-B"}
	tok := signES256Phase72(t, deps.priv,
		validClaimsPhase72(adminID.TenantID, adminID.UserID, adminID.SessionID, []string{scope}))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events?admin=1", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open admin SSE with scope %q: %v", scope, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scope %q: status = %d, want 200", scope, resp.StatusCode)
	}

	// Publish events for BOTH tenants — the admin stream should see
	// both (cross-tenant fan-in is what the scope gates).
	publishCancelledPhase72(t, deps.bus, tenantA, "r-A-cross")
	publishCancelledPhase72(t, deps.bus, tenantB, "r-B-cross")

	sc := bufio.NewScanner(resp.Body)
	deadline := time.Now().Add(1500 * time.Millisecond)
	sawTenantA := false
	sawTenantB := false
	sawAdminAudit := false
	for sc.Scan() && time.Now().Before(deadline) {
		line := sc.Text()
		if strings.HasPrefix(line, "event: audit.admin_scope_used") {
			sawAdminAudit = true
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if strings.Contains(data, "r-A-cross") {
			sawTenantA = true
		}
		if strings.Contains(data, "r-B-cross") {
			sawTenantB = true
		}
		if sawTenantA && sawTenantB && sawAdminAudit {
			break
		}
	}
	if !sawTenantA {
		t.Errorf("scope %q: admin subscriber missed tenant A's event", scope)
	}
	if !sawTenantB {
		t.Errorf("scope %q: admin subscriber missed tenant B's event", scope)
	}
	if !sawAdminAudit {
		t.Errorf("scope %q: admin subscriber did not observe its own audit.admin_scope_used emit", scope)
	}
}

// TestE2E_Phase72_ExpiredToken_Returns401AuthRejected — scenario 5:
// an expired JWT surfaces at the Phase 61 middleware as
// `auth_rejected` (401). Phase 72's `identity_scope_required` is NOT
// reached — the auth layer fail-closes first.
func TestE2E_Phase72_ExpiredToken_Returns401AuthRejected(t *testing.T) {
	deps := newPhase72Deps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	tenantA := identity.Identity{TenantID: "t-A", UserID: "u-A", SessionID: "s-A"}
	tok := signES256Phase72(t, deps.priv, expiredClaimsPhase72(tenantA.TenantID, tenantA.UserID, tenantA.SessionID))

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/events?admin=1", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open expired-token SSE: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired token: status = %d, want 401 (Phase 61 short-circuit)", resp.StatusCode)
	}
	body := readBodyPhase72(t, resp)
	if !strings.Contains(body, `"code":"auth_rejected"`) {
		t.Errorf("expired-token body missing auth_rejected code: %q", body)
	}
	if strings.Contains(body, `"code":"identity_scope_required"`) {
		t.Error("expired token wrongly reached the Phase 72 scope gate — auth layer should have rejected first")
	}
}

// TestE2E_Phase72_DroppedMiddleware_NoScopeOnCtx_Rejected — scenario
// 6: a request reaching the stream Handler WITHOUT going through the
// auth middleware (so no scope set on ctx) AND asking for `?admin=1`
// is rejected from cross-tenant fan-in. Constructs a degenerate mux
// without WithValidator to exercise the no-middleware path.
//
// This is the Phase 60 trust-based posture — the SSE handler resolves
// identity from the X-Harbor-* carrier headers when no middleware
// ran, but `?admin=1` ALWAYS requires a verified scope set (Phase 61
// onward).
func TestE2E_Phase72_DroppedMiddleware_NoScopeOnCtx_Rejected(t *testing.T) {
	// Build a mux WITHOUT WithValidator — the explicit Phase 60
	// trust-based posture (auth middleware not wired). The
	// WithoutValidator opt-in is the test-only escape hatch (D-082).
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     128,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         64,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	defer func() { _ = bus.Close(context.Background()) }()
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: audit.Redactor(red),
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	defer func() { _ = taskReg.Close(context.Background()) }()
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}
	mux, err := transports.NewMux(surface, bus,
		transports.WithKeepalive(50*time.Millisecond),
		transports.WithoutValidator(), // no auth middleware
	)
	if err != nil {
		t.Fatalf("transports.NewMux: %v", err)
	}

	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/events?admin=1", nil)
	// Triple-via-header carrier (Phase 60 trust-based posture).
	req.Header.Set("X-Harbor-Tenant", "t-A")
	req.Header.Set("X-Harbor-User", "u-A")
	req.Header.Set("X-Harbor-Session", "s-A")
	// Deliberately NO Authorization header → ctx carries no scope
	// set → HasScope(admin / console:fleet) returns false.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open dropped-mw SSE: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("dropped-middleware ?admin=1: status = %d, want 403", resp.StatusCode)
	}
	body := readBodyPhase72(t, resp)
	if !strings.Contains(body, `"code":"identity_scope_required"`) {
		t.Errorf("dropped-middleware body missing identity_scope_required: %q", body)
	}
}

// TestE2E_Phase72_BodyVsTokenIdentityMismatch — D-171 per-request
// session + tenant/user-spoof defence. The connection token verifies
// (tenant, user); the SESSION is chosen per-request via
// X-Harbor-Session and REPLACES the token's session claim. The
// tenant/user carrier headers, by contrast, can NEVER widen the
// verified principal — the auth middleware honours only X-Harbor-Session.
//
// Concretely, with token (t-token, u-token, s-token) and headers
// X-Harbor-{Tenant,User}=foreign + X-Harbor-Session=s-foreign, the
// stream must scope to (t-token, u-token, s-foreign):
//   - the header-chosen session wins (D-171),
//   - the token tenant/user win (no spoof),
//   - the token's OWN session (s-token) is NOT received (the request
//     chose a different session),
//   - the fully-foreign tenant is NOT received (headers can't widen it).
func TestE2E_Phase72_BodyVsTokenIdentityMismatch(t *testing.T) {
	deps := newPhase72Deps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	tokenID := identity.Identity{TenantID: "t-token", UserID: "u-token", SessionID: "s-token"}
	tok := signES256Phase72(t, deps.priv, validClaimsPhase72(tokenID.TenantID, tokenID.UserID, tokenID.SessionID, nil))

	// The identity the request resolves to: token tenant/user + the
	// header-chosen session (D-171).
	resolvedID := identity.Identity{TenantID: "t-token", UserID: "u-token", SessionID: "s-foreign"}

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	// The session header is a legitimate per-request override (D-171);
	// the tenant/user headers are a SPOOF attempt the middleware ignores.
	req.Header.Set("X-Harbor-Tenant", "t-foreign")
	req.Header.Set("X-Harbor-User", "u-foreign")
	req.Header.Set("X-Harbor-Session", "s-foreign")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open per-request-session SSE: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("per-request session stream: status = %d, want 200", resp.StatusCode)
	}

	// Publish for the RESOLVED identity (token tenant/user + header
	// session). The stream SHOULD receive it.
	publishCancelledPhase72(t, deps.bus, resolvedID, "r-resolved-session")
	// Publish for the token's OWN session — the request chose a DIFFERENT
	// session, so the stream must NOT receive it.
	publishCancelledPhase72(t, deps.bus, tokenID, "r-token-session-not-chosen")
	// Publish for the fully-foreign tenant — the header can't widen the
	// tenant, so the stream must NOT receive it.
	publishCancelledPhase72(t, deps.bus, identity.Identity{
		TenantID: "t-foreign", UserID: "u-foreign", SessionID: "s-foreign",
	}, "r-foreign-tenant-leak")

	sc := bufio.NewScanner(resp.Body)
	deadline := time.Now().Add(1200 * time.Millisecond)
	sawResolved := false
	var sawTokenSession atomic.Bool
	var sawForeignTenant atomic.Bool
	for sc.Scan() && time.Now().Before(deadline) {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if strings.Contains(data, "r-resolved-session") {
			sawResolved = true
		}
		if strings.Contains(data, "r-token-session-not-chosen") {
			sawTokenSession.Store(true)
		}
		if strings.Contains(data, "r-foreign-tenant-leak") {
			sawForeignTenant.Store(true)
		}
	}
	if sawForeignTenant.Load() {
		t.Fatal("carrier tenant/user header spoofed the verified principal — D-171 must only honour X-Harbor-Session")
	}
	if sawTokenSession.Load() {
		t.Fatal("stream received the token's own session despite a per-request X-Harbor-Session override — session source is broken")
	}
	if !sawResolved {
		t.Error("did not observe the resolved (token tenant/user + header session) event in the window")
	}
}

// readBodyPhase72 reads the response body as a string. The scope-test
// surface is small, the bodies are short JSON envelopes; a full
// io.ReadAll is fine and keeps the assertion shape readable.
func readBodyPhase72(t *testing.T, resp *http.Response) string {
	t.Helper()
	buf := make([]byte, 0, 512)
	tmp := make([]byte, 256)
	for {
		n, err := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
		if len(buf) > 16*1024 {
			break // safety cap
		}
	}
	return string(buf)
}
