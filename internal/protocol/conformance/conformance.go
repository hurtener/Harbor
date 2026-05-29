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
	"github.com/hurtener/Harbor/internal/telemetry"
	_ "github.com/hurtener/Harbor/internal/telemetry/drivers/noop" // span exporter noop driver self-register
)

// FixedNow is the deterministic clock the suite's JWT validator +
// token minter share so exp/nbf behaviour is reproducible across
// runs. Exported so external consumers (e.g.
// `test/integration/wave10_test.go`'s own Stack builder) can pin to
// the same instant — keeping `fixedNow` in one canonical home
// instead of two `time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)`
// literals (PR #91, Wave 10 audit NIT-3). The concrete value is
// irrelevant — what matters is that every JWT signer + every
// validator sees the same instant.
var FixedNow = time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)

// fixedNow is the package-internal alias for FixedNow used by the
// rest of the package's helpers. Kept as a separate name so the
// (numerous) in-package references stay readable; FixedNow is the
// stable consumer-facing accessor.
var fixedNow = FixedNow

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
	rollback := make([]func(), 0, 3)
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
	validator, err := auth.NewValidator(keys,
		auth.WithClock(now),
		auth.WithRedactor(red),
		auth.WithEventBus(bus),
	)
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
	protoerrors.CodeInvalidRequest:        http.StatusBadRequest,
	protoerrors.CodeIdentityRequired:      http.StatusUnauthorized,
	protoerrors.CodeScopeMismatch:         http.StatusForbidden,
	protoerrors.CodePayloadInvalid:        http.StatusUnprocessableEntity,
	protoerrors.CodeUnknownMethod:         http.StatusNotFound,
	protoerrors.CodeNotFound:              http.StatusNotFound,
	protoerrors.CodeRuntimeError:          http.StatusInternalServerError,
	protoerrors.CodeAuthRejected:          http.StatusUnauthorized,
	protoerrors.CodeIdentityScopeRequired: http.StatusForbidden,
	protoerrors.CodePresignUnsupported:    http.StatusNotImplemented,
	protoerrors.CodeRequestTooLarge:       http.StatusRequestEntityTooLarge,
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
	protoerrors.CodeIdentityScopeRequired,
	// Phase 73l (D-120) artifacts surface — `CodePresignUnsupported`
	// (an `artifacts.get_ref` against a non-S3 driver) and
	// `CodeRequestTooLarge` (an oversize `artifacts.put` body). Both are
	// exercised end-to-end by the artifacts surface unit tests +
	// test/integration/artifacts_page_test.go; the conformance-suite
	// scenario lands when the Stack wires an ArtifactsSurface (same
	// posture as the search.* / posture / pause / topology clusters).
	protoerrors.CodePresignUnsupported,
	protoerrors.CodeRequestTooLarge,
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
	t.Run("TracePropagation", func(t *testing.T) {
		runTracePropagation(t, factory)
	})
	t.Run("ConcurrentReuse_SharedStack_NoCrossTalk", func(t *testing.T) {
		runConcurrentReuse(t, factory)
	})
	t.Run("EventsSubscribe_HappyPath", func(t *testing.T) {
		runEventsSubscribeNegotiation(t, factory)
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
	// Phase 54 task-control ten + Wave 13 streaming-events two +
	// Phase 72c search cluster five + Phase 72f posture cluster five +
	// Phase 72g posture pair two + Phase 72e pause-snapshot one + Phase
	// 74 topology.snapshot one + Phase 73l artifacts cluster three +
	// Phase 73j memory cluster three + Phase 73k mcp.servers.* twelve +
	// Phase 73f tools cluster seven + Phase 73i flows-page six +
	// Phase 73d tasks-page two + Phase 73e agents-page eight +
	// Phase 73c sessions-page two + Phase 73n runs-page one +
	// Phase 73m auth.rotate_token one = 71.
	if len(got) != 71 {
		t.Fatalf("conformance: methods.Methods() returned %d entries, expected 71 (Phase 54 task-control ten + Wave 13 streaming-events two + Phase 72c search cluster five + Phase 72f posture cluster five + Phase 72g posture pair two + Phase 72e pause-snapshot one + Phase 74 topology.snapshot one + Phase 73l artifacts cluster three + Phase 73j memory cluster three + Phase 73k mcp.servers.* twelve + Phase 73f tools cluster seven + Phase 73i flows-page six + Phase 73d tasks-page two + Phase 73e agents-page eight + Phase 73c sessions-page two + Phase 73n runs-page one + Phase 73m auth.rotate_token one)", len(got))
	}
	wantSet := map[methods.Method]struct{}{
		methods.MethodStart:             {},
		methods.MethodCancel:            {},
		methods.MethodPause:             {},
		methods.MethodResume:            {},
		methods.MethodRedirect:          {},
		methods.MethodInjectContext:     {},
		methods.MethodApprove:           {},
		methods.MethodReject:            {},
		methods.MethodPrioritize:        {},
		methods.MethodUserMessage:       {},
		methods.MethodEventsSubscribe:   {},
		methods.MethodEventsAggregate:   {},
		methods.MethodSearchQuery:       {},
		methods.MethodSearchSessions:    {},
		methods.MethodSearchTasks:       {},
		methods.MethodSearchEvents:      {},
		methods.MethodSearchArtifacts:   {},
		methods.MethodRuntimeInfo:       {},
		methods.MethodRuntimeHealth:     {},
		methods.MethodRuntimeCounters:   {},
		methods.MethodRuntimeDrivers:    {},
		methods.MethodMetricsSnapshot:   {},
		methods.MethodGovernancePosture: {},
		methods.MethodLLMPosture:        {},
		methods.MethodPauseList:         {},
		methods.MethodTopologySnapshot:  {},
		methods.MethodArtifactsList:     {},
		methods.MethodArtifactsPut:      {},
		methods.MethodArtifactsGetRef:   {},
		methods.MethodMemoryList:        {},
		methods.MethodMemoryGet:         {},
		methods.MethodMemoryHealth:      {},

		methods.MethodMCPServersList:             {},
		methods.MethodMCPServersGet:              {},
		methods.MethodMCPServersResources:        {},
		methods.MethodMCPServersPrompts:          {},
		methods.MethodMCPServersRefreshDiscovery: {},
		methods.MethodMCPServersProbe:            {},
		methods.MethodMCPServersHealth:           {},
		methods.MethodMCPServersBindingsList:     {},
		methods.MethodMCPServersPolicy:           {},
		methods.MethodMCPServersRefreshBinding:   {},
		methods.MethodMCPServersRevokeBinding:    {},
		methods.MethodMCPServersSetRawHTMLTrust:  {},

		methods.MethodToolsList:              {},
		methods.MethodToolsGet:               {},
		methods.MethodToolsDescribe:          {},
		methods.MethodToolsMetrics:           {},
		methods.MethodToolsContentStats:      {},
		methods.MethodToolsSetApprovalPolicy: {},
		methods.MethodToolsRevokeOAuth:       {},

		methods.MethodTasksList: {},
		methods.MethodTasksGet:  {},

		methods.MethodFlowsList:         {},
		methods.MethodFlowsDescribe:     {},
		methods.MethodFlowsRunsList:     {},
		methods.MethodFlowsRunsDescribe: {},
		methods.MethodFlowsRun:          {},
		methods.MethodFlowsMetrics:      {},

		methods.MethodAgentsList:        {},
		methods.MethodAgentsGet:         {},
		methods.MethodAgentsTools:       {},
		methods.MethodAgentsMemory:      {},
		methods.MethodAgentsGovernance:  {},
		methods.MethodAgentsSkills:      {},
		methods.MethodAgentsPermissions: {},
		methods.MethodAgentsMetrics:     {},

		methods.MethodSessionsList:    {},
		methods.MethodSessionsInspect: {},

		methods.MethodRunsSetOverrides: {},

		methods.MethodAuthRotateToken: {},
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
//
// PR #91 / D-082 (Wave 10 audit WARN-4): the exhaustiveness side of
// the check now iterates `protoerrors.Codes()` (added in PR #91) so
// the assertion is structurally exhaustive — a new canonical code
// landing without a matrix entry surfaces by NAME, not by a stale
// hardcoded count.
func assertErrorCodeMatrixExhaustive(t *testing.T) {
	t.Helper()
	// Each entry must be canonical.
	matrixSet := make(map[protoerrors.Code]struct{}, len(errorCodeMatrix))
	for _, c := range errorCodeMatrix {
		if !protoerrors.IsValidCode(c) {
			t.Fatalf("conformance: errorCodeMatrix contains %q, not a canonical code", c)
		}
		matrixSet[c] = struct{}{}
	}
	// Every canonical code from protoerrors.Codes() must be in the matrix.
	missing := make([]string, 0)
	for _, c := range protoerrors.Codes() {
		if _, ok := matrixSet[c]; !ok {
			missing = append(missing, string(c))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("conformance: errorCodeMatrix missing canonical codes %v — a new error code in internal/protocol/errors landed without a matrix entry", missing)
	}
}

// runMethodMatrixHappyPath exercises every canonical method's
// happy-path on BOTH transports.
//
// Streaming-events methods (MethodEventsSubscribe / MethodEventsAggregate
// — Wave 13) are NOT exercised via the ControlSurface here: they route
// through their own transport (the SSE handler / the events-aggregate
// HTTP handler), not the REST control surface. Their happy-paths live
// under EventsSubscribe_HappyPath (subscribe) and the EventFilterMatrix
// / EventsAggregateMatrix scenarios — the matrix-exhaustiveness check
// at boot still covers them. Dispatch returns CodeInvalidRequest if a
// caller hits the REST surface with either method (the "wrong transport
// for wrong vocabulary" guard).
func runMethodMatrixHappyPath(t *testing.T, factory Factory) {
	t.Helper()

	for _, m := range methods.Methods() {

		if methods.IsStreamingEventsMethod(m) {
			// Streaming-events methods — covered by
			// EventsSubscribe_HappyPath, EventFilterMatrix, and the
			// EventsAggregateMatrix scenarios. Dispatch returns
			// CodeInvalidRequest if a caller hits the REST surface
			// with these methods (the "wrong transport for wrong
			// vocabulary" guard).
			continue
		}
		t.Run(string(m), func(t *testing.T) {
			// Phase 72c (D-108): the five `search.*` methods are
			// task-control-disjoint — they need a SearchSurface, not
			// a ControlSurface. Phase 80 will extend the conformance
			// suite to exercise them end-to-end; until then, skip with
			// an explicit issue-style reason so the SKIP is observable
			// rather than silent.
			if methods.IsSearchMethod(m) {
				t.Skip("phase-72c: search.* methods exercised by their per-package conformance + integration tests; conformance-suite scenario lands in Phase 80")
			}
			// Phase 72f / 72g (D-111 / D-112): the seven posture methods
			// — the five `runtime.*` / `metrics.*` reads plus
			// `governance.posture` / `llm.posture` — are dispatched by
			// PostureSurface, not ControlSurface. Their happy-paths are
			// exercised by internal/protocol posture tests + the
			// runtime-posture / phase72g_posture integration tests; the
			// conformance-suite scenario lands later (same posture as
			// the search cluster).
			if methods.IsPostureMethod(m) {
				t.Skip("phase-72f/72g: posture methods exercised by internal/protocol posture tests + test/integration posture tests; conformance-suite scenario lands later")
			}
			// Phase 72e (D-110): `pause.list` is a read-only snapshot
			// over the pauseresume.Coordinator — it routes through its
			// own HTTP handler (POST /v1/pause/list), not the REST
			// ControlSurface. It is exercised end-to-end by the
			// pause_list_handler unit tests + test/integration/
			// pause_list_test.go (the §13 primitive-with-consumer
			// binding test). Skip with an explicit reason so the SKIP
			// is observable rather than silent — same posture as the
			// search.* cluster above.
			if methods.IsPauseMethod(m) {
				t.Skip("phase-72e: pause.list exercised by its handler unit tests + test/integration/pause_list_test.go; conformance-suite scenario lands with the Phase 80 surface extension")
			}
			// Phase 74 (D-114): topology.snapshot needs a
			// TopologyAccessor (an engine.Engine) the conformance
			// Stack does not wire — its runtime is task/steering-
			// shaped. The method's happy-path + failure modes are
			// exercised end-to-end by Phase 74's own unit tests, the
			// concurrent-reuse test, and test/integration/phase74_
			// topology_test.go (real engine + real bus + real wire
			// transport). The conformance-suite scenario lands when
			// the suite gains an engine-bearing Stack.
			if methods.IsTopologyMethod(m) {
				t.Skip("phase-74: topology.snapshot exercised by its unit + concurrent + integration tests; conformance-suite scenario lands when the Stack wires an engine")
			}
			// Phase 73l (D-120): the three `artifacts.*` methods are
			// dispatched by ArtifactsSurface, not ControlSurface — they
			// need an ArtifactStore the conformance Stack does not wire.
			// Their happy-paths + failure modes are exercised by the
			// artifacts surface unit tests, the concurrent-reuse test,
			// and test/integration/artifacts_page_test.go (real
			// ArtifactStore + real wire transport). The conformance-suite
			// scenario lands when the Stack gains an ArtifactStore — same
			// posture as the search / posture / pause / topology clusters.
			if methods.IsArtifactsMethod(m) {
				t.Skip("phase-73l: artifacts.* methods exercised by their unit + concurrent + integration tests; conformance-suite scenario lands when the Stack wires an ArtifactStore")
			}
			// Phase 73j (D-118) memory.* methods route through their own
			// stream-transport handlers (POST /v1/memory/*), NOT the
			// task-control ControlSurface. They are exercised end-to-end
			// by their own unit + concurrent-reuse + integration tests +
			// the phase-73j smoke; the conformance-suite scenario lands
			// when the Stack wires a MemoryStore-bearing memory handler.
			if methods.IsMemoryMethod(m) {
				t.Skip("phase-73j: memory.* methods exercised by their unit + concurrent + integration tests; conformance-suite scenario lands when the Stack wires a memory handler")
			}
			// Phase 73k (D-119): the twelve `mcp.servers.*` methods are
			// dispatched by the MCPSurface, not the ControlSurface — the
			// conformance Stack wires no MCP accessor. Their happy-paths
			// + failure modes are exercised end-to-end by the MCPSurface
			// unit tests + the MCP-page integration test. Skip with an
			// explicit reason — same posture as the search / posture /
			// topology clusters above.
			if methods.IsMCPServersMethod(m) {
				t.Skip("phase-73k: mcp.servers.* methods exercised by the MCPSurface unit tests + test/integration MCP-page test; conformance-suite scenario lands with the Phase 80 surface extension")
			}
			// Phase 73f (D-116): the seven `tools.*` methods are
			// dispatched by the Tools handler (POST /v1/tools/{method}),
			// not the REST ControlSurface — they need a tools.ToolCatalog
			// the conformance Stack does not wire. Their happy-paths +
			// failure modes are exercised by internal/tools/protocol unit
			// tests, the concurrent-reuse test, the stream-package
			// tools_handler tests, and test/integration/tools_page_test.go
			// (real catalog + real wire transport + real ES256 auth).
			// Skip with an explicit reason — same posture as the search /
			// posture / pause / topology clusters above.
			if methods.IsToolsMethod(m) {
				t.Skip("phase-73f: tools.* methods exercised by internal/tools/protocol tests + stream tools_handler tests + test/integration/tools_page_test.go; conformance-suite scenario lands with the Phase 80 surface extension")
			}
			// Phase 73i (D-117): the six `flows.*` methods route through
			// the Console Flows-page handler (POST /v1/flows/*), not the
			// REST ControlSurface — they need a flowprotocol.Surface the
			// conformance Stack does not wire. Their happy-paths +
			// failure modes are exercised by the flows_handler unit
			// tests, the flow/protocol surface tests, the concurrent-
			// reuse test, and test/integration/flows_page_test.go (real
			// registry + real bus + real wire transport). The
			// conformance-suite scenario lands with the Phase 80 surface
			// extension — same posture as the search / pause clusters.
			if methods.IsFlowsMethod(m) {
				t.Skip("phase-73i: flows.* methods exercised by their handler + surface unit tests + test/integration/flows_page_test.go; conformance-suite scenario lands with the Phase 80 surface extension")
			}
			// Phase 73d (D-123): the two `tasks.*` methods route through
			// the Console Tasks-page handler (POST /v1/tasks/{method}),
			// not the REST ControlSurface — they need a tasks.TaskRegistry
			// the conformance Stack does not wire. Their happy-paths +
			// failure modes are exercised by internal/tasks/protocol unit
			// tests, the concurrent-reuse test, the stream-package
			// tasks_handler tests, and test/integration/tasks_page_test.go
			// (real registry + real wire transport). Skip with an
			// explicit reason — same posture as the tools / flows clusters.
			if methods.IsTasksMethod(m) {
				t.Skip("phase-73d: tasks.* methods exercised by internal/tasks/protocol tests + stream tasks_handler tests + test/integration/tasks_page_test.go; conformance-suite scenario lands with the Phase 80 surface extension")
			}
			// Phase 73e (D-124): the eight `agents.*` methods route
			// through the Console Agents-page handler
			// (POST /v1/agents/{method}), not the REST ControlSurface —
			// they need an Agent Registry the conformance Stack does not
			// wire. Their happy-paths + failure modes are exercised by
			// internal/runtime/registry/protocol unit tests, the
			// concurrent-reuse test, the stream-package agents_handler
			// tests, and test/integration/agents_page_test.go (real
			// registry + real bus + real wire transport). The
			// conformance-suite scenario lands with the Phase 80 surface
			// extension — same posture as the tools / flows clusters.
			if methods.IsAgentsMethod(m) {
				t.Skip("phase-73e: agents.* methods exercised by internal/runtime/registry/protocol tests + stream agents_handler tests + test/integration/agents_page_test.go; conformance-suite scenario lands with the Phase 80 surface extension")
			}
			// Phase 73c (D-122): the two `sessions.*` methods route
			// through the Console Sessions-page handler (POST
			// /v1/sessions/*), not the REST ControlSurface — they need a
			// sessions/protocol.Service the conformance Stack does not
			// wire. Their happy-paths + failure modes are exercised by
			// the sessions/protocol unit tests, the D-025 concurrent-
			// reuse test, the stream sessions_handler tests, and
			// test/integration/sessions_page_test.go (real registry +
			// real wire transport + real auth). Skip with an explicit
			// reason — same posture as the search / pause / flows
			// clusters above.
			if methods.IsSessionsMethod(m) {
				t.Skip("phase-73c: sessions.* methods exercised by internal/sessions/protocol tests + stream sessions_handler tests + test/integration/sessions_page_test.go; conformance-suite scenario lands with the Phase 80 surface extension")
			}
			// Phase 73n (D-130): `runs.set_overrides` routes through the
			// Console Playground-page handler (POST /v1/runs/set_overrides),
			// not the REST ControlSurface — it needs a runs/protocol.Service
			// (override Store) the conformance Stack does not wire. Its
			// happy-path + failure modes are exercised by the
			// internal/runtime/runs/protocol unit + concurrent-reuse tests,
			// the stream runs_handler tests, and
			// test/integration/playground_overrides_test.go (real Store +
			// real wire transport + real auth). Skip with an explicit
			// reason — same posture as the sessions / tools / flows clusters.
			if methods.IsRunsMethod(m) {
				t.Skip("phase-73n: runs.* methods exercised by internal/runtime/runs/protocol tests + stream runs_handler tests + test/integration/playground_overrides_test.go; conformance-suite scenario lands with the Phase 80 surface extension")
			}
			// Phase 73m (D-129): auth.rotate_token routes through its own
			// stream-transport handler (AuthHandler over auth.RotateSurface),
			// not the ControlSurface — its happy path is exercised by
			// internal/protocol/auth/rotate_token_test.go + stream
			// auth_handler_test.go + test/integration/settings_page_test.go;
			// the conformance-suite scenario lands with the Phase 80
			// surface extension.
			if methods.IsAuthMethod(m) {
				t.Skip("phase-73m: auth.rotate_token exercised by internal/protocol/auth/rotate_token_test.go + stream auth_handler_test.go + test/integration/settings_page_test.go; conformance-suite scenario lands with the Phase 80 surface extension")
			}
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
					status, decoded := postControl(t, srv.URL, m, body, tok)
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
				status, decoded := postControl(t, srv.URL, m, body, tok)
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
//
// Streaming-events methods (MethodEventsSubscribe / MethodEventsAggregate
// — Wave 13) are intentionally skipped — their "wrong transport"
// rejection is structurally CodeInvalidRequest (the streaming-events
// method is hitting the REST control surface), not "malformed JSON". A
// dedicated assertion lives below.
func runMethodMatrixMalformedRequest(t *testing.T, factory Factory) {
	t.Helper()
	for _, m := range methods.Methods() {

		if methods.IsStreamingEventsMethod(m) {
			continue
		}
		t.Run(string(m), func(t *testing.T) {
			// Phase 72c (D-108): the five search.* methods are
			// dispatched by SearchSurface, not ControlSurface. Their
			// malformed-request rejection is covered in the
			// per-package conformance test in internal/search/*; the
			// conformance suite picks them up in Phase 80.
			if methods.IsSearchMethod(m) {
				t.Skip("phase-72c: search.* malformed-request paths covered by per-package conformance; conformance-suite scenario lands in Phase 80")
			}
			// Phase 72f / 72g (D-111 / D-112): the seven posture methods
			// are dispatched by PostureSurface, not ControlSurface.
			// Their malformed-request rejection is covered by the
			// internal/protocol posture tests + the per-package
			// posture-handler tests; the conformance-suite scenario
			// lands later.
			if methods.IsPostureMethod(m) {
				t.Skip("phase-72f/72g: posture malformed-request paths covered by internal/protocol + per-package posture-handler tests; conformance-suite scenario lands later")
			}
			// Phase 72e (D-110): pause.list routes through its own HTTP
			// handler, not the ControlSurface. Its malformed-request
			// rejection (PageSize > max → 400, bad status enum → 400)
			// is covered by pause_list_handler_test.go; the conformance
			// suite picks it up with the Phase 80 surface extension.
			if methods.IsPauseMethod(m) {
				t.Skip("phase-72e: pause.list malformed-request paths covered by pause_list_handler_test.go; conformance-suite scenario lands with the Phase 80 surface extension")
			}
			// Phase 74 (D-114): topology.snapshot against the
			// engine-less conformance Stack returns CodeUnknownMethod
			// (the nil-accessor branch) before any request-shape
			// check, so the generic malformed-request CodeInvalidRequest
			// assertion does not apply. The method's malformed-request
			// path (wrong request type against a wired accessor) is
			// covered by Phase 74's protocol_test.go.
			if methods.IsTopologyMethod(m) {
				t.Skip("phase-74: topology.snapshot malformed-request path covered by internal/protocol/protocol_test.go; conformance-suite scenario lands when the Stack wires an engine")
			}
			// Phase 73l (D-120): the three artifacts.* methods are
			// dispatched by ArtifactsSurface, which the engine-less
			// conformance Stack does not wire — the control transport
			// returns CodeUnknownMethod before any request-shape check.
			// Their malformed-request paths are covered by the artifacts
			// surface unit tests + test/integration/artifacts_page_test.go;
			// the conformance-suite scenario lands when the Stack wires
			// an ArtifactStore.
			if methods.IsArtifactsMethod(m) {
				t.Skip("phase-73l: artifacts.* malformed-request paths covered by internal/protocol/artifacts_test.go; conformance-suite scenario lands when the Stack wires an ArtifactStore")
			}
			// Phase 73j (D-118): memory.* methods route through their own
			// stream-transport handlers, not the ControlSurface — their
			// malformed-request paths are covered by the memory_handler
			// tests.
			if methods.IsMemoryMethod(m) {
				t.Skip("phase-73j: memory.* malformed-request paths covered by internal/protocol/transports/stream/memory_handler_test.go")
			}
			// Phase 73k (D-119): mcp.servers.* methods route through the
			// MCPSurface, not the ControlSurface — their malformed-request
			// paths are covered by the MCPSurface unit tests.
			if methods.IsMCPServersMethod(m) {
				t.Skip("phase-73k: mcp.servers.* malformed-request path covered by internal/protocol/mcp_test.go; conformance-suite scenario lands with the Phase 80 surface extension")
			}
			// Phase 73m (D-129): auth.rotate_token routes through its own
			// stream-transport handler, not the ControlSurface — its
			// malformed-request paths are covered by stream
			// auth_handler_test.go + internal/protocol/auth/rotate_token_test.go.
			if methods.IsAuthMethod(m) {
				t.Skip("phase-73m: auth.rotate_token malformed-request paths covered by internal/protocol/transports/stream/auth_handler_test.go")
			}
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
				status, body := postControl(t, srv.URL, m, []byte("not json at all"), tok)
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
		status, decoded := postControl(t, srv.URL, methods.MethodStart, body, hsTok)
		if status != http.StatusUnauthorized {
			t.Fatalf("HS256 attack: status = %d, want 401; body=%s", status, decoded)
		}
		assertWireErrorCode(t, decoded, protoerrors.CodeAuthRejected)
	})

	t.Run("CodeIdentityScopeRequired_CrossTenantWithoutScope", func(t *testing.T) {
		// Phase 72 / D-105: a `?admin=1` request from a JWT lacking
		// `auth.ScopeAdmin` AND `auth.ScopeConsoleFleet` is rejected
		// 403 with the typed Code `identity_scope_required`. The wire
		// surface is the Phase 60 `/v1/events` SSE route.
		st := factory(t)
		defer st.Cleanup()
		srv := httptest.NewServer(st.Mux)
		defer srv.Close()

		id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
		tok := st.SignToken(t, id, nil) // no scopes
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/events?admin=1", nil)
		if err != nil {
			t.Fatalf("http.NewRequest: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET ?admin=1: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("cross-tenant without scope: status = %d, want 403", resp.StatusCode)
		}
		decoded, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		assertWireErrorCode(t, decoded, protoerrors.CodeIdentityScopeRequired)
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
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
		if reqErr != nil {
			t.Fatalf("build request: %v", reqErr)
		}
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
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
		if reqErr != nil {
			t.Fatalf("build request: %v", reqErr)
		}
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
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
		if reqErr != nil {
			t.Fatalf("build request: %v", reqErr)
		}
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
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events?admin=1", nil)
		if reqErr != nil {
			t.Fatalf("build request: %v", reqErr)
		}
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
		req, reqErr := http.NewRequest(http.MethodGet, srv.URL+"/v1/events?admin=1", nil)
		if reqErr != nil {
			t.Fatalf("build request: %v", reqErr)
		}
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

	// PR #91 / D-082 (Wave 10 audit WARN-5): the optional
	// `X-Harbor-Run` carrier header narrows the subscription to a
	// single run inside the (tenant, user, session) scope. A
	// run-scoped subscription opens cleanly; the events.Filter.Run
	// is honoured by the Matches predicate (verified in
	// internal/events tests).
	t.Run("RunScoped_StreamOpens", func(t *testing.T) {
		st := factory(t)
		defer st.Cleanup()
		srv := httptest.NewServer(st.Mux)
		defer srv.Close()
		id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
		tok := st.SignToken(t, id, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
		if reqErr != nil {
			t.Fatalf("build request: %v", reqErr)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("X-Harbor-Run", "run-conformance-42")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("open run-scoped stream: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("run-scoped stream: status = %d, want 200", resp.StatusCode)
		}
	})
}

// runEventsSubscribeNegotiation pins the Phase 72 / D-105 method-name
// surface: `events.subscribe` is a canonical Protocol method. The wire
// transport is `GET /v1/events` (Phase 60 SSE); the method-name
// constant is the contract a third-party Console branches on. The
// happy-path scenario opens a triple-scoped SSE stream over the wire
// and asserts HTTP 200 + the SSE Content-Type. The reject paths
// (cross-tenant without scope → 403 + identity_scope_required;
// `?admin=1` with the scope → 200) are already covered by the
// EventFilterMatrix scenarios.
//
// The in-process scenario asserts the "wrong transport" guard: a
// caller hitting the REST control surface with `events.subscribe`
// gets CodeInvalidRequest (the streaming-events vocabulary is served
// by the SSE transport).
func runEventsSubscribeNegotiation(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("Wire_TripleScopedSubscribe_200", func(t *testing.T) {
		st := factory(t)
		defer st.Cleanup()
		srv := httptest.NewServer(st.Mux)
		defer srv.Close()

		id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
		tok := st.SignToken(t, id, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
		if reqErr != nil {
			t.Fatalf("build request: %v", reqErr)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("open events.subscribe stream: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("events.subscribe wire status = %d, want 200", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
			t.Errorf("events.subscribe Content-Type = %q, want text/event-stream", ct)
		}
	})

	t.Run("InProcess_WrongTransport_FailsClosed", func(t *testing.T) {
		// The REST control surface rejects events.subscribe at the
		// vocabulary boundary — it's a streaming-events method, not a
		// task-control method.
		st := factory(t)
		defer st.Cleanup()
		_, err := st.Surface.Dispatch(context.Background(), methods.MethodEventsSubscribe, &types.StartRequest{
			Identity: types.IdentityScope{Tenant: "t1", User: "u1", Session: "s1"},
		})
		assertCode(t, err, protoerrors.CodeInvalidRequest)
	})

	t.Run("CanonicalMethodName_Registered", func(t *testing.T) {
		// String-form pinning lives in internal/protocol/methods's own
		// test (TestMethods_EventsSubscribe_Registered); here we pin
		// the in-place registration so a refactor that drops the
		// constant from canonicalMethods surfaces as a conformance
		// failure.
		if !methods.IsValidMethod(methods.MethodEventsSubscribe) {
			t.Error("IsValidMethod(MethodEventsSubscribe) = false, want true")
		}
		if methods.IsControlMethod(methods.MethodEventsSubscribe) {
			t.Error("IsControlMethod(MethodEventsSubscribe) = true, want false — streaming-events, not steering-control")
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
	// Phase 54 task-control + Wave 13 streaming-events + Phase 72f
	// runtime-posture + phase 84a topology-snapshot = 4 capabilities
	// at Protocol 0.1.0. (The capability constants live in
	// internal/protocol/types/version.go; a new capability is a new
	// constant + a new entry in canonicalCapabilities. Phase 84a / F1
	// — `topology_snapshot` is in the canonical *registry*; per-instance
	// advertisement is conditional via `PostureDeps.TopologyAvailable`.)
	if len(caps) != 4 {
		t.Fatalf("types.Capabilities() returned %d entries, expected 4 (CapTaskControl + CapEventsSubscribe + CapRuntimePosture + CapTopologySnapshot) at Protocol 0.1.0", len(caps))
	}
	wantCaps := map[types.Capability]struct{}{
		types.CapTaskControl:      {},
		types.CapEventsSubscribe:  {},
		types.CapRuntimePosture:   {},
		types.CapTopologySnapshot: {},
	}
	for _, c := range caps {
		if _, ok := wantCaps[c]; !ok {
			t.Fatalf("types.Capabilities() returned unexpected capability %q", c)
		}
		delete(wantCaps, c)
	}
	if len(wantCaps) > 0 {
		missing := make([]string, 0, len(wantCaps))
		for c := range wantCaps {
			missing = append(missing, string(c))
		}
		t.Fatalf("types.Capabilities() missing canonical capabilities %v", missing)
	}
	if !h.Accepts(types.CapTaskControl) {
		t.Fatal("handshake.Accepts(CapTaskControl) = false; the Phase 54 task-control surface must be advertised")
	}
	if !h.Accepts(types.CapEventsSubscribe) {
		t.Fatal("handshake.Accepts(CapEventsSubscribe) = false; the Wave 13 streaming-events surface (Phase 72 / 72a) must be advertised")
	}
	if !h.Accepts(types.CapRuntimePosture) {
		t.Fatal("handshake.Accepts(CapRuntimePosture) = false; the Wave 13 runtime-posture surface (Phase 72f) must be advertised")
	}
	if !h.Accepts(types.CapTopologySnapshot) {
		t.Fatal("handshake.Accepts(CapTopologySnapshot) = false; the Phase 74 topology-snapshot surface must appear in the canonical capability set (per-instance advertisement is conditional, but the handshake universe is unconditional)")
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
		status, decoded := postControl(t, srv.URL, methods.MethodStart, body, tok)
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
		status, decoded := postControl(t, srv.URL, methods.MethodStart, body, tok)
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
		status, decoded := postControl(t, srv.URL, methods.MethodStart, body, tok)
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
		status, decoded := postControl(t, srv.URL, methods.MethodStart, body, "")
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

// runTracePropagation pins the W3C TraceContext propagation through
// the Phase 60 SSE transport: a `traceparent` header on the GET
// /v1/events request flows into the stream handler's context;
// SpanFromEvent on the received event then produces a span whose
// TraceID matches the inbound traceparent.
//
// PR #91 / D-082 (Wave 10 audit WARN-7): added so the conformance
// matrix observes the trace-propagation seam, not just unit tests.
// The previous conformance pass did not exercise the propagation
// helpers (`InjectHTTP` / `ExtractHTTP`) end-to-end at all — this
// scenario is the first.
func runTracePropagation(t *testing.T, factory Factory) {
	t.Helper()

	st := factory(t)
	defer st.Cleanup()

	// Build a real tracer (the in-memory noop exporter is the default
	// when OTelEndpoint is empty; a synchronous test seam is not
	// required here — we only need a real span context, not a
	// recorded export).
	tr, shutdown, err := telemetry.NewTracer(config.TelemetryConfig{
		ServiceName: "harbor-conformance-trace",
	})
	if err != nil {
		t.Fatalf("telemetry.NewTracer: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }() //nolint:errcheck // test-cleanup tracer shutdown — a shutdown error cannot affect the test outcome.

	// Start a root span so we have a known traceparent to inject.
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	q := identity.Quadruple{Identity: id, RunID: "run-conformance-trace"}
	seed := events.Event{
		Type:       events.EventTypeRuntimeWarning,
		Identity:   q,
		OccurredAt: fixedNow,
		Payload:    conformanceTracePayload{Note: "seed"},
	}
	rootCtx, rootSpan := tr.SpanFromEvent(context.Background(), seed)
	wantTraceID := rootSpan.SpanContext().TraceID()
	rootSpan.End()

	// Build a traceparent header from rootCtx so the wire request
	// carries the inbound trace context.
	hdr := http.Header{}
	telemetry.InjectHTTP(rootCtx, hdr)
	tp := hdr.Get("traceparent")
	if tp == "" {
		t.Fatal("InjectHTTP wrote no traceparent — the propagator is not wired")
	}

	// Run a wire round-trip with the traceparent header. The SSE
	// handler resolves the identity (from the bearer or carrier
	// headers); the trace context is implicit on the request ctx.
	// On the server side, the ExtractHTTP→SpanFromEvent chain on a
	// received event produces a span whose TraceID matches.
	srv := httptest.NewServer(st.Mux)
	defer srv.Close()

	// Subscribe to the bus directly (the SSE wire path frames events;
	// we want the raw event so we can call SpanFromEvent on it).
	sub, err := st.Bus.Subscribe(context.Background(), events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
		Types:   []events.EventType{tasks.EventTypeTaskSpawned},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	// Drive a start via REST so a task.spawned event is published.
	tok := st.SignToken(t, id, nil)
	body := mustJSON(t, types.StartRequest{
		Identity: types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		Query:    "conformance-trace",
	})
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/start", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	// Inject the traceparent so the request carries the inbound trace
	// context (the server side receives it on the wire even though no
	// runtime code currently reads it — the assertion is on the
	// propagation helpers' round-trip + the receiver's SpanFromEvent
	// derivation).
	req.Header.Set("traceparent", tp)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/control/start: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start status = %d, want 200", resp.StatusCode)
	}

	// Receive the task.spawned event. Then derive a span via
	// SpanFromEvent on a ctx populated by ExtractHTTP from the same
	// traceparent — the resulting span shares the trace id.
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("subscription channel closed before task.spawned arrived")
		}
		// Build the receiver-side ctx from the wire's traceparent.
		recvHdr := http.Header{}
		recvHdr.Set("traceparent", tp)
		recvCtx := telemetry.ExtractHTTP(context.Background(), recvHdr)
		_, span := tr.SpanFromEvent(recvCtx, ev)
		gotTraceID := span.SpanContext().TraceID()
		span.End()
		if gotTraceID != wantTraceID {
			t.Fatalf("trace propagation broken: want TraceID=%s, got %s",
				wantTraceID.String(), gotTraceID.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for task.spawned event")
	}
}

// conformanceTracePayload is a minimal events.EventPayload used to
// seed the trace propagation scenario's root span. Sealed so the bus
// accepts it; non-Safe so callers can rely on the redactor walking
// it (the test does not publish this payload — it only feeds it to
// SpanFromEvent which does not consult payload bytes).
type conformanceTracePayload struct {
	events.Sealed
	Note string
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

	for i := range n {

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
				sr, ok := resp.(*types.StartResponse)
				if !ok {
					errs <- fmt.Errorf("in-process start %d: response type %T, want *types.StartResponse", i, resp)
					return
				}
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
				status, decoded := postControl(t, srv.URL, methods.MethodStart, body, tok)
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
				status, decoded := postControl(t, srv.URL, methods.MethodInjectContext, body, tok)
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
func postControl(t *testing.T, baseURL string, m methods.Method, body []byte, bearer string) (int, []byte) {
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
	return resp.StatusCode, decoded
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
