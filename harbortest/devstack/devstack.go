// Package devstack centralises per-test dev-stack assembly.
//
// # Source of truth
//
// Since Phase 110d (D-197) this package's `Assemble` is a THIN wrapper
// over `internal/runtime/assemble.Assemble` — the SAME promoted
// fan-out `cmd/harbor/cmd_dev.go::bootDevStack` wraps. Production ↔
// devstack subsystem-wiring parity therefore holds by construction;
// the pre-110d hand-mirrored copy (the D-094 "MUST track production
// field-for-field" discipline, which drifted anyway — the MCP
// ToolPolicy projection drop, the missing cfg-declared OAuth
// providers) is deleted. What remains per-caller is the test-kit-only
// band: dev auth signer, draft store, transports/mux, and the
// per-task run-loop driver mirror (whose POPULATION helpers are
// shared; the subscriber shell is per-caller).
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
	"encoding/json"
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

	"github.com/golang-jwt/jwt/v5"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/devdraft"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/governance"

	// Production driver aggregator (Phase 110c, D-196) — the single
	// sanctioned home of the driver blank-import block (§4.4).
	// Imported by the kit itself (not left to each test file) so a
	// devstack-assembled stack resolves the SAME driver factories and
	// composes the SAME LLM wrapper chain
	// (corrections/downgrade/retry/governance) as production
	// `cmd/harbor/main.go` (SDK friction audit §7: the kit previously
	// hand-curated a partial list and composed the client without the
	// wrappers — invisible under the mock driver, divergent against
	// live providers). The dev-only mock LLM driver is NOT in the set;
	// tests that need it blank-import `internal/llm/mock` themselves.
	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	_ "github.com/hurtener/Harbor/internal/drivers/prod"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/protocol/transports/cors"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/runtime/flow"
	flowprotocol "github.com/hurtener/Harbor/internal/runtime/flow/protocol"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	runtimeposture "github.com/hurtener/Harbor/internal/runtime/posture"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	runsprotocol "github.com/hurtener/Harbor/internal/runtime/runs/protocol"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/search"
	searchartifacts "github.com/hurtener/Harbor/internal/search/artifacts"
	searchevents "github.com/hurtener/Harbor/internal/search/events"
	searchsessions "github.com/hurtener/Harbor/internal/search/sessions"
	searchtasks "github.com/hurtener/Harbor/internal/search/tasks"
	"github.com/hurtener/Harbor/internal/server"
	"github.com/hurtener/Harbor/internal/sessions"
	sessionsprotocol "github.com/hurtener/Harbor/internal/sessions/protocol"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
	tasksprotocol "github.com/hurtener/Harbor/internal/tasks/protocol"
	"github.com/hurtener/Harbor/internal/telemetry"
	"github.com/hurtener/Harbor/internal/tools"
	toolapproval "github.com/hurtener/Harbor/internal/tools/approval"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
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
	// per-run consumer wiring. The four fields are optional
	// OVERRIDES: a set field wins; an unset field falls back to what
	// the cfg implies, exactly like production (Phase 110c, D-196 —
	// the fallbacks consume the same exported projections cmd does).
	//
	// `MemoryStore` is the store the per-task driver calls
	// `GetLLMContext(ctx, q)` against. Nil falls back to the
	// cfg-opened `DevStack.Memory` (nil when `memory.driver` unset).
	// `SkillStore` is the store the kit's skills Directory browses
	// (Phase 111d — D-201: the Directory is the `<skills_context>`
	// producer, mirroring production). Nil falls back to the
	// cfg-opened store when `skills.driver` is set (via
	// `skills.SnapshotFromConfig`).
	// `SkillsContextMax` caps the injected directory view's length
	// when `skills.directory.max_entries` is unset; zero falls back
	// to `cfg.Planner.SkillsContextMaxResolved()` (default 5,
	// single-sourced at `config.DefaultSkillsContextMax`).
	// `PlanningHints`, when non-nil, projects directly onto
	// `RunContext.PlanningHints` for every run the driver spawns;
	// nil falls back to `planner.HintsFromConfig(cfg.Planner.PlanningHints)`.
	MemoryStore      memory.MemoryStore
	SkillStore       skills.SkillStore
	SkillsContextMax int
	PlanningHints    *planner.PlanningHints

	// TracerOptions is forwarded verbatim to
	// `assemble.Options.TracerOptions` (Phase 111f, D-203). Tests
	// inject `telemetry.WithSpanExporter` with an in-memory recorder
	// so the assembly-started bus→tracer bridge's spans are
	// observable without a collector (the Wave C composed E2E is the
	// first consumer).
	TracerOptions []telemetry.TracerOption

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

	// MCPStdioAllowlist is the fail-closed allowlist of permitted stdio
	// commands (matched on argv[0]) for the admin-driven runtime add of a
	// NEW MCP connection (`agent_config.add_mcp_connection`). Mirrors
	// production `tools.mcp_add_connection.stdio_allowlist`. Empty rejects
	// every stdio add (the secure default); an integration test that drives a
	// real stdio fixture through the add path supplies the fixture binary path
	// here.
	MCPStdioAllowlist []string
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

	// Telemetry / Tracer mirror the assembly\'s canonical structured
	// Logger + OTel tracer (Phase 111f, D-203). Both bridges
	// (bus→metrics, bus→tracer) are started by the assembly and join
	// its closer chain — the kit inherits them as a thin caller.
	Telemetry *telemetry.Logger
	Tracer    *telemetry.Tracer

	// Skills is non-nil when the cfg declared `skills.driver` (opened
	// via `skills.SnapshotFromConfig`, mirroring production cmd_dev —
	// Phase 110c, D-196) or when the caller passed
	// `AssembleOpts.SkillStore` (which always wins).
	Skills skills.SkillStore

	// Steering / Surface are nil when SkipSteering is set.
	Steering *steering.Registry
	Surface  *protocol.ControlSurface

	// Sessions is the StateStore-backed SessionRegistry (D-171). Always
	// non-nil after a successful Assemble — it mirrors the production
	// `cmd/harbor` boot path. The ControlSurface is wired with its
	// create-on-first-use ensurer, and (when transports are mounted) the
	// `sessions.*` Protocol routes project over it. Integration tests use
	// it to assert per-request session create-on-first-use + restart
	// re-discovery via the persistent catalog.
	Sessions *sessions.Registry

	// RunLoop / RunLoopDriver are nil when SkipRunLoop is set OR when
	// SkipSteering / SkipCatalog forces the construction to be
	// skipped (the RunLoop needs both the steering Registry and the
	// catalog-applied gates map). Tests that drive a `start` request
	// rely on these — without RunLoop, the spawned task sits at
	// StatusPending forever and the planner never runs.
	RunLoop       *steering.RunLoop
	RunLoopDriver *DevStackRunLoopDriver

	// RunsOverrideStore is the session-level pending-override Store shared
	// by the mounted `runs.set_overrides` route and the run-loop driver's
	// run-start Consume (Phase 92b, D-232). Exposed so kit consumers can
	// drive a session override in a test (and so the shared-store seam is
	// assertable). Nil when the run loop is skipped.
	RunsOverrideStore *runsprotocol.Store

	// AgentConfig is the agent-config control-plane registry shared by the
	// mounted `agent_config.*` routes and the run-loop driver's run-start
	// skills projection (the agent-config control plane). AgentConfigID is
	// the dev agent's registration id the driver projects against — a test
	// drives the Protocol surface against this id so a skills edit reflects
	// on the next run. Nil/"" when the StateStore is unavailable.
	AgentConfig   agentcfg.Registry
	AgentConfigID string

	// SessionOverlay is the SESSION-scoped safe-subset overlay store (the
	// non-admin lower tier of the authorization matrix) shared by the mounted
	// session-safe `agent_config.session.*` routes and the run-loop driver's
	// run-start composition. Keyed by the real (tenant, user, session) triple,
	// so it is session-isolated. Nil when the StateStore is unavailable.
	SessionOverlay sessionoverlay.Store

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

	// MCPToolContext is the MCP Apps tool-context store the MCP providers
	// capture through and the host reads back for `mcp.apps.tool_context`.
	// Nil when SkipCatalog is set. Mirrors the production cmd/harbor wiring.
	MCPToolContext *mcpconsole.ToolContextStore

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
// subcommand boots. Since Phase 110d (D-197) it is a THIN wrapper
// over the promoted `internal/runtime/assemble` fan-out — the same
// entry point `cmd/harbor/cmd_dev.go::bootDevStack` wraps — plus the
// test-kit-only legs (dev auth signer, draft store, transports/mux,
// the per-task run-loop driver mirror).
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
// None for the production set (Phase 110c, D-196): devstack imports
// the `internal/drivers/prod` aggregator itself, so every production
// driver factory AND the full LLM wrapper chain (corrections /
// downgrade / retry / governance) are seated by construction — the
// same registrations `cmd/harbor/main.go` boots with. The hand-curated
// per-test import list (and the drift it invited — SDK friction audit
// §7) is gone. The ONE driver outside the set is the dev-only mock
// LLM; a test that flips the snapshot to `driver: mock` still adds:
//
//	import _ "github.com/hurtener/Harbor/internal/llm/mock"
//
// (existing per-test driver blank imports remain harmless — Go runs a
// package init exactly once regardless of how many importers).
func Assemble(t *testing.T, cfg *config.Config, opts AssembleOpts) *DevStack {
	t.Helper()
	stack, err := assembleWith(cfg, opts)
	if err != nil {
		if stack != nil {
			stack.Close()
		}
		t.Fatalf("devstack: %v", err)
	}
	return stack
}

// assembleWith is the error-returning core of Assemble. Since Phase
// 110d (D-197) the subsystem fan-out lives in ONE place —
// `internal/runtime/assemble.Assemble` — and this core only maps
// AssembleOpts onto assemble.Options and adds the test-kit legs.
// The pre-110d `tryAssemble` (the hand-mirrored ~450-line copy of
// `bootDevStack`, D-094) is deleted: production↔devstack parity is
// now by construction, not by comment discipline.
//
// Returns a partial DevStack on error so the caller's deferred Close
// drains every subsystem that was successfully opened before the
// failure (the assemble package carries the same contract).
func assembleWith(cfg *config.Config, opts AssembleOpts) (*DevStack, error) {
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

	// The ONE config→stack fan-out (Phase 110d, D-197). The in-process
	// sdkmetric.ManualReader keeps the kit self-contained while
	// exercising the SAME MetricsRegistry + bridge + Snapshot code path
	// production runs (production resolves a metrics exporter through
	// the §4.4 driver registry instead).
	core, err := assemble.Assemble(context.Background(), cfg, assemble.Options{
		Logger:           opts.Logger,
		LLMSnapshot:      opts.LLMConfigSnapshot,
		PlannerOverride:  opts.PlannerOverride,
		SkillStore:       opts.SkillStore,
		OAuthProviders:   opts.OAuthProviders,
		PreRegisterTools: opts.PreRegisterTools,
		MCPDefaultIdentity: identity.Identity{
			TenantID:  DefaultDevTenant,
			UserID:    DefaultDevUser,
			SessionID: DefaultDevSession,
		},
		MetricsOptions: []telemetry.MetricsOption{
			telemetry.WithMetricReader(sdkmetric.NewManualReader()),
		},
		TracerOptions: opts.TracerOptions,
		// Phase 111f (D-203): mirror production gate assembly — the
		// Protocol-side scope adapter over the runtime-vocabulary
		// default, same as cmd/harbor\'s bootDevStack.
		ApprovalAuthorizer: server.NewProtocolScopeAuthorizer(toolapproval.NewIdentityAuthorizer()),
		SkipCatalog:        opts.SkipCatalog,
		SkipSteering:       opts.SkipSteering,
		SkipRunLoop:        opts.SkipRunLoop,
	})
	if core != nil {
		// The assembled core closes as ONE closer (its own chain runs in
		// reverse); the test-kit legs appended below close before it.
		stack.closeFns = append(stack.closeFns, core.Close)
		stack.Audit = core.Redactor
		stack.Telemetry = core.Telemetry
		stack.Tracer = core.Tracer
		stack.Bus = core.Bus
		stack.State = core.State
		stack.Artifacts = core.Artifacts
		stack.Tasks = core.Tasks
		stack.LLMClient = core.LLM
		stack.Memory = core.Memory
		stack.Skills = core.Skills
		stack.Sessions = core.Sessions
		stack.Catalog = core.Catalog
		stack.Coordinator = core.Coordinator
		stack.Gates = core.Gates
		stack.MCPRegistry = core.MCPRegistry
		stack.MCPToolContext = core.MCPToolContext
		stack.Steering = core.Steering
		stack.RunLoop = core.RunLoop
		if core.OAuthProviders != nil {
			stack.OAuthProviders = core.OAuthProviders
		}
	}
	if err != nil {
		return stack, err
	}

	// Locals the test-kit legs below read.
	bus := core.Bus
	taskReg := core.Tasks
	// Phase 92b (D-232) — ONE session-override Store shared by the run-loop
	// driver (CONSUME at run start) and the runs Service (SET via
	// runs.set_overrides), mirroring cmd/harbor's shared runsStore so a
	// set through the mounted route actually reaches the run (D-094; closes
	// the §17.6 cross-surface omission the adversarial pass found).
	runsStore := runsprotocol.NewStore()
	metricsReg := core.Metrics
	llmPostureCfg := core.LLMSnapshot

	// Agent-config control plane (D-094 mirror of cmd/harbor's wiring): the
	// versioned desired-state registry keyed by the dev agent's
	// registration id, reusing the assembled StateStore. The SAME registry
	// is handed to the run-loop driver (run-start skills projection) and the
	// mounted `agent_config.*` Protocol service, so a skills edit lands on
	// the next run. Built whenever the assembly opened a StateStore.
	const devAgentConfigID = "harbor-dev-agent"
	if core.State != nil {
		reg, regErr := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: core.State, Bus: bus})
		if regErr != nil {
			return stack, fmt.Errorf("agent-config registry: %w", regErr)
		}
		stack.AgentConfig = reg
		stack.AgentConfigID = devAgentConfigID
		stack.closeFns = append(stack.closeFns, reg.Close)

		// The SESSION-scoped safe-subset overlay store (the non-admin lower
		// tier) reuses the SAME StateStore for session-keyed identity
		// isolation. Shared with the run-loop driver (run-start composition)
		// and the mounted session-safe `agent_config.session.*` service.
		ovStore, ovErr := sessionoverlay.NewStore(core.State, nil)
		if ovErr != nil {
			return stack, fmt.Errorf("agent-config session-overlay store: %w", ovErr)
		}
		stack.SessionOverlay = ovStore
		stack.closeFns = append(stack.closeFns, ovStore.Close)
	}

	// Steering surface + run-loop driver. Skip-aware: the Mux phase
	// below depends on the surface, so SkipSteering implies
	// SkipTransports even if the caller did not set both flags.
	if !opts.SkipSteering {
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
		// D-171: create-on-first-use ensurer — mirrors production
		// `cmd/harbor/cmd_dev.go::bootDevStack`. A `start` on a not-yet-
		// existing session materialises its registry row.
		surfaceOpts = append(surfaceOpts,
			protocol.WithSessionEnsurer(newSessionEnsurer(core.Sessions)))
		surface, surfaceErr := protocol.NewControlSurface(taskReg, core.Steering, surfaceOpts...)
		if surfaceErr != nil {
			return stack, fmt.Errorf("protocol.NewControlSurface: %w", surfaceErr)
		}
		stack.Surface = surface

		// D-097 — the per-task run-loop driver mirror (the production
		// driver is cmd-private; the kit carries its own per D-094 —
		// the run-loop POPULATION helpers are shared, the subscriber
		// shell is per-caller). Built whenever the assembly produced a
		// RunLoop (planner + catalog + steering all present and the
		// caller did not SkipRunLoop).
		if stack.RunLoop != nil {
			// Phase 111d (D-201): build the Phase-39 skills Directory
			// over the effective SkillStore (AssembleOpts override or
			// the cfg-opened store), mirroring production cmd_dev.go.
			// `skills.directory.max_entries` unset falls back to the
			// resolved skills-context budget (AssembleOpts override or
			// `planner.skills_context_max`).
			var skillsDir *skills.Directory
			if stack.Skills != nil {
				sd, sdErr := skills.NewDirectory(stack.Skills, skills.Deps{Bus: bus},
					skills.DirectoryFromConfig(cfg.Skills, resolveSkillsContextMax(opts, cfg)))
				if sdErr != nil {
					return stack, fmt.Errorf("devstack skills directory: %w", sdErr)
				}
				skillsDir = sd
			}
			// Phase 84b (D-189): decode the operator's
			// `multimodal.disposition` block into the planner-homed
			// policy value (D-094 mirror of cmd_dev.go; fail loud on a
			// non-grammar value — defense-in-depth behind the validator).
			dispositionPolicy, dpErr := planner.DispositionPolicyFromConfig(cfg.Multimodal)
			if dpErr != nil {
				return stack, fmt.Errorf("devstack multimodal disposition policy: %w", dpErr)
			}
			driver, drvErr := newDevStackRunLoopDriver(devStackRunLoopDriverOpts{
				bus:     bus,
				runLoop: stack.RunLoop,
				planner: core.Planner,
				tasks:   taskReg, // D-098: helper mirrors production's FSM bridge (D-094 source-of-truth invariant)
				logger:  opts.Logger,
				// Phase 92b (D-232) — the session-override Store the driver
				// Consumes at run start; the SAME instance is handed to the
				// runs Service below so a runs.set_overrides reaches the run
				// (D-094 mirror of cmd_dev.go's shared runsStore).
				sessionOverrides: runsStore,
				// Phase 83f (D-149): per-run consumer wiring. Explicit
				// AssembleOpts overrides win; otherwise the cfg-opened
				// stores + the SAME exported projections production uses
				// (Phase 110c, D-196).
				memory:       resolveMemoryStore(opts, stack),
				memoryRecall: memory.RecallFromConfig(cfg.Memory),
				// Phase 111d (D-201): the Phase-39 Directory is the
				// `<skills_context>` producer (mirrors production).
				skillsDirectory: skillsDir,
				planningHints:   resolvePlanningHints(opts, cfg),
				// Phase 83i (D-152) / 110a (D-194) / 110d (D-197): the
				// catalog + executor are the assembly's — the ONE
				// promoted dispatch executor production wires.
				catalog:         stack.Catalog,
				executor:        core.Executor,
				maxStepsRunLoop: cfg.Planner.MaxSteps,
				// Phase 83m (Item 6, D-156): operator-declared scopes.
				grantedScopes: append([]string(nil), cfg.Tools.GrantedScopes...),
				// Round-7 F11 / D-166 — multimodal input materializer.
				artifactStore: stack.Artifacts,
				// Phase 111e (D-202) — trajectory compression: the
				// assembly-built runner + the operator's token budget
				// (D-094 mirror of cmd_dev.go's projection).
				tokenBudget: cfg.Planner.TokenBudget,
				compression: core.Compression,
				// Phase 84b (D-189) — the per-agent attachment
				// disposition policy (D-094 mirror of cmd_dev.go's
				// projection).
				dispositionPolicy: dispositionPolicy,
				// agent-config registry — run-start skills projection
				// (D-094 mirror of cmd_dev.go).
				agentConfig:   stack.AgentConfig,
				agentConfigID: stack.AgentConfigID,
				// session-scoped safe-subset overlay — run-start composition
				// (D-094 mirror of cmd_dev.go).
				sessionOverlay: stack.SessionOverlay,
			})
			if drvErr != nil {
				return stack, fmt.Errorf("devstack RunLoop driver: %w", drvErr)
			}
			if startErr := driver.start(context.Background()); startErr != nil {
				return stack, fmt.Errorf("devstack RunLoop driver start: %w", startErr)
			}
			stack.RunLoopDriver = driver
			stack.RunsOverrideStore = runsStore
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
		// The session-erasure cascade (`sessions.delete`) is wired
		// only when every scoped store the cascade deletes is present (the
		// SessionRegistry + State + Memory + Artifacts). The same condition
		// drives the eraser wiring below and the CapSessionLifecycle
		// advertisement here, so the capability is honest about the route.
		sessionLifecycleAvailable := stack.Sessions != nil && stack.State != nil &&
			stack.Memory != nil && stack.Artifacts != nil
		// Phase 72f / 72g (D-111 / D-112): mirror `bootDevStack` — wire
		// the single posture surface so all seven posture methods route
		// through it. Governance identity-tier ENFORCEMENT is wired by
		// the shared assembly (Phase 111a, D-198); the posture provider
		// below is the read-only projection of the same config.
		postureSurface, postErr := protocol.NewPostureSurface(protocol.PostureDeps{
			Build: types.RuntimeInfo{
				BuildVersion:   "devstack",
				BuildCommit:    "devstack",
				BuildGoVersion: goruntime.Version(),
				// The host's renderable MCP App display modes — the Console
				// reads these off runtime.info to seed the `ui/initialize`
				// host-context `availableDisplayModes`.
				MCPAppDisplayModes: cfg.Tools.MCPAppHostDisplayModes(),
			},
			Clock:    time.Now,
			BootedAt: time.Now(),
			Health: func(_ context.Context) []types.SubsystemHealth {
				return runtimeposture.HealthFromConfig(cfg)
			},
			// §17.6 F3: Counters + Metrics wired to live runtime state —
			// the task registry's per-identity running/background counts,
			// the assembly's SessionRegistry (Phase 110d closes the stale
			// nil-SessionLister drift — the kit HAS assembled a session
			// registry since D-171), and the MetricsRegistry's bus-fed
			// counter snapshot. Tracks the production boot field-for-field.
			Counters: runtimeposture.CountersProvider(taskReg, stack.Sessions, stack.MCPRegistry),
			Drivers: func() []types.SubsystemDriver {
				return runtimeposture.DriversFromConfig(cfg)
			},
			Metrics:     runtimeposture.MetricsProvider(metricsReg, slog.Default()),
			Governance:  governance.NewPostureProvider(governance.ConfigFromOperator(cfg.Governance)),
			LLM:         llm.NewPostureProvider(llmPostureCfg),
			Redactor:    stack.Audit,
			Bus:         bus,
			DisplayName: "harbor devstack",
			InstanceID:  "harbor-devstack",
			// Round-8 F1 / phase 84a — D-094 mirror. The devstack is
			// planner/RunLoop-shaped (same as the production `harbor
			// dev` boot); no engine-graph topology accessor is wired.
			TopologyAvailable: false,
			// The agent-config control plane is mounted below only when
			// stack.AgentConfig != nil; advertise `agent_config` from the
			// SAME boolean so the capability can never claim an absent
			// surface (the agentConfigService variable does not exist yet
			// at this construction point).
			AgentConfigAvailable: stack.AgentConfig != nil,
			// Advertise session_lifecycle iff the `sessions.delete` eraser
			// is wired below.
			SessionLifecycleAvailable: sessionLifecycleAvailable,
		})
		if postErr != nil {
			return stack, fmt.Errorf("protocol.NewPostureSurface: %w", postErr)
		}
		muxOpts = append(muxOpts, transports.WithPostureSurface(postureSurface))

		// Phase 83w F6 (D-164): mount the twelve `mcp.servers.*` methods
		// so the Console MCP Connections page renders live data. The
		// devstack mirrors the production `cmd/harbor` boot path
		// (CLAUDE.md §17.6 / §17.6 source-of-truth invariant — the
		// fixture must not diverge from production). The V1 dev posture
		// uses NoOAuthAccessor since dev typically attaches MCP without
		// OAuth — the OAuth-binding verbs fail loud per CLAUDE.md §13
		// while the read-only methods (list / get / resources / prompts /
		// health) — the surface the Connections page leans on — serve
		// real data.
		if stack.MCPRegistry != nil {
			mcpRegAccessor, mraErr := mcpconsole.NewRegistryAccessor(stack.MCPRegistry)
			if mraErr != nil {
				return stack, fmt.Errorf("mcp accessor: %w", mraErr)
			}
			mcpSurface, msErr := protocol.NewMCPSurface(protocol.MCPDeps{
				MCP:      mcpRegAccessor,
				OAuth:    mcpconsole.NewNoOAuthAccessor(),
				Redactor: stack.Audit,
				Bus:      bus,
			})
			if msErr != nil {
				return stack, fmt.Errorf("mcp surface: %w", msErr)
			}
			muxOpts = append(muxOpts, transports.WithMCPSurface(mcpSurface))
		}

		// Mount the MCP Apps host surface (`mcp.servers.read_resource`
		// + the `mcp.apps.call_tool` proxy) for parity with the
		// production cmd/harbor boot path (CLAUDE.md §17.6 — the fixture
		// must not diverge from production). Without this a test routed
		// through the devstack mux gets 404 on the Apps methods while
		// `harbor dev` serves them.
		//
		// Gated on the MCP/catalog band being present (the same `MCPRegistry`
		// signal the MCP-surface block above uses) — under `SkipCatalog` the
		// whole band is nil and there is nothing to mount. But WHEN the band IS
		// present, the accessor is constructed FAIL-LOUD (matching cmd_dev):
		// the catalog band always builds the catalog + artifact store +
		// tool-context store alongside the registry, so a nil sub-dep here is a
		// real wiring regression, not a config choice — a per-dep silent guard
		// would mask it and let the wave-end E2E pass against a fixture that
		// diverges from production (CLAUDE.md §13 / §17.6).
		if stack.MCPRegistry != nil {
			appsAccessor, aaErr := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
				Registry:    stack.MCPRegistry,
				Catalog:     stack.Catalog,
				Store:       stack.Artifacts,
				Bus:         bus,
				ToolContext: stack.MCPToolContext,
				// Mirror of cmd_dev.go: the app→host exposure gate reads CURRENT
				// agent-config desired state (the planner-snapshot /
				// app-call-current asymmetry), UNIONing the session overlay's
				// narrow-only disables. Inert when no registry is wired.
				AgentConfig:    stack.AgentConfig,
				AgentID:        stack.AgentConfigID,
				SessionOverlay: stack.SessionOverlay,
				Threshold:      cfg.Artifacts.HeavyOutputThresholdBytes,
			})
			if aaErr != nil {
				return stack, fmt.Errorf("mcp apps accessor: %w", aaErr)
			}
			appsSurface, asErr := protocol.NewAppsSurface(protocol.AppsDeps{
				Resource:    appsAccessor,
				Invoker:     appsAccessor,
				ToolContext: appsAccessor,
			})
			if asErr != nil {
				return stack, fmt.Errorf("mcp apps surface: %w", asErr)
			}
			muxOpts = append(muxOpts, transports.WithAppsSurface(appsSurface))
		}

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
		// Phase 125: mount the `state.history` windowed event-replay
		// route. The devstack mirrors the production `cmd/harbor` boot
		// path (CLAUDE.md §17.6) — the durable bus + the artifact store
		// are the same instances the runtime publishes/stores against,
		// so the wave-end E2E exercises the real windowed read.
		if stack.Artifacts != nil {
			muxOpts = append(muxOpts, transports.WithStateHistory(bus, stack.Artifacts))
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
			// Phase 107a parity (D-195 dated-note follow-up): mirror the
			// production `cmd/harbor` boot path — the Enricher projects the
			// run-loop driver's per-task trajectory map onto `tasks.get`
			// reads. Only wired when the assembly produced a driver
			// (SkipRunLoop / no-planner stacks read un-enriched tasks,
			// same as a production boot without a run loop).
			var projectorOpts []tasksprotocol.RegistryProjectorOption
			if stack.RunLoopDriver != nil {
				projectorOpts = append(projectorOpts,
					tasksprotocol.WithEnricher(&devStackEnricher{
						trajectoryFn: stack.RunLoopDriver.TrajectoryByTaskID,
					}))
			}
			tasksProjector, tpErr := tasksprotocol.NewRegistryProjector(stack.Tasks, projectorOpts...)
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
		// D-171: mount the two `sessions.*` Console routes over the
		// SessionRegistry so an integration test exercises the real
		// sessions.list / sessions.inspect path (create-on-first-use,
		// listing, restart re-discovery). Mirrors production
		// `cmd/harbor/cmd_dev.go::bootDevStack`.
		if stack.Sessions != nil {
			sessionsProjector, spErr := sessionsprotocol.NewListerProjector(stack.Sessions)
			if spErr != nil {
				return stack, fmt.Errorf("sessions/protocol projector: %w", spErr)
			}
			sessionsOpts := []sessionsprotocol.Option{
				sessionsprotocol.WithBus(bus),
				sessionsprotocol.WithRedactor(stack.Audit),
			}
			// Wire the session-erasure cascade (`sessions.delete`)
			// over the real scoped stores when all are present, so an
			// integration test exercises the full three-store erasure path.
			// Mirrors production `cmd/harbor/cmd_dev.go::bootDevStack`.
			if sessionLifecycleAvailable {
				eraser, eErr := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
					Registry:  stack.Sessions,
					State:     stack.State,
					Memory:    stack.Memory,
					Artifacts: stack.Artifacts,
					Bus:       bus,
					Redactor:  stack.Audit,
				})
				if eErr != nil {
					return stack, fmt.Errorf("sessions/protocol eraser: %w", eErr)
				}
				sessionsOpts = append(sessionsOpts, sessionsprotocol.WithEraser(eraser))
			}
			sessionsService, ssErr := sessionsprotocol.NewService(sessionsProjector, sessionsOpts...)
			if ssErr != nil {
				return stack, fmt.Errorf("sessions/protocol service: %w", ssErr)
			}
			muxOpts = append(muxOpts, transports.WithSessionsService(sessionsService))
		}
		// Phase 72c (D-108) + Phase 73l (D-120) parity: mount the five
		// `search.*` methods and the artifacts surface. Production
		// `cmd/harbor/cmd_dev.go::bootDevStack` wires both at mux
		// construction; the devstack previously omitted them — a
		// tests-track-production gap the §17.5 Protocol-track audit's
		// Auth-column probe surfaced (CLAUDE.md §17.6).
		if stack.Artifacts != nil {
			artDriverName := cfg.Artifacts.Driver
			if artDriverName == "" {
				artDriverName = "inmem"
			}
			artifactsSurface, asErr := protocol.NewArtifactsSurface(protocol.ArtifactsDeps{
				Store:        stack.Artifacts,
				Redactor:     stack.Audit,
				Bus:          bus,
				Clock:        time.Now,
				DriverName:   artDriverName,
				MaxBodyBytes: cfg.Protocol.ResolvedMaxRequestBytes(),
			})
			if asErr != nil {
				return stack, fmt.Errorf("protocol artifacts surface: %w", asErr)
			}
			muxOpts = append(muxOpts, transports.WithArtifactsSurface(artifactsSurface))
		}
		if stack.Sessions != nil && stack.Tasks != nil && stack.Artifacts != nil {
			searchDeps := search.Deps{Redactor: stack.Audit, AdminScope: server.SearchAdminScopeFromAuth}
			searchSessions, seErr := searchsessions.New(stack.Sessions, searchDeps)
			if seErr != nil {
				return stack, fmt.Errorf("search sessions: %w", seErr)
			}
			searchTasks, seErr := searchtasks.New(stack.Sessions, stack.Tasks, searchDeps)
			if seErr != nil {
				return stack, fmt.Errorf("search tasks: %w", seErr)
			}
			searchArtifacts, seErr := searchartifacts.New(stack.Artifacts, searchDeps)
			if seErr != nil {
				return stack, fmt.Errorf("search artifacts: %w", seErr)
			}
			searchers := []search.Searcher{searchSessions, searchTasks, searchArtifacts}
			if replayer, ok := bus.(events.Replayer); ok {
				searchEvents, seErr2 := searchevents.New(replayer, searchDeps)
				if seErr2 != nil {
					return stack, fmt.Errorf("search events: %w", seErr2)
				}
				searchers = append(searchers, searchEvents)
			}
			searchRegistry, srErr := search.NewRegistry(searchers...)
			if srErr != nil {
				return stack, fmt.Errorf("search registry: %w", srErr)
			}
			searchSurface, ssErr := protocol.NewSearchSurface(searchRegistry, server.SearchAdminScopeFromAuth)
			if ssErr != nil {
				return stack, fmt.Errorf("search surface: %w", ssErr)
			}
			muxOpts = append(muxOpts, transports.WithSearch(searchSurface))
		}
		// Phase 73n (D-130) / 92b (D-232): mount the Console Playground-page
		// route (`runs.set_overrides`) over the SAME `runsStore` the
		// run-loop driver Consumes from (created at function scope above) —
		// so a recorded session override actually reaches the next run, not
		// a void. WithValidModels rejects an unknown session model swap at
		// set time (fail loud, mirroring the tenant layer + cmd/harbor).
		runsService, rsErr := runsprotocol.NewService(runsStore,
			runsprotocol.WithBus(bus),
			runsprotocol.WithRedactor(stack.Audit),
			runsprotocol.WithValidModels(devstackValidModels(cfg)),
		)
		if rsErr != nil {
			return stack, fmt.Errorf("runs/protocol service: %w", rsErr)
		}
		muxOpts = append(muxOpts, transports.WithRunsService(runsService))
		// Mount the admin-scoped agent-config control-plane routes
		// (`POST /v1/agent_config/*`) over the SAME registry the run-loop
		// driver projects at run start (D-094 mirror of cmd/harbor) — so a
		// skills edit through the mounted route reaches the next run.
		if stack.AgentConfig != nil {
			agentConfigOpts := []agentcfgprotocol.Option{
				agentcfgprotocol.WithSkillStore(stack.Skills),
				agentcfgprotocol.WithBus(bus),
				agentcfgprotocol.WithCoordinator(stack.Coordinator),
				agentcfgprotocol.WithStdioAllowlist(append([]string(nil), opts.MCPStdioAllowlist...)),
				// session-safe lower tier (non-admin): the overlay store backs
				// the `agent_config.session.*` verbs (D-094 mirror of cmd/harbor).
				agentcfgprotocol.WithSessionOverlay(stack.SessionOverlay),
				// the configured ModelProfiles gate set_llm_params (D-094 mirror
				// of cmd/harbor): a per-agent model pin is validated at set time.
				agentcfgprotocol.WithValidModels(devstackValidModels(cfg)),
			}
			// the runtime MCP-attach concrete (D-094 mirror of cmd/harbor's
			// devMCPConnectionAttacher) drives the real dial → initialize →
			// discover → register lifecycle for an admin add of a NEW MCP
			// connection against the LIVE catalog + registry + bus. Built only
			// when the catalog band is present.
			if stack.Catalog != nil && stack.MCPRegistry != nil {
				attacher := NewMCPConnectionAttacher(stack.Catalog, stack.MCPRegistry, bus, nil,
					resolveDevIdentity(opts), stack.OAuthProviders)
				stack.closeFns = append(stack.closeFns, attacher.Close)
				agentConfigOpts = append(agentConfigOpts, agentcfgprotocol.WithConnectionAttacher(attacher))
			}
			agentConfigService, acErr := agentcfgprotocol.NewService(stack.AgentConfig, agentConfigOpts...)
			if acErr != nil {
				return stack, fmt.Errorf("agent-config/protocol service: %w", acErr)
			}
			muxOpts = append(muxOpts, transports.WithAgentConfigService(agentConfigService))
		}
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
		// Phase 111b (D-199) — mirror production: mount the tool-OAuth
		// callback endpoint over the SAME Stack.OAuthProviders the
		// assembly's catalog band produced (thin-caller parity per
		// 110d / D-197). Mounted WITHOUT auth middleware by design —
		// the provider redirect carries no Harbor JWT; the one-time
		// `state` nonce is the capability and the handler restores
		// identity from the provider's own flow record. Registered
		// BEFORE the /v1/ catch-all so the exact match wins.
		var cbOpts []toolauth.CallbackOption
		if opts.Logger != nil {
			cbOpts = append(cbOpts, toolauth.WithCallbackLogger(opts.Logger))
		}
		router.Handle(toolauth.CallbackRoutePattern,
			toolauth.CallbackHandler(stack.OAuthProviders, cbOpts...))
		router.Handle("/v1/", mux)
		stack.Mux = router
		// Phase 83v (D-162) — CORS middleware. D-094 source-of-truth
		// invariant: devstack tracks the production `bootDevStack` field-
		// for-field. The middleware wraps the WHOLE router so every
		// surface is reachable cross-origin from an allowed Console origin
		// (or any origin when CORSDevAllowAny is set). Empty allowlist +
		// CORSDevAllowAny=false is the default-deny posture — the
		// middleware passes through with no CORS headers (the pre-83v
		// behavior).
		stack.Handler = cors.Wrap(router, cors.Config{
			AllowedOrigins: append([]string(nil), cfg.Server.AllowedOrigins...),
			DevAllowAny:    cfg.Server.CORSDevAllowAny,
		})
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

// resolveMemoryStore returns the per-task driver's MemoryStore: an
// explicit AssembleOpts override wins; otherwise the cfg-opened store
// (mirroring production, which threads its cfg-opened store — Phase
// 110c, D-196).
func resolveMemoryStore(opts AssembleOpts, stack *DevStack) memory.MemoryStore {
	if opts.MemoryStore != nil {
		return opts.MemoryStore
	}
	return stack.Memory
}

// resolveSkillsContextMax returns the per-task driver's skills cap: an
// explicit positive AssembleOpts override wins; otherwise the cfg's
// resolved value (`config.SkillsContextMaxResolved`, single-sourced
// default — Phase 110c, D-196).
func resolveSkillsContextMax(opts AssembleOpts, cfg *config.Config) int {
	if opts.SkillsContextMax > 0 {
		return opts.SkillsContextMax
	}
	return cfg.Planner.SkillsContextMaxResolved()
}

// devstackValidModels returns the configured model names (the keys of
// `cfg.LLM.ModelProfiles`) the runs Service validates a session model swap
// against at set time — the D-094 mirror of cmd/harbor's validModels.
func devstackValidModels(cfg *config.Config) []string {
	models := make([]string, 0, len(cfg.LLM.ModelProfiles))
	for m := range cfg.LLM.ModelProfiles {
		models = append(models, m)
	}
	return models
}

// resolvePlanningHints returns the per-task driver's planning hints:
// an explicit AssembleOpts override wins; otherwise the cfg's YAML
// block via the SAME exported projection production calls
// (`planner.HintsFromConfig` — Phase 110c, D-196).
func resolvePlanningHints(opts AssembleOpts, cfg *config.Config) *planner.PlanningHints {
	if opts.PlanningHints != nil {
		return opts.PlanningHints
	}
	return planner.HintsFromConfig(cfg.Planner.PlanningHints)
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
	// Phase 111d (D-201): the skills surface is the Phase-39
	// Directory (the `<skills_context>` producer), mirroring
	// production's swap off raw SkillStore.Search.
	memory          memory.MemoryStore
	memoryRecall    memory.RecallSettings
	skillsDirectory *skills.Directory
	planningHints   *planner.PlanningHints

	// Phase 83i (D-152) — tool dispatch + Catalog projection.
	catalog         tools.ToolCatalog
	executor        steering.ToolExecutor
	maxStepsRunLoop int

	// Phase 83m (Item 6, D-156) — operator-declared GrantedScopes.
	grantedScopes []string

	// Round-7 F11 / D-166 — artifact store for multimodal materializer.
	artifactStore artifacts.ArtifactStore

	// Phase 111e (D-202) — trajectory compression projection, the
	// D-094 mirror of the production driver's fields.
	tokenBudget int
	compression *planner.CompressionRunner

	// Phase 84b (D-189) — per-agent attachment disposition policy
	// (D-094 mirror of the production driver's field).
	dispositionPolicy planner.DispositionPolicy

	// admin-set tenant-default LLM override resolver — the D-094 mirror
	// of the production driver's field. OPTIONAL (nil = no overrides);
	// the kit does not construct a governance policy by default, so this
	// stays nil and the run uses agent/config defaults, but the SHAPE
	// tracks production so the mirror does not drift.
	tenantOverrides devStackTenantOverrideResolver

	// session-level pending-override Store — Consumed (one-shot) at run
	// start and composed OVER the tenant default (session › tenant ›
	// config), the D-094 mirror of the production driver. It is the SAME
	// Store the devstack's runs Service writes into, so a
	// runs.set_overrides through the mounted route reaches the run.
	sessionOverrides *runsprotocol.Store

	// agent-config registry + the agent's registration id, read once at run
	// start to project the active skills-set (D-094 mirror of the
	// production driver). Nil = no projection.
	agentConfig   agentcfg.Registry
	agentConfigID string

	// session-scoped safe-subset overlay store (D-094 mirror of production):
	// the session user layer + narrow-only disables + personal skills,
	// composed over the admin agent config at run start. Nil = none.
	sessionOverlay sessionoverlay.Store

	// Phase 107a parity (D-195 dated-note follow-up) — per-task
	// trajectory map for the Enricher seam, the D-094 mirror of the
	// production driver. Trajectories are stored before RunLoop.Run
	// and retained after completion for tasks.get enrichment. Reads
	// are safe under RLock; writes acquire the full mutex. An evicted
	// task returns nil.
	trajMu       sync.RWMutex
	trajectories map[tasks.TaskID]*planner.Trajectory

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
	// `perTaskRunLoopDriverOpts` godoc. Phase 111d (D-201): the
	// skills surface is the Directory.
	memory          memory.MemoryStore
	memoryRecall    memory.RecallSettings
	skillsDirectory *skills.Directory
	planningHints   *planner.PlanningHints

	// Phase 83i (D-152): tool dispatch + Catalog projection +
	// Trajectory wiring. Optional; nil catalog ⇒ planner sees no
	// tools, nil executor ⇒ CallTool decisions get appended with no
	// observation. Tests that need full end-to-end pass real values.
	catalog         tools.ToolCatalog
	executor        steering.ToolExecutor
	maxStepsRunLoop int

	// Phase 83m (Item 6, D-156) — operator-declared GrantedScopes.
	grantedScopes []string

	// Round-7 F11 / D-166 — artifact store for multimodal materializer.
	artifactStore artifacts.ArtifactStore

	// Phase 111e (D-202) — trajectory compression: the per-run token
	// budget + the assembly-built runner. Zero/nil = compression off.
	tokenBudget int
	compression *planner.CompressionRunner

	// Phase 84b (D-189) — per-agent attachment disposition policy.
	dispositionPolicy planner.DispositionPolicy

	// admin-set tenant-default LLM override resolver (D-094 mirror).
	tenantOverrides devStackTenantOverrideResolver

	// session-level pending-override Store (D-094 mirror).
	sessionOverrides *runsprotocol.Store

	// agent-config registry + agent id (D-094 mirror of production).
	agentConfig   agentcfg.Registry
	agentConfigID string

	// session-scoped safe-subset overlay store (D-094 mirror of production).
	sessionOverlay sessionoverlay.Store
}

// devStackTenantOverrideResolver mirrors cmd/harbor's
// tenantOverrideResolver — the narrow read seam the run loop uses to
// resolve an admin-set tenant default at run start.
type devStackTenantOverrideResolver interface {
	Get(ctx context.Context, tenant string) (governance.TenantOverrideSpec, bool, error)
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
	if opts.tasks == nil {
		return nil, fmt.Errorf("devstack RunLoop driver: tasks is nil")
	}
	return &DevStackRunLoopDriver{
		bus:             opts.bus,
		runLoop:         opts.runLoop,
		planner:         opts.planner,
		tasks:           opts.tasks,
		logger:          opts.logger,
		memory:          opts.memory,
		memoryRecall:    opts.memoryRecall,
		skillsDirectory: opts.skillsDirectory,
		planningHints:   opts.planningHints,
		catalog:         opts.catalog,
		executor:        opts.executor,
		maxStepsRunLoop: opts.maxStepsRunLoop,
		grantedScopes:   append([]string(nil), opts.grantedScopes...),
		artifactStore:   opts.artifactStore,
		tokenBudget:     opts.tokenBudget,
		compression:     opts.compression,
		// Phase 84b (D-189) — disposition policy passthrough.
		dispositionPolicy: opts.dispositionPolicy,
		tenantOverrides:   opts.tenantOverrides,
		sessionOverrides:  opts.sessionOverrides,
		agentConfig:       opts.agentConfig,
		agentConfigID:     opts.agentConfigID,
		sessionOverlay:    opts.sessionOverlay,
		trajectories:      make(map[tasks.TaskID]*planner.Trajectory),
	}, nil
}

// projectAgentConfigSkills mirrors the production driver's run-start
// agent-config skills projection (D-094): it calls the SAME shared
// projection function the production driver uses, so the two binaries
// cannot drift (CLAUDE.md §17.6).
func (d *DevStackRunLoopDriver) projectAgentConfigSkills(ctx context.Context, q identity.Quadruple, views []skills.SkillView) ([]skills.SkillView, error) {
	return projection.ActiveSkillViews(ctx, d.agentConfig, d.sessionOverlay, d.agentConfigID, q, views)
}

// projectAgentConfigCatalog mirrors the production driver's run-start
// agent-config tool-exposure projection (D-094): the SAME shared projection
// excludes a paused MCP server's tools / disabled tools from the run's view.
func (d *DevStackRunLoopDriver) projectAgentConfigCatalog(ctx context.Context, q identity.Quadruple, filter tools.CatalogFilter) (tools.PlannerCatalogView, error) {
	return projection.ActivePlannerCatalogView(ctx, d.agentConfig, d.sessionOverlay, d.agentConfigID, q, d.catalog, filter)
}

// projectAgentConfigPromptLayers overlays the agent's durable layered system
// prompt resolved from the active config onto the run's resolved override
// bundle at run start, via the SAME shared projection the production driver
// uses (D-094 mirror, CLAUDE.md §17.6).
func (d *DevStackRunLoopDriver) projectAgentConfigPromptLayers(ctx context.Context, q identity.Quadruple, ov *planner.LLMOverrides) (*planner.LLMOverrides, error) {
	return projection.ApplyPromptLayers(ctx, d.agentConfig, d.sessionOverlay, d.agentConfigID, q, ov)
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

	// Compile the per-task output schema ONCE at run start (D-094 mirror
	// of cmd/harbor/cmd_dev_runloop.go). A compile failure fails the run
	// LOUD with the output_invalid terminal code — the LLM is never
	// called on a degraded run (§13). Nil for the common schemaless task.
	var compiledSchema *planner.OutputSchemaValidator
	if len(task.OutputSchema) > 0 {
		cs, cErr := planner.CompileOutputSchema(task.OutputSchema)
		if cErr != nil {
			if mErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
				Code:    planner.TaskErrorCodeOutputInvalid,
				Message: "output-schema compile failed: " + cErr.Error(),
			}); mErr != nil && d.logger != nil {
				d.logger.Warn("devstack runloop: MarkFailed(output_invalid) failed",
					slog.String("task_id", string(taskID)),
					slog.String("err", mErr.Error()))
			}
			return
		}
		compiledSchema = cs
	}

	// Memory + skills are session-scoped (D-149) — see the production
	// driver for the rationale. The fetch quadruple zeroes RunID.
	sessionQ := identity.Quadruple{Identity: q.Identity}
	var memBlocks *planner.MemoryBlocks
	if d.memory != nil {
		mb, mErr := runctx.FetchMemoryBlocks(taskCtx, d.memory, sessionQ, task.Query, d.memoryRecall, d.logger)
		if mErr != nil {
			if d.logger != nil {
				d.logger.Warn("devstack runloop: FetchMemoryBlocks failed",
					slog.String("task_id", string(taskID)),
					slog.String("err", mErr.Error()))
			}
			if fErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
				Code:    "runtime_fetch_error",
				Message: fmt.Sprintf("FetchMemoryBlocks: %v", mErr),
			}); fErr != nil && d.logger != nil {
				d.logger.Warn("devstack runloop: MarkFailed(runtime_fetch_error) failed",
					slog.String("task_id", string(taskID)),
					slog.String("err", fErr.Error()))
			}
			return
		}
		memBlocks = mb
	}

	var skillsCtx []any
	if d.skillsDirectory != nil {
		// Phase 111d (D-201) — mirror the production runloop: the
		// Phase-39 Directory view (pinned-then-recent, identity-
		// scoped, capability-filtered, redacted) is the
		// `<skills_context>` producer; the keyword-shaped raw-Search
		// path is deleted (executing the D-195 deprecation notice).
		views, sErr := d.skillsDirectory.View(taskCtx, skills.DirectoryCapability{
			AllowedTools: tools.VisibleNames(d.catalog, tools.CatalogFilter{
				TenantID:      q.TenantID,
				UserID:        q.UserID,
				SessionID:     q.SessionID,
				GrantedScopes: d.grantedScopes,
			}),
		})
		if sErr != nil {
			if d.logger != nil {
				d.logger.Warn("devstack runloop: skills Directory.View failed",
					slog.String("task_id", string(taskID)),
					slog.String("err", sErr.Error()))
			}
			if fErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
				Code:    "runtime_fetch_error",
				Message: fmt.Sprintf("skills Directory.View: %v", sErr),
			}); fErr != nil && d.logger != nil {
				d.logger.Warn("devstack runloop: MarkFailed(runtime_fetch_error) failed",
					slog.String("task_id", string(taskID)),
					slog.String("err", fErr.Error()))
			}
			return
		}
		// Project the agent's active config skills-set ONCE at run start —
		// the D-094 mirror of the production driver's agent-config skills
		// projection. A registry read error fails the run loudly.
		gated, gErr := d.projectAgentConfigSkills(taskCtx, q, views)
		if gErr != nil {
			if d.logger != nil {
				d.logger.Warn("devstack runloop: agent-config skills projection failed",
					slog.String("task_id", string(taskID)),
					slog.String("err", gErr.Error()))
			}
			if fErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
				Code:    "runtime_fetch_error",
				Message: "agent-config skills projection: " + gErr.Error(),
			}); fErr != nil && d.logger != nil {
				d.logger.Warn("devstack runloop: MarkFailed(runtime_fetch_error) failed",
					slog.String("task_id", string(taskID)),
					slog.String("err", fErr.Error()))
			}
			return
		}
		skillsCtx = runctx.ProjectSkillsDirectory(gated)
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
		// Phase 110a (D-194): the per-run view is the promoted
		// `tools.NewPlannerView` — the same constructor production
		// wires (the pre-110a devstack-local mirror is deleted).
		// Agent-config tool-exposure projection (D-094 mirror of
		// cmd_dev_runloop.go): a paused MCP server's tools / disabled tools
		// are excluded via the SAME shared projection production uses; a
		// registry read error fails the run loud.
		view, vErr := d.projectAgentConfigCatalog(taskCtx, q, tools.CatalogFilter{
			TenantID:      q.TenantID,
			UserID:        q.UserID,
			SessionID:     q.SessionID,
			GrantedScopes: d.grantedScopes,
		})
		if vErr != nil {
			if d.logger != nil {
				d.logger.Warn("devstack runloop: agent-config tool-exposure projection failed",
					slog.String("task_id", string(taskID)),
					slog.String("err", vErr.Error()))
			}
			if fErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
				Code:    "runtime_fetch_error",
				Message: "agent-config tool-exposure projection: " + vErr.Error(),
			}); fErr != nil && d.logger != nil {
				d.logger.Warn("devstack runloop: MarkFailed(runtime_fetch_error) failed",
					slog.String("task_id", string(taskID)),
					slog.String("err", fErr.Error()))
			}
			return
		}
		catalogView = view
	}

	// D-094 mirror of cmd/harbor/cmd_dev_runloop.go: per-run
	// OnToolDispatched hook that advances Task.ToolCount via the
	// registry by `count` after every successful tool dispatch (D-274:
	// count is 1 for a CallTool, len(Branches) for a CallParallel).
	// Errors are surfaced loud — silent degradation of an observability
	// counter is forbidden per §13.
	dispatchHook := func(hookCtx context.Context, count int) error {
		for range count {
			if err := d.tasks.IncrementToolCount(hookCtx, taskID); err != nil {
				return fmt.Errorf("tasks.IncrementToolCount(%q): %w", taskID, err)
			}
		}
		return nil
	}

	// Phase 110b (D-195) — Emit/OnChunk parity. The kit previously
	// wired NEITHER closure, so planner telemetry (`planner.decision`
	// / `planner.finish`) and token streaming (`llm.completion.chunk`)
	// were silently dead on the official test surface — devstack
	// validated weaker semantics than production ships (§17.6). Both
	// now come from the SAME promoted constructors production wires,
	// with the driver-lifetime d.subCtx bounding every publish (D-207,
	// closing D-195's correction — the cmd mirror passes its subCtx
	// identically).
	emit := events.IdentityStampingEmitterContext(d.subCtx, d.bus, q, d.logger)
	chunkPub := llm.NewChunkPublisherContext(d.subCtx, d.bus, q, string(taskID), d.logger)
	onChunk := func(delta string, done bool, kind planner.ChunkKind) {
		// D-272 streaming posture (D-276 mirror of the production driver):
		// on a schema-constrained task, SUPPRESS assistant-content and
		// reasoning token DELTAS at this OnChunk → llm.completion.chunk
		// seam. Step-boundary `done` signals still fire but forward with
		// an EMPTY delta — never the done chunk's own text — so no token
		// content leaks on the schema path regardless of driver flush
		// behaviour; tool-dispatch events are unaffected.
		if compiledSchema != nil {
			if !done {
				return
			}
			chunkPub("", done, string(kind))
			return
		}
		chunkPub(delta, done, string(kind))
	}

	// Round-7 F11 / D-166 — the SAME promoted input-artifact policy
	// production calls (Phase 110b — D-195). Phase 84b (D-189): the
	// disposition is resolved per attachment by the planner-homed
	// pure resolver (hint > agent policy > runtime default); this
	// driver is a THIN caller (D-094 mirror of cmd_dev_runloop.go).
	inputArtifacts := runctx.ResolveInputArtifacts(taskCtx, d.artifactStore, q, task.InputArtifactIDs, d.logger, runctx.InputArtifactOptions{
		Hints:   runctx.DispositionHints(task.InputArtifactDispositions),
		Policy:  d.dispositionPolicy,
		Catalog: catalogView,
		Emit:    emit,
	})

	// Phase 107f (D-176 mirror of cmd/harbor/cmd_dev_runloop.go §17.6
	// parity): build the read-only session-artifact manifest the planner
	// renders into `<session_artifacts>`. Session-scoped List (TaskID
	// empty wildcard); a List error → no manifest (logged), never a
	// fabricated one.
	sessionArtifacts := d.resolveSessionArtifacts(taskCtx, sessionQ)

	// Resolve the admin-set tenant default once at run start (D-094 mirror
	// of cmd/harbor/cmd_dev_runloop.go). A resolution error fails the run
	// loudly rather than silently dropping the policy.
	llmOverrides, ovErr := d.resolveLLMOverrides(taskCtx, q)
	if ovErr != nil {
		if mErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
			Code:    planner.TaskErrorCodeRunLoopError,
			Message: "tenant-override resolution failed: " + ovErr.Error(),
		}); mErr != nil && d.logger != nil {
			d.logger.Warn("devstack runloop: MarkFailed after override-resolution error failed",
				slog.String("task_id", string(taskID)),
				slog.String("err", mErr.Error()))
		}
		return
	}

	// Overlay the agent's durable layered system prompt resolved from the
	// active config at run start, via the SAME shared projection the
	// production driver uses (D-094 mirror, CLAUDE.md §17.6). A read error
	// fails the run loudly.
	llmOverrides, plErr := d.projectAgentConfigPromptLayers(taskCtx, q, llmOverrides)
	if plErr != nil {
		if mErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
			Code:    planner.TaskErrorCodeRunLoopError,
			Message: "prompt-layer projection failed: " + plErr.Error(),
		}); mErr != nil && d.logger != nil {
			d.logger.Warn("devstack runloop: MarkFailed after prompt-layer-projection error failed",
				slog.String("task_id", string(taskID)),
				slog.String("err", mErr.Error()))
		}
		return
	}

	spec := steering.RunSpec{
		Planner: d.planner,
		Base: planner.RunContext{
			Quadruple:        q,
			Query:            task.Query,
			Goal:             task.Query,
			LLMOverrides:     llmOverrides, // admin-set tenant default (D-094 mirror)
			MemoryBlocks:     memBlocks,
			SkillsContext:    skillsCtx,
			RepairCounters:   counters,
			PlanningHints:    d.planningHints,
			Catalog:          catalogView,
			Trajectory:       traj,
			Emit:             emit,    // Phase 110b (D-195) — planner-side telemetry parity
			OnChunk:          onChunk, // Phase 110b (D-195) — per-token streaming parity
			InputArtifacts:   inputArtifacts,
			SessionArtifacts: sessionArtifacts,
			// Phase 111e (D-202) — per-run token budget for the runloop's
			// compression gate (D-094 mirror of production).
			Budget: planner.Budget{TokenBudget: d.tokenBudget},
			// the per-task output schema compiled at run start (D-094
			// mirror of production) — engages the React driver's existing
			// per-turn steering with zero planner change. Nil when absent.
			OutputSchema: compiledSchema,
		},
		TaskID:           taskID,
		ToolExecutor:     d.executor,
		OnToolDispatched: dispatchHook, // Phase 83m item 7 — advance Task.ToolCount on dispatch
		MaxSteps:         d.maxStepsRunLoop,
		Compression:      d.compression, // Phase 111e (D-202) — trajectory compression runner
	}
	// Phase 107a parity (D-195 dated-note follow-up — D-094 mirror of
	// cmd/harbor/cmd_dev_runloop.go): save the trajectory ref before
	// Run so the Enricher can read it post-completion (including
	// concurrently — the map is mutex-guarded per D-025).
	d.trajMu.Lock()
	d.trajectories[taskID] = traj
	d.trajMu.Unlock()

	// Twin of cmd/harbor/cmd_dev_runloop.go: stamp the acting agent's
	// registration id as southbound provenance (D-094 mirror). Provenance
	// only — never an isolation principal (§6); empty agentConfigID is a
	// no-op.
	runCtx := tools.WithInvokingAgent(d.subCtx, d.agentConfigID)
	fin, err := d.runLoop.Run(runCtx, spec)
	if err != nil {
		code := planner.TaskErrorCodeRunLoopError
		switch {
		case errors.Is(err, context.Canceled):
			code = planner.TaskErrorCodeCancelled
		case compiledSchema != nil && (errors.Is(err, llm.ErrRetryExhausted) || errors.Is(err, llm.ErrDowngradeExhausted)):
			// D-276 mirror: a schema-constrained run that exhausted the
			// correction budget fails LOUD with output_invalid.
			code = planner.TaskErrorCodeOutputInvalid
		}
		if mErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
			Code:    code,
			Message: err.Error(),
		}); mErr != nil && d.logger != nil {
			d.logger.Warn("devstack runloop: MarkFailed failed",
				slog.String("task_id", string(taskID)),
				slog.String("err", mErr.Error()))
		}
		return
	}
	if fin.Reason == planner.FinishGoal {
		// Phase 110b (D-195) — answer-envelope parity, now via the ONE
		// shared builder (runctx.FinishAnswerEnvelope, D-276) production +
		// RunOnce also call: schemaless → the byte-identical three-key
		// envelope; schema-constrained → capture + validate the terminal
		// payload and add the validated `answer_payload`. A schema-invalid
		// answer fails the task LOUD with output_invalid — never a
		// MarkComplete of an unvalidated envelope (§13). Built BEFORE the
		// memory writeback so a schema failure never persists a turn.
		envelope, envErr := runctx.FinishAnswerEnvelope(fin, traj, compiledSchema)
		if envErr != nil {
			if mErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
				Code:    planner.TaskErrorCodeOutputInvalid,
				Message: "terminal output failed schema validation: " + envErr.Error(),
			}); mErr != nil && d.logger != nil {
				d.logger.Warn("devstack runloop: MarkFailed(output_invalid) failed",
					slog.String("task_id", string(taskID)),
					slog.String("err", mErr.Error()))
			}
			return
		}

		// Phase 83i (D-152) — memory writeback mirror. AssistantResponse
		// is the envelope's Answer (the validated payload string on a
		// schema run; the extracted answer text otherwise).
		if d.memory != nil {
			turn := memory.ConversationTurn{
				UserMessage:       task.Query,
				AssistantResponse: envelope.Answer,
				Timestamp:         time.Now(),
			}
			if mErr := d.memory.AddTurn(taskCtx, sessionQ, turn); mErr != nil && d.logger != nil {
				d.logger.Warn("devstack runloop: memory.AddTurn failed; run still marked complete",
					slog.String("task_id", string(taskID)),
					slog.String("err", mErr.Error()))
			}
		}
		raw, encErr := json.Marshal(envelope)
		if encErr != nil {
			if d.logger != nil {
				d.logger.Error("devstack runloop: marshal TaskResult.Value failed",
					slog.String("task_id", string(taskID)),
					slog.String("err", encErr.Error()))
			}
			raw = []byte("{}")
		}
		if mErr := d.tasks.MarkComplete(taskCtx, taskID, tasks.TaskResult{Value: raw}); mErr != nil && d.logger != nil {
			d.logger.Warn("devstack runloop: MarkComplete failed",
				slog.String("task_id", string(taskID)),
				slog.String("err", mErr.Error()))
		}
		return
	}
	if mErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
		Code:    planner.TaskErrorCodeForFinish(fin.Reason),
		Message: "RunLoop finished without satisfying goal: " + string(fin.Reason),
	}); mErr != nil && d.logger != nil {
		d.logger.Warn("devstack runloop: MarkFailed failed",
			slog.String("task_id", string(taskID)),
			slog.String("err", mErr.Error()))
	}
}

// resolveLLMOverrides mirrors `cmd/harbor/cmd_dev_runloop.go`'s run-start
// resolution (D-094 parity): the tenant default (via the resolver) is
// composed UNDER the Consumed one-shot session override (session › tenant
// › config) via the production `runsprotocol.ComposeLLMOverrides`. A
// resolver error propagates so the caller fails the run loudly; the
// session Consume cannot error (in-process map read).
func (d *DevStackRunLoopDriver) resolveLLMOverrides(ctx context.Context, q identity.Quadruple) (*planner.LLMOverrides, error) {
	var tenant *planner.LLMOverrides
	if d.tenantOverrides != nil {
		spec, set, err := d.tenantOverrides.Get(ctx, q.TenantID)
		if err != nil {
			return nil, err
		}
		if set {
			tenant = &planner.LLMOverrides{
				Model:             spec.Model,
				Temperature:       spec.Temperature,
				MaxTokens:         spec.MaxTokens,
				ReasoningEffort:   spec.ReasoningEffort,
				ExtraInstructions: spec.ExtraInstructions,
			}
		}
	}
	// Per-agent arm — the agent-config LLM-params section (admin-pinned,
	// versioned), resolved via the SAME shared projection the production run
	// loop calls (D-094 parity). It overrides the tenant-wide baseline per
	// field.
	agentLayer, err := projection.ActiveLLMOverrides(ctx, d.agentConfig, d.agentConfigID, q)
	if err != nil {
		return nil, err
	}
	var session *runsprotocol.PendingOverride
	if d.sessionOverrides != nil {
		if po, found := d.sessionOverrides.Consume(q.Identity); found {
			session = &po
		}
	}
	return runsprotocol.ComposeLLMOverrides(session, agentLayer, tenant), nil
}

// resolveSessionArtifacts mirrors `cmd/harbor/cmd_dev_runloop.go`'s
// session-artifact manifest build (Phase 107f — D-176, §17.6 parity). It
// lists `ArtifactStore.List` scoped to the run's `(tenant, user,
// session)` triple (TaskID empty = session-wide wildcard) and hands the
// refs to the shared `planner.BuildArtifactManifest`, so the harness and
// the production run loop produce byte-identical manifests.
//
// Fail-soft: a nil store or a List error yields NO manifest (logged) —
// the turn proceeds, never a fabricated one (CLAUDE.md §5).
func (d *DevStackRunLoopDriver) resolveSessionArtifacts(
	ctx context.Context, sessionQ identity.Quadruple,
) []planner.ArtifactManifestEntry {
	if d.artifactStore == nil {
		return nil
	}
	scope := artifacts.ArtifactScope{
		TenantID:  sessionQ.TenantID,
		UserID:    sessionQ.UserID,
		SessionID: sessionQ.SessionID,
	}
	refs, err := d.artifactStore.List(ctx, scope)
	if err != nil {
		if d.logger != nil {
			d.logger.Warn("devstack RunLoop driver: session-artifact List failed; proceeding with no manifest",
				slog.String("session_id", sessionQ.SessionID),
				slog.String("err", err.Error()))
		}
		return nil
	}
	return planner.BuildArtifactManifest(refs)
}

// TrajectoryByTaskID returns the planner trajectory for a completed run,
// or nil when the task's trajectory has been evicted or never existed.
// Reads are safe under concurrent access (RLock / D-025). The D-094
// mirror of the production driver's accessor — the Enricher seam's
// trajectory source (Phase 107a parity, D-195 dated-note follow-up).
func (d *DevStackRunLoopDriver) TrajectoryByTaskID(taskID tasks.TaskID) *planner.Trajectory {
	d.trajMu.RLock()
	defer d.trajMu.RUnlock()
	return d.trajectories[taskID]
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
