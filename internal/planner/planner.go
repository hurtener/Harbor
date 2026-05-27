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
	"encoding/json"
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
	// Quadruple is the (tenant, user, session, run) identity scope.
	// The triple (Identity field) is the isolation boundary; RunID is
	// the per-execution scope inside a session.
	Quadruple identity.Quadruple

	// Query is the user-facing query that started the run. Set once
	// at run start; never mutated.
	Query string

	// Goal is the current planner-visible goal. May be redirected by
	// a REDIRECT control signal; the Runtime updates the field
	// between planner steps.
	Goal string

	// LLMContext is the visible-to-LLM context (memories, system
	// notes, prior turn summaries). The Runtime populates from the
	// MemoryView + steering injections; the Planner reads only.
	LLMContext map[string]any

	// ToolContext is the tool-only handle bundle. Phase 43 closes
	// the split (serialisable half + handle registry); Phase 42
	// ships the skeleton.
	ToolContext ToolContext

	// Trajectory is the append-only execution log. The Planner reads
	// the history (compaction artefact, prior steps); the Runtime
	// appends each step. Phase 43 closes the fail-loudly Serialize
	// contract; Phase 42 ships the type skeleton with a stub Serialize
	// that returns ErrTrajectoryNotImplemented.
	Trajectory *Trajectory

	// Hints are caller-provided ordering / parallel / budget nudges.
	// Planners MAY honour them; the Runtime enforces the hard caps
	// (system absolute_max_parallel, identity-tier budget) outside
	// the planner.
	Hints PlanningNudges

	// RepairCounters carries the per-run across-step failure counters
	// the runtime increments when Phase 44's schema-repair pipeline had
	// to fix an LLM-output-format failure, and resets on a clean turn at
	// that surface (Phase 83c — D-145). Nil means "no augmentation": the
	// ReAct prompt builder renders no repair guidance.
	//
	// The pointer is owned by the runtime, which constructs ONE
	// RepairCounters per run and threads the SAME pointer through every
	// per-step RunContext. The counters live here — on the per-run
	// scope — and NEVER on the planner struct: a mutable counter field
	// on the shared `ReActPlanner` artifact would violate the D-025
	// concurrent-reuse contract (CLAUDE.md §5 + §13). See D-145.
	RepairCounters *RepairCounters

	// PlanningHints carries runtime-supplied planning constraints the
	// ReAct prompt builder renders into the `<planning_constraints>`
	// section (Phase 83a anchor; Phase 83c body — D-145). Nil means "no
	// hints": the optional section is omitted from the prompt entirely.
	//
	// Unlike Hints (caller nudges the planner MAY honour), PlanningHints
	// is operator/runtime steering: tenant-specific policy, or guiding
	// the planner around a known-bad path, without forking the prompt
	// (brief 13 §2.5). The Runtime populates it from the per-run options;
	// the planner reads only.
	PlanningHints *PlanningHints

	// Catalog is the planner-facing tool view (schemas only — never
	// Descriptors). Phase 26 ships the production ToolCatalog; the
	// runtime engine phase wires a ToolCatalogView adapter.
	Catalog ToolCatalogView

	// Memory is the declared-policy memory view. The runtime engine
	// phase wires a MemoryView adapter over the production MemoryStore.
	Memory MemoryView

	// Skills is the skills subsystem's search/get surface. Phase 37
	// (parallel to Phase 42 — Wave 8 Stage A) ships the production
	// surface; Phase 42 declares the planner-facing view.
	Skills SkillLookup

	// Artifacts is the artifact store. Heavy outputs MUST round-trip
	// through ArtifactRef per D-022 + D-026; the planner is the
	// caller responsible for upgrading inline bytes to refs at the
	// LLM edge.
	Artifacts artifacts.ArtifactStore

	// InputArtifacts (Round-7 F11 / D-166) carry the operator-uploaded
	// multimodal inputs the run consumes on its FIRST planner turn —
	// pre-resolved by the run loop (no async I/O inside the planner).
	// The planner's first-turn user-content builder routes each entry
	// by MIME: `image/*` materializes as `llm.ImagePart{DataURL}` with
	// inline base64 bytes (Path 1, D-166); everything else stays as
	// an `ArtifactStub` the LLM routes through the tool catalog.
	// Empty on text-only turns AND on every turn after the first.
	InputArtifacts []InputArtifactView

	// Control is the accumulated steering signals (control events
	// observed since the last planner step). The Planner reads;
	// the Runtime owns the inbox.
	Control ControlSignals

	// Budget is the per-run hard caps: deadline, hop budget, cost cap.
	// The Runtime enforces them; the Planner observes them to make
	// budget-aware decisions.
	Budget Budget

	// Clock is the (typically `time.Now`) clock the Planner reads. A
	// controllable clock lets tests fix time across a planner step.
	Clock func() time.Time

	// Emit publishes onto the event bus with the run's identity
	// quadruple already attached. The Planner uses Emit to surface
	// planner-side telemetry (`planner.decision`, `planner.finish`,
	// `planner.error` — see events.go). May be nil in tests; concretes
	// MUST nil-check.
	Emit func(events.Event)

	// ReasoningReplay is a per-run override for the agent's reasoning-
	// replay policy (Phase 83e — D-148). When nil, the planner uses
	// the agent's configured `config.PlannerConfig.ReasoningReplay`.
	// When non-nil, this value wins — letting a tenant-specific or
	// run-specific policy override the per-agent default (e.g. an
	// operator disabling replay for a cost-sensitive run). The Runtime
	// populates it from the per-run options; the planner reads only.
	ReasoningReplay *ReasoningReplayMode

	// MemoryBlocks carries the pre-fetched, identity-scoped memory
	// blobs the planner injects into the system prompt as UNTRUSTED-
	// framed `<read_only_*_memory>` sections (Phase 83d — D-146). The
	// Runtime populates this from the MemoryStore per its declared
	// scoping policy; the planner only renders. Nil means "no memory
	// to inject" — the wrappers are omitted entirely. The Runtime is
	// responsible for filtering the blob to the run's identity BEFORE
	// it reaches this field; the prompt builder never re-applies
	// identity filtering (RFC §6.6).
	MemoryBlocks *MemoryBlocks

	// SkillsContext carries pre-retrieved skill bodies the runtime
	// resolved for this run (Phase 83d — D-146). The planner renders
	// them into a single UNTRUSTED-framed `<skills_context>` section.
	// Nil/empty means "no skills to inject" — the section is omitted.
	// The element type is `any` for V1: callers may supply
	// `planner.Skill` values, maps, or operator-defined structs; the
	// renderer compact-JSON-encodes whatever is passed. The runtime,
	// not the planner, decides which skills land here (RFC §6.7).
	SkillsContext []any

	// OnReasoning is a per-step callback the Planner invokes with the
	// provider-side reasoning trace captured by the LLM call (Phase 83m
	// item 8). The Runtime sets it on each per-step RunContext so the
	// runloop can copy the trace onto `trajectory.Step.ReasoningTrace`
	// when it appends the step. May be nil — a planner that finds no
	// callback skips the emission silently (no observability surface
	// wired this run).
	//
	// Why a side-channel rather than a field on Decision: the Decision
	// sum is the planner→runtime instruction contract (CallTool,
	// CallParallel, SpawnTask, AwaitTask, RequestPause, Finish) and
	// must stay narrow so future planner concretes (Deterministic,
	// Workflow, Plan-Execute) implement it cleanly without populating
	// a field most planners never produce. Reasoning is per-step
	// observation, not per-step instruction — this seam matches.
	//
	// Concurrent-reuse (D-025): the callback closure is captured per
	// run on the runloop's stack; the planner reads it from rc, never
	// from itself. N concurrent runs see N independent closures.
	OnReasoning func(string)

	// OnChunk is the per-step streaming callback the Planner invokes
	// per token delta from the LLM provider (Phase 107). The Runtime
	// sets it on each per-step RunContext so the runloop publishes
	// `llm.completion.chunk` events. May be nil — a planner without
	// streaming wired skips the emission silently.
	//
	// Concurrent-reuse (D-025): same pattern as OnReasoning — per-run
	// closure on the stack, never on the shared planner artifact.
	OnChunk func(delta string, done bool, kind ChunkKind)

	// DiscoveredTools (Phase 107c / D-167) is the per-run list of
	// tool names the LLM discovered via meta-tools during this run.
	// The React planner reads this to add discovered tools to
	// subsequent turns' Tools[] declarations. Stack-local-per-run
	// (D-025) — never on the planner struct.
	DiscoveredTools []string

	// PendingToolCalls (Phase 107c / D-167) carries remaining
	// serialized native ToolCalls when the LLM emits N>1 calls in
	// one response (AC-19 serialization fallback). The planner
	// consumes PendingToolCalls before consulting the LLM again.
	// Stack-local-per-run (D-025).
	PendingToolCalls []ToolCallDeferred

	// OnPendingToolCalls (Phase 107c / D-167 — AC-19 + AC-19a) is the
	// per-step callback the planner invokes BEFORE returning a
	// Decision to surface the post-step `PendingToolCalls` queue back
	// to the runloop. rc is passed BY VALUE to Next; without this
	// bridge any append to `rc.PendingToolCalls` inside Next dies
	// with the planner's stack frame, and the AC-19 multi-ToolCall
	// serialisation fallback becomes dead code. The runloop captures
	// a stack-local slice via the closure (D-025: per-run, never on
	// the planner artifact) and writes it back into `spec.Base` so
	// the next iteration's value-copy carries the queue forward.
	//
	// Nil callback is a no-op (tests that exercise Next directly
	// without a runloop). Operators should never set it; the
	// runloop owns the closure.
	OnPendingToolCalls func([]ToolCallDeferred)
}

// ToolCallDeferred is a pending native tool-call the planner will
// dispatch on the next step (AC-19 serialization fallback).
type ToolCallDeferred struct {
	Name    string
	Args    json.RawMessage
	CallID  string
}

// ChunkKind is a sealed enum for the streaming-chunk kind (Phase 107).
// Values: ChunkContent (model output text), ChunkReasoning (thinking
// trace — Phase 107a renders).
type ChunkKind string

const (
	ChunkContent   ChunkKind = "content"
	ChunkReasoning ChunkKind = "reasoning"
)

// MemoryBlocks carries the two memory tiers the ReAct planner injects
// into its system prompt with UNTRUSTED anti-prompt-injection framing
// (Phase 83d — D-146). Both fields are optional: a nil tier is omitted
// from the prompt entirely (no empty wrapper is rendered).
//
// The fields are typed `any` deliberately. The Runtime's MemoryStore
// (Phase 23) and memory strategies (Phase 24) produce free-form
// structured blobs whose shape is policy-dependent — a struct, a map,
// a slice of entries. The prompt builder compact-JSON-encodes whatever
// is supplied; a value `json.Marshal` rejects fails loudly with
// [ErrMemoryBlockUnserializable] rather than degrading to an empty
// wrapper.
//
// Identity contract: the Runtime MUST have already filtered each blob
// to the run's `(tenant, user, session)` scope before populating this
// struct. The prompt builder never re-applies identity filtering — it
// renders exactly what it is handed (RFC §6.6, CLAUDE.md §6).
type MemoryBlocks struct {
	// External is the long-term / retrieved memory tier. Rendered into
	// the `<read_only_external_memory>` prompt section. Nil to omit.
	External any
	// Conversation is the short-term / session memory tier. Rendered
	// into the `<read_only_conversation_memory>` prompt section. Nil
	// to omit.
	Conversation any
}

// ReasoningReplayMode controls whether the ReAct planner re-injects a
// prior step's captured reasoning trace into the next turn's prompt
// (Phase 83e — D-148). The predecessor never replayed; Harbor's stance
// is never-replay default for ALL models, with a per-agent operator
// opt-in. V1 ships exactly two modes — no `provider_native` mode,
// because Bifrost's docs do not address signed-thinking-block round-
// trips.
type ReasoningReplayMode string

// Reasoning replay modes (D-148). The zero value is deliberately the
// empty string, which `EffectiveReasoningReplay` resolves to
// `ReasoningReplayNever` — replay is OFF unless an operator opts in.
const (
	// ReasoningReplayNever — the trajectory renderer emits prior
	// `{tool, args}` JSON only; captured reasoning is never re-injected
	// into prompts. The default for ALL models.
	ReasoningReplayNever ReasoningReplayMode = "never"
	// ReasoningReplayText — the trajectory renderer prepends each prior
	// step's captured reasoning trace as a text block ABOVE the prior
	// `{tool, args}` JSON in the assistant turn. Per-agent operator
	// opt-in for workloads that benefit from CoT continuity.
	ReasoningReplayText ReasoningReplayMode = "text"
)

// IsValidReasoningReplayMode reports whether m is one of the canonical
// replay modes. The empty string is accepted — it is the unset
// sentinel that resolves to `ReasoningReplayNever`. Config validation
// uses this to reject typo'd values loudly pre-boot.
func IsValidReasoningReplayMode(m ReasoningReplayMode) bool {
	switch m {
	case "", ReasoningReplayNever, ReasoningReplayText:
		return true
	default:
		return false
	}
}

// EffectiveReasoningReplay resolves the replay mode in effect for a
// planner step (D-148). The per-run `RunContext.ReasoningReplay`
// override wins when non-nil; otherwise the agent-configured
// `configured` value applies. An empty `configured` value — or an
// empty override — resolves to `ReasoningReplayNever`: replay is OFF
// unless an operator explicitly opted in. Any non-canonical value also
// resolves to `ReasoningReplayNever` (fail-closed; config validation
// already rejects bad values pre-boot, so this is defence in depth).
func EffectiveReasoningReplay(rc RunContext, configured ReasoningReplayMode) ReasoningReplayMode {
	mode := configured
	if rc.ReasoningReplay != nil {
		mode = *rc.ReasoningReplay
	}
	switch mode {
	case ReasoningReplayText:
		return ReasoningReplayText
	default:
		return ReasoningReplayNever
	}
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
	// Cancelled is true when a CANCEL control event was observed for
	// this run. The Planner SHOULD return Finish{Reason: Cancelled}
	// at the next step boundary.
	Cancelled bool
	// PauseRequested is true when a PAUSE control event was observed.
	// The Planner SHOULD return RequestPause{Reason: AwaitInput} at
	// the next step boundary.
	PauseRequested bool
	// InjectedContext carries INJECT_CONTEXT payloads accumulated
	// since the last planner step. The Planner SHOULD merge them
	// into its prompt construction.
	InjectedContext []map[string]any
	// UserMessages carries USER_MESSAGE payloads accumulated since
	// the last planner step.
	UserMessages []string
	// RedirectGoal is non-empty when a REDIRECT control event
	// updated the goal between planner steps.
	RedirectGoal string
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

// InputArtifactView is a pre-resolved multimodal input artifact the
// planner consumes on its first turn. Round-7 F11 / D-166 — the run
// loop reads the operator-supplied IDs from `tasks.Task.InputArtifactIDs`,
// looks each one up in the ArtifactStore, and (for `image/*` MIMEs)
// pre-fetches the bytes so the planner can construct `llm.ImagePart`
// with `DataURL` inline without async I/O. For non-image MIMEs the
// `Bytes` slot stays nil — the planner emits an `ArtifactStub` text
// block instead, and the LLM routes to whichever tool advertises the
// MIME via `tools.Tool.HandlesMIME`.
//
// The shape is intentionally narrow: the planner's prompt assembly
// is synchronous, so every field it needs lives here. Future expansion
// (e.g. summaries, audio waveform thumbnails) extends the struct
// rather than reaching back into the artifact store.
type InputArtifactView struct {
	// ID is the content-addressed artifact identifier.
	ID string
	// MIME is the artifact's IANA media type. Drives the per-MIME
	// dispatcher in the materializer.
	MIME string
	// SizeBytes is the artifact's byte length (metadata, not the bytes).
	SizeBytes int64
	// Filename is the operator-supplied original filename, if known.
	// Surfaced to the LLM as a hint when the provider supports it.
	Filename string
	// Bytes is the materialized payload, populated by the run loop
	// for `image/*` MIMEs (the Path 1 inline-bytes case). Nil for
	// every other MIME (the LLM sees an ArtifactStub ref instead).
	Bytes []byte
}

// PlanningNudges are caller-provided nudges the planner MAY honour.
// The Runtime hard caps win in every case.
//
// Phase 83c renamed this type from `PlanningHints` to free that name
// for the richer runtime-supplied [PlanningHints] struct rendered into
// the `<planning_constraints>` prompt section (D-145). PlanningNudges
// stays the type of [RunContext.Hints] — the legacy parallel/transport
// nudge surface; [PlanningHints] is the new operator-steering surface.
type PlanningNudges struct {
	// MaxParallel hints the maximum CallParallel branch count the
	// planner should produce. The Runtime's system cap
	// (absolute_max_parallel = 50, per RFC §6.2 / Phase 47) wins.
	// Zero means no hint.
	MaxParallel int
	// PreferTransport hints the planner should prefer tools of the
	// given transport kind when multiple tools satisfy the same
	// goal. Empty means no preference.
	PreferTransport string
}

// RepairCounters carries the per-run, across-step failure counters
// that drive the ReAct planner's escalating repair guidance (Phase
// 83c — D-145). Each counter tracks one class of LLM-output-format
// failure the runtime had to repair:
//
//   - FinishRepair  — a `_finish` action that failed Phase 44
//     validation (malformed finish args, missing answer).
//   - ArgsRepair    — a tool-call action whose args failed the tool's
//     schema validation.
//   - MultiAction   — the LLM emitted more than one JSON action block
//     in a single turn (multi-action / multi-JSON emission).
//
// The runtime increments the matching counter when Phase 44's repair
// pipeline had to fix an output, and resets it when a clean turn
// lands at that surface (a clean finish resets FinishRepair; a clean
// single-action-with-valid-args turn resets ArgsRepair AND
// MultiAction). The ReAct prompt builder reads the counters per turn
// and merges escalating `reminder → warning → critical` guidance into
// the system prompt for that turn only — closing the across-step
// feedback loop Phase 44's per-step repair leaves open (brief 13
// §2.2).
//
// **Concurrent-reuse (D-145 + D-025).** RepairCounters lives on the
// per-run [RunContext], NEVER on the `ReActPlanner` struct. The
// runtime constructs one RepairCounters per run and threads the same
// pointer through every per-step RunContext; the shared planner
// artifact stays immutable. A counter field on the planner would be a
// §13-forbidden mutable-state-on-a-compiled-artifact bug.
//
// **Parallel-branch failures do NOT increment these counters.** A
// `parallel` plan whose branches fail at tool execution is a
// tool-execution failure, not an LLM-output-format failure — the
// counters track only the latter (Phase 83c non-goal; see
// repair_guidance.go).
type RepairCounters struct {
	// FinishRepair counts consecutive `_finish` actions that failed
	// Phase 44 validation. Reset to 0 on a clean finish.
	FinishRepair int
	// ArgsRepair counts consecutive tool-call actions whose args
	// failed schema validation. Reset to 0 on a clean single action.
	ArgsRepair int
	// MultiAction counts consecutive turns that emitted more than one
	// JSON action block. Reset to 0 on a clean single action.
	MultiAction int
}

// BudgetHints carries the optional budget caps a runtime may surface
// to the planner through [PlanningHints.Budget] (Phase 83c — D-145).
// Every field is a pointer: nil means "no cap for this dimension", so
// a partial BudgetHints renders only the dimensions it pins. The
// caps are advisory prompt content — the runtime's Governance
// subsystem (Phase 47+) and [Budget] enforce the hard caps; the
// planner reads BudgetHints to make budget-aware decisions.
type BudgetHints struct {
	// MaxSteps is the advisory planner-step ceiling. Nil = no hint.
	MaxSteps *int
	// MaxCostUSD is the advisory cost ceiling, in USD. Nil = no hint.
	MaxCostUSD *float64
	// MaxLatencyMS is the advisory wall-clock latency ceiling, in
	// milliseconds. Nil = no hint.
	MaxLatencyMS *int64
}

// PlanningHints carries runtime-supplied planning constraints the
// ReAct prompt builder renders into the `<planning_constraints>`
// section of the system prompt (Phase 83c — D-145). It is the
// operator/runtime steering surface: tenant-specific policy, or
// guiding the planner around a known-bad path, expressed as prompt
// content rather than Go code (brief 13 §2.5).
//
// Every field is optional. An empty field is omitted from the
// rendered section entirely — never emitted as an empty line. A
// PlanningHints whose every field is empty renders the empty string,
// so the prompt builder omits the `<planning_constraints>` section.
//
// PlanningHints is distinct from [PlanningNudges] ([RunContext.Hints]):
// nudges are the legacy parallel/transport hints the planner MAY
// honour; PlanningHints is the richer runtime-steering surface
// rendered directly into the prompt.
type PlanningHints struct {
	// Constraints is free-form constraint text rendered verbatim.
	Constraints string
	// PreferredOrder lists tool names the planner should prefer to
	// invoke in the given order.
	PreferredOrder []string
	// ParallelGroups lists groups of tool names that may be invoked
	// in parallel — each inner slice is one independent group.
	ParallelGroups [][]string
	// DisallowTools lists tool names the planner must NOT invoke.
	DisallowTools []string
	// PreferredTools lists tool names the planner should prefer when
	// multiple tools satisfy the same goal.
	PreferredTools []string
	// Budget carries advisory budget caps. Nil = no budget hints.
	Budget *BudgetHints
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
