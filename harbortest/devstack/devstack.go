// Package devstack centralises per-test dev-stack assembly.
//
// # Source of truth
//
// This package's `Assemble` function MUST track the production boot
// stack in `cmd/harbor/cmd_dev.go::bootDevStack` field-for-field. When
// the production boot order changes — and it will — this helper
// changes in the same PR. The §17.6 "fix what the integration test
// finds — no matter where the bug lives" rule requires it: if the
// production boot is the source of truth, the helper that pretends
// to be production-shaped MUST stay aligned, or the tests it backs
// silently drift.
//
// # What this package replaces
//
// Before D-094, four integration test files each duplicated ~100–200
// LOC of stack assembly (audit + events + state + tasks + steering +
// protocol + auth + transports + catalog + builder):
//
//   - `test/integration/wave11_test.go::buildWave11Stack`
//   - `test/integration/phase64_harbor_dev_helpers_test.go::buildPhase64TestStack`
//   - `test/integration/phase64a_catalog_wiring_test.go::buildPhase64aEnv`
//   - `test/integration/phase31_approval_gates_test.go::buildPhase31Env`
//
// Each tested a slightly different layer subset. The `AssembleOpts`
// `Skip*` knobs let a caller opt out of layers it does not exercise
// (auth / transports / catalog / steering); everything else is
// always built so the tests prove the layers the production binary
// composes still compose under the helper.
//
// # Real drivers everywhere — no mocks at the seam (CLAUDE.md §17.3)
//
// The helper opens REAL drivers via the registered factories — the
// patterns audit redactor, the inmem events / state / artifacts /
// tasks / memory drivers. The four test files MUST blank-import the
// driver packages so registration fires before Assemble is called;
// see the helper's godoc on `Assemble` for the canonical import
// block.
//
// # Identity propagation
//
// The helper takes NO identity in its signature. Tests construct
// their own (`identity.Quadruple`) and pass them into individual
// calls. Every layer the helper wires reads identity from `ctx` per
// CLAUDE.md §6.
//
// # Concurrent reuse (D-025)
//
// The returned `*DevStack` is shaped like a compiled artifact: every
// field is concurrent-safe under N parallel invocations (the
// underlying drivers' concurrent-reuse tests already gate this).
// `DevStack.Close` is idempotent and safe to defer.
package devstack

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/memory"
	memoryinmem "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
	toolapproval "github.com/hurtener/Harbor/internal/tools/approval"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	toolcatalog "github.com/hurtener/Harbor/internal/tools/catalog"
)

// DefaultDevTenant / DefaultDevUser / DefaultDevSession match the
// `cmd/harbor` package-private dev-token constants. The Assemble
// helper mints a Bearer token under this identity when SkipAuth is
// false; tests that exercise the wire surface use this triple in
// their request bodies + JWT-validation expectations.
const (
	DefaultDevTenant  = "dev"
	DefaultDevUser    = "dev"
	DefaultDevSession = "dev"

	// DefaultKID is the kid header the in-test ES256 signer stamps
	// on tokens. Matches `cmd/harbor`'s DevKID convention.
	DefaultKID = "harbor-test"

	// DefaultTokenTTL pins the validity of minted dev tokens to one
	// hour — short enough that a forgotten token cannot leak past
	// CI run boundaries, long enough that no test will hit refresh.
	DefaultTokenTTL = 1 * time.Hour
)

// AssembleOpts controls which layers the helper builds. The zero
// value builds everything the cfg implies — LLM / memory / artifacts
// / tasks plus auth + transports + catalog + steering.
//
// Each `Skip*` is binary: when set, the corresponding `DevStack`
// field is left nil. Tests assert against the field they exercise.
type AssembleOpts struct {
	// SkipAuth disables Validator construction + dev-token minting.
	// `DevStack.Validator` / `DevStack.Token` are nil. Use for tests
	// that exercise the catalog or in-process invariants and never
	// touch the wire.
	SkipAuth bool

	// SkipTransports disables `transports.NewMux` + the HTTP router.
	// `DevStack.Handler` / `DevStack.Mux` are nil. Implies that the
	// caller never opens an httptest.Server. Always implies the
	// `tools.entries[]` catalog-wiring layer can still fire — the
	// catalog builder does not depend on transports.
	SkipTransports bool

	// SkipCatalog disables `tools.NewCatalog` + the Phase 64a
	// `catalog.Builder` apply path. `DevStack.Catalog` /
	// `DevStack.Coordinator` / `DevStack.Gates` are nil. Use for
	// tests that only need the bus / state / tasks layers.
	SkipCatalog bool

	// SkipSteering disables `steering.NewRegistry` + the
	// ControlSurface. `DevStack.Steering` / `DevStack.Surface` are
	// nil. Implies SkipTransports because a Mux requires a
	// ControlSurface.
	SkipSteering bool

	// SkipRunLoop disables the `steering.RunLoop` construction and
	// the per-task driver that subscribes to `task.spawned` to drive
	// it (D-097, the production wiring that closes #114). When set,
	// `DevStack.RunLoop` / `DevStack.RunLoopDriver` are nil. Tests
	// that don't need the planner-step loop (anything that doesn't
	// drive a `start` request to completion) set this to opt out;
	// `wave11_test.go`'s post-D-097 wire-side approve E2E LEAVES the
	// flag false so the production RunLoop fires.
	//
	// SkipRunLoop implies the in-test bridge for APPROVE/REJECT
	// resolution is no longer needed (the production bridge in
	// `steering.applier.routeThroughGate` fires from the RunLoop's
	// drain), so callers that previously installed
	// `runWave11WireBridge`-shaped goroutines can drop them.
	//
	// SkipRunLoop has no effect when SkipSteering or SkipCatalog is
	// set: the RunLoop requires both the steering Registry and the
	// catalog-applied gates map (the §13 primitive-with-consumer
	// rule applied to the V1 wiring).
	SkipRunLoop bool

	// OAuthProviders pre-populates the OAuth-provider map the
	// catalog Builder consults when an entry declares
	// `tools.entries[].oauth`. Empty by default.
	OAuthProviders map[string]toolauth.OAuthProvider

	// PreRegisterTools is the descriptor list registered with the
	// catalog BEFORE the Builder applies. Use this to register
	// in-test tool fixtures (echo, stub, etc.) that operator config
	// in `cfg.Tools.Entries` then wraps. Ignored when SkipCatalog is
	// true.
	PreRegisterTools []tools.ToolDescriptor

	// LLMConfigSnapshot, when non-nil, overrides the LLM config
	// snapshot the helper would otherwise compute from `cfg.LLM`.
	// Phase 64 / D-089's `HARBOR_DEV_ALLOW_MOCK=1` path drives the
	// production cmd to override `driver` to "mock"; the wave11
	// integration test does the same thing. Pass an explicit
	// snapshot to flip the driver without re-writing the yaml.
	LLMConfigSnapshot *llm.ConfigSnapshot

	// Identity overrides the dev-token's identity triple. Empty
	// fields fall back to DefaultDev{Tenant,User,Session}.
	Identity struct {
		Tenant  string
		User    string
		Session string
	}
}

// DevStack is the bundle Assemble returns. Fields are nil when the
// corresponding layer was skipped via AssembleOpts.
type DevStack struct {
	// Cfg is the *config.Config the caller passed in. Pinned on the
	// stack so tests can read driver-specific knobs without
	// threading the cfg through their own helpers.
	Cfg *config.Config

	// Audit / Bus / State / Artifacts / Tasks are always non-nil
	// after a successful Assemble — they are the runtime's
	// load-bearing core. The Memory / LLMClient fields are only
	// non-nil when the cfg declared a driver for them.
	Audit     audit.Redactor
	Bus       events.EventBus
	State     state.StateStore
	Artifacts artifacts.ArtifactStore
	Tasks     tasks.TaskRegistry
	LLMClient llm.LLMClient
	Memory    memory.MemoryStore

	// Steering / Surface are nil when SkipSteering is set.
	Steering *steering.Registry
	Surface  *protocol.ControlSurface

	// RunLoop / RunLoopDriver are nil when SkipRunLoop is set OR when
	// SkipSteering / SkipCatalog forces the construction to be
	// skipped (the RunLoop needs both the steering Registry and the
	// catalog-applied gates map). Tests that drive a `start` request
	// rely on these — without RunLoop, the spawned task sits at
	// StatusPending forever and the planner never runs.
	RunLoop       *steering.RunLoop
	RunLoopDriver *DevStackRunLoopDriver

	// Catalog / Coordinator / Gates / OAuthProviders are nil when
	// SkipCatalog is set. The Gates map is keyed by tool name and
	// populated by the catalog Builder; tests that drive
	// `gate.ResolveApproval` reach for it.
	Catalog        tools.ToolCatalog
	Coordinator    pauseresume.Coordinator
	Gates          map[string]*toolapproval.ApprovalGate
	OAuthProviders map[string]toolauth.OAuthProvider

	// Validator / SigningKey / KID / Token are nil/empty when
	// SkipAuth is set. The Token is a signed Bearer the caller
	// stamps on outgoing HTTP requests; SigningKey is the matching
	// private key callers use to mint additional tokens (e.g. a
	// bogus token for the failure-mode test).
	Validator  auth.Validator
	SigningKey *ecdsa.PrivateKey
	KID        string
	Token      string

	// Mux / Handler are nil when SkipTransports is set. Handler is
	// the composed mux that exposes /healthz + /readyz + /v1/*; it
	// is the value tests pass to httptest.NewServer.
	Mux     *http.ServeMux
	Handler http.Handler

	// Close runs every subsystem's Close in reverse dependency
	// order. Idempotent: safe to defer; safe to call multiple
	// times.
	Close func()

	// closeFns is the ordered closer slice Close walks in reverse.
	// Exposed only for tests in this package.
	closeFns []func(context.Context) error
}

// devKeySet implements auth.KeySet by mapping the canonical kid
// (DefaultKID) to the in-test ES256 public key. Mirrors the
// `cmd/harbor` package-private `devKeySet` shape so the helper's
// validator construction matches production.
type devKeySet struct {
	kid string
	pub *ecdsa.PublicKey
}

func (k *devKeySet) KeyByID(kid string) (crypto.PublicKey, string, error) {
	if kid != k.kid {
		return nil, "", fmt.Errorf("kid %q not known", kid)
	}
	return k.pub, "ES256", nil
}

// Assemble builds the dev stack the production `harbor dev`
// subcommand boots. See package doc for the source-of-truth
// invariant and the import block tests must blank-import.
//
// The helper is `*testing.T`-flavoured: every failure is a
// `t.Fatalf` so tests don't need to thread error returns. On
// success, the caller defers `stack.Close()` immediately.
//
//	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
//	defer stack.Close()
//
// # Required blank imports
//
// Assemble does NOT itself blank-import driver packages — that would
// surface in production binaries that vendor harbortest. Each test
// file MUST include the driver imports it needs, e.g.:
//
//	import (
//	    _ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
//	    _ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
//	    _ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
//	    _ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
//	    _ "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
//	    _ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
//	    _ "github.com/hurtener/Harbor/internal/llm/mock"
//	)
//
// The helper opens the audit redactor by direct construction (the
// patterns driver is the only V1 redactor; the seam is documented
// future-proofing). All other layers use the factory `Open`.
func Assemble(t *testing.T, cfg *config.Config, opts AssembleOpts) *DevStack {
	t.Helper()
	stack, err := tryAssemble(cfg, opts)
	if err != nil {
		if stack != nil {
			stack.Close()
		}
		t.Fatalf("devstack: %v", err)
	}
	return stack
}

// tryAssemble is the error-returning core of Assemble. Split out from
// the t-flavoured wrapper so:
//
//   - error paths can be unit-tested without faking *testing.T;
//   - the Assemble wrapper has one t.Fatal call site (cleaner audit);
//   - the dependency-order remains visible in one function (matching
//     the production cmd_dev.go::bootDevStack layout).
//
// Returns a partial DevStack on error so the caller's deferred Close
// drains every subsystem that was successfully opened before the
// failure.
func tryAssemble(cfg *config.Config, opts AssembleOpts) (*DevStack, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cfg is required (call config.Load + Validate or build a minimal cfg by hand)")
	}

	stack := &DevStack{
		Cfg:            cfg,
		OAuthProviders: opts.OAuthProviders,
		KID:            DefaultKID,
	}
	stack.Close = func() {
		ctx := context.Background()
		for i := len(stack.closeFns) - 1; i >= 0; i-- {
			_ = stack.closeFns[i](ctx)
		}
		// Idempotency: a second Close walks an empty slice.
		stack.closeFns = nil
	}

	// Audit. The patterns driver is the V1 redactor used by every
	// integration test. Constructed by direct call (audit.Open's
	// factory path is wired but redundant given the single-driver
	// shape at V1).
	stack.Audit = auditpatterns.New()

	// Events. Real inmem driver per CLAUDE.md §17.3 #1.
	bus, err := events.Open(context.Background(), cfg.Events, stack.Audit)
	if err != nil {
		return stack, fmt.Errorf("events.Open: %w", err)
	}
	stack.Bus = bus
	stack.closeFns = append(stack.closeFns, bus.Close)

	// State.
	stateStore, err := state.Open(context.Background(), cfg.State)
	if err != nil {
		return stack, fmt.Errorf("state.Open: %w", err)
	}
	stack.State = stateStore
	stack.closeFns = append(stack.closeFns, stateStore.Close)

	// Artifacts.
	artStore, err := artifacts.Open(context.Background(), cfg.Artifacts)
	if err != nil {
		return stack, fmt.Errorf("artifacts.Open: %w", err)
	}
	stack.Artifacts = artStore
	stack.closeFns = append(stack.closeFns, artStore.Close)

	// LLM. Only opened when the cfg names a driver — phase31 /
	// phase64a tests pass a cfg without an LLM block and the helper
	// skips this layer. The wave11 / phase64 tests pin an explicit
	// snapshot via LLMConfigSnapshot to flip the driver to "mock"
	// without rewriting their yaml.
	if cfg.LLM.Driver != "" || opts.LLMConfigSnapshot != nil {
		var llmCfg llm.ConfigSnapshot
		if opts.LLMConfigSnapshot != nil {
			llmCfg = *opts.LLMConfigSnapshot
		} else {
			llmCfg = llm.ConfigSnapshot{
				Driver:               cfg.LLM.Driver,
				Provider:             cfg.LLM.Provider,
				Model:                cfg.LLM.Model,
				APIKey:               cfg.LLM.APIKey,
				BaseURL:              cfg.LLM.BaseURL,
				Timeout:              cfg.LLM.Timeout,
				ContextWindowReserve: cfg.LLM.ContextWindowReserve,
				HeavyOutputThreshold: cfg.Artifacts.HeavyOutputThresholdBytes,
				ModelProfiles:        copyModelProfiles(cfg.LLM.ModelProfiles),
			}
		}
		llmClient, llmErr := llm.Open(context.Background(), llmCfg, llm.Deps{
			Artifacts: artStore,
			Bus:       bus,
		})
		if llmErr != nil {
			return stack, fmt.Errorf("llm.Open: %w", llmErr)
		}
		stack.LLMClient = llmClient
		stack.closeFns = append(stack.closeFns, llmClient.Close)
	}

	// Memory. Only opened when the cfg names a driver. The
	// rolling_summary strategy needs a real Summarizer (Phase 23 /
	// D-089) — the helper rejects that path with a clear error
	// because the integration tests we target all use strategy=none.
	// A future test that needs rolling_summary against an LLM-backed
	// summarizer can extend this helper or open memory by hand.
	if cfg.Memory.Driver != "" {
		if cfg.Memory.Strategy == "rolling_summary" {
			return stack, fmt.Errorf("memory.strategy=rolling_summary is not wired in the helper; " +
				"open memoryinmem.New directly with Options{Summarizer: ...} if you need it")
		}
		memCfg := memory.ConfigSnapshot{
			Driver:             cfg.Memory.Driver,
			DSN:                cfg.Memory.DSN,
			Strategy:           memory.Strategy(cfg.Memory.Strategy),
			BudgetTokens:       cfg.Memory.BudgetTokens,
			RecoveryBacklogMax: cfg.Memory.RecoveryBacklogMax,
		}
		// Use the inmem driver directly when the cfg picks it so we
		// stay aligned with the bootDevStack split (registry.Open
		// vs direct New). The registry path also works for non-
		// rolling-summary, but the direct path is symmetric with
		// production for the strategy=none case.
		var ms memory.MemoryStore
		var openErr error
		if cfg.Memory.Driver == "inmem" {
			ms, openErr = memoryinmem.New(memCfg, memory.Deps{
				State: stateStore,
				Bus:   bus,
			}, memoryinmem.Options{})
		} else {
			ms, openErr = memory.Open(context.Background(), memCfg, memory.Deps{
				State: stateStore,
				Bus:   bus,
			})
		}
		if openErr != nil {
			return stack, fmt.Errorf("memory.Open: %w", openErr)
		}
		stack.Memory = ms
		stack.closeFns = append(stack.closeFns, ms.Close)
	}

	// Tasks.
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    stateStore,
		Bus:      bus,
		Redactor: stack.Audit,
		Cfg:      cfg.Tasks,
	})
	if err != nil {
		return stack, fmt.Errorf("tasks.Open: %w", err)
	}
	stack.Tasks = taskReg
	stack.closeFns = append(stack.closeFns, taskReg.Close)

	// Catalog + Coordinator + Builder. Skip-aware.
	if !opts.SkipCatalog {
		cat := tools.NewCatalog()
		for _, d := range opts.PreRegisterTools {
			if regErr := cat.Register(d); regErr != nil {
				return stack, fmt.Errorf("PreRegisterTools[%q]: %w", d.Tool.Name, regErr)
			}
		}
		// Wire the bus into the Coordinator so pause.requested /
		// pause.resumed events land on the bus — wire consumers
		// (integration tests, the Console) need the typed Decision
		// marker (D-096) to discriminate approve/reject without
		// parsing free-form Reason strings. Bare pauseresume.New()
		// was the gap that motivated issue #113's audit finding;
		// fix lives at the helper boundary so every test stack
		// inherits the wiring instead of repeating the omission.
		coord := pauseresume.New(pauseresume.WithBus(bus))
		gates := make(map[string]*toolapproval.ApprovalGate)
		providers := opts.OAuthProviders
		if providers == nil {
			providers = make(map[string]toolauth.OAuthProvider)
		}
		// Apply the Builder only when the cfg declares entries —
		// matches `bootDevStack`'s early-return on empty entries
		// (no Builder.Apply call when there's nothing to wire).
		if len(cfg.Tools.Entries) > 0 {
			b := toolcatalog.New(cfg.Tools.Entries, toolcatalog.Deps{
				Catalog:        cat,
				Coordinator:    coord,
				Bus:            bus,
				Redactor:       stack.Audit,
				OAuthProviders: providers,
				AppliedGates:   gates,
			})
			if applyErr := b.Apply(context.Background()); applyErr != nil {
				return stack, fmt.Errorf("catalog.Builder.Apply: %w", applyErr)
			}
			for _, g := range gates {
				gate := g
				stack.closeFns = append(stack.closeFns,
					func(ctx context.Context) error { return gate.Close(ctx) })
			}
		}
		stack.Catalog = cat
		stack.Coordinator = coord
		stack.Gates = gates
		stack.OAuthProviders = providers
	}

	// Steering + ControlSurface. Skip-aware. The Mux phase below
	// depends on the surface, so SkipSteering implies SkipTransports
	// even if the caller did not set both flags.
	if !opts.SkipSteering {
		steerReg := steering.NewRegistry()
		surface, surfaceErr := protocol.NewControlSurface(taskReg, steerReg)
		if surfaceErr != nil {
			return stack, fmt.Errorf("protocol.NewControlSurface: %w", surfaceErr)
		}
		stack.Steering = steerReg
		stack.Surface = surface

		// D-097 — production wiring: a `steering.RunLoop` per spawned
		// task. Mirrors `cmd/harbor/cmd_dev.go::bootDevStack` (the
		// source-of-truth invariant per D-094). Skip-aware: the
		// RunLoop requires both the steering Registry (constructed
		// above) and the catalog-applied gates map (so SkipCatalog
		// also disables the loop), and the caller can opt out via
		// SkipRunLoop.
		if !opts.SkipRunLoop && !opts.SkipCatalog && stack.LLMClient != nil {
			plnr := react.New(stack.LLMClient)
			rl, rlErr := steering.NewRunLoop(steerReg, stack.Coordinator,
				steering.WithRunLoopBus(bus),
				steering.WithTaskRegistry(taskReg),
				steering.WithApprovalGates(stack.Gates),
			)
			if rlErr != nil {
				return stack, fmt.Errorf("steering.NewRunLoop: %w", rlErr)
			}
			stack.RunLoop = rl

			driver, drvErr := newDevStackRunLoopDriver(devStackRunLoopDriverOpts{
				bus:     bus,
				runLoop: rl,
				planner: plnr,
			})
			if drvErr != nil {
				return stack, fmt.Errorf("devstack RunLoop driver: %w", drvErr)
			}
			if startErr := driver.start(context.Background()); startErr != nil {
				return stack, fmt.Errorf("devstack RunLoop driver start: %w", startErr)
			}
			stack.RunLoopDriver = driver
			stack.closeFns = append(stack.closeFns, driver.close)
		}
	}

	// Auth. The dev signer mints an ephemeral ES256 keypair + a
	// Bearer token under the configured identity. Skip-aware.
	if !opts.SkipAuth {
		priv, keyErr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if keyErr != nil {
			return stack, fmt.Errorf("generate key: %w", keyErr)
		}
		keySet := &devKeySet{kid: DefaultKID, pub: &priv.PublicKey}
		validator, vErr := auth.NewValidator(keySet,
			auth.WithRedactor(stack.Audit),
			auth.WithEventBus(bus),
		)
		if vErr != nil {
			return stack, fmt.Errorf("auth.NewValidator: %w", vErr)
		}
		stack.SigningKey = priv
		stack.Validator = validator

		tenant := opts.Identity.Tenant
		if tenant == "" {
			tenant = DefaultDevTenant
		}
		user := opts.Identity.User
		if user == "" {
			user = DefaultDevUser
		}
		session := opts.Identity.Session
		if session == "" {
			session = DefaultDevSession
		}
		token, tErr := signDevToken(priv, tenant, user, session)
		if tErr != nil {
			return stack, fmt.Errorf("sign dev token: %w", tErr)
		}
		stack.Token = token
	}

	// Transports + router. Requires the Surface + Validator (when
	// auth is enabled). SkipTransports OR SkipSteering both leave
	// these nil — a Mux without a Surface is meaningless.
	if !opts.SkipTransports && !opts.SkipSteering {
		muxOpts := []transports.Option{}
		if stack.Validator != nil {
			muxOpts = append(muxOpts, transports.WithValidator(stack.Validator))
		} else {
			// When auth is skipped but transports are not, the
			// caller wants the wire surface without JWT validation
			// (rare — used only by tests that compose their own
			// auth path). The transports package exposes
			// `WithoutValidator` for that explicit opt-out.
			muxOpts = append(muxOpts, transports.WithoutValidator())
		}
		mux, muxErr := transports.NewMux(stack.Surface, bus, muxOpts...)
		if muxErr != nil {
			return stack, fmt.Errorf("transports.NewMux: %w", muxErr)
		}
		router := http.NewServeMux()
		router.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok","subcommand":"dev"}`))
		})
		router.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ready"}`))
		})
		router.Handle("/v1/", mux)
		stack.Mux = router
		stack.Handler = router
	}

	return stack, nil
}

// signDevToken mints an ES256 dev token with the canonical claim
// shape `cmd/harbor`'s dev signer uses: `(iss, sub, aud, exp, nbf,
// iat, tenant, user, session, scopes=[admin, console:fleet])`. The
// kid header is `DefaultKID`.
func signDevToken(priv *ecdsa.PrivateKey, tenant, user, session string) (string, error) {
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss":     "harbor-test",
		"sub":     user,
		"aud":     "harbor",
		"exp":     now.Add(DefaultTokenTTL).Unix(),
		"nbf":     now.Add(-1 * time.Minute).Unix(),
		"iat":     now.Unix(),
		"tenant":  tenant,
		"user":    user,
		"session": session,
		"scopes":  []string{"admin", "console:fleet"},
	})
	tok.Header["kid"] = DefaultKID
	return tok.SignedString(priv)
}

// DevStackRunLoopDriver mirrors `cmd/harbor`'s package-private
// `perTaskRunLoopDriver`. The duplication is intentional per D-094's
// source-of-truth invariant: both ship the same shape (subscribe to
// `task.spawned`, launch a goroutine per spawned foreground task,
// drive the planner via `RunLoop.Run`, drain on Close). When the
// production shape evolves, both move in the same PR.
//
// The driver is exported as a pointer-shaped opaque type — tests
// inspect via the `RunLoop` field rather than reaching into the
// driver's internals.
type DevStackRunLoopDriver struct {
	bus     events.EventBus
	runLoop *steering.RunLoop
	planner planner.Planner

	subCtx     context.Context
	subCancel  context.CancelFunc
	sub        events.Subscription
	subLoopWG  sync.WaitGroup
	runsWG     sync.WaitGroup
	started    bool
	closedOnce sync.Once
}

type devStackRunLoopDriverOpts struct {
	bus     events.EventBus
	runLoop *steering.RunLoop
	planner planner.Planner
}

func newDevStackRunLoopDriver(opts devStackRunLoopDriverOpts) (*DevStackRunLoopDriver, error) {
	if opts.bus == nil {
		return nil, fmt.Errorf("devstack RunLoop driver: bus is nil")
	}
	if opts.runLoop == nil {
		return nil, fmt.Errorf("devstack RunLoop driver: runLoop is nil")
	}
	if opts.planner == nil {
		return nil, fmt.Errorf("devstack RunLoop driver: planner is nil")
	}
	return &DevStackRunLoopDriver{
		bus:     opts.bus,
		runLoop: opts.runLoop,
		planner: opts.planner,
	}, nil
}

func (d *DevStackRunLoopDriver) start(ctx context.Context) error {
	if d.started {
		return nil
	}
	d.subCtx, d.subCancel = context.WithCancel(context.Background())
	sub, err := d.bus.Subscribe(d.subCtx, events.Filter{
		Admin: true,
		Types: []events.EventType{tasks.EventTypeTaskSpawned},
	})
	if err != nil {
		d.subCancel()
		return fmt.Errorf("subscribe(task.spawned): %w", err)
	}
	d.sub = sub
	d.started = true

	// Anchor subCtx to the supplied ctx so a stack teardown that
	// cancels the boot ctx propagates into the driver.
	go func() {
		select {
		case <-ctx.Done():
			d.subCancel()
		case <-d.subCtx.Done():
		}
	}()

	d.subLoopWG.Add(1)
	go d.subscribeLoop()
	return nil
}

func (d *DevStackRunLoopDriver) subscribeLoop() {
	defer d.subLoopWG.Done()
	for ev := range d.sub.Events() {
		d.handleEvent(ev)
	}
}

func (d *DevStackRunLoopDriver) handleEvent(ev events.Event) {
	payload, ok := ev.Payload.(tasks.TaskSpawnedPayload)
	if !ok {
		return
	}
	if payload.Kind != tasks.KindForeground {
		return
	}
	q := identity.Quadruple{
		Identity: ev.Identity.Identity,
		RunID:    string(payload.TaskID),
	}
	if err := identity.Validate(q.Identity); err != nil {
		return
	}
	d.runsWG.Add(1)
	go func() {
		defer d.runsWG.Done()
		spec := steering.RunSpec{
			Planner: d.planner,
			Base: planner.RunContext{
				Quadruple: q,
			},
			TaskID: payload.TaskID,
		}
		_, _ = d.runLoop.Run(d.subCtx, spec)
	}()
}

func (d *DevStackRunLoopDriver) close(_ context.Context) error {
	d.closedOnce.Do(func() {
		if !d.started {
			return
		}
		d.subCancel()
		if d.sub != nil {
			d.sub.Cancel()
		}
		d.subLoopWG.Wait()
		d.runsWG.Wait()
	})
	return nil
}

// copyModelProfiles converts the config-package map shape into the
// llm-package ModelProfile map. Mirrors `cmd/harbor/cmd_dev.go`'s
// helper of the same name — duplicated here because that helper is
// unexported and the source-of-truth invariant means the helper
// MUST track production.
func copyModelProfiles(in map[string]config.LLMModelProfileConfig) map[string]llm.ModelProfile {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]llm.ModelProfile, len(in))
	for name, p := range in {
		mp := llm.ModelProfile{
			ContextWindowTokens: p.ContextWindowTokens,
			TokenEstimator:      p.TokenEstimator,
			JSONSchemaMode:      p.JSONSchemaMode,
			ReasoningEffort:     llm.ReasoningEffort(p.ReasoningEffort),
			MaxRetries:          p.MaxRetries,
		}
		if p.DefaultMaxTokens != nil {
			v := *p.DefaultMaxTokens
			mp.DefaultMaxTokens = &v
		}
		out[name] = mp
	}
	return out
}
