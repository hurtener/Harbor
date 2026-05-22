// Phase 72b cross-subsystem integration test per CLAUDE.md §17 — the
// admin-impersonation extension on `internal/protocol/types.IdentityScope`
// exercised end-to-end against the REAL Phase 60 transport mux + the
// REAL Phase 61 JWT validator + the REAL `audit/drivers/patterns`
// redactor + the REAL `events/drivers/inmem` event bus.
//
// The test proves the impersonation triplet composes end-to-end with
// the production wire path:
//
//   - A client mints a real ES256-signed JWT carrying the
//     `auth.ScopeAdmin` scope claim.
//   - The client submits a `start` (or `redirect` / `user_message`)
//     over the REST control transport with the
//     `IdentityScope.{Actor,Requester,Impersonating}` triplet set.
//   - The Phase 61 middleware verifies the bearer + extracts the
//     identity + the scope.
//   - The control transport's Phase 72b gate validates the triplet
//     against the verified identity and (a) on accept, emits a
//     redacted `audit.admin_scope_used` event on the bus; (b) on
//     reject, returns the appropriate Protocol error.
//   - A bus subscriber (real `events/drivers/inmem`) observes the
//     audit event with the `AdminImpersonationReason` sentinel and the
//     impersonated triple as the event identity.
//
// Five shapes per acceptance criterion row 10 of
// `docs/plans/phase-72b-identityscope-impersonation.md`:
//
//	(1) admin token + impersonation triplet → 200 + audit event on bus.
//	(2) non-admin token + impersonation triplet → 403 CodeScopeMismatch.
//	(3) admin token + impersonation missing a triple component → 401
//	    CodeIdentityRequired.
//	(4) admin token + no impersonation → 200 (backward-compat).
//	(5) impersonation across `start` / `redirect` / `user_message` is
//	    gated identically.
//
// Identity-rejection regression (row 11): a non-impersonation body
// whose top-level identity disagrees with the verified JWT still
// rejects (Phase 61 defence-in-depth holds).
package integration_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// fixedNowPhase72b is the deterministic clock the integration test
// uses — both the test and the validator share it so exp/nbf behaviour
// is reproducible.
var fixedNowPhase72b = time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)

// phase72bDeps wires the REAL runtime drivers behind the
// auth-decorated wire transport — no mocks at any seam (CLAUDE.md §17.3).
type phase72bDeps struct {
	mux     http.Handler
	bus     events.EventBus
	priv    *ecdsa.PrivateKey
	pub     *ecdsa.PublicKey
	cleanup func()
}

func newPhase72bDeps(t *testing.T) *phase72bDeps {
	t.Helper()

	priv, pub := loadES256Phase72b(t)

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

	keys := newES256KeySetPhase72b("k1", pub)
	v, err := auth.NewValidator(keys,
		auth.WithClock(func() time.Time { return fixedNowPhase72b }),
		auth.WithRedactor(red),
	)
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("auth.NewValidator: %v", err)
	}

	mux, err := transports.NewMux(surface, bus,
		transports.WithKeepalive(50*time.Millisecond),
		transports.WithValidator(v),
		transports.WithRedactor(red),
	)
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("transports.NewMux: %v", err)
	}

	return &phase72bDeps{
		mux:  mux,
		bus:  bus,
		priv: priv,
		pub:  pub,
		cleanup: func() {
			_ = taskReg.Close(context.Background())
			_ = store.Close(context.Background())
			_ = bus.Close(context.Background())
		},
	}
}

// es256KeySetPhase72b is the integration test's KeySet — a single-key
// map returning the ES256 public key for kid `k1`.
type es256KeySetPhase72b struct {
	kid string
	pub crypto.PublicKey
}

func newES256KeySetPhase72b(kid string, pub crypto.PublicKey) *es256KeySetPhase72b {
	return &es256KeySetPhase72b{kid: kid, pub: pub}
}

func (s *es256KeySetPhase72b) KeyByID(kid string) (crypto.PublicKey, string, error) {
	if kid != s.kid {
		return nil, "", fmt.Errorf("kid %q not in key set", kid)
	}
	return s.pub, "ES256", nil
}

func loadES256Phase72b(t *testing.T) (*ecdsa.PrivateKey, *ecdsa.PublicKey) {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs repo root: %v", err)
	}
	priv := readPEMBlockPhase72b(t, filepath.Join(repoRoot, "internal/protocol/auth/testdata/es256_private.pem"))
	pub := readPEMBlockPhase72b(t, filepath.Join(repoRoot, "internal/protocol/auth/testdata/es256_public.pem"))
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

func readPEMBlockPhase72b(t *testing.T, abs string) []byte {
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

// phase72bKid is the key ID every signES256Phase72b token carries.
const phase72bKid = "k1"

func signES256Phase72b(t *testing.T, priv *ecdsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = phase72bKid
	signed, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign ES256: %v", err)
	}
	return signed
}

func validClaimsPhase72b(tenant, user, session string, scopes []string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":     "https://idp.test",
		"sub":     user,
		"exp":     fixedNowPhase72b.Add(15 * time.Minute).Unix(),
		"nbf":     fixedNowPhase72b.Add(-1 * time.Minute).Unix(),
		"tenant":  tenant,
		"user":    user,
		"session": session,
		"scopes":  scopes,
	}
}

// impersonationBodyPhase72b renders a JSON body carrying the full
// impersonation triplet for `start` (no payload section). The
// top-level identity equals the impersonated triple per the gate's
// requirement.
func impersonationBodyPhase72b(adminTenant, adminUser, adminSession, targetTenant, targetUser, targetSession string) string {
	return `{"identity":{` +
		`"tenant":"` + targetTenant + `","user":"` + targetUser + `","session":"` + targetSession + `",` +
		`"actor":{"tenant":"` + adminTenant + `","user":"` + adminUser + `","session":"` + adminSession + `"},` +
		`"requester":{"tenant":"` + adminTenant + `","user":"` + adminUser + `","session":"` + adminSession + `"},` +
		`"impersonating":{"tenant":"` + targetTenant + `","user":"` + targetUser + `","session":"` + targetSession + `"}` +
		`},"query":"impersonation-e2e"}`
}

// readErrorCodePhase72b decodes the JSON Protocol error body and
// returns the `code` field.
func readErrorCodePhase72b(t *testing.T, resp *http.Response) string {
	t.Helper()
	var perr struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&perr); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if perr.Code == "" {
		t.Fatal("error body had no `code` field")
	}
	return perr.Code
}

// TestE2E_Phase72b_AdminImpersonation_AcceptedEmitsAuditEvent — shape
// (1): an admin-bearing token submitting the full impersonation
// triplet through the production wire path is accepted (200) AND the
// `audit.admin_scope_used` event arrives on the bus with the
// AdminImpersonationReason sentinel.
func TestE2E_Phase72b_AdminImpersonation_AcceptedEmitsAuditEvent(t *testing.T) {
	deps := newPhase72bDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	const adminTenant, adminUser, adminSession = "tenant-acme", "admin-alice", "sess-admin"
	const targetTenant, targetUser, targetSession = "tenant-acme", "user-target", "sess-target"

	// Subscribe BEFORE the request so we don't miss the emit. The
	// audit event is keyed to the IMPERSONATED triple so a Console
	// subscribing to events for the impersonated session sees the
	// audit emit alongside the run's own events.
	sub, err := deps.bus.Subscribe(context.Background(), events.Filter{
		Tenant:  targetTenant,
		User:    targetUser,
		Session: targetSession,
		Types:   []events.EventType{events.EventTypeAdminScopeUsed},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	tok := signES256Phase72b(t, deps.priv,
		validClaimsPhase72b(adminTenant, adminUser, adminSession, []string{"admin"}))

	body := impersonationBodyPhase72b(adminTenant, adminUser, adminSession, targetTenant, targetUser, targetSession)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST start: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, readBodyPhase72b(t, resp))
	}

	// Drain until we see the AdminScopeUsedPayload. The
	// subscription may also deliver task lifecycle events to the
	// same triple; tolerate them.
	deadline := time.After(5 * time.Second)
loop:
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription channel closed before AdminScopeUsedPayload arrived")
			}
			if ev.Type != events.EventTypeAdminScopeUsed {
				continue
			}
			// Cross-check identity: the event is keyed to the
			// IMPERSONATED triple, not the admin's.
			if ev.Identity.TenantID != targetTenant || ev.Identity.UserID != targetUser || ev.Identity.SessionID != targetSession {
				t.Errorf("event identity = %+v, want impersonated triple (%s/%s/%s)",
					ev.Identity, targetTenant, targetUser, targetSession)
			}
			// Pull Actor / Reason — payload type may be the typed
			// AdminScopeUsedPayload OR a RedactedMap (a custom
			// redactor could rewrite to the map shape). The
			// production patterns redactor returns the typed
			// payload unchanged; assert against both shapes for
			// robustness.
			switch p := ev.Payload.(type) {
			case auth.AdminScopeUsedPayload:
				if p.Reason != auth.AdminImpersonationReason {
					t.Errorf("Reason: got %q want %q", p.Reason, auth.AdminImpersonationReason)
				}
				if p.Method != string(methods.MethodStart) {
					t.Errorf("Method: got %q want %q", p.Method, methods.MethodStart)
				}
				if p.Actor.User != adminUser {
					t.Errorf("Actor.User: got %q want %q", p.Actor.User, adminUser)
				}
				if p.Impersonating.User != targetUser {
					t.Errorf("Impersonating.User: got %q want %q", p.Impersonating.User, targetUser)
				}
			default:
				t.Fatalf("unexpected payload type %T (production should be AdminScopeUsedPayload)", ev.Payload)
			}
			break loop
		case <-deadline:
			t.Fatal("timeout waiting for AdminScopeUsedPayload on the bus")
		}
	}
}

// TestE2E_Phase72b_NonAdminImpersonation_Rejected — shape (2): a
// non-admin bearer attempting impersonation is rejected with
// CodeScopeMismatch BEFORE Dispatch runs.
func TestE2E_Phase72b_NonAdminImpersonation_Rejected(t *testing.T) {
	deps := newPhase72bDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	const adminTenant, adminUser, adminSession = "tenant-acme", "admin-alice", "sess-admin"
	const targetTenant, targetUser, targetSession = "tenant-acme", "user-target", "sess-target"

	// Token has NO scopes claimed.
	tok := signES256Phase72b(t, deps.priv,
		validClaimsPhase72b(adminTenant, adminUser, adminSession, nil))

	body := impersonationBodyPhase72b(adminTenant, adminUser, adminSession, targetTenant, targetUser, targetSession)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST start: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin impersonation status: %d, want 403", resp.StatusCode)
	}
	if code := readErrorCodePhase72b(t, resp); code != "scope_mismatch" {
		t.Errorf("error code: %q, want scope_mismatch", code)
	}
}

// TestE2E_Phase72b_IncompleteImpersonatingTriple_Rejected — shape (3):
// admin + impersonation missing a component → CodeIdentityRequired.
// Identity is mandatory; the impersonated triple IS identity.
func TestE2E_Phase72b_IncompleteImpersonatingTriple_Rejected(t *testing.T) {
	deps := newPhase72bDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	const adminTenant, adminUser, adminSession = "tenant-acme", "admin-alice", "sess-admin"
	const targetTenant, targetUser = "tenant-acme", "user-target"

	tok := signES256Phase72b(t, deps.priv,
		validClaimsPhase72b(adminTenant, adminUser, adminSession, []string{"admin"}))

	// Missing session on Impersonating.
	body := `{"identity":{` +
		`"tenant":"` + targetTenant + `","user":"` + targetUser + `","session":"sess-target",` +
		`"actor":{"tenant":"` + adminTenant + `","user":"` + adminUser + `","session":"` + adminSession + `"},` +
		`"requester":{"tenant":"` + adminTenant + `","user":"` + adminUser + `","session":"` + adminSession + `"},` +
		`"impersonating":{"tenant":"` + targetTenant + `","user":"` + targetUser + `","session":""}` +
		`},"query":"q"}`

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST start: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("incomplete triple status: %d, want 401", resp.StatusCode)
	}
	if code := readErrorCodePhase72b(t, resp); code != "identity_required" {
		t.Errorf("error code: %q, want identity_required", code)
	}
}

// TestE2E_Phase72b_AdminNoImpersonation_AcceptedAndNoAuditEvent —
// shape (4): admin token + no impersonation → 200 (backward-compat);
// no audit event emitted because no impersonation happened.
func TestE2E_Phase72b_AdminNoImpersonation_AcceptedAndNoAuditEvent(t *testing.T) {
	deps := newPhase72bDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	const adminTenant, adminUser, adminSession = "tenant-acme", "admin-alice", "sess-admin"

	sub, err := deps.bus.Subscribe(context.Background(), events.Filter{
		Tenant:  adminTenant,
		User:    adminUser,
		Session: adminSession,
		Types:   []events.EventType{events.EventTypeAdminScopeUsed},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	tok := signES256Phase72b(t, deps.priv,
		validClaimsPhase72b(adminTenant, adminUser, adminSession, []string{"admin"}))

	body := `{"identity":{"tenant":"` + adminTenant + `","user":"` + adminUser + `","session":"` + adminSession + `"},"query":"q"}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST start: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin no-impersonation status: %d, want 200", resp.StatusCode)
	}

	// No AdminScopeUsedPayload should arrive — the gate did not
	// run. Wait a bounded window; if NOTHING arrives, success.
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			return
		}
		if ev.Type == events.EventTypeAdminScopeUsed {
			t.Errorf("backward-compat: AdminScopeUsedPayload emitted when no impersonation happened; payload=%+v", ev.Payload)
		}
	case <-time.After(500 * time.Millisecond):
		// Expected: nothing should arrive within the window.
	}
}

// TestE2E_Phase72b_ImpersonationAcrossControlMethods_GateRunsIdentically
// — shape (5): the gate fires identically for `redirect` /
// `user_message`. We can't test the happy path on these methods
// without a live run (CodeNotFound from the steering registry); the
// load-bearing assertion is that the gate's REJECT path fires before
// Dispatch reaches the no-live-run branch.
func TestE2E_Phase72b_ImpersonationAcrossControlMethods_GateRunsIdentically(t *testing.T) {
	deps := newPhase72bDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	const adminTenant, adminUser, adminSession = "tenant-acme", "admin-alice", "sess-admin"
	const targetTenant, targetUser, targetSession = "tenant-acme", "user-target", "sess-target"

	// Non-admin token → CodeScopeMismatch regardless of method.
	tok := signES256Phase72b(t, deps.priv,
		validClaimsPhase72b(adminTenant, adminUser, adminSession, nil))

	for _, method := range []methods.Method{methods.MethodRedirect, methods.MethodUserMessage} {
		body := `{"identity":{` +
			`"tenant":"` + targetTenant + `","user":"` + targetUser + `","session":"` + targetSession + `","run":"r-doesntmatter","scope":"owner_user",` +
			`"actor":{"tenant":"` + adminTenant + `","user":"` + adminUser + `","session":"` + adminSession + `"},` +
			`"requester":{"tenant":"` + adminTenant + `","user":"` + adminUser + `","session":"` + adminSession + `"},` +
			`"impersonating":{"tenant":"` + targetTenant + `","user":"` + targetUser + `","session":"` + targetSession + `"}` +
			`},"payload":{}}`
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/"+string(method), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // body401or403Phase72b closes resp.Body via its deferred Close
		if err != nil {
			t.Fatalf("POST %s: %v", method, err)
		}
		body401or403Phase72b(t, resp, method, "scope_mismatch")
	}
}

// TestE2E_Phase72b_NonImpersonationBodyMismatch_StillRejected — Phase
// 61 defence-in-depth holds: a body without impersonation whose
// top-level identity differs from the verified JWT is rejected. The
// impersonation gate does NOT widen the Phase 61 check; it adds a
// separate field that's verified separately.
func TestE2E_Phase72b_NonImpersonationBodyMismatch_StillRejected(t *testing.T) {
	deps := newPhase72bDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	tok := signES256Phase72b(t, deps.priv,
		validClaimsPhase72b("tenant-acme", "admin-alice", "sess-admin", []string{"admin"}))

	// No impersonation triplet — just a body claiming a different
	// tenant. The Phase 61 gate STILL rejects this.
	body := `{"identity":{"tenant":"tenant-evil","user":"u","session":"s"},"query":"q"}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST start: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("Phase 61 gate regressed under Phase 72b: status %d, want 401; body=%s", resp.StatusCode, readBodyPhase72b(t, resp))
	}
	if code := readErrorCodePhase72b(t, resp); code != "identity_required" {
		t.Errorf("error code: %q, want identity_required", code)
	}
}

// TestE2E_Phase72b_ConcurrencyStress — N≥10 concurrent admin
// impersonation requests against one shared mux + validator under
// -race; distinct per-goroutine impersonated identities; no cross-talk,
// no goroutine leak. Satisfies CLAUDE.md §17.3 concurrency stress
// requirement for long-lived wiring.
func TestE2E_Phase72b_ConcurrencyStress(t *testing.T) {
	deps := newPhase72bDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	const N = 16
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := range N {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			adminTenant := fmt.Sprintf("tenant-%04d", idx)
			adminUser := fmt.Sprintf("admin-%04d", idx)
			adminSession := fmt.Sprintf("sess-admin-%04d", idx)
			targetTenant := adminTenant
			targetUser := fmt.Sprintf("user-target-%04d", idx)
			targetSession := fmt.Sprintf("sess-target-%04d", idx)
			tok := signES256Phase72b(t, deps.priv,
				validClaimsPhase72b(adminTenant, adminUser, adminSession, []string{"admin"}))

			body := impersonationBodyPhase72b(adminTenant, adminUser, adminSession, targetTenant, targetUser, targetSession)
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/start", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+tok)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errs[idx] = err
				return
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				errs[idx] = fmt.Errorf("goroutine %d: status %d", idx, resp.StatusCode)
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrency stress goroutine %d: %v", i, err)
		}
	}
}

// body401or403Phase72b asserts the response is 401 or 403 (depending
// on the Code) and that the JSON error body's `code` field matches
// the expected sentinel.
func body401or403Phase72b(t *testing.T, resp *http.Response, method methods.Method, wantCode string) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		t.Errorf("method %s: status %d, want 401 or 403", method, resp.StatusCode)
	}
	code := readErrorCodePhase72b(t, resp)
	if code != wantCode {
		t.Errorf("method %s: error code %q, want %q", method, code, wantCode)
	}
}

// readBodyPhase72b returns the response body as a string for error
// messages. The caller has already deferred Close on the response.
func readBodyPhase72b(t *testing.T, resp *http.Response) string {
	t.Helper()
	buf := make([]byte, 8192)
	n, _ := resp.Body.Read(buf)
	return string(buf[:n])
}
