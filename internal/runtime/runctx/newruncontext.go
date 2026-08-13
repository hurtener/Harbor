// newruncontext.go — the shared production RunContext factory.
//
// A headless embedder that wants to run a single goal after assembling
// a stack previously had to hand-build a `planner.RunContext` field by
// field, re-deriving the memory / skills / artifact / streaming
// projections the run-loop drivers already compose. NewRunContext is
// the ONE factory that turns stack-derived subsystem handles into a
// fully-projected RunContext, composing the SAME projection helpers the
// drivers use (FetchMemoryBlocks, ProjectSkillsDirectory,
// ResolveInputArtifacts, the bus chunk publisher) — never a second
// hand-rolled construction site. The one-call Stack.RunOnce runner is
// its first consumer; an embedder building its own RunSpec composes it
// directly.
//
// Import direction: this file may import `tools` / `llm` (the
// projection + streaming surfaces) in addition to the package's
// existing `planner` / `memory` / `skills` / `artifacts` / `events`
// imports — none of those import this package, so no cycle. The factory
// stays out of `steering`: it produces the RunContext (RunSpec.Base);
// the caller wraps it in a RunSpec with the planner / executor /
// max-steps it owns.
package runctx

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/tools"
)

// Sources carries the stack-derived subsystem handles NewRunContext
// projects into a planner.RunContext. Every field is OPTIONAL except an
// identity-complete quadruple (passed separately): a nil subsystem
// leaves the corresponding RunContext field nil and the planner omits
// the matching wrapper — exactly as the run-loop drivers behave when a
// dev stack did not open the subsystem.
//
// assemble.Stack.RunOnce populates Sources from the assembled stack; a
// headless embedder constructing its own RunSpec populates it by hand.
type Sources struct {
	// Memory is the session-scoped memory store. When non-nil, the
	// session's rolling-summary + recent turns are projected into
	// RunContext.MemoryBlocks; semantic recall fires when MemoryRecall
	// is enabled.
	Memory       memory.MemoryStore
	MemoryRecall memory.RecallSettings

	// SkillsDirectory is the bounded, capability-filtered browse window
	// projected into RunContext.SkillsContext. When non-nil its View is
	// taken under the run's identity scope and the run's visible-tool
	// set (derived from Catalog + GrantedScopes).
	SkillsDirectory *skills.Directory

	// Catalog is the production tool catalog. When non-nil it is
	// projected into the planner-facing schema-only RunContext.Catalog
	// view under the run's identity + granted-scope filter.
	Catalog tools.ToolCatalog

	// Artifacts is the artifact store the input-artifact materializer
	// reads from (only touched when WithInputArtifacts supplies IDs).
	Artifacts artifacts.ArtifactStore

	// Bus is the event bus the planner-side telemetry emitter and the
	// per-token chunk publisher write through. When nil, RunContext.Emit
	// and RunContext.OnChunk are left nil (no streaming surface).
	Bus events.EventBus

	// Logger receives the projection helpers' Debug/Warn records. Nil
	// falls back to slog.Default().
	Logger *slog.Logger

	// GrantedScopes is the operator-declared authorization-scope list
	// threaded into the catalog + skills capability filters. Tools whose
	// AuthScopes exceed this set are invisible to the run.
	GrantedScopes []string

	// PlanningHints is the optional runtime-supplied planning-constraint
	// bundle threaded straight onto RunContext.PlanningHints.
	PlanningHints *planner.PlanningHints

	// LLMOverrides is the optional run-start-resolved LLM-parameter
	// bundle. A headless caller with no tenant/agent control plane wired
	// leaves it nil (the run uses the configured defaults).
	LLMOverrides *planner.LLMOverrides

	// Budget is the per-run hard caps (token budget, deadline, hop
	// budget) projected onto RunContext.Budget. The zero value disables
	// compression and applies the runtime defaults.
	Budget planner.Budget

	// OutputSchema is the optional, opt-in run-level output schema (raw
	// JSON-Schema bytes). When non-empty NewRunContext compiles it ONCE
	// and threads the compiled validator onto RunContext.OutputSchema, so
	// the terminal answer is validated against it (fail-loud). A compile
	// failure fails the run loudly rather than silently degrading to an
	// unconstrained run (CLAUDE.md §13). Empty means "no schema".
	OutputSchema json.RawMessage
}

// runContextConfig holds the per-call knobs the functional options set.
// The type is extensible: a sibling streaming option (a per-run sink)
// adds a field here without touching NewRunContext's signature.
type runContextConfig struct {
	inputArtifactIDs  []string
	inputDispositions map[string]string
	dispositionPolicy planner.DispositionPolicy
	skillReader       *skills.RunSkillReaderSnapshot
}

// Option configures a NewRunContext call. The functional-option shape
// keeps the factory signature stable as new per-run knobs land.
type Option func(*runContextConfig)

// WithInputArtifacts pre-resolves the operator-uploaded artifact IDs
// into the run's first-turn multimodal inputs (the materializer renders
// them as Content.Parts). Without it the run is text-only.
func WithInputArtifacts(ids ...string) Option {
	return func(c *runContextConfig) {
		c.inputArtifactIDs = append(c.inputArtifactIDs, ids...)
	}
}

// WithInputArtifactDispositions supplies the per-attachment disposition
// hint map (artifact ID → "inline"/"ref"/"tool:<name>") — the top
// precedence layer of disposition resolution.
func WithInputArtifactDispositions(hints map[string]string) Option {
	return func(c *runContextConfig) { c.inputDispositions = hints }
}

// WithDispositionPolicy supplies the per-agent disposition policy map
// (the middle precedence layer).
func WithDispositionPolicy(policy planner.DispositionPolicy) Option {
	return func(c *runContextConfig) { c.dispositionPolicy = policy }
}

// WithSkillReaderSnapshot installs the immutable read projection already
// selected for this effective agent and run. The caller must construct one
// snapshot at run start and reuse it when attaching the tool-invocation
// context; NewRunContext uses it for the Directory projection only.
func WithSkillReaderSnapshot(snapshot skills.RunSkillReaderSnapshot) Option {
	return func(c *runContextConfig) { c.skillReader = &snapshot }
}

// NewRunContext projects the stack-derived sources into a fully-formed
// planner.RunContext for the run identified by q and driven toward
// goal. It is the single production home for run-loop RunContext
// population that a one-call runner or a headless RunSpec builder
// composes; the per-task drivers keep their own population bodies (they
// thread control-plane projections this factory deliberately omits) but
// call the SAME underlying helpers, so the projected memory / skills /
// input-artifact / streaming surface is identical. The read-only
// cross-turn session-artifact manifest the dev drivers attach is one of
// those control-plane projections this factory omits; a headless run that
// needs input artifacts resolves them explicitly via WithInputArtifacts.
//
// Identity is mandatory and fails loud: an incomplete quadruple returns
// a wrapped identity error rather than a half-scoped RunContext. Memory
// and skills are SESSION-scoped (the fetch quadruple zeroes RunID) so a
// run inherits the session's accumulated state; a fetch error is
// returned unwrapped enough for the caller to fail the run loudly (no
// silent degradation, CLAUDE.md §5/§13).
func NewRunContext(
	ctx context.Context,
	src Sources,
	q identity.Quadruple,
	goal string,
	opts ...Option,
) (planner.RunContext, error) {
	if err := identity.Validate(q.Identity); err != nil {
		return planner.RunContext{}, fmt.Errorf("runctx: NewRunContext identity: %w", err)
	}
	logger := src.Logger
	if logger == nil {
		logger = slog.Default()
	}

	var cfg runContextConfig
	for _, o := range opts {
		o(&cfg)
	}
	cfg.dispositionPolicy = cfg.dispositionPolicy.Clone()

	// Run-level output schema — compiled ONCE here (the runtime edge) so
	// the same compiled validator serves both the planner's per-turn
	// generation steering and the runtime-edge final validation. A
	// compile failure fails the run loudly (no silent degradation to an
	// unconstrained run, CLAUDE.md §13).
	var outputSchema *planner.OutputSchemaValidator
	if len(src.OutputSchema) > 0 {
		compiled, err := planner.CompileOutputSchema(src.OutputSchema)
		if err != nil {
			return planner.RunContext{}, fmt.Errorf("runctx: output schema: %w", err)
		}
		outputSchema = compiled
	}

	// Identity-attached projection ctx: the skills Directory (and the
	// identity-scoped artifact reads) read the triple from ctx. Preserve the
	// historical empty-RunID path for callers that do not install a per-run
	// reader snapshot; otherwise retain the complete quadruple so the reader
	// binding can verify its exact run. A failure here is a programmer error
	// (the identity already validated above) — fail loud.
	projCtx, err := identity.With(ctx, q.Identity)
	if q.RunID != "" {
		projCtx, err = identity.WithRun(ctx, q.Identity, q.RunID)
	}
	if err != nil {
		return planner.RunContext{}, fmt.Errorf("runctx: NewRunContext identity ctx: %w", err)
	}
	if cfg.skillReader != nil {
		if cfg.skillReader.Quadruple() != q {
			return planner.RunContext{}, fmt.Errorf("runctx: skill reader snapshot: %w", skills.ErrInvalidRunSkillReaderSnapshot)
		}
		projCtx = skills.WithRunSkillReaderSnapshot(projCtx, *cfg.skillReader)
	}

	// Session-scoped quadruple (RunID zeroed): memory + skills span runs
	// within a session, so the projection reads the session's
	// accumulated state rather than only this (empty) run's slice.
	sessionQ := identity.Quadruple{Identity: q.Identity}

	filter := tools.CatalogFilter{
		TenantID:      q.TenantID,
		UserID:        q.UserID,
		SessionID:     q.SessionID,
		GrantedScopes: src.GrantedScopes,
	}

	// Memory projection — the SAME helper the drivers call.
	var memBlocks *planner.MemoryBlocks
	if src.Memory != nil {
		mb, err := FetchMemoryBlocks(projCtx, src.Memory, sessionQ, goal, src.MemoryRecall, logger)
		if err != nil {
			return planner.RunContext{}, fmt.Errorf("runctx: memory projection: %w", err)
		}
		memBlocks = mb
	}

	// Catalog view — the promoted planner-facing projection under the
	// run's identity + granted-scope filter (schemas only, never raw
	// internals).
	var catalogView planner.ToolCatalogView
	if src.Catalog != nil {
		catalogView = tools.NewPlannerView(src.Catalog, filter)
	}

	// Skills projection — the bounded Directory view under the run's
	// visible-tool capability set, projected via the SAME helper the
	// drivers call.
	var skillsCtx []any
	if src.SkillsDirectory != nil {
		views, err := src.SkillsDirectory.View(projCtx, skills.DirectoryCapability{
			AllowedTools: tools.VisibleNames(src.Catalog, filter),
		})
		if err != nil {
			return planner.RunContext{}, fmt.Errorf("runctx: skills projection: %w", err)
		}
		skillsCtx = ProjectSkillsDirectory(views)
	}

	// Streaming surface — the identity-stamping telemetry emitter and
	// the per-token chunk publisher, both bus-backed and bounded by the
	// caller's ctx. Left nil when no bus is wired.
	var emit func(events.Event)
	var onChunk func(delta string, done bool, kind planner.ChunkKind)
	if src.Bus != nil {
		emit = events.IdentityStampingEmitterContext(projCtx, src.Bus, q, logger)
		chunkPub := llm.NewChunkPublisherContext(projCtx, src.Bus, q, q.RunID, logger)
		onChunk = func(delta string, done bool, kind planner.ChunkKind) {
			chunkPub(delta, done, string(kind))
		}
	}

	// Input-artifact projection — the SAME thin caller the drivers use
	// (per-attachment disposition resolved by the planner-homed pure
	// resolver; this helper adds only the I/O + logging/emission).
	inputArtifacts := ResolveInputArtifacts(projCtx, src.Artifacts, q, cfg.inputArtifactIDs, logger, InputArtifactOptions{
		Hints:   DispositionHints(cfg.inputDispositions),
		Policy:  cfg.dispositionPolicy,
		Catalog: catalogView,
		Emit:    emit,
	})

	return planner.RunContext{
		Quadruple:         q,
		Query:             goal,
		Goal:              goal, // initial goal = the request; runtime REDIRECT may mutate
		LLMOverrides:      src.LLMOverrides,
		MemoryBlocks:      memBlocks,
		SkillsContext:     skillsCtx,
		RepairCounters:    &planner.RepairCounters{},
		PlanningHints:     src.PlanningHints,
		Catalog:           catalogView,
		Trajectory:        &planner.Trajectory{Query: goal},
		Emit:              emit,
		OnChunk:           onChunk,
		InputArtifacts:    inputArtifacts,
		DispositionPolicy: cfg.dispositionPolicy,
		Budget:            src.Budget,
		OutputSchema:      outputSchema,
	}, nil
}
