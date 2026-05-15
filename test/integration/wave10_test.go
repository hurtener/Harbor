// Wave 10 cross-subsystem integration test per CLAUDE.md §17.5 + §17.7
// step 5 — the wave-end E2E, bundled with the final phase (Phase 62).
//
// Wave 10 closes the telemetry / events / Protocol-versioning /
// wire-transport / auth cluster:
//
//   - Phase 55 OTel traces — Tracer wrapper deriving spans from
//     events.Event + W3C TraceContext propagation.
//   - Phase 56 Metrics — MetricsRegistry + harbor_events_total counter,
//     the OTLP/Prometheus exporter seam, and the cardinality lint.
//   - Phase 57 Durable event log — StateStore-backed event log driver.
//   - Phase 58 Protocol single-source — types/methods/errors AST
//     build-gate.
//   - Phase 59 Protocol versioning — Version + Deprecation +
//     Capability/Handshake + canonical version-negotiation shape.
//   - Phase 60 Protocol wire transport — REST/JSON control surface +
//     SSE event stream over net/http.
//   - Phase 61 Protocol auth — JWT validator + Middleware wrapping the
//     Phase 60 transports.
//   - Phase 62 Protocol conformance suite — the exhaustive
//     primitive-with-consumer closer for the Protocol layer.
//
// The wave-end E2E proves these COMPOSE:
//
//  1. The Phase 62 conformance suite is consumed against the FULL
//     assembled Wave 10 surface — the same suite, the same drivers,
//     from inside test/integration/ (a different consumer profile
//     than the package-local one). Identity propagation, every
//     Protocol method, every error code, every event-filter shape,
//     the version handshake, the auth pipeline — all exercised
//     against the real-driver stack.
//  2. The Phase 59 VersionHandshake is the contract a Console
//     negotiates against: the test pins the current handshake's
//     shape end-to-end.
//  3. A failure mode: an unknown-kid token is rejected at the
//     validator (CodeAuthRejected). The runtime is never reached.
//  4. N≥10 concurrent runs against the assembled surface — distinct
//     per-goroutine identity, no cross-talk, goroutine baseline
//     restored on teardown.
//
// Per CLAUDE.md §17.3:
//
//  1. Real drivers everywhere on the seam — events.EventBus (inmem),
//     state.StateStore (inmem), tasks.TaskRegistry (inprocess),
//     protocol.ControlSurface, protocol/auth.Validator (real ES256
//     keypair), protocol/transports.NewMux.
//  2. Identity propagation through every layer — the JWT-derived
//     triple flows JWT → middleware → ctx → Dispatch → spawned task
//     (asserted via tasks.Get).
//  3. ≥1 failure mode — the unknown-kid rejection + the missing-bearer
//     rejection, both exercised via the conformance auth-pipeline
//     scenarios.
//  4. -race is the CI gate.
//  5. N≥10 concurrency stress — TestE2E_Wave10_Concurrency_NoCrossTalk
//     runs 16 distinct identity stacks against ONE shared mux.
//  6. No time.Sleep-as-synchronisation for load-bearing waits — the
//     short settle sleeps before goroutine-baseline snapshots are
//     scheduler-noise tolerances, not synchronisation.
package integration_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/conformance"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// fixedNowWave10 is the deterministic clock the Wave 10 E2E shares
// across token-minting and the validator's WithClock. Reproducible
// exp/nbf behaviour across runs.
var fixedNowWave10 = time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)

const wave10Kid = "harbor-wave10-k1"

// wave10TestdataRoot resolves the path to the auth package's
// testdata/ from this test's run cwd (test/integration/).
func wave10TestdataRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs repo root: %v", err)
	}
	return filepath.Join(root, "internal", "protocol", "auth", "testdata")
}

// TestE2E_Wave10_Conformance_AgainstAssembledSurface — the load-bearing
// wave-end shape: the Phase 62 conformance suite consumed from
// test/integration/ against the full assembled Wave 10 surface. The
// suite exercises every Protocol method, every error code, every
// event-filter shape, the version handshake, and the auth pipeline
// against real drivers. A pass here means the Wave 10 surface composes
// end-to-end with no hidden seams.
func TestE2E_Wave10_Conformance_AgainstAssembledSurface(t *testing.T) {
	// The conformance suite's NewDefaultFactory wires the entire Wave
	// 10 Protocol layer behind one Factory. We pass the absolute
	// path to internal/protocol/auth/testdata so the suite's keypair
	// loader works from test/integration/'s cwd.
	conformance.RunSuite(t, conformance.NewDefaultFactory(wave10TestdataRoot(t)))
}

// wave10Deps is the assembled real-driver Wave 10 stack — the same
// shape the conformance Factory builds, owned at the integration-test
// level so the failure-mode and concurrency tests can use it
// directly.
type wave10Deps struct {
	bus     events.EventBus
	tasks   tasks.TaskRegistry
	surface *protocol.ControlSurface
	steer   *steering.Registry
	mux     http.Handler
	priv    *ecdsa.PrivateKey
	pub     *ecdsa.PublicKey
	cleanup func()
}

func newWave10Deps(t *testing.T) *wave10Deps {
	t.Helper()

	priv, pub := loadES256Wave10(t)

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
		Redactor: red,
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("tasks.Open: %v", err)
	}
	steerReg := steering.NewRegistry()
	surface, err := protocol.NewControlSurface(taskReg, steerReg)
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}
	keys := &wave10KeySet{kid: wave10Kid, pub: pub}
	now := func() time.Time { return fixedNowWave10 }
	validator, err := auth.NewValidator(keys, auth.WithClock(now))
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("auth.NewValidator: %v", err)
	}
	mux, err := transports.NewMux(surface, bus,
		transports.WithKeepalive(50*time.Millisecond),
		transports.WithValidator(validator),
	)
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("transports.NewMux: %v", err)
	}
	return &wave10Deps{
		bus:     bus,
		tasks:   taskReg,
		surface: surface,
		steer:   steerReg,
		mux:     mux,
		priv:    priv,
		pub:     pub,
		cleanup: func() {
			_ = taskReg.Close(context.Background())
			_ = store.Close(context.Background())
			_ = bus.Close(context.Background())
		},
	}
}

// wave10KeySet — the integration test's KeySet.
type wave10KeySet struct {
	kid string
	pub crypto.PublicKey
}

func (s *wave10KeySet) KeyByID(kid string) (crypto.PublicKey, string, error) {
	if kid != s.kid {
		return nil, "", fmt.Errorf("kid %q not in wave10 key set", kid)
	}
	return s.pub, "ES256", nil
}

func loadES256Wave10(t *testing.T) (*ecdsa.PrivateKey, *ecdsa.PublicKey) {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs repo root: %v", err)
	}
	priv := readPEMWave10(t, filepath.Join(repoRoot, "internal/protocol/auth/testdata/es256_private.pem"))
	pub := readPEMWave10(t, filepath.Join(repoRoot, "internal/protocol/auth/testdata/es256_public.pem"))
	ecPriv, err := x509.ParseECPrivateKey(priv)
	if err != nil {
		k, perr := x509.ParsePKCS8PrivateKey(priv)
		if perr != nil {
			t.Fatalf("parse ES256 private (EC=%v PKCS8=%v)", err, perr)
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

func readPEMWave10(t *testing.T, abs string) []byte {
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

func signES256Wave10(t *testing.T, priv *ecdsa.PrivateKey, claims jwt.MapClaims, kid string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = kid
	out, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign ES256: %v", err)
	}
	return out
}

func wave10Claims(id identity.Identity, scopes []string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":     "https://idp.test",
		"sub":     id.UserID,
		"exp":     fixedNowWave10.Add(15 * time.Minute).Unix(),
		"nbf":     fixedNowWave10.Add(-1 * time.Minute).Unix(),
		"tenant":  id.TenantID,
		"user":    id.UserID,
		"session": id.SessionID,
		"scopes":  scopes,
	}
}

// TestE2E_Wave10_VersionHandshake_ContractStable — Phase 59's
// VersionHandshake is the negotiation primitive a Console (or any
// third-party Protocol client) consults to detect version skew BEFORE
// exercising a surface. The wave-end E2E pins the handshake's current
// shape end-to-end — a silent surface drift would surface here as a
// failure.
func TestE2E_Wave10_VersionHandshake_ContractStable(t *testing.T) {
	if types.ProtocolVersion != "0.1.0" {
		t.Fatalf("types.ProtocolVersion = %q, Wave 10 E2E is pinned to 0.1.0", types.ProtocolVersion)
	}
	h := types.CurrentHandshake()
	if h.ProtocolVersion != "0.1.0" {
		t.Fatalf("handshake.ProtocolVersion = %q, want 0.1.0", h.ProtocolVersion)
	}
	if !h.Accepts(types.CapTaskControl) {
		t.Fatal("handshake.Accepts(CapTaskControl) = false; the Phase 54 task-control surface must be advertised")
	}
	caps := h.Capabilities
	if len(caps) != 1 {
		t.Fatalf("handshake.Capabilities = %v, want exactly {task_control}", caps)
	}
	deps := types.Deprecations()
	if len(deps) != 0 {
		t.Errorf("types.Deprecations() returned %d entries at 0.1.0, expected empty registry", len(deps))
	}
}

// TestE2E_Wave10_FailureMode_UnknownKidTokenRejected — the §17.3 #3
// failure-mode coverage: a JWT with a `kid` the validator's KeySet
// does not know fails closed at the auth edge with CodeAuthRejected
// before the runtime is ever reached. Asserts the audit-rejection
// path AND that no task was spawned on the underlying TaskRegistry.
func TestE2E_Wave10_FailureMode_UnknownKidTokenRejected(t *testing.T) {
	deps := newWave10Deps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	id := identity.Identity{TenantID: "t-wave10", UserID: "u-wave10", SessionID: "s-wave10"}
	// Sign with the right private key but with a kid the validator
	// does not know — the KeySet returns "kid not in wave10 key set"
	// which the validator wraps as ErrUnknownKey → CodeAuthRejected.
	tok := signES256Wave10(t, deps.priv, wave10Claims(id, nil), "harbor-unknown-kid")

	body, _ := json.Marshal(types.StartRequest{
		Identity: types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		Query:    "should never spawn a task",
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/start", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown-kid: status = %d, want 401", resp.StatusCode)
	}
	respBody, _ := io.ReadAll(resp.Body)
	var env struct {
		Code protoerrors.Code `json:"code"`
	}
	if err := json.Unmarshal(respBody, &env); err != nil {
		t.Fatalf("decode error envelope: %v (body=%s)", err, respBody)
	}
	if env.Code != protoerrors.CodeAuthRejected {
		t.Fatalf("unknown-kid: error code = %q, want %q", env.Code, protoerrors.CodeAuthRejected)
	}

	// Defence-in-depth: the runtime was never reached. The
	// TaskRegistry has no tasks under this identity (the rejection
	// happened in the middleware, before Dispatch).
	idCtx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	listed, err := deps.tasks.List(idCtx, id, tasks.TaskFilter{})
	if err != nil {
		t.Fatalf("tasks.List: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("tasks.List under rejected identity returned %d entries; want 0 (the request never reached the runtime)", len(listed))
	}
}

// TestE2E_Wave10_Concurrency_NoCrossTalk — the §17.3 concurrency
// stress: N≥10 concurrent identity stacks against ONE shared mux.
// Distinct per-goroutine triple; each goroutine submits a `start` over
// REST with its own bearer; identity isolation holds (a foreign
// triple would surface as the wrong tenant on the spawned task);
// goroutine baseline restored on teardown.
func TestE2E_Wave10_Concurrency_NoCrossTalk(t *testing.T) {
	const n = 16

	deps := newWave10Deps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	time.Sleep(20 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	errs := make(chan error, n)
	taskIDs := make(chan struct {
		i  int
		id identity.Identity
		ti tasks.TaskID
	}, n)

	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  fmt.Sprintf("tenant-w10-%d", i),
				UserID:    "user-w10",
				SessionID: fmt.Sprintf("session-w10-%d", i),
			}
			tok := signES256Wave10(t, deps.priv, wave10Claims(id, nil), wave10Kid)
			body, _ := json.Marshal(types.StartRequest{
				Identity: types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
				Query:    fmt.Sprintf("conc-%d", i),
			})
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/"+string(methods.MethodStart),
				bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+tok)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errs <- fmt.Errorf("run %d POST: %w", i, err)
				return
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				respBody, _ := io.ReadAll(resp.Body)
				errs <- fmt.Errorf("run %d status %d: %s", i, resp.StatusCode, respBody)
				return
			}
			var sr types.StartResponse
			if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
				errs <- fmt.Errorf("run %d decode: %w", i, err)
				return
			}
			if sr.TaskID == "" {
				errs <- fmt.Errorf("run %d empty TaskID", i)
				return
			}
			taskIDs <- struct {
				i  int
				id identity.Identity
				ti tasks.TaskID
			}{i, id, tasks.TaskID(sr.TaskID)}
		}()
	}
	wg.Wait()
	close(errs)
	close(taskIDs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}

	// Identity isolation: every spawned task carries the originating
	// goroutine's triple. A context-bleed bug would surface as a task
	// owned by the wrong tenant.
	for got := range taskIDs {
		idCtx, err := identity.With(context.Background(), got.id)
		if err != nil {
			t.Errorf("run %d identity.With: %v", got.i, err)
			continue
		}
		task, err := deps.tasks.Get(idCtx, got.ti)
		if err != nil {
			t.Errorf("run %d tasks.Get(%q): %v", got.i, got.ti, err)
			continue
		}
		if task.Identity.TenantID != got.id.TenantID {
			t.Errorf("run %d: task tenant = %q, want %q (cross-talk)", got.i, task.Identity.TenantID, got.id.TenantID)
		}
		if task.Identity.SessionID != got.id.SessionID {
			t.Errorf("run %d: task session = %q, want %q (cross-talk)", got.i, task.Identity.SessionID, got.id.SessionID)
		}
	}

	// Goroutine baseline restored — every per-request goroutine has
	// joined (the SSE handler's keepalive goroutines, the response
	// readers). Small slack for scheduler noise.
	time.Sleep(100 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > baseline+10 {
		t.Errorf("goroutine leak: baseline %d, after %d", baseline, after)
	}
}

// Sanity: every Wave 10 phase's package surface is reachable via the
// canonical import path declared in this file. A build failure here is
// the signal that a Wave 10 package was renamed / removed and the
// integration test must update.
var _ = []any{
	(*events.EventBus)(nil),
	(*tasks.TaskRegistry)(nil),
	(*protocol.ControlSurface)(nil),
	(*steering.Registry)(nil),
	(*state.StateStore)(nil),
	(*auth.Validator)(nil),
	(*config.EventsConfig)(nil),
	types.Capabilities,
	types.CurrentHandshake,
	methods.IsValidMethod,
	protoerrors.IsValidCode,
}
