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
//
// # Phase 65 (D-099) hot-reload deliberately NOT mirrored
//
// The production `harbor dev` hot-reload supervisor
// (`cmd/harbor/cmd_dev_hot_reload.go`) wraps `bootDevStack` — it lives
// at the runDev level, not inside bootDevStack itself. The helper
// mirrors bootDevStack's field-for-field assembly, NOT the surrounding
// supervisor: integration tests that need to exercise the hot-reload
// shape construct their own supervisor against the helper's assembled
// stack (the supervisor's exported constructor takes the boot opts and
// the initial stack — both reproducible here). Per D-094's
// "helper-tracks-production" rule, this is a deliberate scope choice,
// not drift: a hot-reload "helper" that owned the rebuild loop would
// duplicate the cmd-side orchestrator with no test using it. When the
// rebuild orchestrator's shape next changes, both files (this one and
// `cmd/harbor/cmd_dev_hot_reload.go`) are revisited together.
package devstack

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/golang-jwt/jwt/v5"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/devdraft"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/memory"
	memoryinmem "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/flow"
	flowprotocol "github.com/hurtener/Harbor/internal/runtime/flow/protocol"
	"github.com/hurtener/Harbor/internal/runtime/notifications"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	runtimeposture "github.com/hurtener/Harbor/internal/runtime/posture"
	runsprotocol "github.com/hurtener/Harbor/internal/runtime/runs/protocol"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/skills"
	_ "github.com/hurtener/Harbor/internal/skills/drivers/localdb" // §4.4: registers the V1 "localdb" skill driver for tests that open one
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
	tasksprotocol "github.com/hurtener/Harbor/internal/tasks/protocol"
	"github.com/hurtener/Harbor/internal/telemetry"
	"github.com/hurtener/Harbor/internal/tools"
	toolapproval "github.com/hurtener/Harbor/internal/tools/approval"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/builtin"
	toolcatalog "github.com/hurtener/Harbor/internal/tools/catalog"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
	toolsprotocol "github.com/hurtener/Harbor/internal/tools/protocol"
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

	// Logger, when non-nil, is threaded through the auth.Middleware
	// wrapper for the draft handler so the helper's auth-rejection
	// log lines match production exactly (D-094 helper-tracks-
	// production rule; audit W2). When nil, the wrapper omits the
	// MWLogger option — silent rejection in tests is fine.
	Logger *slog.Logger

	// PlannerOverride, when non-nil, replaces the registry-resolved
	// planner concrete the helper would otherwise build from
	// `cfg.Planner` (D-103). Tests that need a stub / scripted /
	// pausing planner pass their own instance here; production code
	// never sets this field (the registry path is the only way to
	// reach a planner concrete in `harbor dev`). The override is
	// applied AFTER the LLM client is built so the same `stack.LLMClient`
	// the registry would have used is still available to the test.
	PlannerOverride planner.Planner

	// Identity overrides the dev-token's identity triple. Empty
	// fields fall back to DefaultDev{Tenant,User,Session}.
	Identity struct {
		Tenant  string
		User    string
		Session string
	}

	// Phase 83f (D-149) — mirror the production cmd_dev.go
	// per-run consumer wiring. The four fields are optional; nil /
	// zero leaves the planner's matching wrapper omitted (matching
	// production's behaviour when an operator did not configure the
	// underlying subsystem).
	//
	// `MemoryStore` is the store the per-task driver calls
	// `GetLLMContext(ctx, q)` against — the test passes a real
	// inmem store keyed to the run's identity.
	// `SkillStore` is the store the driver calls `Search(ctx, q, query, cap)`
	// against — the test passes a real localdb store.
	// `SkillsContextMax` caps the Search result count; zero resolves
	// to the package default (5).
	// `PlanningHints`, when non-nil, projects directly onto
	// `RunContext.PlanningHints` for every run the driver spawns.
	MemoryStore      memory.MemoryStore
	SkillStore       skills.SkillStore
	SkillsContextMax int
	PlanningHints    *planner.PlanningHints

	// TopologyAccessor, when non-nil, is wired into the
	// ControlSurface via protocol.WithTopologyAccessor so the Phase 74
	// `topology.snapshot` method returns a real projection (D-114).
	// Production `harbor dev` hosts no engine-graph (its runtime is
	// planner/RunLoop-shaped), so its ControlSurface leaves the
	// accessor nil; the Phase 74 integration test constructs a real
	// `engine.Engine` and passes it here so the topology surface is
	// exercised end-to-end with real drivers (CLAUDE.md §17.6 — the
	// test fixture wires what the test needs; the production absence
	// is documented, not a bug). Ignored when SkipSteering is set.
	TopologyAccessor protocol.TopologyAccessor

	// ScopeChecker, when non-nil, overrides the ControlSurface's
	// admin-cross-tenant scope predicate (Phase 74 / D-114). The
	// integration test injects a deterministic checker to exercise
	// the cross-tenant admin path without standing up an
	// auth.Middleware. Ignored when SkipSteering is set.
	ScopeChecker protocol.ScopeChecker

	// DraftRoot overrides the on-disk root the Phase 66 / D-100
	// draft Store materialises drafts under. Empty falls back to a
	// per-test temp dir (the helper picks one via testing.TempDir).
	// Tests that want to share a root across multiple Assemble calls
	// (rare) supply the same string twice.
	//
	// Cleanup responsibility (audit W5): when DraftRoot is empty, the
	// helper picks the temp dir AND registers an os.RemoveAll cleanup
	// on stack.Close. When DraftRoot is supplied explicitly, the
	// caller OWNS the directory and is responsible for cleanup — the
	// helper does NOT call os.RemoveAll on an operator-supplied path
	// (it would clobber a caller-managed scratch dir). Use t.TempDir
	// + DraftRoot together if you want both control and auto-cleanup.
	DraftRoot string
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

	// Phase 83g (D-150): the MCP Registry the dev stack populates
	// from cfg.Tools.MCPServers. Nil when SkipCatalog is set or no
	// servers are configured. Integration tests inspect this
	// directly to assert each configured server reached the Registry.
	MCPRegistry *mcpdrv.Registry

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

	// DraftStore is the Phase 66 / D-100 draft scratchpad. Always
	// non-nil after a successful Assemble — the helper mirrors
	// production (D-094 source-of-truth invariant). Tests that
	// exercise the draft surface read DraftStore.Root() for the on-
	// disk path or drive the HTTP handler mounted at
	// devdraft.RoutePrefix.
	DraftStore *devdraft.Store

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
			//nolint:errcheck // test-stack teardown; a Close error is non-actionable and the test is already done
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

	// Phase 56 / 72f: the MetricsRegistry + bus→metrics bridge. The
	// devstack mirrors the production `cmd/harbor` boot path field-for-
	// field (CLAUDE.md §17.6 — the fixture must not diverge from
	// production) so the posture surface's `metrics.snapshot` projects
	// a LIVE counter snapshot, not an empty stub.
	//
	// The reader is an in-process sdkmetric.ManualReader injected via
	// the `WithMetricReader` seam: production resolves the metric
	// exporter through the §4.4 driver registry (the prometheus driver
	// is blank-imported in `cmd/harbor/main.go`), but `harbortest` is a
	// library every integration test imports — requiring each of them
	// to blank-import a driver would be fragile. The ManualReader keeps
	// `devstack.Assemble` self-contained while exercising the SAME
	// MetricsRegistry + bridge + Snapshot code path production runs.
	metricsReg, metricsShutdown, err := telemetry.NewMetricsRegistry(cfg.Telemetry,
		telemetry.WithMetricReader(sdkmetric.NewManualReader()))
	if err != nil {
		return stack, fmt.Errorf("telemetry.NewMetricsRegistry: %w", err)
	}
	stack.closeFns = append(stack.closeFns, metricsShutdown)
	metricsBridgeStop, err := telemetry.BridgeBusToMetrics(context.Background(), bus, metricsReg, events.Filter{Admin: true})
	if err != nil {
		return stack, fmt.Errorf("telemetry.BridgeBusToMetrics: %w", err)
	}
	stack.closeFns = append(stack.closeFns, func(context.Context) error { metricsBridgeStop(); return nil })

	// Phase 72d (D-109): the long-lived `notification.*` Subscriber —
	// mirrors `cmd/harbor/cmd_dev.go::bootDevStack` field-for-field
	// (§17.6 source-of-truth invariant). Without a live Run() the
	// `notification.*` topic has no producer; the goroutine is
	// cancelled + joined on Close.
	notifSubscriber := notifications.NewSubscriber(bus, slog.Default())
	notifCtx, notifCancel := context.WithCancel(context.Background())
	notifDone := make(chan struct{})
	go func() {
		defer close(notifDone)
		if err := notifSubscriber.Run(notifCtx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Default().Warn("devstack: notification subscriber stopped with error",
				slog.String("error", err.Error()))
		}
	}()
	stack.closeFns = append(stack.closeFns, func(context.Context) error {
		notifCancel()
		<-notifDone
		return nil
	})

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
	// llmPostureCfg holds the resolved LLM ConfigSnapshot so the Phase
	// 72g posture surface (wired into the mux below) can project it.
	// Captured here even when the LLM client itself is skipped — the
	// posture surface is a read-only projection and works against a
	// zero snapshot too.
	var llmPostureCfg llm.ConfigSnapshot
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
				// Phase 83l / D-155 — D-094 mirror of the production
				// bug fix in `cmd/harbor/cmd_dev.go::bootDevStack`. The
				// snapshot construction was previously missing these
				// three fields; without them an operator-declared
				// custom provider was silently dropped + the corrections
				// layer ran with stale defaults.
				CustomProviders:    copyCustomProviders(cfg.LLM.CustomProviders),
				NetworkDefaults:    copyNetworkDefaults(cfg.LLM.NetworkDefaults),
				DisableCorrections: disableCorrectionsFromConfig(cfg.LLM.Corrections),
			}
		}
		llmPostureCfg = llmCfg
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
		// Phase 83n / D-153 — mirror cmd_dev.go's built-in tool
		// registration so devstack-driven tests see the same opt-in
		// surface as the production dev binary (D-094 invariant).
		if err := builtin.Register(cat, cfg.Tools.BuiltIn); err != nil {
			return stack, fmt.Errorf("tools/builtin: %w", err)
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

		// Phase 83g (D-150): mirror cmd_dev.go's MCP southbound
		// consumer wiring so devstack-driven integration tests see
		// the same boot-time MCP-server attachment the production
		// dev binary performs. Skip silently when the cfg has no
		// MCP servers configured.
		mcpRegistry := mcpdrv.NewRegistry()
		for _, ms := range cfg.Tools.MCPServers {
			if err := attachDevStackMCPServer(context.Background(), ms, cat, mcpRegistry, bus, opts.Logger, &stack.closeFns); err != nil {
				return stack, fmt.Errorf("mcp[%s]: %w", ms.Name, err)
			}
		}
		stack.MCPRegistry = mcpRegistry
	}

	// Steering + ControlSurface. Skip-aware. The Mux phase below
	// depends on the surface, so SkipSteering implies SkipTransports
	// even if the caller did not set both flags.
	if !opts.SkipSteering {
		steerReg := steering.NewRegistry()
		// Phase 74 (D-114): wire the optional topology accessor + scope
		// checker. Production `harbor dev` passes neither (no engine-
		// graph); the Phase 74 integration test passes a real engine
		// + a deterministic scope checker so the topology.snapshot
		// surface is exercised end-to-end.
		surfaceOpts := []protocol.Option{}
		if opts.TopologyAccessor != nil {
			surfaceOpts = append(surfaceOpts, protocol.WithTopologyAccessor(opts.TopologyAccessor))
			// Wire the bus so a cross-tenant topology.snapshot admin
			// read emits audit.admin_scope_used (RFC §6.13 / D-114).
			surfaceOpts = append(surfaceOpts, protocol.WithEventBus(bus))
		}
		if opts.ScopeChecker != nil {
			surfaceOpts = append(surfaceOpts, protocol.WithScopeChecker(opts.ScopeChecker))
		}
		surface, surfaceErr := protocol.NewControlSurface(taskReg, steerReg, surfaceOpts...)
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
			// D-103 (closes #126) — the planner concrete is resolved via
			// the `internal/planner` driver registry, mirroring the
			// production `cmd/harbor/cmd_dev.go::bootDevStack` path per
			// D-094's helper-tracks-production rule. PlannerOverride
			// lets tests inject a stub / scripted / pausing planner
			// without re-implementing the wiring; production code never
			// sets the override.
			var plnr planner.Planner
			if opts.PlannerOverride != nil {
				plnr = opts.PlannerOverride
			} else {
				plannerCfg := plannerConfigFromConfig(cfg.Planner)
				resolved, plnrErr := planner.Resolve(context.Background(), plannerCfg,
					planner.FactoryDeps{LLM: stack.LLMClient})
				if plnrErr != nil {
					return stack, fmt.Errorf("planner.Resolve: %w", plnrErr)
				}
				plnr = resolved
			}
			rl, rlErr := steering.NewRunLoop(steerReg, stack.Coordinator,
				steering.WithRunLoopBus(bus),
				steering.WithTaskRegistry(taskReg),
				steering.WithApprovalGates(stack.Gates),
			)
			if rlErr != nil {
				return stack, fmt.Errorf("steering.NewRunLoop: %w", rlErr)
			}
			stack.RunLoop = rl

			// Phase 83i (D-152): mirror production's tool dispatch +
			// Catalog projection + Trajectory wiring. The catalog is
			// the SAME stack.Catalog the test (and operator) already
			// populated via PreRegisterTools + MCP attachment;
			// devStackToolExecutor dispatches against it.
			var devExecutor steering.ToolExecutor
			if stack.Catalog != nil {
				devExecutor = devStackToolExecutor{cat: stack.Catalog, logger: opts.Logger}
			}

			driver, drvErr := newDevStackRunLoopDriver(devStackRunLoopDriverOpts{
				bus:     bus,
				runLoop: rl,
				planner: plnr,
				tasks:   taskReg, // D-098: helper mirrors production's FSM bridge (D-094 source-of-truth invariant)
				logger:  opts.Logger,
				// Phase 83f (D-149): mirror production's per-run consumer
				// wiring. The fields are exposed via AssembleOpts so the
				// per-83f integration test populates them with a real
				// MemoryStore + SkillStore.
				memory:           opts.MemoryStore,
				skills:           opts.SkillStore,
				skillsContextMax: opts.SkillsContextMax,
				planningHints:    opts.PlanningHints,
				// Phase 83i (D-152): tool dispatch + Catalog + Trajectory.
				catalog:         stack.Catalog,
				executor:        devExecutor,
				maxStepsRunLoop: cfg.Planner.MaxSteps,
				// Phase 83m (Item 6, D-156): mirror the production
				// granted-scopes plumb-through so the devstack's
				// catalog view sees the same operator-declared scopes.
				grantedScopes: append([]string(nil), cfg.Tools.GrantedScopes...),
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

	// Phase 66 / D-100 — draft-save scaffolding. Constructed before
	// transports so the helper-owned cleanup walks the on-disk
	// scratch dir on Close. The Store itself has no Close (the on-
	// disk dir is operator-owned in production); we register an os.
	// RemoveAll cleanup so per-test temp dirs do not accumulate.
	draftRoot := opts.DraftRoot
	if strings.TrimSpace(draftRoot) == "" {
		tmp, tmpErr := os.MkdirTemp("", "harbortest-devdraft-")
		if tmpErr != nil {
			return stack, fmt.Errorf("devdraft: mkdir temp root: %w", tmpErr)
		}
		draftRoot = tmp
		stack.closeFns = append(stack.closeFns, func(_ context.Context) error {
			return os.RemoveAll(tmp)
		})
	}
	draftStore, dsErr := devdraft.NewStore(devdraft.Options{
		Root: draftRoot,
		Bus:  bus,
	})
	if dsErr != nil {
		return stack, fmt.Errorf("devdraft.NewStore: %w", dsErr)
	}
	stack.DraftStore = draftStore
	// Phase 83m (Item 3, D-156): mirror the production bootDevStack —
	// every constructed subsystem registers its Close. The Store's
	// V1 Close is a no-op but the contract carries forward to any
	// future driver that owns goroutines / persistent handles.
	stack.closeFns = append(stack.closeFns, draftStore.Close)

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
		// Phase 72f / 72g (D-111 / D-112): mirror `bootDevStack` — wire
		// the single posture surface so all seven posture methods route
		// through it. §17.6 source-of-truth invariant: this helper
		// tracks the production boot field-for-field.
		postureSurface, postErr := protocol.NewPostureSurface(protocol.PostureDeps{
			Build: types.RuntimeInfo{
				BuildVersion:   "devstack",
				BuildCommit:    "devstack",
				BuildGoVersion: goruntime.Version(),
			},
			Clock:    time.Now,
			BootedAt: time.Now(),
			Health: func(_ context.Context) []types.SubsystemHealth {
				return runtimeposture.HealthFromConfig(cfg)
			},
			// §17.6 F3: Counters + Metrics wired to live runtime state —
			// the task registry's per-identity running/background counts
			// and the MetricsRegistry's bus-fed counter snapshot. The
			// devstack does not assemble a session registry, so the
			// SessionLister is nil — SessionsActive then reports 0
			// (honest: the fixture runs no sessions), never a fabricated
			// value. This tracks the production boot field-for-field.
			Counters: runtimeposture.CountersProvider(taskReg, nil),
			Drivers: func() []types.SubsystemDriver {
				return runtimeposture.DriversFromConfig(cfg)
			},
			Metrics:     runtimeposture.MetricsProvider(metricsReg, slog.Default()),
			Governance:  governance.NewPostureProvider(governanceConfigForDevstack(cfg.Governance)),
			LLM:         llm.NewPostureProvider(llmPostureCfg),
			Redactor:    stack.Audit,
			Bus:         bus,
			DisplayName: "harbor devstack",
			InstanceID:  "harbor-devstack",
		})
		if postErr != nil {
			return stack, fmt.Errorf("protocol.NewPostureSurface: %w", postErr)
		}
		muxOpts = append(muxOpts, transports.WithPostureSurface(postureSurface))
		// Phase 72e: mount the `pause.list` snapshot route. The
		// devstack mirrors the production `cmd/harbor` boot path
		// (CLAUDE.md §17.6 — the fixture must not diverge from
		// production) — the unified Coordinator + the artifact store +
		// the configured heavy-content threshold are wired so the
		// wave-end E2E exercises the real route.
		if stack.Coordinator != nil && stack.Artifacts != nil {
			muxOpts = append(muxOpts, transports.WithPauseList(
				stack.Coordinator, stack.Artifacts, cfg.Artifacts.HeavyOutputThresholdBytes))
		}
		// Phase 73j (D-118): mount the three `memory.*` read routes for
		// the Console Memory page. The devstack mirrors production
		// (CLAUDE.md §17.6) — the MemoryStore + the artifact store +
		// the heavy-content threshold are wired so the wave-end E2E
		// exercises the real routes.
		if stack.Memory != nil {
			muxOpts = append(muxOpts, transports.WithMemory(stack.Memory, cfg.Memory.Driver))
		}
		// Phase 73f: mount the `tools.*` route family. The devstack
		// mirrors the production `cmd/harbor` boot path (CLAUDE.md
		// §17.6) — the catalog projector is built over the same tool
		// catalog the runtime dispatches against so the wave-end E2E
		// exercises the real route.
		if stack.Catalog != nil {
			toolsProjector, projErr := toolsprotocol.NewCatalogProjector(stack.Catalog)
			if projErr != nil {
				return stack, fmt.Errorf("tools/protocol projector: %w", projErr)
			}
			toolsService, svcErr := toolsprotocol.NewService(toolsProjector,
				toolsprotocol.WithBus(bus),
				toolsprotocol.WithRedactor(stack.Audit),
			)
			if svcErr != nil {
				return stack, fmt.Errorf("tools/protocol service: %w", svcErr)
			}
			muxOpts = append(muxOpts, transports.WithToolsService(toolsService))
		}
		// Phase 73i (D-117): mount the six Console Flows-page routes.
		// The devstack mirrors the production `cmd/harbor` boot path
		// (CLAUDE.md §17.6) — an empty flow.Registry + the real
		// artifact store + the configured heavy-content threshold are
		// wired so the wave-end E2E exercises the real routes.
		if stack.Artifacts != nil && stack.Tasks != nil {
			flowRegistry := flow.NewRegistry()
			flowCatalog, fcErr := flowprotocol.NewRegistryCatalog(
				flowRegistry, stack.Artifacts, cfg.Artifacts.HeavyOutputThresholdBytes)
			if fcErr != nil {
				return stack, fmt.Errorf("flow protocol catalog: %w", fcErr)
			}
			taskReg := stack.Tasks
			flowInvoker, fiErr := flowprotocol.NewFuncInvoker(
				func(launchCtx context.Context, id identity.Identity, flowID string, _ map[string]any) (string, time.Time, error) {
					runCtx, rerr := identity.WithRun(launchCtx, id, "flow-run-"+flowID)
					if rerr != nil {
						return "", time.Time{}, fmt.Errorf("flows.run: identity scope incomplete: %w", rerr)
					}
					handle, serr := taskReg.SpawnTool(runCtx, tasks.SpawnToolRequest{
						Identity:    identity.Quadruple{Identity: id},
						ToolName:    flowID,
						Description: "Console flows.run invocation of " + flowID,
					})
					if serr != nil {
						return "", time.Time{}, fmt.Errorf("flows.run: spawn failed: %w", serr)
					}
					return string(handle.ID), time.Now(), nil
				}, flowRegistry)
			if fiErr != nil {
				return stack, fmt.Errorf("flow protocol invoker: %w", fiErr)
			}
			flowsSurface, fsErr := flowprotocol.NewSurface(flowCatalog, flowInvoker)
			if fsErr != nil {
				return stack, fmt.Errorf("flow protocol surface: %w", fsErr)
			}
			muxOpts = append(muxOpts, transports.WithFlows(flowsSurface))
		}
		// Phase 73d (D-123): mount the two Console Tasks-page read
		// routes. The devstack mirrors the production `cmd/harbor` boot
		// path (CLAUDE.md §17.6) — the registry projector is built over
		// the same TaskRegistry the runtime drives so the wave-end E2E
		// exercises the real routes.
		if stack.Tasks != nil {
			tasksProjector, tpErr := tasksprotocol.NewRegistryProjector(stack.Tasks)
			if tpErr != nil {
				return stack, fmt.Errorf("tasks/protocol projector: %w", tpErr)
			}
			tasksService, tsErr := tasksprotocol.NewService(tasksProjector,
				tasksprotocol.WithBus(bus),
				tasksprotocol.WithRedactor(stack.Audit),
			)
			if tsErr != nil {
				return stack, fmt.Errorf("tasks/protocol service: %w", tsErr)
			}
			muxOpts = append(muxOpts, transports.WithTasksService(tasksService))
		}
		// Phase 73n (D-130): mount the Console Playground-page route
		// (`runs.set_overrides`). The devstack mirrors the production
		// `cmd/harbor` boot path (CLAUDE.md §17.6) — the override Store
		// is the same in-process artifact `cmd/harbor` constructs, so
		// the wave-end E2E exercises the real route.
		runsService, rsErr := runsprotocol.NewService(runsprotocol.NewStore(),
			runsprotocol.WithBus(bus),
			runsprotocol.WithRedactor(stack.Audit),
		)
		if rsErr != nil {
			return stack, fmt.Errorf("runs/protocol service: %w", rsErr)
		}
		muxOpts = append(muxOpts, transports.WithRunsService(runsService))
		mux, muxErr := transports.NewMux(stack.Surface, bus, muxOpts...)
		if muxErr != nil {
			return stack, fmt.Errorf("transports.NewMux: %w", muxErr)
		}
		router := http.NewServeMux()
		router.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			//nolint:errcheck // health-probe response write; a failure is non-actionable
			_, _ = w.Write([]byte(`{"status":"ok","subcommand":"dev"}`))
		})
		router.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			//nolint:errcheck // readiness-probe response write; a failure is non-actionable
			_, _ = w.Write([]byte(`{"status":"ready"}`))
		})
		// Phase 66 / D-100 — mirror production: mount the draft
		// handler at devdraft.RoutePrefix under the same auth
		// middleware as the Protocol mux. The handler is registered
		// BEFORE the /v1/ catch-all so Go's longest-prefix-match
		// routes /v1/dev/drafts/* to the draft handler. The DraftStore
		// is always constructed (the helper carries the same shape
		// production does — D-094 source-of-truth invariant); when
		// SkipAuth is set, the draft handler is mounted bare so tests
		// can inject identity themselves.
		if stack.DraftStore != nil {
			draftHandler, dErr := devdraft.NewHandler(stack.DraftStore, nil)
			if dErr != nil {
				return stack, fmt.Errorf("devdraft.NewHandler: %w", dErr)
			}
			var mounted http.Handler = draftHandler
			if stack.Validator != nil {
				// D-094 mirror: production threads opts.logger via
				// auth.MWLogger so auth-rejection lines show up in
				// operator logs. The helper threads opts.Logger when
				// non-nil; nil is the silent-rejection test default
				// (audit W2).
				var mwOpts []auth.MiddlewareOption
				if opts.Logger != nil {
					mwOpts = append(mwOpts, auth.MWLogger(opts.Logger))
				}
				mounted = auth.Middleware(stack.Validator, mwOpts...)(draftHandler)
			}
			router.Handle(devdraft.RoutePrefix+"/", mounted)
		}
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

// plannerConfigFromConfig mirrors `cmd/harbor/cmd_dev.go`'s
// helper of the same name (D-094 source-of-truth invariant). Maps the
// operator-facing `config.PlannerConfig` onto the registry-facing
// `planner.PlannerConfig` boundary. D-103 (closes issue #126). Empty
// Driver defaults to "react" — the V1 reference planner — so a config
// that omits the planner block boots unchanged from the pre-D-103
// hardcoded path.
func plannerConfigFromConfig(cfg config.PlannerConfig) planner.PlannerConfig {
	driver := cfg.Driver
	if driver == "" {
		driver = "react"
	}
	var extra map[string]string
	if len(cfg.Extra) > 0 {
		extra = make(map[string]string, len(cfg.Extra))
		for k, v := range cfg.Extra {
			extra[k] = v
		}
	}
	return planner.PlannerConfig{
		Driver:   driver,
		MaxSteps: cfg.MaxSteps,
		Extra:    extra,
	}
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
	tasks   tasks.TaskRegistry // D-098: the FSM the driver advances on Run exit
	logger  *slog.Logger       // audit N5: opt-in; matches production's Warn logging when supplied

	// Phase 83f (D-149) per-run consumer wiring — mirrors the
	// production driver's matching fields. Optional; nil = no
	// projection (the planner omits the corresponding wrapper).
	memory           memory.MemoryStore
	skills           skills.SkillStore
	skillsContextMax int
	planningHints    *planner.PlanningHints

	// Phase 83i (D-152) — tool dispatch + Catalog projection.
	catalog         tools.ToolCatalog
	executor        steering.ToolExecutor
	maxStepsRunLoop int

	// Phase 83m (Item 6, D-156) — operator-declared GrantedScopes.
	grantedScopes []string

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
	tasks   tasks.TaskRegistry
	logger  *slog.Logger // optional; when non-nil, Mark* failures log Warn (matches production)

	// Phase 83f (D-149): per-run consumer wiring. See production
	// `perTaskRunLoopDriverOpts` godoc.
	memory           memory.MemoryStore
	skills           skills.SkillStore
	skillsContextMax int
	planningHints    *planner.PlanningHints

	// Phase 83i (D-152): tool dispatch + Catalog projection +
	// Trajectory wiring. Optional; nil catalog ⇒ planner sees no
	// tools, nil executor ⇒ CallTool decisions get appended with no
	// observation. Tests that need full end-to-end pass real values.
	catalog         tools.ToolCatalog
	executor        steering.ToolExecutor
	maxStepsRunLoop int

	// Phase 83m (Item 6, D-156) — operator-declared GrantedScopes.
	grantedScopes []string
}

const devStackRuntimeSkillsContextMaxDefault = 5

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
	if opts.tasks == nil {
		return nil, fmt.Errorf("devstack RunLoop driver: tasks is nil")
	}
	skillsCap := opts.skillsContextMax
	if skillsCap <= 0 {
		skillsCap = devStackRuntimeSkillsContextMaxDefault
	}
	return &DevStackRunLoopDriver{
		bus:              opts.bus,
		runLoop:          opts.runLoop,
		planner:          opts.planner,
		tasks:            opts.tasks,
		logger:           opts.logger,
		memory:           opts.memory,
		skills:           opts.skills,
		skillsContextMax: skillsCap,
		planningHints:    opts.planningHints,
		catalog:          opts.catalog,
		executor:         opts.executor,
		maxStepsRunLoop:  opts.maxStepsRunLoop,
		grantedScopes:    append([]string(nil), opts.grantedScopes...),
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
		d.runOne(q, payload.TaskID)
	}()
}

// runOne mirrors cmd/harbor/cmd_dev_runloop.go::perTaskRunLoopDriver.
// runOne (D-098). The helper is a 1:1 reflection of the production
// bridge per D-094's source-of-truth invariant: integration tests
// must observe the same FSM transitions production observes.
//
// The bridge advances the task FSM Pending → Running → {Complete,
// Failed} based on the RunLoop's exit shape. See the production
// implementation's docstring for the full Reason → Mark* mapping.
// Errors from Mark* are silently dropped here (the helper does not
// hold a slog.Logger; production's bridge logs Warn instead): a Mark*
// failure post-Run is benign for the helper because the test asserts
// on the FSM state directly, not on driver logs.
func (d *DevStackRunLoopDriver) runOne(q identity.Quadruple, taskID tasks.TaskID) {
	taskCtx, idErr := identity.With(d.subCtx, q.Identity)
	if idErr != nil {
		return
	}
	if err := d.tasks.MarkRunning(taskCtx, taskID); err != nil {
		// Pending → Running failed (raced with Cancel, or registry
		// unhealthy). Skip Run — the eventual terminal Mark* would
		// fail too. Match production's logging when a logger was
		// supplied (audit N5; D-094 helper-tracks-production).
		if d.logger != nil {
			d.logger.Warn("devstack runloop: MarkRunning failed",
				slog.String("task_id", string(taskID)),
				slog.String("err", err.Error()))
		}
		return
	}

	// Phase 83f (D-149) — mirror the production runOne's per-run
	// consumer wiring. Same fail-loud semantics: a memory or skills
	// fetch error fails the run with `runtime_fetch_error` and the LLM
	// is never called. The implementation mirrors
	// cmd/harbor/cmd_dev_runloop.go::perTaskRunLoopDriver.runOne.
	task, gErr := d.tasks.Get(taskCtx, taskID)
	if gErr != nil {
		if d.logger != nil {
			d.logger.Warn("devstack runloop: tasks.Get failed",
				slog.String("task_id", string(taskID)),
				slog.String("err", gErr.Error()))
		}
		if mErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
			Code:    "runtime_fetch_error",
			Message: fmt.Sprintf("tasks.Get: %v", gErr),
		}); mErr != nil && d.logger != nil {
			d.logger.Warn("devstack runloop: MarkFailed(runtime_fetch_error) failed",
				slog.String("task_id", string(taskID)),
				slog.String("err", mErr.Error()))
		}
		return
	}

	// Memory + skills are session-scoped (D-149) — see the production
	// driver for the rationale. The fetch quadruple zeroes RunID.
	sessionQ := identity.Quadruple{Identity: q.Identity}
	var memBlocks *planner.MemoryBlocks
	if d.memory != nil {
		patch, mErr := d.memory.GetLLMContext(taskCtx, sessionQ)
		if mErr != nil {
			if d.logger != nil {
				d.logger.Warn("devstack runloop: memory.GetLLMContext failed",
					slog.String("task_id", string(taskID)),
					slog.String("err", mErr.Error()))
			}
			if fErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
				Code:    "runtime_fetch_error",
				Message: fmt.Sprintf("memory.GetLLMContext: %v", mErr),
			}); fErr != nil && d.logger != nil {
				d.logger.Warn("devstack runloop: MarkFailed(runtime_fetch_error) failed",
					slog.String("task_id", string(taskID)),
					slog.String("err", fErr.Error()))
			}
			return
		}
		if mb := devStackProjectMemoryBlocks(patch); mb != nil {
			memBlocks = mb
		}
	}

	var skillsCtx []any
	if d.skills != nil && task.Query != "" {
		// Phase 83m (Item 4, D-156): mirror the production runloop —
		// extract keyword tokens before handing the query to the FTS5-
		// backed skills driver. Falls back to the raw Query if
		// extraction yields nothing useful so Search still has signal.
		searchQuery := devStackExtractSkillKeywords(task.Query)
		if searchQuery == "" {
			searchQuery = task.Query
		}
		ranked, sErr := d.skills.Search(taskCtx, sessionQ, searchQuery, d.skillsContextMax)
		if sErr != nil {
			if d.logger != nil {
				d.logger.Warn("devstack runloop: skills.Search failed",
					slog.String("task_id", string(taskID)),
					slog.String("err", sErr.Error()))
			}
			if fErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
				Code:    "runtime_fetch_error",
				Message: fmt.Sprintf("skills.Search: %v", sErr),
			}); fErr != nil && d.logger != nil {
				d.logger.Warn("devstack runloop: MarkFailed(runtime_fetch_error) failed",
					slog.String("task_id", string(taskID)),
					slog.String("err", fErr.Error()))
			}
			return
		}
		skillsCtx = devStackProjectSkillsContext(ranked)
	}

	counters := &planner.RepairCounters{}

	// Phase 83i (D-152) — mirror the production driver: per-run
	// Trajectory + Catalog view + executor + outer max-steps.
	traj := &planner.Trajectory{Query: task.Query}
	var catalogView planner.ToolCatalogView
	if d.catalog != nil {
		// Phase 83m (Item 6, D-156): mirror the production runloop —
		// the per-run CatalogFilter carries the operator-configured
		// GrantedScopes so AuthScopes-protected tools are gated the
		// same way they are in `cmd/harbor`.
		catalogView = devStackCatalogView{cat: d.catalog, filter: tools.CatalogFilter{
			TenantID:      q.TenantID,
			UserID:        q.UserID,
			SessionID:     q.SessionID,
			GrantedScopes: append([]string(nil), d.grantedScopes...),
		}}
	}

	spec := steering.RunSpec{
		Planner: d.planner,
		Base: planner.RunContext{
			Quadruple:      q,
			Query:          task.Query,
			Goal:           task.Query,
			MemoryBlocks:   memBlocks,
			SkillsContext:  skillsCtx,
			RepairCounters: counters,
			PlanningHints:  d.planningHints,
			Catalog:        catalogView,
			Trajectory:     traj,
		},
		TaskID:       taskID,
		ToolExecutor: d.executor,
		MaxSteps:     d.maxStepsRunLoop,
	}
	fin, err := d.runLoop.Run(d.subCtx, spec)
	if err != nil {
		code := "runloop_error"
		if errors.Is(err, context.Canceled) {
			code = "cancelled"
		}
		if mErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
			Code:    code,
			Message: err.Error(),
		}); mErr != nil {
			d.logger.Warn("devstack runloop: MarkFailed failed",
				slog.String("task_id", string(taskID)),
				slog.String("err", mErr.Error()))
		}
		return
	}
	if fin.Reason == planner.FinishGoal {
		// Phase 83i (D-152) — memory writeback mirror.
		if d.memory != nil {
			turn := memory.ConversationTurn{
				UserMessage:       task.Query,
				AssistantResponse: devStackExtractAssistantAnswer(fin),
				Timestamp:         time.Now(),
			}
			if mErr := d.memory.AddTurn(taskCtx, sessionQ, turn); mErr != nil && d.logger != nil {
				d.logger.Warn("devstack runloop: memory.AddTurn failed; run still marked complete",
					slog.String("task_id", string(taskID)),
					slog.String("err", mErr.Error()))
			}
		}
		if mErr := d.tasks.MarkComplete(taskCtx, taskID, tasks.TaskResult{}); mErr != nil {
			d.logger.Warn("devstack runloop: MarkComplete failed",
				slog.String("task_id", string(taskID)),
				slog.String("err", mErr.Error()))
		}
		return
	}
	if mErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
		Code:    string(fin.Reason),
		Message: "RunLoop finished without satisfying goal: " + string(fin.Reason),
	}); mErr != nil {
		d.logger.Warn("devstack runloop: MarkFailed failed",
			slog.String("task_id", string(taskID)),
			slog.String("err", mErr.Error()))
	}
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

// governanceConfigForDevstack projects the config-package
// GovernanceConfig onto the governance.Config shape the Phase 72g
// posture surface reads. Mirrors `cmd/harbor/cmd_dev.go`'s
// `governanceConfigFromConfig` — duplicated here because that helper
// is unexported and the §17.6 source-of-truth invariant means this
// helper MUST track production field-for-field.
func governanceConfigForDevstack(in config.GovernanceConfig) governance.Config {
	tiers := make(map[string]governance.TierConfig, len(in.IdentityTiers))
	for name, tc := range in.IdentityTiers {
		tiers[name] = governance.TierConfig{
			BudgetCeilingUSD: tc.BudgetCeilingUSD,
			RateLimit: governance.RateLimitConfig{
				Capacity:       tc.RateLimit.Capacity,
				RefillTokens:   tc.RateLimit.RefillTokens,
				RefillInterval: tc.RateLimit.RefillInterval,
			},
			MaxTokens: tc.MaxTokens,
		}
	}
	return governance.Config{
		DefaultTier:   in.DefaultTier,
		IdentityTiers: tiers,
	}
}

// copyCustomProviders mirrors `cmd/harbor/cmd_dev.go::copyCustomProviders`
// (Phase 83l / D-155). Production-bug fix — before 83l the snapshot
// dropped CustomProviders, NetworkDefaults, and Corrections.
func copyCustomProviders(in []config.LLMCustomProviderConfig) []llm.CustomProviderSpec {
	if len(in) == 0 {
		return nil
	}
	out := make([]llm.CustomProviderSpec, 0, len(in))
	for _, p := range in {
		out = append(out, llm.CustomProviderSpec{
			Name:                 p.Name,
			BaseURL:              p.BaseURL,
			APIKeyEnvVar:         p.APIKeyEnvVar,
			Models:               append([]string(nil), p.Models...),
			BaseProviderType:     p.BaseProviderType,
			Timeout:              p.Timeout,
			MaxRetries:           p.MaxRetries,
			RetryBackoffInitial:  p.RetryBackoffInitial,
			RetryBackoffMax:      p.RetryBackoffMax,
			Concurrency:          p.Concurrency,
			BufferSize:           p.BufferSize,
			RequestPathOverrides: copyStringMap(p.RequestPathOverrides),
		})
	}
	return out
}

// copyNetworkDefaults mirrors `cmd/harbor/cmd_dev.go::copyNetworkDefaults`.
func copyNetworkDefaults(in config.LLMNetworkDefaults) llm.NetworkDefaults {
	return llm.NetworkDefaults{
		Timeout:             in.Timeout,
		MaxRetries:          in.MaxRetries,
		RetryBackoffInitial: in.RetryBackoffInitial,
		RetryBackoffMax:     in.RetryBackoffMax,
		Concurrency:         in.Concurrency,
		BufferSize:          in.BufferSize,
	}
}

// disableCorrectionsFromConfig mirrors
// `cmd/harbor/cmd_dev.go::disableCorrectionsFromConfig`.
func disableCorrectionsFromConfig(cfg config.LLMCorrectionsConfig) bool {
	if cfg.Enabled == nil {
		return false
	}
	return !*cfg.Enabled
}

// copyStringMap is the local map-clone helper used by
// copyCustomProviders so we don't depend on `cmd/harbor`'s unexported
// `cloneStringMap`.
func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
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

// devStackProjectMemoryBlocks mirrors the production projection
// helper at cmd/harbor/cmd_dev_runloop.go::projectMemoryBlocks.
// Per Phase 83f (D-149) the projection shape stays identical
// across the production driver and the devstack mirror.
func devStackProjectMemoryBlocks(patch memory.LLMContextPatch) *planner.MemoryBlocks {
	if len(patch.RecentTurns) == 0 && patch.Summary == "" {
		return nil
	}
	recent := make([]map[string]any, 0, len(patch.RecentTurns))
	for _, turn := range patch.RecentTurns {
		recent = append(recent, map[string]any{
			"user":      turn.UserMessage,
			"assistant": turn.AssistantResponse,
		})
	}
	conversation := map[string]any{
		"strategy":     string(patch.Strategy),
		"recent_turns": recent,
	}
	if patch.Summary != "" {
		conversation["summary"] = patch.Summary
	}
	return &planner.MemoryBlocks{Conversation: conversation}
}

// devStackProjectSkillsContext mirrors the production projection
// helper at cmd/harbor/cmd_dev_runloop.go::projectSkillsContext.
func devStackProjectSkillsContext(ranked []skills.RankedSkill) []any {
	if len(ranked) == 0 {
		return nil
	}
	out := make([]any, 0, len(ranked))
	for _, r := range ranked {
		entry := map[string]any{
			"name":  r.Skill.Name,
			"title": r.Skill.Title,
		}
		if r.Skill.Description != "" {
			entry["description"] = r.Skill.Description
		}
		if len(r.Skill.Steps) > 0 {
			entry["steps"] = r.Skill.Steps
		}
		out = append(out, entry)
	}
	return out
}

// attachDevStackMCPServer mirrors cmd/harbor/cmd_dev.go's
// attachDevMCPServer per D-094's source-of-truth invariant. Phase 83g
// (D-150) — boot-time MCP southbound consumer wiring.
//
// Phase 83m (Item 1, D-156): `DefaultIdentity` is the fallback for
// transport-side events only; per-call subscriptions stamp the
// inflight caller's identity via the driver's `pushIdentity(ctx, cfg)`
// helper. Mirror of the production attachDevMCPServer godoc.
func attachDevStackMCPServer(
	ctx context.Context,
	ms config.MCPServerConfig,
	cat tools.ToolCatalog,
	reg *mcpdrv.Registry,
	bus events.EventBus,
	logger *slog.Logger,
	closeFns *[]func(context.Context) error,
) error {
	mode := mcpdrv.MCPTransportMode(ms.TransportMode)
	if mode == "" {
		mode = mcpdrv.TransportAuto
	}
	provider, err := mcpdrv.New(mcpdrv.Config{
		Name:          ms.Name,
		TransportMode: mode,
		URL:           ms.URL,
		Command:       append([]string(nil), ms.Command...),
		Headers:       devStackCloneStringMap(ms.Headers),
		KeepAlive:     ms.KeepAlive,
		Logger:        logger,
		Bus:           bus,
		DefaultIdentity: identity.Identity{
			TenantID:  DefaultDevTenant,
			UserID:    DefaultDevUser,
			SessionID: DefaultDevSession,
		},
	})
	if err != nil {
		return fmt.Errorf("mcp.New: %w", err)
	}
	if connectErr := provider.Connect(ctx); connectErr != nil {
		_ = provider.Close(ctx)
		return fmt.Errorf("provider.Connect: %w", connectErr)
	}
	*closeFns = append(*closeFns, provider.Close)

	descriptors, discoverErr := provider.Discover(ctx)
	if discoverErr != nil {
		return fmt.Errorf("provider.Discover: %w", discoverErr)
	}
	for _, d := range descriptors {
		if regErr := cat.Register(d); regErr != nil {
			return fmt.Errorf("catalog.Register(%q): %w", d.Tool.Name, regErr)
		}
	}
	urlOrCommand := ms.URL
	if urlOrCommand == "" {
		urlOrCommand = strings.Join(ms.Command, " ")
	}
	if regErr := reg.Register(mcpdrv.ServerRegistration{
		Provider:     provider,
		Transport:    string(mode),
		URLOrCommand: urlOrCommand,
		InitialState: mcpdrv.ServerStateOnline,
	}); regErr != nil {
		return fmt.Errorf("registry.Register: %w", regErr)
	}
	if logger != nil {
		logger.Info("devstack: MCP server attached",
			slog.String("name", ms.Name),
			slog.String("transport", string(mode)),
			slog.Int("tools_registered", len(descriptors)),
		)
	}
	return nil
}

func devStackCloneStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// devStackCatalogView mirrors `cmd/harbor/cmd_dev_catalog_view.go`'s
// runtimeCatalogView per D-094 source-of-truth. Phase 83i (D-152).
type devStackCatalogView struct {
	cat    tools.ToolCatalog
	filter tools.CatalogFilter
}

func (v devStackCatalogView) Resolve(name string) (tools.Tool, bool) {
	desc, ok := v.cat.Resolve(name)
	return desc.Tool, ok
}

func (v devStackCatalogView) List() []tools.Tool {
	return v.cat.List(v.filter)
}

// devStackSkillKeywordStopwords mirrors cmd_dev_runloop.go's
// skillKeywordStopwords per D-094 source-of-truth. Phase 83m (Item 4,
// D-156).
var devStackSkillKeywordStopwords = map[string]struct{}{
	"a": {}, "an": {}, "the": {}, "and": {}, "or": {}, "but": {},
	"if": {}, "is": {}, "are": {}, "was": {}, "were": {}, "be": {},
	"been": {}, "being": {}, "have": {}, "has": {}, "had": {},
	"do": {}, "does": {}, "did": {}, "of": {}, "to": {}, "in": {},
	"on": {}, "at": {}, "for": {}, "with": {}, "by": {}, "from": {},
	"as": {}, "into": {}, "that": {}, "this": {}, "it": {}, "i": {},
	"you": {}, "we": {}, "they": {}, "my": {}, "your": {},
}

// devStackMaxSkillKeywords mirrors cmd_dev_runloop.go's
// maxSkillKeywords. Phase 83m (Item 4, D-156).
const devStackMaxSkillKeywords = 10

// devStackExtractSkillKeywords mirrors cmd_dev_runloop.go's
// extractSkillKeywords per D-094. Phase 83m (Item 4, D-156): turns a
// raw task Query into the keyword-shaped string the FTS5 ranker
// performs best on. Returns an empty string for an all-stopword
// pathological input; the caller falls back to the raw Query so
// Search still has SOMETHING to rank against.
func devStackExtractSkillKeywords(query string) string {
	if query == "" {
		return ""
	}
	lower := strings.ToLower(query)
	tokens := strings.FieldsFunc(lower, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	seen := make(map[string]struct{}, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		if len(tok) <= 1 {
			continue
		}
		if _, drop := devStackSkillKeywordStopwords[tok]; drop {
			continue
		}
		if _, dup := seen[tok]; dup {
			continue
		}
		seen[tok] = struct{}{}
		out = append(out, tok)
		if len(out) >= devStackMaxSkillKeywords {
			break
		}
	}
	return strings.Join(out, " ")
}

// devStackExtractAssistantAnswer mirrors cmd_dev_runloop.go's
// extractAssistantAnswer per D-094. Phase 83i (D-152).
func devStackExtractAssistantAnswer(fin planner.Finish) string {
	switch p := fin.Payload.(type) {
	case string:
		return p
	case map[string]any:
		if v, ok := p["answer"]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	if fin.Payload == nil {
		return string(fin.Reason)
	}
	return fmt.Sprintf("%v", fin.Payload)
}

// devStackToolExecutor mirrors cmd/harbor/cmd_dev_executor.go's
// devToolExecutor per D-094 source-of-truth. Phase 83i (D-152).
type devStackToolExecutor struct {
	cat    tools.ToolCatalog
	logger *slog.Logger
}

func (e devStackToolExecutor) ExecuteDecision(ctx context.Context, _ planner.RunContext, decision planner.Decision) (any, any, error) {
	switch d := decision.(type) {
	case planner.CallTool:
		if d.Tool == "" {
			return nil, nil, errors.New("CallTool.Tool is empty")
		}
		desc, ok := e.cat.Resolve(d.Tool)
		if !ok {
			return nil, nil, fmt.Errorf("%w: %q", tools.ErrToolNotFound, d.Tool)
		}
		if desc.Invoke == nil {
			return nil, nil, fmt.Errorf("tool %q is registered without an Invoke function", d.Tool)
		}
		result, err := desc.Invoke(ctx, d.Args)
		if err != nil {
			if e.logger != nil {
				e.logger.Warn("devstack executor: tool invoke failed",
					slog.String("tool", d.Tool),
					slog.String("err", err.Error()))
			}
			return nil, nil, fmt.Errorf("tool %q invoke: %w", d.Tool, err)
		}
		raw := result.Value
		if raw == nil && len(result.Meta) > 0 {
			raw = map[string]any{"meta": result.Meta}
		}
		// V1.1 devstack: llmObservation == raw (the test mirror does
		// not promote heavy results; production cmd_dev_executor.go
		// does D-026 promotion).
		return raw, raw, nil
	default:
		return nil, nil, fmt.Errorf("%w: %T", steering.ErrDecisionShapeUnsupported, decision)
	}
}
