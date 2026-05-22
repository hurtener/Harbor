// Package planner ships Harbor's swappable reasoning-policy seam.
//
// The Runtime owns mechanism (sessions, runs, tasks, events, streaming,
// pause/resume, artifacts, tool execution, memory injection); the
// Planner owns policy (next-action selection, tool choice, finish
// detection). The contract is a single interface:
//
//	type Planner interface {
//	    Next(ctx context.Context, run RunContext) (Decision, error)
//	}
//
// `Decision` is a sealed sum-type with six shapes (CallTool,
// CallParallel, SpawnTask, AwaitTask, RequestPause, Finish — see
// decision.go). The Runtime executes the decision; the Planner never
// reaches into Runtime internals. Tools, memory, skills, artifacts,
// pause/resume, steering — every capability the planner can read is
// reachable through `RunContext`, the only surface the planner sees.
//
// Phase 42 ships the interface + the sum + the views + a stub
// finish.Planner that always returns Finish{Reason: Goal}. Phase 45
// ships the reference ReAct concrete; Phase 48 ships the deterministic
// concrete. The conformance harness skeleton (Phase 49) lives in
// internal/planner/conformance/.
//
// Import-graph contract (binding — see CLAUDE.md §1 + §13):
// `internal/planner/...` MUST NOT import `internal/runtime/...`. The
// conformance/importgraph_test.go walks every Go file under the
// planner subtree and fails the build on a `internal/runtime/...`
// import.
//
// Concurrent-reuse contract (D-025): every concrete Planner MUST be
// safe to share across N concurrent goroutines. Per-run state lives in
// `ctx` + `RunContext`, never on the receiver. See concurrent_test.go
// for the N=128 reuse test the stub planner passes.
//
// Wake-on-resolution contract (D-032): when a planner emits a
// `SpawnTask` without retain-turn, it MUST consume
// `tasks.TaskRegistry.WatchGroup` to learn when the group resolves.
// The three modes (push / poll / hybrid) are documented at the
// `internal/tasks/groups.go` package godoc; Phase 42's `WakeMode` enum
// + optional `WakeAware` interface let concretes declare which mode
// they use so the conformance pack can assert the round-trip.
package planner

import (
	"context"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

// Planner is the swappable reasoning-policy contract. Implementations
// MUST be safe to share across N concurrent goroutines (D-025): a
// shared Planner instance receives many calls; per-run state lives in
// `ctx` + the `RunContext` argument, never on the receiver.
//
// `Next` returns ONE Decision per call. The Runtime executes the
// decision and re-invokes Next with the resulting trajectory. The
// Runtime owns the loop; the Planner owns the policy.
type Planner interface {
	Next(ctx context.Context, run RunContext) (Decision, error)
}

// RunContext is the only surface the Planner sees. All fields are
// either value types, narrow read interfaces, or function closures —
// never concrete Runtime structs. The Runtime constructs a fresh
// RunContext per planner step; reading from `ctx` for cancellation,
// `Quadruple` for identity, and the view interfaces for tools / memory
// / skills / artifacts is the entire API surface.
//
// The Runtime is responsible for:
//
//   - Wiring `Catalog` to a visibility-filtered ToolCatalogView.
//   - Wiring `Memory` to a declared-policy MemoryView.
//   - Wiring `Skills` to the skills subsystem's lookup surface.
//   - Wiring `Artifacts` to the production ArtifactStore.
//   - Populating `Control` with the accumulated steering signals.
//   - Setting `Budget` from the per-run options.
//   - Providing `Clock` (typically `time.Now`).
//   - Providing `Emit` that publishes onto the EventBus with the
//     run's identity quadruple attached.
//
// The Planner is responsible for:
//
//   - Reading from RunContext, never writing back.
//   - Returning a Decision (one of six shapes — see decision.go).
//   - Never blocking on the Runtime's internals. Long-running work
//     ALWAYS goes through SpawnTask / AwaitTask, not via a goroutine
//     spawned inside Next.
type RunContext struct {
	Catalog     ToolCatalogView
	Artifacts   artifacts.ArtifactStore
	Skills      SkillLookup
	Memory      MemoryView
	LLMContext  map[string]any
	Trajectory  *Trajectory
	Clock       func() time.Time
	Emit        func(events.Event)
	Control     ControlSignals
	Quadruple   identity.Quadruple
	Hints       PlanningHints
	Goal        string
	Query       string
	ToolContext ToolContext
	Budget      Budget
}

// ToolCatalogView is the planner-facing read view over the production
// ToolCatalog (Phase 26). The view exposes schemas only — never
// ToolDescriptors — so the planner cannot dispatch tools directly.
// The Runtime owns dispatch; the Planner returns CallTool decisions.
//
// Implementations MUST already apply visibility filtering — the
// planner sees the set of tools the run's identity may call, not the
// full catalog.
type ToolCatalogView interface {
	// Resolve returns the Tool by name and a presence bool. The Tool
	// value carries schemas, transport kind, side-effect class, and
	// cost / latency hints — everything the planner needs to make a
	// CallTool decision without reaching into the descriptor.
	Resolve(name string) (tools.Tool, bool)

	// List returns every tool visible to the run. The slice ordering
	// is the catalog's natural order (typically registration order);
	// planners that need a stable ordering MUST sort the result.
	List() []tools.Tool
}

// MemoryView is the planner-facing read view over the declared-policy
// memory snapshot. The Runtime constructs a MemoryView at planner-step
// start from the production MemoryStore + scoping policy; the Planner
// reads the snapshot, never queries the store directly.
type MemoryView interface {
	// Snapshot returns the memory entries visible to the planner
	// step. The map shape is intentionally opaque at Phase 42 — the
	// production MemoryView adapter (later wave) defines the keying
	// convention. Empty map + nil error is the no-memory case.
	Snapshot(ctx context.Context) (map[string]any, error)
}

// SkillLookup is the planner-facing read view over the skills subsystem.
// Phase 37 ships the production surface; Phase 42 declares the planner-
// facing shape so the planner package compiles without importing
// internal/skills (parallel fork at Wave 8 Stage A).
type SkillLookup interface {
	// Search returns up to `limit` skills matching `query`. Empty
	// slice + nil error is the no-match case.
	Search(ctx context.Context, query string, limit int) ([]SkillResult, error)

	// Get returns the full skill by id, or (nil, nil) on miss.
	Get(ctx context.Context, id string) (*Skill, error)
}

// Skill is the planner-facing projection of a skill record. The
// production internal/skills package (Phase 37+) defines the full
// record shape; the planner only needs the Name / Description / Body
// to compose an LLM prompt and the optional ToolTemplates for
// auto-instantiated tools.
type Skill struct {
	// ID is the skill's stable identifier (provider-namespaced).
	ID string
	// Name is the human-readable name.
	Name string
	// Description is the one-line summary the planner shows the LLM.
	Description string
	// Body is the skill's prompt-injection content.
	Body string
	// Tags categorise the skill for filtering / search.
	Tags []string
}

// SkillResult is the search projection — a hit with a relevance score.
type SkillResult struct {
	Skill
	// Score is the search backend's relevance score, in [0.0, 1.0].
	// Higher is more relevant.
	Score float64
}

// ControlSignals carries the steering observations the planner sees
// at step start. The Runtime owns the inbox; the Planner reads.
//
// Phase 42 ships a minimal struct — the unified pause/resume primitive
// + steering subsystem (later phases) populate the fields. Concrete
// signals (Cancel, Pause, Approve, Reject, InjectContext, Redirect,
// UserMessage, Prioritize, External) are observed via the typed
// slices; planners react in their Next implementation.
type ControlSignals struct {
	RedirectGoal    string
	InjectedContext []map[string]any
	UserMessages    []string
	Cancelled       bool
	PauseRequested  bool
}

// Budget carries the per-run hard caps the planner observes. The
// Runtime enforces them outside the planner — the planner reads to
// make budget-aware decisions (e.g. choose a cheaper model when
// CostRemaining is low).
type Budget struct {
	// Deadline is the absolute wall-clock deadline for the run. Zero
	// value means no deadline. The Runtime's ctx is set to expire at
	// Deadline; the planner SHOULD honour ctx.Err() between long
	// phases of work.
	Deadline time.Time
	// HopBudget is the maximum number of planner steps remaining.
	// Negative means no cap. Decrements per planner step.
	HopBudget int
	// CostCap is the maximum LLM cost (USD-equivalent micros) for
	// the run. Zero means no cap. The Runtime's Governance subsystem
	// enforces; the planner reads.
	CostCap int64
	// CostSpent is the cost accumulated so far this run. Same units
	// as CostCap.
	CostSpent int64
	// TokenBudget is the maximum estimated token count the planner-
	// observed trajectory may carry before the runtime invokes the
	// trajectory summariser (Phase 46). Zero means no token-budget
	// enforcement; the trajectory grows unbounded.
	//
	// The runtime's [trajectory.CompressionRunner] reads this field
	// and, when exceeded, invokes the configured [trajectory.Summariser]
	// to produce a [trajectory.TrajectorySummary] that replaces the raw
	// step history in subsequent prompt builds (RFC §6.2, brief 02 §4,
	// D-055). Compression is a runtime concern; the planner sees only
	// the compacted view via [RunContext.Trajectory.Summary].
	TokenBudget int
}

// PlanningHints are caller-provided nudges the planner MAY honour.
// The Runtime hard caps win in every case.
type PlanningHints struct {
	PreferTransport string
	MaxParallel     int
}

// PauseReason is the planner-side enum mirroring RFC §6.3's pause
// taxonomy. The unified pause/resume primitive package (later phase)
// MAY canonicalise via a typedef bridge; the enum values match the
// canonical strings exactly.
type PauseReason string

// Pause reasons (RFC §6.3 — settled).
const (
	// PauseApprovalRequired — a human needs to approve a planner-
	// chosen tool call before execution.
	PauseApprovalRequired PauseReason = "approval_required"
	// PauseAwaitInput — the planner needs additional input from the
	// user / supervisor before continuing.
	PauseAwaitInput PauseReason = "await_input"
	// PauseExternalEvent — the run is waiting on an external event
	// (webhook, scheduled trigger, A2A callback).
	PauseExternalEvent PauseReason = "external_event"
	// PauseConstraintsConflict — the planner detected a constraint
	// conflict (budget vs. tool requirement, identity scope mismatch)
	// that requires operator resolution.
	PauseConstraintsConflict PauseReason = "constraints_conflict"
)

// IsValidPauseReason reports whether r is one of the four canonical
// pause reasons. Used by the conformance pack to verify
// RequestPause.Reason is well-formed.
func IsValidPauseReason(r PauseReason) bool {
	switch r {
	case PauseApprovalRequired, PauseAwaitInput, PauseExternalEvent, PauseConstraintsConflict:
		return true
	default:
		return false
	}
}

// FinishReason is the planner-side enum for the terminal reason a
// run finished. The Runtime maps FinishReason → Protocol
// `task.completed` / `task.failed` payloads.
type FinishReason string

// Finish reasons.
const (
	// FinishGoal — the planner satisfied the user goal. The
	// canonical success terminal.
	FinishGoal FinishReason = "goal"
	// FinishNoPath — the planner could not find a path to the goal
	// (schema repair exhausted, no tool satisfies the requirement,
	// constraint conflict). Phase 44 emits this from the repair
	// pipeline's graceful-failure path.
	FinishNoPath FinishReason = "no_path"
	// FinishCancelled — the run was cancelled (CANCEL control event,
	// parent task cascade, deadline expiration honoured early).
	FinishCancelled FinishReason = "cancelled"
	// FinishDeadlineExceeded — the run hit its Budget.Deadline.
	FinishDeadlineExceeded FinishReason = "deadline_exceeded"
	// FinishConstraintsConflict — the run terminated because a
	// constraint conflict could not be resolved (operator denied an
	// approval; budget cap reached during a required tool call).
	FinishConstraintsConflict FinishReason = "constraints_conflict"
)

// IsValidFinishReason reports whether r is one of the canonical
// finish reasons.
func IsValidFinishReason(r FinishReason) bool {
	switch r {
	case FinishGoal, FinishNoPath, FinishCancelled, FinishDeadlineExceeded, FinishConstraintsConflict:
		return true
	default:
		return false
	}
}
