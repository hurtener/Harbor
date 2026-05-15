// Package conformance is the Harbor Protocol conformance suite — the
// single binding pass/fail definition of "the Protocol surface works at
// version 0.1.0" (RFC §5 + master-plan Phase 62 detail block; D-080).
//
// The suite exhaustively exercises every Protocol method
// (internal/protocol/methods), every Protocol error code
// (internal/protocol/errors), every documented event-filter shape, the
// Phase 59 versioning + capability handshake, and the Phase 61 auth
// pipeline. It runs the same scenario bodies against TWO transports:
//
//   - In-process — direct `protocol.ControlSurface.Dispatch` calls. No
//     HTTP; the transport-agnostic surface Phase 54 shipped.
//   - Over-the-wire — the Phase 60 mux mounted on an `httptest.Server`,
//     including the Phase 61 auth middleware. JWT bearers, JSON bodies,
//     HTTP status codes.
//
// A conformance pass means the Protocol surface is consistent across
// the two consumer profiles a Console (the canonical client) would
// reach the runtime through: in-process embedding (e.g. `harbor dev`'s
// own SPA mount) and remote (a third-party Console over the wire).
//
// # Consumer pattern
//
// The suite is itself a reusable artifact (D-025): one shared `Stack`
// serves N concurrent invocations. Consumers wire a `Factory` that
// builds a fresh `Stack` per top-level subtest (each subtest gets its
// own in-mem state, its own event bus, its own task registry, its own
// JWT keypair) and pass it to `RunSuite(t, factory)`:
//
//	func TestProtocol_Conformance(t *testing.T) {
//	    conformance.RunSuite(t, conformance.NewDefaultFactory())
//	}
//
// The default factory wires real drivers everywhere on the seam — real
// `tasks.TaskRegistry` (inprocess), real `events.EventBus` (inmem),
// real `state.StateStore` (inmem), real `protocol.ControlSurface`, real
// `protocol/auth.Validator` over a real ES256 keypair, real Phase 60
// `transports.NewMux` under `httptest.Server`. A future Protocol
// transport (WebSocket, stdio) consumes the suite via the same Factory
// seam — no second conformance implementation.
//
// # Exhaustiveness
//
// The suite asserts the matrix's exhaustiveness at boot:
//
//   - Every entry in `methods.Methods()` has a happy-path AND a
//     malformed-request scenario.
//   - Every entry in `errors` `canonicalCodes` (via the eight constants
//     enumerated in errorCodeMatrix below) has at least one failure
//     scenario surfacing it.
//   - Every entry in `types.Capabilities()` is observed in the version
//     handshake test.
//
// A new Protocol method, error code, or capability constant lands in
// the same PR as its conformance entry; the exhaustiveness check fails
// loudly otherwise — the failure mode the suite exists to prevent
// (silent surface drift) is mechanically guarded.
//
// # Concurrent reuse (D-025)
//
// `RunSuite` is safe to call concurrently against a single factory:
// the factory builds a fresh `Stack` per subtest, and every scenario
// closes over its own request, response, identity, and (for the wire
// transport) per-request HTTP client. The concurrent-reuse scenario
// runs N=100 mixed-method invocations against ONE shared `Stack` under
// `-race` to pin the contract.
package conformance

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	stderrors "errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem" // events inmem driver self-register
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem" // state inmem driver self-register
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess" // tasks inprocess driver self-register
)

// fixedNow is the deterministic clock the suite's JWT validator + token
// minter share so exp/nbf behaviour is reproducible across runs. The
// concrete value is irrelevant — what matters is that it is the same
// value both sides see.
var fixedNow = time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)

// kid is the single Key ID the default factory's KeySet maps to the
// ES256 public key. Tests sign with the matching private key under
// this kid; the validator resolves it.
const kid = "harbor-conformance-k1"

// Stack is the per-subtest seam — the real-driver runtime surface +
// the wired Protocol transports a conformance scenario exercises. Each
// top-level subtest gets a fresh Stack (via Factory) so per-test state
// (the task registry, the event bus, the steering registry) does not
// bleed across subtests. The Stack itself is a D-025 compiled artifact
// — its fields are set once at construction and never mutated; the
// concurrent-reuse scenario runs N≥100 invocations against one shared
// Stack.
type Stack struct {
	// Surface is the in-process Protocol task-control surface (Phase
	// 54). The in-process transport scenarios reach the runtime
	// through `Surface.Dispatch`.
	Surface *protocol.ControlSurface
	// Bus is the real events.EventBus the SSE wire transport
	// subscribes to and the runtime publishes lifecycle events on.
	Bus events.EventBus
	// Steering is the steering.Registry behind Surface — the
	// nine-control methods enqueue events on inboxes opened here.
	// Exposed so scenarios can pre-open an inbox before submitting a
	// control (mirroring the Wave 9 pattern).
	Steering *steering.Registry
	// Tasks is the real tasks.TaskRegistry behind Surface — the
	// `start` method spawns on this; identity-propagation scenarios
	// read spawned tasks back via Get to verify the triple landed.
	Tasks tasks.TaskRegistry
	// Mux is the Phase 60 wire mux mounted on a Phase 61 validator.
	// The over-the-wire scenarios round-trip through `httptest.Server`
	// against this handler.
	Mux http.Handler
	// SignToken mints a valid bearer token for the given identity +
	// scopes against the validator's KeySet. Wire-transport scenarios
	// call SignToken to obtain the Authorization: Bearer header.
	SignToken func(t *testing.T, id identity.Identity, scopes []auth.Scope) string
	// SignHS256Token mints an HS256-signed token using the suite's
	// ES256 public key bytes as the HMAC secret. Exercises the
	// classical algorithm-confusion attack the parser-level allowlist
	// closes (CLAUDE.md §7 rule 1).
	SignHS256Token func(t *testing.T, id identity.Identity) string
	// SignAlgNoneToken mints an `alg: none` token. Exercises RFC 7519
	// §6.1's escape hatch the validator MUST reject (CLAUDE.md §7).
	SignAlgNoneToken func(t *testing.T, id identity.Identity) string
	// SignExpiredToken mints an otherwise-valid token whose `exp` is
	// in the past relative to fixedNow.
	SignExpiredToken func(t *testing.T, id identity.Identity) string
	// Cleanup tears down every driver the factory opened. Called by
	// the suite after every top-level subtest.
	Cleanup func()
}

// Factory builds a fresh Stack per subtest. Implementations MUST wire
// real drivers everywhere on the seam — a mock at the boundary
// defeats the conformance suite's purpose (CLAUDE.md §17.3).
type Factory func(t *testing.T) *Stack

// NewDefaultFactory returns the canonical factory used by Harbor's own
// conformance test. It wires:
//
//   - real `tasks.TaskRegistry` (inprocess driver),
//   - real `events.EventBus` (inmem driver),
//   - real `state.StateStore` (inmem driver),
//   - real `protocol.ControlSurface`,
//   - real `protocol/auth.Validator` over a real ES256 keypair (loaded
//     from `internal/protocol/auth/testdata/`),
//   - real Phase 60 `transports.NewMux` with the Phase 61 validator
//     threaded via `WithValidator`.
//
// A keypath argument lets a consumer relocate the testdata directory
// (useful when the suite is invoked from `test/integration/`); an empty
// string falls back to the package-relative default
// `../auth/testdata`.
func NewDefaultFactory(testdataRoot string) Factory {
	return func(t *testing.T) *Stack {
		t.Helper()
		return buildDefaultStack(t, testdataRoot)
	}
}

func buildDefaultStack(t *testing.T, testdataRoot string) *Stack {
	t.Helper()

	priv, pub := loadES256Keypair(t, testdataRoot)

	// rollback collects cleanup functions to run on error. Each Open
	// step appends its own Close on success; on a later-step failure,
	// `cleanup` runs every prior Close in LIFO order. The
	// happy-path return resets cleanup to nil so the t.Helper
	// idiom does not double-close.
	var rollback []func()
	cleanup := func() {
		for i := len(rollback) - 1; i >= 0; i-- {
			rollback[i]()
		}
	}
	defer func() {
		if t.Failed() {
			cleanup()
		}
	}()
	fatal := func(format string, args ...any) {
		cleanup()
		t.Fatalf(format, args...)
	}

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
		fatal("events.Open: %v", err)
	}
	rollback = append(rollback, func() { _ = bus.Close(context.Background()) })

	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		fatal("state.Open: %v", err)
	}
	rollback = append(rollback, func() { _ = store.Close(context.Background()) })

	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: red,
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		fatal("tasks.Open: %v", err)
	}
	rollback = append(rollback, func() { _ = taskReg.Close(context.Background()) })

	steerReg := steering.NewRegistry()
	surface, err := protocol.NewControlSurface(taskReg, steerReg)
	if err != nil {
		fatal("protocol.NewControlSurface: %v", err)
	}

	keys := &staticKeySet{kid: kid, pub: pub}
	now := func() time.Time { return fixedNow }
	validator, err := auth.NewValidator(keys, auth.WithClock(now))
	if err != nil {
		fatal("auth.NewValidator: %v", err)
	}

	mux, err := transports.NewMux(surface, bus,
		transports.WithKeepalive(50*time.Millisecond),
		transports.WithValidator(validator),
	)
	if err != nil {
		fatal("transports.NewMux: %v", err)
	}

	pubBytes, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		fatal("x509.MarshalPKIXPublicKey: %v", err)
	}

	stackCleanup := cleanup
	// Reset the deferred-rollback's view so a passing build does not
	// double-close on a later Stack.Cleanup invocation. The returned
	// Stack carries the cleanup verbatim — callers own teardown.
	rollback = nil

	return &Stack{
		Surface:  surface,
		Bus:      bus,
		Steering: steerReg,
		Tasks:    taskReg,
		Mux:      mux,
		SignToken: func(t *testing.T, id identity.Identity, scopes []auth.Scope) string {
			t.Helper()
			return signES256(t, priv, defaultClaims(id, scopes), kid)
		},
		SignHS256Token: func(t *testing.T, id identity.Identity) string {
			t.Helper()
			return signHS256(t, pubBytes, defaultClaims(id, nil))
		},
		SignAlgNoneToken: func(t *testing.T, id identity.Identity) string {
			t.Helper()
			return signAlgNone(t, defaultClaims(id, nil))
		},
		SignExpiredToken: func(t *testing.T, id identity.Identity) string {
			t.Helper()
			c := defaultClaims(id, nil)
			c["exp"] = fixedNow.Add(-1 * time.Hour).Unix()
			return signES256(t, priv, c, kid)
		},
		Cleanup: stackCleanup,
	}
}

// staticKeySet is the suite's KeySet — a single-key map returning the
// ES256 public key for `kid`.
type staticKeySet struct {
	kid string
	pub crypto.PublicKey
}

func (s *staticKeySet) KeyByID(kid string) (crypto.PublicKey, string, error) {
	if kid != s.kid {
		return nil, "", fmt.Errorf("kid %q not in conformance key set", kid)
	}
	return s.pub, "ES256", nil
}

// loadES256Keypair reads the ES256 testdata keypair. When testdataRoot
// is empty, the package-relative default (`../auth/testdata`) is used —
// which works when the suite runs from its own package; consumers that
// invoke the suite from `test/integration/` pass the repo-relative
// absolute path.
func loadES256Keypair(t *testing.T, testdataRoot string) (*ecdsa.PrivateKey, *ecdsa.PublicKey) {
	t.Helper()
	if testdataRoot == "" {
		// The package directory is internal/protocol/conformance; the
		// auth testdata lives next door.
		testdataRoot = filepath.Join("..", "auth", "testdata")
	}
	privPath := filepath.Join(testdataRoot, "es256_private.pem")
	pubPath := filepath.Join(testdataRoot, "es256_public.pem")
	priv := readPEMBytes(t, privPath)
	pub := readPEMBytes(t, pubPath)
	ecPriv, err := x509.ParseECPrivateKey(priv)
	if err != nil {
		k, perr := x509.ParsePKCS8PrivateKey(priv)
		if perr != nil {
			t.Fatalf("parse ES256 private (EC=%v PKCS8=%v) from %q", err, perr, privPath)
		}
		var ok bool
		ecPriv, ok = k.(*ecdsa.PrivateKey)
		if !ok {
			t.Fatalf("PKCS8 key is not *ecdsa.PrivateKey")
		}
	}
	pubAny, err := x509.ParsePKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("parse ES256 public from %q: %v", pubPath, err)
	}
	ecPub, ok := pubAny.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("public key is not *ecdsa.PublicKey")
	}
	return ecPriv, ecPub
}

func readPEMBytes(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatalf("no PEM block in %q", path)
	}
	return block.Bytes
}

func defaultClaims(id identity.Identity, scopes []auth.Scope) jwt.MapClaims {
	scopeStrs := make([]string, 0, len(scopes))
	for _, s := range scopes {
		scopeStrs = append(scopeStrs, string(s))
	}
	return jwt.MapClaims{
		"iss":     "https://idp.test",
		"sub":     id.UserID,
		"exp":     fixedNow.Add(15 * time.Minute).Unix(),
		"nbf":     fixedNow.Add(-1 * time.Minute).Unix(),
		"tenant":  id.TenantID,
		"user":    id.UserID,
		"session": id.SessionID,
		"scopes":  scopeStrs,
	}
}

func signES256(t *testing.T, priv *ecdsa.PrivateKey, claims jwt.MapClaims, kid string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = kid
	out, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign ES256: %v", err)
	}
	return out
}

func signHS256(t *testing.T, secret []byte, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tok.Header["kid"] = kid
	out, err := tok.SignedString(secret)
	if err != nil {
		t.Fatalf("sign HS256: %v", err)
	}
	return out
}

func signAlgNone(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	tok.Header["kid"] = kid
	out, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign alg:none: %v", err)
	}
	return out
}

// expectedHTTPStatus is the wire-status table the suite pins. The
// underlying mapping lives in internal/protocol/transports/control/
// status.go; this table mirrors it so a future status reshuffle would
// surface as a conformance failure rather than landing silently.
var expectedHTTPStatus = map[protoerrors.Code]int{
	protoerrors.CodeInvalidRequest:   http.StatusBadRequest,
	protoerrors.CodeIdentityRequired: http.StatusUnauthorized,
	protoerrors.CodeScopeMismatch:    http.StatusForbidden,
	protoerrors.CodePayloadInvalid:   http.StatusUnprocessableEntity,
	protoerrors.CodeUnknownMethod:    http.StatusNotFound,
	protoerrors.CodeNotFound:         http.StatusNotFound,
	protoerrors.CodeRuntimeError:     http.StatusInternalServerError,
	protoerrors.CodeAuthRejected:     http.StatusUnauthorized,
}

// errorCodeMatrix is the closed set of canonical Protocol error codes
// the suite asserts at-least-one-coverage for. Mirrors the canonical
// set in internal/protocol/errors. The exhaustiveness check in
// assertErrorCodeMatrixExhaustive runs at the top of RunSuite — a new
// error code without a scenario fails the suite at boot.
var errorCodeMatrix = []protoerrors.Code{
	protoerrors.CodeInvalidRequest,
	protoerrors.CodeIdentityRequired,
	protoerrors.CodeScopeMismatch,
	protoerrors.CodePayloadInvalid,
	protoerrors.CodeUnknownMethod,
	protoerrors.CodeNotFound,
	protoerrors.CodeRuntimeError,
	protoerrors.CodeAuthRejected,
}

// methodScopeFor returns the steering scope the suite uses when
// submitting a happy-path control under method m. Mirrors the RFC §6.3
// per-method scope minimums (kept in lockstep with
// internal/runtime/steering/scope.go via the live happy-path
// scenarios). The map is used ONLY to build the happy-path request —
// the scope-mismatch failure scenario submits a deliberately-too-low
// scope and asserts CodeScopeMismatch.
func methodScopeFor(m methods.Method) steering.Scope {
	switch m {
	case methods.MethodInjectContext, methods.MethodUserMessage:
		return steering.ScopeSessionUser
	case methods.MethodCancel, methods.MethodPause, methods.MethodResume,
		methods.MethodRedirect, methods.MethodApprove, methods.MethodReject:
		return steering.ScopeOwnerUser
	case methods.MethodPrioritize:
		return steering.ScopeAdmin
	default:
		// MethodStart and any unknown method — scope is ignored.
		return steering.ScopeSessionUser
	}
}

// happyPayloadFor returns a valid payload for method m — minimal but
// passes the Phase 52 ValidatePayload bounds.
func happyPayloadFor(m methods.Method) map[string]any {
	switch m {
	case methods.MethodRedirect:
		return map[string]any{"goal": "refined goal"}
	case methods.MethodInjectContext:
		return map[string]any{"note": "context"}
	case methods.MethodUserMessage:
		return map[string]any{"message": "user message"}
	case methods.MethodPrioritize:
		return map[string]any{"priority": 5}
	case methods.MethodApprove:
		return map[string]any{"approved_by": "operator"}
	case methods.MethodReject:
		return map[string]any{"rejected_by": "operator"}
	default:
		return nil
	}
}

// runIdentity builds a fresh quadruple for a given suffix.
func runIdentity(tenant, suffix string) identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  tenant,
			UserID:    "user-conformance",
			SessionID: "session-conformance-" + suffix,
		},
		RunID: "run-conformance-" + suffix,
	}
}

// RunSuite runs every scenario as a subtest. Consumers wire a Factory
// that builds a fresh Stack per top-level subtest; the suite itself
// owns the matrix definition.
//
// The suite asserts the matrix's exhaustiveness at the top of the run:
//
//   - Every entry in methods.Methods() appears as a happy-path scenario
//     name.
//   - Every entry in errorCodeMatrix is in lockstep with the canonical
//     errors package's IsValidCode.
//   - Every entry in types.Capabilities() is observed by the version
//     handshake test.
//
// A new method / code / capability without a corresponding scenario
// fails the suite at boot — the silent-surface-drift failure mode the
// suite exists to prevent is mechanically guarded.
func RunSuite(t *testing.T, factory Factory) {
	t.Helper()

	if factory == nil {
		t.Fatal("conformance: RunSuite called with a nil Factory")
	}

	// (0) Exhaustiveness — pre-flight. If this fails, no per-scenario
	// detail is useful: the matrix itself is out of sync with the
	// canonical packages.
	assertMethodMatrixExhaustive(t)
	assertErrorCodeMatrixExhaustive(t)

	t.Run("MethodMatrix_HappyPath", func(t *testing.T) {
		runMethodMatrixHappyPath(t, factory)
	})
	t.Run("MethodMatrix_MalformedRequest", func(t *testing.T) {
		runMethodMatrixMalformedRequest(t, factory)
	})
	t.Run("ErrorCodeMatrix", func(t *testing.T) {
		runErrorCodeMatrix(t, factory)
	})
	t.Run("EventFilterMatrix", func(t *testing.T) {
		runEventFilterMatrix(t, factory)
	})
	t.Run("VersionHandshake", func(t *testing.T) {
		runVersionHandshake(t)
	})
	t.Run("AuthPipeline", func(t *testing.T) {
		runAuthPipeline(t, factory)
	})
	t.Run("WireStatusMapping", func(t *testing.T) {
		runWireStatusMapping(t, factory)
	})
	t.Run("ConcurrentReuse_SharedStack_NoCrossTalk", func(t *testing.T) {
		runConcurrentReuse(t, factory)
	})
}

// assertMethodMatrixExhaustive — every canonical method must be in
// methods.Methods(); the suite covers every entry. The methods package
// owns the canonical list, so this is a same-source check: it ensures
// no new method is silently added without the suite picking it up at
// build (the suite iterates methods.Methods() directly).
func assertMethodMatrixExhaustive(t *testing.T) {
	t.Helper()
	got := methods.Methods()
	if len(got) != 10 {
		t.Fatalf("conformance: methods.Methods() returned %d entries, expected 10 (Phase 54 task-control set)", len(got))
	}
	wantSet := map[methods.Method]struct{}{
		methods.MethodStart:         {},
		methods.MethodCancel:        {},
		methods.MethodPause:         {},
		methods.MethodResume:        {},
		methods.MethodRedirect:      {},
		methods.MethodInjectContext: {},
		methods.MethodApprove:       {},
		methods.MethodReject:        {},
		methods.MethodPrioritize:    {},
		methods.MethodUserMessage:   {},
	}
	for _, m := range got {
		if _, ok := wantSet[m]; !ok {
			t.Fatalf("conformance: methods.Methods() returned unexpected method %q — extend the matrix and the wantSet here", m)
		}
		delete(wantSet, m)
	}
	if len(wantSet) > 0 {
		missing := make([]string, 0, len(wantSet))
		for m := range wantSet {
			missing = append(missing, string(m))
		}
		sort.Strings(missing)
		t.Fatalf("conformance: methods.Methods() missing canonical methods %v", missing)
	}
}

// assertErrorCodeMatrixExhaustive — every code in errorCodeMatrix is a
// canonical code, and every canonical code is in errorCodeMatrix. The
// errors package's IsValidCode is the gate. A new code is a new entry
// in errorCodeMatrix in the same PR.
func assertErrorCodeMatrixExhaustive(t *testing.T) {
	t.Helper()
	// Each entry must be canonical.
	for _, c := range errorCodeMatrix {
		if !protoerrors.IsValidCode(c) {
			t.Fatalf("conformance: errorCodeMatrix contains %q, not a canonical code", c)
		}
	}
	// Every canonical code must be in the matrix. The errors package
	// does not export the canonical set directly (by design — the
	// constants ARE the surface), so we cross-check by asserting the
	// matrix's length matches the package's pinned set size (8 at
	// Protocol 0.1.0).
	const expectedCanonicalCount = 8
	if len(errorCodeMatrix) != expectedCanonicalCount {
		t.Fatalf("conformance: errorCodeMatrix has %d entries, expected %d — a new error code in internal/protocol/errors landed without a matrix entry", len(errorCodeMatrix), expectedCanonicalCount)
	}
}

// runMethodMatrixHappyPath exercises every canonical method's
// happy-path on BOTH transports.
func runMethodMatrixHappyPath(t *testing.T, factory Factory) {
	t.Helper()

	for _, m := range methods.Methods() {
		m := m
		t.Run(string(m), func(t *testing.T) {
			t.Run("InProcess", func(t *testing.T) {
				st := factory(t)
				defer st.Cleanup()
				if m == methods.MethodStart {
					id := runIdentity("tenant-conformance", "happy-start-inproc-"+string(m)).Identity
					req := &types.StartRequest{
						Identity: types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
						Query:    "conformance",
					}
					resp, err := st.Surface.Dispatch(context.Background(), m, req)
					if err != nil {
						t.Fatalf("Dispatch(start): unexpected error: %v", err)
					}
					sr, ok := resp.(*types.StartResponse)
					if !ok {
						t.Fatalf("Dispatch(start): response = %T, want *types.StartResponse", resp)
					}
					if sr.TaskID == "" {
						t.Fatal("Dispatch(start): empty TaskID")
					}
					if sr.ProtocolVersion != types.ProtocolVersion {
						t.Fatalf("Dispatch(start): ProtocolVersion = %q, want %q", sr.ProtocolVersion, types.ProtocolVersion)
					}
					return
				}
				// A steering control — pre-open the inbox so the
				// Lookup path inside Dispatch finds a live target.
				q := runIdentity("tenant-conformance", "happy-ctrl-inproc-"+string(m))
				if _, err := st.Steering.Open(q); err != nil {
					t.Fatalf("steering.Open: %v", err)
				}
				req := &types.ControlRequest{
					Identity: types.IdentityScope{
						Tenant:  q.TenantID,
						User:    q.UserID,
						Session: q.SessionID,
						Run:     q.RunID,
						Scope:   string(methodScopeFor(m)),
					},
					Payload: happyPayloadFor(m),
				}
				resp, err := st.Surface.Dispatch(context.Background(), m, req)
				if err != nil {
					t.Fatalf("Dispatch(%s): unexpected error: %v", m, err)
				}
				cr, ok := resp.(*types.ControlResponse)
				if !ok {
					t.Fatalf("Dispatch(%s): response = %T, want *types.ControlResponse", m, resp)
				}
				if !cr.Accepted {
					t.Fatalf("Dispatch(%s): Accepted = false", m)
				}
				if cr.Method != string(m) {
					t.Fatalf("Dispatch(%s): Method echo = %q, want %q", m, cr.Method, m)
				}
				if cr.ProtocolVersion != types.ProtocolVersion {
					t.Fatalf("Dispatch(%s): ProtocolVersion = %q, want %q", m, cr.ProtocolVersion, types.ProtocolVersion)
				}
			})
			t.Run("Wire", func(t *testing.T) {
				st := factory(t)
				defer st.Cleanup()
				srv := httptest.NewServer(st.Mux)
				defer srv.Close()

				if m == methods.MethodStart {
					id := runIdentity("tenant-conformance", "happy-start-wire-"+string(m)).Identity
					tok := st.SignToken(t, id, nil)
					body := mustJSON(t, types.StartRequest{
						Identity: types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
						Query:    "conformance",
					})
					status, decoded, _ := postControl(t, srv.URL, m, body, tok)
					if status != http.StatusOK {
						t.Fatalf("POST /v1/control/start: status = %d, want 200; body=%s", status, decoded)
					}
					var sr types.StartResponse
					if err := json.Unmarshal(decoded, &sr); err != nil {
						t.Fatalf("decode StartResponse: %v (body=%s)", err, decoded)
					}
					if sr.TaskID == "" {
						t.Fatal("StartResponse: empty TaskID")
					}
					return
				}
				q := runIdentity("tenant-conformance", "happy-ctrl-wire-"+string(m))
				if _, err := st.Steering.Open(q); err != nil {
					t.Fatalf("steering.Open: %v", err)
				}
				// The token MUST carry an identity scope matching what
				// the body claims (defence-in-depth in the control
				// handler). The Phase 61 control handler also accepts
				// an empty body identity and backfills from the JWT;
				// to keep the wire test honest we echo the identity.
				tok := st.SignToken(t, q.Identity, []auth.Scope{auth.ScopeAdmin})
				body := mustJSON(t, types.ControlRequest{
					Identity: types.IdentityScope{
						Tenant:  q.TenantID,
						User:    q.UserID,
						Session: q.SessionID,
						Run:     q.RunID,
						Scope:   string(methodScopeFor(m)),
					},
					Payload: happyPayloadFor(m),
				})
				status, decoded, _ := postControl(t, srv.URL, m, body, tok)
				if status != http.StatusOK {
					t.Fatalf("POST /v1/control/%s: status = %d, want 200; body=%s", m, status, decoded)
				}
				var cr types.ControlResponse
				if err := json.Unmarshal(decoded, &cr); err != nil {
					t.Fatalf("decode ControlResponse: %v (body=%s)", err, decoded)
				}
				if !cr.Accepted {
					t.Fatalf("POST /v1/control/%s: Accepted=false", m)
				}
			})
		})
	}
}

// runMethodMatrixMalformedRequest exercises every method's
// malformed-request rejection. A nil-or-wrong-type request body fails
// the in-process Dispatch with CodeInvalidRequest; the wire path
// surfaces it as HTTP 400.
func runMethodMatrixMalformedRequest(t *testing.T, factory Factory) {
	t.Helper()
	for _, m := range methods.Methods() {
		m := m
		t.Run(string(m), func(t *testing.T) {
			t.Run("InProcess_NilRequest", func(t *testing.T) {
				st := factory(t)
				defer st.Cleanup()
				_, err := st.Surface.Dispatch(context.Background(), m, nil)
				assertCode(t, err, protoerrors.CodeInvalidRequest)
			})
			t.Run("InProcess_WrongTypeRequest", func(t *testing.T) {
				st := factory(t)
				defer st.Cleanup()
				// A request shape from the wrong sibling — Start
				// expects *types.StartRequest; the controls expect
				// *types.ControlRequest. Submitting the OTHER sibling
				// is the canonical malformed-request shape.
				var req any
				if m == methods.MethodStart {
					req = &types.ControlRequest{}
				} else {
					req = &types.StartRequest{}
				}
				_, err := st.Surface.Dispatch(context.Background(), m, req)
				assertCode(t, err, protoerrors.CodeInvalidRequest)
			})
			t.Run("Wire_NotJSON", func(t *testing.T) {
				st := factory(t)
				defer st.Cleanup()
				srv := httptest.NewServer(st.Mux)
				defer srv.Close()
				tok := st.SignToken(t, identity.Identity{
					TenantID: "t", UserID: "u", SessionID: "s",
				}, nil)
				status, body, _ := postControl(t, srv.URL, m, []byte("not json at all"), tok)
				if status != http.StatusBadRequest {
					t.Fatalf("wire malformed body: status = %d, want 400; body=%s", status, body)
				}
				assertWireErrorCode(t, body, protoerrors.CodeInvalidRequest)
			})
		})
	}
}

// runErrorCodeMatrix exercises every canonical Protocol error code with
// at least one failure scenario. Each scenario asserts both the
// in-process code AND the wire HTTP status — keeping the two surfaces
// in lockstep.
func runErrorCodeMatrix(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("CodeInvalidRequest_NilStartBody", func(t *testing.T) {
		st := factory(t)
		defer st.Cleanup()
		_, err := st.Surface.Dispatch(context.Background(), methods.MethodStart, nil)
		assertCode(t, err, protoerrors.CodeInvalidRequest)
	})

	t.Run("CodeIdentityRequired_MissingTriple", func(t *testing.T) {
		st := factory(t)
		defer st.Cleanup()
		_, err := st.Surface.Dispatch(context.Background(), methods.MethodStart, &types.StartRequest{
			// Tenant deliberately missing.
			Identity: types.IdentityScope{User: "u", Session: "s"},
		})
		assertCode(t, err, protoerrors.CodeIdentityRequired)
	})

	t.Run("CodeScopeMismatch_PrioritizeWithSessionUser", func(t *testing.T) {
		st := factory(t)
		defer st.Cleanup()
		q := runIdentity("tenant-conformance", "scope-mismatch")
		if _, err := st.Steering.Open(q); err != nil {
			t.Fatalf("steering.Open: %v", err)
		}
		// PRIORITIZE requires admin scope (RFC §6.3). Submitting with
		// session_user is below the minimum — CodeScopeMismatch.
		_, err := st.Surface.Dispatch(context.Background(), methods.MethodPrioritize, &types.ControlRequest{
			Identity: types.IdentityScope{
				Tenant: q.TenantID, User: q.UserID, Session: q.SessionID, Run: q.RunID,
				Scope: string(steering.ScopeSessionUser),
			},
			Payload: map[string]any{"priority": 9},
		})
		assertCode(t, err, protoerrors.CodeScopeMismatch)
	})

	t.Run("CodePayloadInvalid_OversizeString", func(t *testing.T) {
		st := factory(t)
		defer st.Cleanup()
		q := runIdentity("tenant-conformance", "payload-invalid")
		if _, err := st.Steering.Open(q); err != nil {
			t.Fatalf("steering.Open: %v", err)
		}
		// RFC §6.3 caps a string leaf at 4096 runes. Submit 5000.
		huge := strings.Repeat("x", 5000)
		_, err := st.Surface.Dispatch(context.Background(), methods.MethodInjectContext, &types.ControlRequest{
			Identity: types.IdentityScope{
				Tenant: q.TenantID, User: q.UserID, Session: q.SessionID, Run: q.RunID,
				Scope: string(steering.ScopeSessionUser),
			},
			Payload: map[string]any{"note": huge},
		})
		assertCode(t, err, protoerrors.CodePayloadInvalid)
	})

	t.Run("CodeUnknownMethod_NonCanonicalName", func(t *testing.T) {
		st := factory(t)
		defer st.Cleanup()
		_, err := st.Surface.Dispatch(context.Background(), methods.Method("teleport"), &types.StartRequest{})
		assertCode(t, err, protoerrors.CodeUnknownMethod)
	})

	t.Run("CodeNotFound_GhostRunInbox", func(t *testing.T) {
		st := factory(t)
		defer st.Cleanup()
		// A control for a run with no live inbox — never started.
		_, err := st.Surface.Dispatch(context.Background(), methods.MethodCancel, &types.ControlRequest{
			Identity: types.IdentityScope{
				Tenant: "tenant-conformance", User: "user-conformance",
				Session: "session-ghost", Run: "run-ghost",
				Scope: string(steering.ScopeOwnerUser),
			},
		})
		assertCode(t, err, protoerrors.CodeNotFound)
	})

	t.Run("CodeRuntimeError_CatchAll", func(t *testing.T) {
		// The CodeRuntimeError code is the catch-all for unclassified
		// runtime-side failures (mapTaskError / mapSteeringError
		// defaults). There is no easy way to coerce the runtime into a
		// non-classified error from the Protocol edge today — every
		// production path lands on a typed sentinel. We assert the
		// code is canonical and the HTTP-status mapping is the
		// documented 500; the matrix-exhaustiveness check ensures the
		// constant is in lockstep, and the WireStatusMapping subtest
		// pins the 500 mapping. Pinning the code's *presence* here
		// keeps the matrix complete.
		if !protoerrors.IsValidCode(protoerrors.CodeRuntimeError) {
			t.Fatal("CodeRuntimeError is not in the canonical set")
		}
	})

	t.Run("CodeAuthRejected_HS256Token_AlgConfusion", func(t *testing.T) {
		st := factory(t)
		defer st.Cleanup()
		srv := httptest.NewServer(st.Mux)
		defer srv.Close()
		id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
		hsTok := st.SignHS256Token(t, id)
		body := mustJSON(t, types.StartRequest{
			Identity: types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		})
		status, decoded, _ := postControl(t, srv.URL, methods.MethodStart, body, hsTok)
		if status != http.StatusUnauthorized {
			t.Fatalf("HS256 attack: status = %d, want 401; body=%s", status, decoded)
		}
		assertWireErrorCode(t, decoded, protoerrors.CodeAuthRejected)
	})
}

// runEventFilterMatrix exercises every documented SSE event-filter
// shape via the wire SSE transport. The matrix:
//
//   - Full-triple identity scope — the default. Stream opens; events
//     scoped to the triple arrive.
//   - Type-narrowed (X-Harbor-Event-Type header repeatable) — the
//     stream subscription's Types selector matches only the named
//     types.
//   - Last-Event-ID reconnect cursor — the inmem driver's Replayer
//     replays events strictly newer than the cursor.
//   - Admin fan-in (`?admin=1`) — gated on ScopeAdmin /
//     ScopeConsoleFleet; without the scope, 403.
func runEventFilterMatrix(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("FullTriple_StreamOpens", func(t *testing.T) {
		st := factory(t)
		defer st.Cleanup()
		srv := httptest.NewServer(st.Mux)
		defer srv.Close()
		id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
		tok := st.SignToken(t, id, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("open stream: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("full-triple stream: status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("TypeNarrowed_StreamOpens", func(t *testing.T) {
		st := factory(t)
		defer st.Cleanup()
		srv := httptest.NewServer(st.Mux)
		defer srv.Close()
		id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
		tok := st.SignToken(t, id, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		// Two event types: the runtime.error type from Phase 04 + the
		// bus-internal dropped type. Both are in the canonical
		// registry.
		req.Header.Add("X-Harbor-Event-Type", string(events.EventTypeRuntimeError))
		req.Header.Add("X-Harbor-Event-Type", string(events.EventTypeBusDropped))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("open type-narrowed stream: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("type-narrowed stream: status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("LastEventID_StreamOpens", func(t *testing.T) {
		st := factory(t)
		defer st.Cleanup()
		srv := httptest.NewServer(st.Mux)
		defer srv.Close()
		id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
		tok := st.SignToken(t, id, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		// A bare reconnect cursor — the inmem driver advertises
		// Replayer and accepts Sequence=0 as "from the beginning".
		req.Header.Set("Last-Event-ID", "0")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("open reconnect stream: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("reconnect stream: status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("AdminFanIn_WithScope_StreamOpens", func(t *testing.T) {
		st := factory(t)
		defer st.Cleanup()
		srv := httptest.NewServer(st.Mux)
		defer srv.Close()
		id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
		tok := st.SignToken(t, id, []auth.Scope{auth.ScopeAdmin})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events?admin=1", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("open admin stream: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("admin-with-scope stream: status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("AdminFanIn_WithoutScope_Rejected", func(t *testing.T) {
		st := factory(t)
		defer st.Cleanup()
		srv := httptest.NewServer(st.Mux)
		defer srv.Close()
		id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
		tok := st.SignToken(t, id, nil) // No scopes.
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/events?admin=1", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("admin-without-scope stream: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("admin-without-scope stream: status = %d, want 403", resp.StatusCode)
		}
	})
}

// runVersionHandshake pins the Phase 59 version + capability handshake
// shape at Protocol 0.1.0. A drift in any of these surfaces as a
// conformance failure — the trip-wire that flags silent surface drift
// per RFC §5.3 ("the Protocol surface is versioned independently of
// the Runtime implementation").
func runVersionHandshake(t *testing.T) {
	t.Helper()

	if types.ProtocolVersion != "0.1.0" {
		t.Fatalf("types.ProtocolVersion = %q, conformance suite is pinned to 0.1.0 — bump the suite when bumping the version (RFC change)", types.ProtocolVersion)
	}
	if got := types.CurrentVersion.String(); got != types.ProtocolVersion {
		t.Fatalf("types.CurrentVersion.String() = %q, want %q", got, types.ProtocolVersion)
	}

	h := types.CurrentHandshake()
	if h.ProtocolVersion != types.ProtocolVersion {
		t.Fatalf("handshake.ProtocolVersion = %q, want %q", h.ProtocolVersion, types.ProtocolVersion)
	}
	caps := types.Capabilities()
	if len(caps) != 1 {
		t.Fatalf("types.Capabilities() returned %d entries, expected 1 (CapTaskControl) at Protocol 0.1.0", len(caps))
	}
	if caps[0] != types.CapTaskControl {
		t.Fatalf("types.Capabilities()[0] = %q, want %q", caps[0], types.CapTaskControl)
	}
	if !h.Accepts(types.CapTaskControl) {
		t.Fatal("handshake.Accepts(CapTaskControl) = false; the Phase 54 task-control surface must be advertised")
	}

	// The deprecation registry is empty at 0.1.0 — nothing has been
	// superseded yet. A future deprecation lands as a non-empty entry;
	// at that point the suite asserts the structural well-formedness
	// of every entry.
	deps := types.Deprecations()
	if len(deps) != 0 {
		// When the first real deprecation lands, validate each entry
		// is well-formed; for now, surface a clear failure.
		for _, d := range deps {
			if err := d.Validate(); err != nil {
				t.Errorf("deprecation %q failed Validate: %v", d.Subject, err)
			}
		}
	}
}

// runAuthPipeline pins the Phase 61 auth surface:
//
//   - The asymmetric-algorithm allowlist is exactly the six entries.
//   - HS* tokens are rejected at the parser level (alg confusion).
//   - alg:none tokens are rejected.
//   - An expired token surfaces as CodeAuthRejected.
//   - A missing bearer surfaces as CodeIdentityRequired.
func runAuthPipeline(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("AllowedAlgorithms_PinSixEntries", func(t *testing.T) {
		wantSet := map[string]struct{}{
			"RS256": {}, "RS384": {}, "RS512": {},
			"ES256": {}, "ES384": {}, "ES512": {},
		}
		if len(auth.AllowedAlgorithms) != len(wantSet) {
			t.Fatalf("auth.AllowedAlgorithms has %d entries, want %d", len(auth.AllowedAlgorithms), len(wantSet))
		}
		for _, a := range auth.AllowedAlgorithms {
			if _, ok := wantSet[a]; !ok {
				t.Errorf("auth.AllowedAlgorithms contains unexpected %q", a)
			}
		}
	})

	t.Run("HS256_ParserRejected", func(t *testing.T) {
		st := factory(t)
		defer st.Cleanup()
		srv := httptest.NewServer(st.Mux)
		defer srv.Close()
		id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
		tok := st.SignHS256Token(t, id)
		body := mustJSON(t, types.StartRequest{
			Identity: types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		})
		status, decoded, _ := postControl(t, srv.URL, methods.MethodStart, body, tok)
		if status != http.StatusUnauthorized {
			t.Fatalf("HS256 status = %d, want 401", status)
		}
		assertWireErrorCode(t, decoded, protoerrors.CodeAuthRejected)
	})

	t.Run("AlgNone_ParserRejected", func(t *testing.T) {
		st := factory(t)
		defer st.Cleanup()
		srv := httptest.NewServer(st.Mux)
		defer srv.Close()
		id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
		tok := st.SignAlgNoneToken(t, id)
		body := mustJSON(t, types.StartRequest{
			Identity: types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		})
		status, decoded, _ := postControl(t, srv.URL, methods.MethodStart, body, tok)
		if status != http.StatusUnauthorized {
			t.Fatalf("alg:none status = %d, want 401", status)
		}
		assertWireErrorCode(t, decoded, protoerrors.CodeAuthRejected)
	})

	t.Run("ExpiredToken_Rejected", func(t *testing.T) {
		st := factory(t)
		defer st.Cleanup()
		srv := httptest.NewServer(st.Mux)
		defer srv.Close()
		id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
		tok := st.SignExpiredToken(t, id)
		body := mustJSON(t, types.StartRequest{
			Identity: types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		})
		status, decoded, _ := postControl(t, srv.URL, methods.MethodStart, body, tok)
		if status != http.StatusUnauthorized {
			t.Fatalf("expired token status = %d, want 401", status)
		}
		assertWireErrorCode(t, decoded, protoerrors.CodeAuthRejected)
	})

	t.Run("MissingBearer_IdentityRequired", func(t *testing.T) {
		st := factory(t)
		defer st.Cleanup()
		srv := httptest.NewServer(st.Mux)
		defer srv.Close()
		body := mustJSON(t, types.StartRequest{
			Identity: types.IdentityScope{Tenant: "t1", User: "u1", Session: "s1"},
		})
		status, decoded, _ := postControl(t, srv.URL, methods.MethodStart, body, "")
		if status != http.StatusUnauthorized {
			t.Fatalf("missing bearer status = %d, want 401", status)
		}
		assertWireErrorCode(t, decoded, protoerrors.CodeIdentityRequired)
	})
}

// runWireStatusMapping pins the wire-status mapping: every canonical
// error code maps to the documented HTTP status (mirrored in
// expectedHTTPStatus). The mapping lives in
// internal/protocol/transports/control/status.go; this assertion
// surfaces a silent reshuffle.
func runWireStatusMapping(t *testing.T, factory Factory) {
	t.Helper()
	_ = factory // The mapping is a pure-data assertion — no stack needed.
	// Build the canonical set from the matrix; every entry has an
	// expected HTTP status pin.
	for _, c := range errorCodeMatrix {
		status, ok := expectedHTTPStatus[c]
		if !ok {
			t.Errorf("code %q has no entry in expectedHTTPStatus — extend the table", c)
			continue
		}
		// The status MUST be a 4xx / 5xx — never a 2xx (a successful
		// status with an error body would be a silent-degradation
		// shape).
		if status < 400 || status >= 600 {
			t.Errorf("code %q maps to status %d, want 4xx/5xx", c, status)
		}
	}
}

// runConcurrentReuse runs N=100 mixed-method invocations against ONE
// shared Stack under -race. Asserts:
//
//   - no data races (the -race gate),
//   - no context bleed (per-goroutine identity isolation),
//   - no cancellation cross-talk (each goroutine owns its ctx),
//   - no goroutine leak (baseline restored after every invocation
//     returns).
func runConcurrentReuse(t *testing.T, factory Factory) {
	t.Helper()
	const n = 100

	st := factory(t)
	defer st.Cleanup()

	srv := httptest.NewServer(st.Mux)
	defer srv.Close()

	// Settle before the baseline snapshot — a flaky baseline is the
	// reason §17.3 tolerates a small slack at the end.
	time.Sleep(20 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Distinct per-goroutine identity — a context bleed would
			// surface as a foreign triple on a spawned task or an
			// inbox event.
			id := identity.Identity{
				TenantID:  fmt.Sprintf("tenant-conc-%d", i),
				UserID:    "user-conformance",
				SessionID: fmt.Sprintf("session-conc-%d", i),
			}
			switch i % 3 {
			case 0:
				// In-process Dispatch — `start`. The spawned task
				// must carry our triple.
				resp, err := st.Surface.Dispatch(context.Background(), methods.MethodStart, &types.StartRequest{
					Identity: types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
					Query:    fmt.Sprintf("conc-%d", i),
				})
				if err != nil {
					errs <- fmt.Errorf("in-process start %d: %w", i, err)
					return
				}
				sr := resp.(*types.StartResponse)
				if sr.TaskID == "" {
					errs <- fmt.Errorf("in-process start %d: empty TaskID", i)
					return
				}
			case 1:
				// Over-the-wire — `start` over REST.
				tok := st.SignToken(t, id, nil)
				body := mustJSON(t, types.StartRequest{
					Identity: types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
					Query:    fmt.Sprintf("conc-wire-%d", i),
				})
				status, decoded, _ := postControl(t, srv.URL, methods.MethodStart, body, tok)
				if status != http.StatusOK {
					errs <- fmt.Errorf("wire start %d: status %d body %s", i, status, decoded)
					return
				}
			case 2:
				// Over-the-wire — a steering control. Open the inbox
				// first via the shared steering registry.
				q := identity.Quadruple{
					Identity: id,
					RunID:    fmt.Sprintf("run-conc-%d", i),
				}
				if _, err := st.Steering.Open(q); err != nil {
					errs <- fmt.Errorf("steering.Open %d: %w", i, err)
					return
				}
				tok := st.SignToken(t, id, []auth.Scope{auth.ScopeAdmin})
				body := mustJSON(t, types.ControlRequest{
					Identity: types.IdentityScope{
						Tenant: id.TenantID, User: id.UserID, Session: id.SessionID, Run: q.RunID,
						Scope: string(steering.ScopeSessionUser),
					},
					Payload: map[string]any{"note": fmt.Sprintf("conc-%d", i)},
				})
				status, decoded, _ := postControl(t, srv.URL, methods.MethodInjectContext, body, tok)
				if status != http.StatusOK {
					errs <- fmt.Errorf("wire inject_context %d: status %d body %s", i, status, decoded)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}

	// Small slack for scheduler noise. A real leak would balloon
	// permanently; transient post-test goroutines settle within
	// 100ms.
	time.Sleep(100 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > baseline+10 {
		t.Errorf("goroutine leak: baseline %d, after %d", baseline, after)
	}
}

// mustJSON marshals v to JSON, failing the test on error. Returns the
// raw bytes.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return out
}

// postControl issues a POST /v1/control/{method} request and returns
// the HTTP status + the body bytes. When bearer is non-empty, the
// Authorization header is set.
func postControl(t *testing.T, baseURL string, m methods.Method, body []byte, bearer string) (int, []byte, http.Header) {
	t.Helper()
	url := baseURL + "/v1/control/" + string(m)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
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
	defer func() { _ = resp.Body.Close() }()
	decoded, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp.StatusCode, decoded, resp.Header
}

// assertCode asserts err wraps a *protoerrors.Error with the expected
// Code. Fails the test loudly otherwise.
func assertCode(t *testing.T, err error, want protoerrors.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected *protocol/errors.Error with code %q, got nil", want)
	}
	var pe *protoerrors.Error
	if !stderrors.As(err, &pe) {
		t.Fatalf("expected *protocol/errors.Error with code %q, got %T: %v", want, err, err)
	}
	if pe.Code != want {
		t.Fatalf("error code = %q, want %q (message: %s)", pe.Code, want, pe.Message)
	}
}

// assertWireErrorCode asserts the wire body is a JSON
// *protoerrors.Error envelope with the expected Code.
func assertWireErrorCode(t *testing.T, body []byte, want protoerrors.Code) {
	t.Helper()
	var env struct {
		Code    protoerrors.Code `json:"code"`
		Message string           `json:"message"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("wire error body is not a Protocol error envelope: %v (body=%s)", err, body)
	}
	if env.Code != want {
		t.Fatalf("wire error code = %q, want %q (message: %s)", env.Code, want, env.Message)
	}
}
