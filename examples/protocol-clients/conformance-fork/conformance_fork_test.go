package conformancefork

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"

	// Test-scoped conformance harness (CLAUDE.md §13 carve-out): a worked
	// example assembling a fork's real-driver stack pulls the same V1
	// in-memory drivers the canonical Factory wires, by blank import, so
	// the registry factories resolve them. Not a production binary —
	// importing internal/drivers/prod here would pull the whole driver
	// aggregator into an example whose point is the minimal fork seam.
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem" // events inmem driver self-register
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/conformance"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem" // state inmem driver self-register
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess" // tasks inprocess driver self-register
)

// forkKID is the single Key ID this fork's KeySet maps to the ES256
// public key. A real fork picks its own; the suite's tokens carry it in
// the JWT header and the validator resolves it.
const forkKID = "harbor-conformance-fork-k1"

const forkAgentID = "harbor-conformance-agent"

type forkAgentResolver struct{}

func (forkAgentResolver) ResolveAgent(_ context.Context, _ identity.Identity, agentID string) (bool, error) {
	return agentID == forkAgentID, nil
}

func (forkAgentResolver) EffectiveAgentID(requested string) (string, error) {
	if requested != "" {
		return requested, nil
	}
	return forkAgentID, nil
}

// TestConformanceFork certifies a Protocol-server fork's assembly
// against the FULL Harbor conformance suite by wiring a custom
// conformance.Factory and handing it to conformance.RunSuite.
//
// This is the worked example the conformance-certification page points
// at: where Harbor's own gate is one line over NewDefaultFactory, a
// fork wires its OWN stack (forkFactory below) and runs the identical
// suite over it. A pass here means this assembly is wire-compatible with
// the pinned Protocol version across both consumer profiles (in-process
// and over-the-wire); a mis-wire fails the suite, never silently.
func TestConformanceFork(t *testing.T) {
	conformance.RunSuite(t, forkFactory(testdataRoot(t)))
}

// forkFactory returns a conformance.Factory that builds a fresh,
// real-driver *conformance.Stack per top-level subtest. This is the
// substitution point a fork owns: every field of the returned Stack —
// the control surface, the wire mux, the event bus, the token-minting
// closures — is wired here, so a fork swaps each for its own
// implementation while keeping RunSuite unchanged.
//
// The wiring mirrors what an embedder assembles from Harbor's runtime
// packages: a real in-memory event bus, state store, and task registry;
// a real ControlSurface over a real steering registry; a real wire mux
// with the real auth middleware; and a real ES256 validator. No mocks
// on any seam (CLAUDE.md §17.3) — the suite would reject them.
func forkFactory(testdataRoot string) conformance.Factory {
	return func(t *testing.T) *conformance.Stack {
		t.Helper()
		return buildForkStack(t, testdataRoot)
	}
}

func buildForkStack(t *testing.T, testdataRoot string) *conformance.Stack {
	t.Helper()

	priv, pub := loadES256Keypair(t, testdataRoot)

	// rollback collects the Close of every driver opened so far; on a
	// later-step failure the partial stack is torn down in LIFO order.
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
	agentReach := auth.NewAgentReachAuthorizer()
	surface, err := protocol.NewControlSurface(taskReg, steerReg,
		protocol.WithAgentResolver(forkAgentResolver{}),
		protocol.WithAgentReachAuthorizer(agentReach))
	if err != nil {
		fatal("protocol.NewControlSurface: %v", err)
	}

	// The suite shares one deterministic instant across token minting and
	// the validator's clock so exp/nbf behaviour is reproducible.
	// conformance.FixedNow is the canonical home for it.
	now := func() time.Time { return conformance.FixedNow }

	validator, err := auth.NewValidator(&forkKeySet{kid: forkKID, pub: pub},
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
	rollback = nil // a passing build hands ownership to Stack.Cleanup

	return &conformance.Stack{
		Surface:  surface,
		Bus:      bus,
		Steering: steerReg,
		Tasks:    taskReg,
		Mux:      mux,
		SignToken: func(t *testing.T, id identity.Identity, scopes []auth.Scope) string {
			t.Helper()
			return signES256(t, priv, defaultClaims(id, scopes), forkKID)
		},
		SignHS256Token: func(t *testing.T, id identity.Identity) string {
			t.Helper()
			// The classical algorithm-confusion attack: sign with HS256
			// using the ES256 public key bytes as the HMAC secret. The
			// asymmetric-only allowlist MUST reject it (CLAUDE.md §7).
			return signHS256(t, pubBytes, defaultClaims(id, nil))
		},
		SignAlgNoneToken: func(t *testing.T, id identity.Identity) string {
			t.Helper()
			return signAlgNone(t, defaultClaims(id, nil))
		},
		SignExpiredToken: func(t *testing.T, id identity.Identity) string {
			t.Helper()
			c := defaultClaims(id, nil)
			c["exp"] = conformance.FixedNow.Add(-1 * time.Hour).Unix()
			return signES256(t, priv, c, forkKID)
		},
		Cleanup: stackCleanup,
	}
}

// forkKeySet is this fork's KeySet — a single-key map returning the
// ES256 public key for forkKID.
type forkKeySet struct {
	kid string
	pub crypto.PublicKey
}

func (s *forkKeySet) KeyByID(kid string) (crypto.PublicKey, string, error) {
	if kid != s.kid {
		return nil, "", fmt.Errorf("kid %q not in fork key set", kid)
	}
	return s.pub, "ES256", nil
}

// defaultClaims builds the claim set the suite's tokens carry: a
// well-formed issuer, the identity triple, exp/nbf around FixedNow, and
// the requested scopes.
func defaultClaims(id identity.Identity, scopes []auth.Scope) jwt.MapClaims {
	scopeStrs := make([]string, 0, len(scopes))
	for _, s := range scopes {
		scopeStrs = append(scopeStrs, string(s))
	}
	return jwt.MapClaims{
		"iss":         "https://idp.test",
		"sub":         id.UserID,
		"exp":         conformance.FixedNow.Add(15 * time.Minute).Unix(),
		"nbf":         conformance.FixedNow.Add(-1 * time.Minute).Unix(),
		"tenant":      id.TenantID,
		"user":        id.UserID,
		"session":     id.SessionID,
		"scopes":      scopeStrs,
		"agent_reach": []string{forkAgentID},
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
	tok.Header["kid"] = forkKID
	out, err := tok.SignedString(secret)
	if err != nil {
		t.Fatalf("sign HS256: %v", err)
	}
	return out
}

func signAlgNone(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	tok.Header["kid"] = forkKID
	out, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign alg:none: %v", err)
	}
	return out
}

// testdataRoot resolves the canonical ES256 keypair under
// internal/protocol/auth/testdata from this example's run cwd
// (examples/protocol-clients/conformance-fork/). A fork ships its own
// signing keys; this example reuses Harbor's test keypair so the worked
// harness needs no key generation to run.
func testdataRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "internal", "protocol", "auth", "testdata"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	return root
}

// loadES256Keypair reads the ES256 testdata keypair (EC or PKCS8
// encoding). This mirrors the canonical loader so the worked harness
// resolves the same keys the default factory does.
func loadES256Keypair(t *testing.T, testdataRoot string) (*ecdsa.PrivateKey, *ecdsa.PublicKey) {
	t.Helper()
	priv := readPEMBytes(t, filepath.Join(testdataRoot, "es256_private.pem"))
	pub := readPEMBytes(t, filepath.Join(testdataRoot, "es256_public.pem"))

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
