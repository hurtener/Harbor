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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
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
	"github.com/hurtener/Harbor/internal/protocol/transports/cors"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/runsnapshot"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	runsprotocol "github.com/hurtener/Harbor/internal/runtime/runs/protocol"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/server"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/bootpacks"
	"github.com/hurtener/Harbor/internal/skills/importer"
	"github.com/hurtener/Harbor/internal/skills/publication"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/telemetry"
	"github.com/hurtener/Harbor/internal/tools"
	toolapproval "github.com/hurtener/Harbor/internal/tools/approval"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
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
	// devAgentConfigID is the synthetic boot agent selected by the devstack
	// run-loop and granted by the minted dev bearer.
	devAgentConfigID = "harbor-dev-agent"

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
	RunLoopDriver *serve.RunLoopDriver

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
	// RunSnapshots is the process-local retirement drain shared by the mounted
	// agent-config service and RunLoopDriver.
	RunSnapshots *runsnapshot.Gate
	// AgentReach is the shared effective-agent gate used by this stack's
	// control and stream projections.
	AgentReach auth.AgentReachAuthorizer
	// PublicationStore is the one authorized StateStore-backed store shared by
	// the mounted Protocol publication surface and run-loop composition.
	PublicationStore     publication.Store
	PublicationRuntimeID string

	// SessionOverlay is the SESSION-scoped safe-subset overlay store (the
	// non-admin lower tier of the authorization matrix) shared by the mounted
	// session-safe `agent_config.session.*` routes and the run-loop driver's
	// run-start composition. Keyed by the real (tenant, user, session) triple,
	// so it is session-isolated. Nil when the StateStore is unavailable.
	SessionOverlay sessionoverlay.Store
	// SessionPersonalSkillAuthority is the complete durable cutover/controller
	// graph shared by Protocol session-personal methods and run-start snapshots.
	// Nil when the skills subsystem is absent.
	SessionPersonalSkillAuthority *serve.SessionPersonalSkillAuthority

	// Catalog / Coordinator / Gates / OAuthProviders are nil when
	// SkipCatalog is set. The Gates map is keyed by tool name and
	// populated by the catalog Builder; tests that drive
	// `gate.ResolveApproval` reach for it.
	Catalog        tools.ToolCatalog
	Coordinator    pauseresume.Coordinator
	Gates          map[string]*toolapproval.ApprovalGate
	OAuthProviders map[string]toolauth.OAuthProvider
	// OAuthProviderSet + OAuthProviderBuilder back the Protocol-installed,
	// zero-URL broker-pull provider (set_oauth_provider / remove_oauth_provider)
	// — mirrored from the assembled core so integration tests exercise the same
	// real install path. Nil when SkipCatalog is set.
	OAuthProviderSet     toolauth.ProviderSet
	OAuthProviderBuilder *toolauth.ProviderBuilder

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
	stack, err := assembleWith(t.Context(), cfg, opts)
	if err != nil {
		if stack != nil {
			stack.Close()
		}
		t.Fatalf("devstack: %v", err)
	}
	return stack
}

// EnsureBootAgentLifecycle explicitly provisions this DevStack's one
// boot-declared agent for id's tenant. The production runtime never infers a
// tenant declaration from a run request; integration callers that exercise a
// different tenant must make that provisioning step explicit too. The helper
// is idempotent and preserves terminal or corrupt lifecycle records fail
// closed through serve.EnsureBootAgentLifecycle.
func (s *DevStack) EnsureBootAgentLifecycle(ctx context.Context, id identity.Identity) error {
	if s == nil || s.State == nil || s.AgentConfig == nil || s.AgentConfigID == "" {
		return errors.New("devstack boot agent lifecycle is not wired")
	}
	if err := serve.EnsureBootAgentLifecycle(ctx, s.State, s.AgentConfig, id, s.AgentConfigID); err != nil {
		return fmt.Errorf("devstack boot agent lifecycle: %w", err)
	}
	return nil
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
func assembleWith(ctx context.Context, cfg *config.Config, opts AssembleOpts) (*DevStack, error) {
	if ctx == nil {
		return nil, errors.New("assemble context is required")
	}
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
	core, err := assemble.Assemble(ctx, cfg, assemble.Options{
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
		stack.OAuthProviderSet = core.OAuthProviderSet
		stack.OAuthProviderBuilder = core.OAuthProviderBuilder
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
	runSnapshots := runsnapshot.NewGate()
	stack.RunSnapshots = runSnapshots
	metricsReg := core.Metrics
	llmPostureCfg := core.LLMSnapshot

	// Agent-config control plane (D-094 mirror of cmd/harbor's wiring): the
	// versioned desired-state registry keyed by the dev agent's
	// registration id, reusing the assembled StateStore. The SAME registry
	// is handed to the run-loop driver (run-start skills projection) and the
	// mounted `agent_config.*` Protocol service, so a skills edit lands on
	// the next run. Built whenever the assembly opened a StateStore.
	// tenantPolicy is the admin-set tenant-default LLM-override policy — ONE
	// instance shared by the run-loop driver (consume at run start) and the
	// mounted governance surface (set/get), mirroring production. Built
	// whenever the assembly opened a StateStore.
	var tenantPolicy *governance.TenantOverridePolicy
	var setPosturePolicy *governance.SetPosturePolicy
	var bootLifecycleEnsurer agentcfg.BootLifecycleEnsurer
	if core.State != nil {
		reg, regErr := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: core.State, Bus: bus})
		if regErr != nil {
			return stack, fmt.Errorf("agent-config registry: %w", regErr)
		}
		stack.AgentConfig = reg
		stack.AgentConfigID = devAgentConfigID
		stack.closeFns = append(stack.closeFns, reg.Close)
		// The synthetic boot agent is a real selected agent for the devstack's
		// token identity. Phase 233a's durable session-personal resolver uses
		// the agent-level active slot as its lifecycle fence, so materialise an
		// empty first revision only when that slot is truly absent. Existing
		// slots are deliberately untouched: in particular a terminal tombstone
		// must remain terminal across a reconstructed stack.
		if lifecycleErr := serve.EnsureBootAgentLifecycle(ctx, core.State, reg, resolveDevIdentity(opts), devAgentConfigID); lifecycleErr != nil {
			return stack, fmt.Errorf("devstack synthetic agent lifecycle: %w", lifecycleErr)
		}
		bootLifecycleEnsurer = func(runCtx context.Context, id identity.Identity, agentID string) error {
			return serve.EnsureBootAgentLifecycle(runCtx, core.State, reg, id, agentID)
		}

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
		if stack.Skills != nil {
			authority, authorityErr := serve.NewSessionPersonalSkillAuthority(
				ctx,
				core.State,
				stack.Skills,
				cfg.Skills.SessionPersonalCutover.Tenants,
			)
			if authorityErr != nil {
				return stack, fmt.Errorf("agent-config session-personal skill authority: %w", authorityErr)
			}
			stack.SessionPersonalSkillAuthority = authority
		}

		tp, tpErr := governance.NewTenantOverridePolicy(core.State, bus, devstackValidModels(cfg), nil)
		if tpErr != nil {
			return stack, fmt.Errorf("governance tenant-override policy: %w", tpErr)
		}
		tenantPolicy = tp
		stack.closeFns = append(stack.closeFns, tp.Close)

		spp, sppErr := governance.NewSetPosturePolicy(core.State, bus,
			governance.ConfigFromOperator(cfg.Governance), nil, core.GovernanceTierSource,
			core.GovernanceEnforcementActive)
		if sppErr != nil {
			return stack, fmt.Errorf("governance set-posture policy: %w", sppErr)
		}
		setPosturePolicy = spp
		stack.closeFns = append(stack.closeFns, spp.Close)
	}

	// The MCP-attach concrete is built inside the steering block below (the
	// run-loop driver consumes it as its run-start ConnectionReattacher) but is
	// also consumed by the mux builder further down, so it is declared here.
	var attacher agentcfgprotocol.ConnectionAttacher
	var agentResolver protocol.AgentResolver
	var publicationStore publication.Store
	var publicationRuntimeID string
	var sharedSealer toolauth.Sealer

	// The HA-66 boot baseline — the SAME eager loader/composer production
	// serve.Boot runs (CLAUDE.md §17.6 parity). The immutable index is
	// opened, validated and collision-pre-read HERE — before the run-loop
	// driver is constructed — so the SAME frozen pointer feeds the driver's
	// run-start boot baseline, the composition preview, and the boot-ownership
	// wiring. Loading is guarded ONLY by the declarations, never by
	// SkipTransports / SkipSteering: a component-skipped stack may fail
	// loud if a mandatory catalog / registry collaborator is absent, but it
	// may never silently start a driver with no baseline just because the
	// transport/mux leg was skipped.
	var bootIndex *bootpacks.Index
	if len(cfg.Skills.BootAgentPacks) > 0 {
		var bErr error
		// Assign the OUTER variable (plain `=`, mirroring serve.Boot) — a
		// short-declaration `:=` here would shadow it with a block-local copy
		// and leave every later consumer (driver, preview, ownership) holding
		// a typed-nil index.
		bootIndex, bErr = serve.OpenBootPackIndex(ctx, cfg, stack.Catalog, stack.Artifacts)
		if bErr != nil {
			return stack, bErr
		}
		if vErr := serve.ValidateBootAgentPacksForAgent(cfg, stack.AgentConfigID); vErr != nil {
			return stack, vErr
		}
		retReg, ok := stack.AgentConfig.(agentcfg.RetirementRegistry)
		if !ok {
			return stack, fmt.Errorf("devstack boot_agent_packs: agent-config registry does not implement the retirement/read seam")
		}
		if pErr := serve.PreReadBootPackCollisions(ctx, bootIndex, retReg); pErr != nil {
			return stack, pErr
		}
	}

	// Steering surface + run-loop driver. Skip-aware: the Mux phase
	// below depends on the surface, so SkipSteering implies
	// SkipTransports even if the caller did not set both flags.
	if !opts.SkipSteering {
		// Resolve the one shared KEK-backed sealer before constructing the
		// control surface and run-loop driver. This lets config-only KEK
		// deployments use the same restart-stable Agent-reach admission
		// authority as broker-backed deployments, without a second sealer.
		var sealerErr error
		sharedSealer, sealerErr = serve.ResolveSharedKEKSealer(cfg, stack.OAuthProviderBuilder)
		if sealerErr != nil {
			return stack, sealerErr
		}
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
			protocol.WithSessionEnsurer(serve.NewSessionEnsurerAdapter(core.Sessions)))
		surfaceOpts = append(surfaceOpts, protocol.WithPauseCoordinator(core.Coordinator))
		// Caller-named-agent validation — mirrors production
		// `internal/runtime/serve`. The SAME registry + boot agent id the
		// kit's run-loop driver projects from, so the twin cannot drift
		// from the binary on which agents a `start` may name. An assembly
		// with no StateStore has no registry: the adapter then refuses
		// every named agent, which is the fail-closed posture, never a
		// silent accept.
		stack.AgentReach = auth.NewAgentReachAuthorizer()
		var agentReachAdmissions *tasks.AgentReachAdmissionAuthority
		if stack.OAuthProviderBuilder != nil && stack.OAuthProviderBuilder.AdmissionSealer() != nil {
			var admissionErr error
			agentReachAdmissions, admissionErr = tasks.NewAgentReachAdmissionAuthority(stack.OAuthProviderBuilder.AdmissionSealer())
			if admissionErr != nil {
				return stack, fmt.Errorf("devstack agent reach admission authority: %w", admissionErr)
			}
		}
		if agentReachAdmissions == nil && sharedSealer != nil {
			var admissionErr error
			agentReachAdmissions, admissionErr = tasks.NewAgentReachAdmissionAuthority(sharedSealer)
			if admissionErr != nil {
				return stack, fmt.Errorf("devstack agent reach admission authority: %w", admissionErr)
			}
		}
		if agentReachAdmissions != nil {
			publicationRuntimeID = publication.NewRuntimeID("harbor-devstack")
			publicationStore, err = serve.NewSkillPublicationStore(core.State, publicationRuntimeID, stack.AgentReach)
			if err != nil {
				return stack, fmt.Errorf("devstack skill publication store: %w", err)
			}
			stack.PublicationStore = publicationStore
			stack.PublicationRuntimeID = publicationRuntimeID
			stack.closeFns = append(stack.closeFns, publicationStore.Close)
		}
		agentResolver = serve.NewAgentResolverAdapter(stack.AgentConfig, stack.AgentConfigID, serve.WithBootLifecycleEnsurer(bootLifecycleEnsurer))
		surfaceOpts = append(surfaceOpts,
			protocol.WithAgentResolver(agentResolver),
			protocol.WithAgentReachAuthorizer(stack.AgentReach),
			protocol.WithAgentReachAdmissionAuthority(agentReachAdmissions))
		surface, surfaceErr := protocol.NewControlSurface(taskReg, core.Steering, surfaceOpts...)
		if surfaceErr != nil {
			return stack, fmt.Errorf("protocol.NewControlSurface: %w", surfaceErr)
		}
		stack.Surface = surface

		// The MCP-attach concrete backing agent_config.add_mcp_connection AND the
		// run-start ATTACH pass — the promoted concrete (the kit's mirror is
		// deleted). Its Close joins the closer chain so a runtime-added subprocess
		// drains on teardown.
		//
		// It is constructed HERE, before the run-loop driver below, because the
		// driver takes it as its ConnectionReattacher: one attach implementation
		// serves both the admin add verb and the run-start re-establishment, so a
		// kit-side integration test exercises the same real leg production runs
		// (CLAUDE.md §17.6 — a kit that wired only the detacher would let the
		// restart-survival test pass against a stack that cannot re-attach). The
		// two variables are declared in the outer scope because the mux builder
		// below also consumes the attacher.
		var devReattacher projection.ConnectionReattacher
		if stack.Catalog != nil && stack.MCPRegistry != nil {
			// Thread the tool-context store so a RUNTIME-ADDED server captures
			// an app-declaring tool call's context exactly as a boot-config one
			// does — the kit must mirror production here, or an integration test
			// would pass against a stack that silently cannot capture. The
			// explicit nil check avoids handing a typed-nil pointer to the
			// interface (which would read as "a capturer is wired").
			var mcpToolCtx mcpdrv.ToolContextCapturer
			if stack.MCPToolContext != nil {
				mcpToolCtx = stack.MCPToolContext
			}
			att := serve.NewMCPConnectionAttacher(stack.Catalog, stack.MCPRegistry, bus, opts.Logger,
				resolveDevIdentity(opts), stack.OAuthProviders, stack.OAuthProviderSet, mcpToolCtx,
				// The same CURRENT-boot-policy gates production threads: the
				// fail-closed stdio allowlist and the credential-injection opt-in,
				// re-applied at every run-start re-attach.
				serve.WithReattachGates(append([]string(nil), opts.MCPStdioAllowlist...), cfg.Tools.AllowWireInjection))
			stack.closeFns = append(stack.closeFns, att.Close)
			attacher = att
			devReattacher = att
		}

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
			// The promoted per-task run-loop driver — the SAME concrete
			// production boots (the kit's hand-mirrored copy is deleted in
			// favor of this single home). Explicit AssembleOpts overrides win;
			// otherwise the cfg-opened stores + the shared exported
			// projections production uses.
			var devDetacher projection.ConnectionDetacher
			if stack.Catalog != nil && stack.MCPRegistry != nil {
				devDetacher = serve.NewMCPConnectionDetacher(stack.Catalog, stack.MCPRegistry, opts.Logger)
			}
			var devProviderReconciler projection.OAuthProviderReconciler
			if stack.OAuthProviderSet != nil && stack.OAuthProviderBuilder != nil {
				if concrete := serve.NewOAuthProviderInstaller(stack.OAuthProviderBuilder, stack.OAuthProviderSet, cfg.Tools.AllowWireOAuthDescriptor, opts.Logger); concrete != nil {
					devProviderReconciler = concrete
				}
			}
			// The HA-66 P0 composition at the run-loop seam — the same
			// serve.Boot fix: the frozen index reaches the reader interface
			// ONLY when a baseline is bound, so a no-boot stack hands the
			// driver an ACTUAL nil (never a typed-nil pointer inside a
			// non-nil interface whose Lookup panics at run start).
			var devBootReader agentcfgprotocol.BootPackReader
			if bootIndex != nil {
				devBootReader = bootIndex
			}
			driver, drvErr := serve.NewRunLoopDriver(serve.RunLoopDriverOptions{
				Bus:                      bus,
				RunLoop:                  stack.RunLoop,
				Planner:                  core.Planner,
				Tasks:                    taskReg,
				Logger:                   opts.Logger,
				SessionOverrides:         runsStore,
				Memory:                   resolveMemoryStore(opts, stack),
				MemoryRecall:             memory.RecallFromConfig(cfg.Memory),
				SkillsDirectory:          skillsDir,
				PlanningHints:            resolvePlanningHints(opts, cfg),
				SkillStore:               stack.Skills,
				SessionPersonalSkills:    devSessionPersonalStore(stack.SessionPersonalSkillAuthority),
				SessionSkillCutover:      devSessionPersonalCutover(stack.SessionPersonalSkillAuthority),
				Catalog:                  stack.Catalog,
				Executor:                 core.Executor,
				MaxStepsRunLoop:          cfg.Planner.MaxSteps,
				TrancheSteps:             steering.EffectiveTrancheSteps(cfg.Planner.MaxSteps),
				GrantedScopes:            append([]string(nil), cfg.Tools.GrantedScopes...),
				ArtifactStore:            stack.Artifacts,
				TokenBudget:              cfg.Planner.TokenBudget,
				Compression:              core.Compression,
				DispositionPolicy:        dispositionPolicy,
				TenantOverrides:          tenantPolicy,
				AgentConfig:              stack.AgentConfig,
				AgentConfigID:            stack.AgentConfigID,
				EnsureBootAgentLifecycle: bootLifecycleEnsurer,
				RunSnapshots:             runSnapshots,
				AgentReachAdmissions:     agentReachAdmissions,
				PublicationStore:         publicationStore,
				PublicationRuntimeID:     publicationRuntimeID,
				SessionOverlay:           stack.SessionOverlay,
				BootPackReader:           devBootReader,
				RunCompletionHook:        projection.RunCompletionHookFromConfig(cfg.Runtime.Hooks.RunCompletion),
				ConnectionDetacher:       devDetacher,
				ConnectionReattacher:     devReattacher,
				BootDeclaredMCP:          serve.BootDeclaredMCPServerSet(cfg),
				OAuthProviderReconciler:  devProviderReconciler,
				NamingDefault:            cfg.Runtime.Naming,
				SessionTitler:            stack.Sessions,
				NamingLLM:                stack.LLMClient,
			})
			if drvErr != nil {
				return stack, fmt.Errorf("devstack RunLoop driver: %w", drvErr)
			}
			if startErr := driver.Start(ctx); startErr != nil {
				return stack, fmt.Errorf("devstack RunLoop driver start: %w", startErr)
			}
			stack.RunLoopDriver = driver
			stack.RunsOverrideStore = runsStore
			stack.closeFns = append(stack.closeFns, driver.Close)
		}
	}

	// Auth. The dev signer mints an ephemeral ES256 keypair + a
	// Bearer token under the configured identity. Skip-aware.
	// rotateSurface backs the dev-only auth.rotate_token method — the kit now
	// mounts it (parity with the promoted band; the mirror omitted it).
	var rotateSurface *auth.RotateSurface
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

		rs, rsErr := auth.NewRotateSurface(&devstackTokenIssuer{priv: priv}, stack.Audit,
			auth.WithRotateBus(bus))
		if rsErr != nil {
			return stack, fmt.Errorf("auth.NewRotateSurface: %w", rsErr)
		}
		rotateSurface = rs

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
		lg := opts.Logger
		if lg == nil {
			lg = slog.Default()
		}

		// The Protocol-installed OAuth provider installer (set_oauth_provider /
		// remove_oauth_provider + the run-start provider reconcile).
		var oauthProviderInstaller agentcfgprotocol.ProviderInstaller
		if stack.OAuthProviderSet != nil && stack.OAuthProviderBuilder != nil {
			if concrete := serve.NewOAuthProviderInstaller(stack.OAuthProviderBuilder, stack.OAuthProviderSet, cfg.Tools.AllowWireOAuthDescriptor, opts.Logger); concrete != nil {
				oauthProviderInstaller = concrete
			}
		}

		// The Protocol-installed inference provider installer (set_llm_provider)
		// + the boot-connect of a config-declared brokered primary. Wired
		// whenever an LLM driver is opened (the shared LiveKey is present).
		var llmProviderInstaller agentcfgprotocol.LLMProviderInstaller
		var inferenceBrokerNames []string
		if core.LLMLiveKey != nil {
			if concrete := serve.NewLLMProviderInstaller(core.LLMLiveKey, core.LLMSnapshot.Provider,
				cfg.LLM.InferenceBrokers, bus, stack.Audit, "harbor-devstack", lg); concrete != nil {
				llmProviderInstaller = concrete
				inferenceBrokerNames = concrete.BrokerNames()
				stack.closeFns = append(stack.closeFns, concrete.Close)
				if cfg.LLM.CredentialSource == "remote" {
					if cErr := concrete.BootConnectPrimary(ctx, cfg.LLM.InferenceBroker); cErr != nil {
						return nil, fmt.Errorf("llm brokered primary: %w", cErr)
					}
				}
			}
		}

		// Single-homed Protocol surface construction + fan-out. The kit's
		// hand-mirrored mux block is deleted in favor of this shared builder,
		// which closes the drift the mirror carried: the kit now GAINS the
		// agents / auth-rotate / governance-override / governance-key-rotate
		// surfaces the mirror omitted.
		signedOAuthMCPCapabilityAuthorities, authorityErr := serve.SignedOAuthMCPCapabilityAuthoritiesFromConfig(context.Background(), cfg, opts.Logger)
		if authorityErr != nil {
			return stack, authorityErr
		}
		// The HA-66 boot baseline + HA-64/65 projections + HA-56 render
		// admission — the SAME serve-band wiring production serve.Boot
		// composes, so devstack exercises the exact loader/composer path
		// (CLAUDE.md §17.6). The shared sealer / boot index / projection
		// loops close through the stack's closer chain.
		turnsProj, turnsSvc, turnsCloser, tErr := serve.OpenTurnsProjection(ctx, cfg, serve.TurnsProjectionDeps{
			Bus: bus, Sessions: stack.Sessions, Tasks: stack.Tasks, Artifacts: stack.Artifacts, Logger: lg,
		})
		if tErr != nil {
			return stack, tErr
		}
		if turnsCloser != nil {
			stack.closeFns = append(stack.closeFns, turnsCloser)
		}
		var turnsStore turns.Store
		if turnsSvc != nil {
			turnsStore = turnsSvc.Store()
		}
		rollupsStore, rollupsWorker, rollupsCloser, rErr := serve.OpenRollupsProjection(ctx, cfg, serve.RollupsProjectionDeps{Bus: bus, Logger: lg})
		if rErr != nil {
			return stack, rErr
		}
		if rollupsCloser != nil {
			stack.closeFns = append(stack.closeFns, rollupsCloser)
		}
		retReg, retirementOK := stack.AgentConfig.(agentcfg.RetirementRegistry)
		if !retirementOK && (cfg.Tools.MCPAppRenderAdmission.Enabled || len(cfg.Skills.BootAgentPacks) > 0) {
			return stack, fmt.Errorf("devstack agent-config registry does not implement the retirement/read seam required by the enabled v1.28 surface")
		}
		// The HA-56 render-admission authority + gate pair — wired ONLY
		// when the operator explicitly opted in (sealer availability is
		// NOT feature enablement: an OAuth broker sealer alone never
		// enables the surface). Mirrors serve.Boot's wiring so the kit
		// exercises the exact composition (CLAUDE.md §17.6).
		admissionAuthority, admissionGate, admErr := serve.WireRenderAdmission(serve.RenderAdmissionAuthorityDeps{
			Enabled:        cfg.Tools.MCPAppRenderAdmission.Enabled,
			Sessions:       stack.Sessions,
			AgentConfig:    retReg,
			SessionOverlay: stack.SessionOverlay,
			Registry:       stack.MCPRegistry,
			Sealer:         sharedSealer,
		})
		if admErr != nil {
			return stack, admErr
		}
		// The HA-61 two-phase import service — the SAME composition
		// serve.Boot builds (production ↔ devstack parity by
		// construction): the production importer over the caller-owned
		// artifact store, the ONE shared sealer, the runtime StateStore
		// ledger, the configured SkillStore, the registry's
		// retirement/read seam, and the capability-policy adapter over
		// the wrapped catalog. Nil (routes stay 501) only when a
		// mandatory seam is genuinely absent.
		var importService *agentcfgprotocol.UserSkillImportService
		if stack.Skills != nil && sharedSealer != nil && retirementOK {
			imp, impErr := importer.New(importer.Deps{Store: stack.Artifacts})
			if impErr != nil {
				return stack, fmt.Errorf("devstack user skill import service: %w", impErr)
			}
			importService, err = agentcfgprotocol.NewUserSkillImportService(
				imp, stack.Artifacts, sharedSealer, core.State, stack.Skills,
				retReg,
				agentcfgprotocol.NewUserSkillImportCapabilityPolicy(stack.AgentConfig,
					stack.SessionOverlay, stack.Catalog, cfg.Tools.GrantedScopes),
				agentcfgprotocol.WithImportAgentReach(stack.AgentReach),
				agentcfgprotocol.WithImportSessionReach(auth.NewSessionReachAuthorizer()),
				agentcfgprotocol.WithImportLogger(lg),
			)
			if err != nil {
				return stack, fmt.Errorf("devstack user skill import service: %w", err)
			}
		}
		var previewService *agentcfgprotocol.CompositionPreviewService
		if retirementOK {
			var pErr error
			// The SAME preview path serve.Boot builds: the frozen boot
			// index as the BootPackReader, or the EMPTY immutable reader
			// when no baseline is declared — boot config removal never
			// 501s the preview and an independently persisted active
			// revision appears as provenance "revision".
			previewService, pErr = agentcfgprotocol.NewCompositionPreviewService(
				retReg, serve.PreviewBootReader(bootIndex),
				agentcfgprotocol.WithPreviewAgentReach(stack.AgentReach),
				agentcfgprotocol.WithPreviewSessionReach(auth.NewSessionReachAuthorizer()),
				agentcfgprotocol.WithPreviewBus(bus),
				agentcfgprotocol.WithPreviewRedactor(stack.Audit),
				agentcfgprotocol.WithPreviewLogger(lg),
			)
			if pErr != nil {
				return stack, fmt.Errorf("devstack composition preview service: %w", pErr)
			}
		}

		// The HA-66 P0 composition at the mux seam — the same serve.Boot
		// fix: a SEPARATE actual-nil BootOwnership interface, populated only
		// when the index is non-nil, so a no-boot stack keeps every pack
		// mutation guard inert instead of panicking on the first OwnsName.
		var devBootOwnership agentcfgprotocol.BootOwnership
		if bootIndex != nil {
			devBootOwnership = bootIndex
		}
		muxInput := serve.MuxInput{
			Cfg:                            cfg,
			Surface:                        stack.Surface,
			Bus:                            bus,
			Redactor:                       stack.Audit,
			Logger:                         lg,
			Metrics:                        metricsReg,
			LLMSnapshot:                    llmPostureCfg,
			Tasks:                          stack.Tasks,
			Sessions:                       stack.Sessions,
			Agents:                         core.Agents,
			Artifacts:                      stack.Artifacts,
			Memory:                         stack.Memory,
			Catalog:                        stack.Catalog,
			Coordinator:                    stack.Coordinator,
			MCPRegistry:                    stack.MCPRegistry,
			MCPToolContext:                 stack.MCPToolContext,
			State:                          stack.State,
			Skills:                         stack.Skills,
			AgentPackLLM:                   stack.LLMClient,
			AgentConfig:                    stack.AgentConfig,
			AgentConfigID:                  stack.AgentConfigID,
			AgentResolver:                  agentResolver,
			BootLifecycleEnsurer:           bootLifecycleEnsurer,
			RunSnapshots:                   runSnapshots,
			SessionOverlay:                 stack.SessionOverlay,
			SessionPersonalSkillController: devSessionPersonalController(stack.SessionPersonalSkillAuthority),
			RunsStore:                      runsStore,
			RunLoopDriver:                  stack.RunLoopDriver,
			OAuthProviders:                 stack.OAuthProviders,
			TenantOverridePolicy:           tenantPolicy,
			SetPosturePolicy:               setPosturePolicy,
			KeyRotator:                     core.KeyRotator,
			ValidModels:                    devstackValidModels(cfg),
			MCPAttacher:                    attacher,
			MCPStdioAllowlist:              append([]string(nil), opts.MCPStdioAllowlist...),
			BootDeclaredMCP:                serve.BootDeclaredMCPServerNames(cfg),
			BootDeclaredOAuth:              serve.BootDeclaredOAuthProviderNames(cfg),
			AllowWireOAuthDescriptor:       cfg.Tools.AllowWireOAuthDescriptor,
			AllowWireInjection:             cfg.Tools.AllowWireInjection,
			OAuthProviderInstaller:         oauthProviderInstaller,
			LLMProviderInstaller:           llmProviderInstaller,
			InferenceBrokers:               inferenceBrokerNames,
			Validator:                      stack.Validator,
			AuthSurface:                    rotateSurface,
			AgentReach:                     stack.AgentReach,
			PublicationStore:               publicationStore,
			PublicationRuntimeID:           publicationRuntimeID,
			DisplayName:                    "harbor devstack",
			InstanceID:                     "harbor-devstack",
			BuildVersion:                   "devstack",
			BuildCommit:                    "devstack",
			TopologyAvailable:              false,
			RenderAdmissionAuthority:       admissionAuthority,
			RenderAdmissionGate:            admissionGate,
			TurnsProjector:                 turnsProj,
			TurnsStore:                     turnsStore,
			RollupsStore:                   rollupsStore,
			RollupsQuality:                 rollupsWorker,
			UserSkillImportService:         importService,
			CompositionPreviewService:      previewService,
			BootOwnership:                  devBootOwnership,
		}
		muxInput.SignedOAuthMCPCapabilityAuthorities = signedOAuthMCPCapabilityAuthorities
		built, bErr := serve.BuildMux(muxInput)
		if bErr != nil {
			return stack, bErr
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
		// Draft handler — mounted under the same auth middleware as the
		// Protocol mux (bare when SkipAuth so tests inject identity themselves).
		if stack.DraftStore != nil {
			draftHandler, dErr := devdraft.NewHandler(stack.DraftStore, nil)
			if dErr != nil {
				return stack, fmt.Errorf("devdraft.NewHandler: %w", dErr)
			}
			var mounted http.Handler = draftHandler
			if stack.Validator != nil {
				var mwOpts []auth.MiddlewareOption
				if opts.Logger != nil {
					mwOpts = append(mwOpts, auth.MWLogger(opts.Logger))
				}
				mounted = auth.Middleware(stack.Validator, mwOpts...)(draftHandler)
			}
			router.Handle(devdraft.RoutePrefix+"/", mounted)
		}
		// Tool-OAuth callback endpoint — mounted WITHOUT auth middleware (the
		// one-time state nonce is the capability). Registered before the /v1/
		// catch-all so the exact match wins.
		var cbOpts []toolauth.CallbackOption
		if opts.Logger != nil {
			cbOpts = append(cbOpts, toolauth.WithCallbackLogger(opts.Logger))
		}
		router.Handle(toolauth.CallbackRoutePattern,
			toolauth.CallbackHandler(stack.OAuthProviders, cbOpts...))
		router.Handle("/v1/", built.Mux)
		stack.Mux = router
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
		"iss":         "harbor-test",
		"sub":         user,
		"aud":         "harbor",
		"exp":         now.Add(DefaultTokenTTL).Unix(),
		"nbf":         now.Add(-1 * time.Minute).Unix(),
		"iat":         now.Unix(),
		"tenant":      tenant,
		"user":        user,
		"session":     session,
		"scopes":      []string{"admin", "console:fleet"},
		"agent_reach": []string{"harbor-dev-agent"},
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

func devSessionPersonalStore(authority *serve.SessionPersonalSkillAuthority) *sessionoverlay.DurableStore {
	if authority == nil {
		return nil
	}
	return authority.Personal
}

func devSessionPersonalCutover(authority *serve.SessionPersonalSkillAuthority) sessionoverlay.CutoverModeReader {
	if authority == nil {
		return nil
	}
	return authority.Cutover
}

func devSessionPersonalController(authority *serve.SessionPersonalSkillAuthority) agentcfgprotocol.SessionPersonalSkillController {
	if authority == nil {
		return nil
	}
	return authority.Controller
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

// resolveDevIdentity resolves the dev identity triple from AssembleOpts,
// substituting the package defaults.
func resolveDevIdentity(opts AssembleOpts) identity.Identity {
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
	return identity.Identity{TenantID: tenant, UserID: user, SessionID: session}
}

// devstackTokenIssuer implements auth.TokenIssuer for the kit's dev-only
// auth.rotate_token surface. It re-mints an ES256 token for the verified
// identity using the same ephemeral key the validator trusts.
type devstackTokenIssuer struct {
	priv *ecdsa.PrivateKey
}

func (i *devstackTokenIssuer) IssueToken(_ context.Context, id identity.Identity, _ []auth.Scope, now time.Time) (string, time.Time, error) {
	token, err := signDevToken(i.priv, id.TenantID, id.UserID, id.SessionID)
	if err != nil {
		return "", time.Time{}, err
	}
	return token, now.Add(DefaultTokenTTL), nil
}
