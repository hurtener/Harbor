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
// Harbor ships the interface + the sum + the views + a stub
// finish.Planner that always returns Finish{Reason: Goal}. Harbor
// ships the reference ReAct concrete; Harbor ships the deterministic
// concrete. The conformance harness skeleton lives in
// internal/planner/conformance/.
//
// Import-graph contract (binding — see CLAUDE.md §1 + §13):
// `internal/planner/...` MUST NOT import `internal/runtime/...`. The
// conformance/importgraph_test.go walks every Go file under the
// planner subtree and fails the build on a `internal/runtime/...`
// import.
//
// Concurrent-reuse contract: every concrete Planner MUST be
// safe to share across N concurrent goroutines. Per-run state lives in
// `ctx` + `RunContext`, never on the receiver. See concurrent_test.go
// for the N=128 reuse test the stub planner passes.
//
// Wake-on-resolution contract: when a planner emits a
// `SpawnTask` without retain-turn, it MUST consume
// `tasks.TaskRegistry.WatchGroup` to learn when the group resolves.
// The three modes (push / poll / hybrid) are documented at the
// `internal/tasks/groups.go` package godoc; the `WakeMode` enum
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
// MUST be safe to share across N concurrent goroutines: a
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
// The Runtime (assembler) is responsible for:
//
//   - Wiring `Catalog` to a visibility-filtered ToolCatalogView.
//   - Populating `MemoryBlocks` + `SkillsContext` with pre-fetched,
//     identity-scoped content. These are the
//     fields the shipped planner concretes actually read; see the
//     note on `Memory` / `Skills` below.
//   - Wiring `Artifacts` to the production ArtifactStore.
//   - Populating `Control` with the accumulated steering signals.
//   - Setting `Budget` from the per-run options.
//   - Providing `Clock` (typically `time.Now`).
//   - Providing `Emit` that publishes onto the EventBus with the
//     run's identity quadruple attached.
//
// The `Memory` (MemoryView) and `Skills` (SkillLookup) view fields
// are reserved for future planner drivers that pull memory / skills
// mid-run: NO shipped planner concrete reads them today (the react
// planner consumes only the pre-fetched `MemoryBlocks` /
// `SkillsContext` — SDK friction audit,
// docs/notes/sdk-friction-audit.md §3). Assemblers should populate
// these fields; wiring the views is optional until a driver
// consumes them.
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

	// ToolContext is the tool-only handle bundle. Harbor closes
	// the split (serialisable half + handle registry); Harbor
	// ships the skeleton.
	ToolContext ToolContext

	// Trajectory is the append-only execution log. The Planner reads
	// the history (compaction artefact, prior steps); the Runtime
	// appends each step. Harbor closes the fail-loudly Serialize
	// contract; Harbor ships the type skeleton with a stub Serialize
	// that returns ErrTrajectoryNotImplemented.
	Trajectory *Trajectory

	// Hints are caller-provided ordering / parallel / budget nudges.
	// Planners MAY honour them; the Runtime enforces the hard caps
	// (system absolute_max_parallel, identity-tier budget) outside
	// the planner.
	Hints PlanningNudges

	// RepairCounters carries the per-run across-step failure counters
	// the runtime increments when the schema-repair pipeline had
	// to fix an LLM-output-format failure, and resets on a clean turn at
	// that surface. Nil means "no augmentation": the
	// ReAct prompt builder renders no repair guidance.
	//
	// The pointer is owned by the runtime, which constructs ONE
	// RepairCounters per run and threads the SAME pointer through every
	// per-step RunContext. The counters live here — on the per-run
	// scope — and NEVER on the planner struct: a mutable counter field
	// on the shared `ReActPlanner` artifact would violate the concurrent-reuse
	// concurrent-reuse contract (CLAUDE.md §5 + §13).
	RepairCounters *RepairCounters

	// PlanningHints carries runtime-supplied planning constraints the
	// ReAct prompt builder renders into the `<planning_constraints>`
	// section (anchor; body). Nil means "no
	// hints": the optional section is omitted from the prompt entirely.
	//
	// Unlike Hints (caller nudges the planner MAY honour), PlanningHints
	// is operator/runtime steering: tenant-specific policy, or guiding
	// the planner around a known-bad path, without forking the prompt.
	// The Runtime populates it from the per-run options;
	// the planner reads only.
	PlanningHints *PlanningHints

	// Catalog is the planner-facing tool view (schemas only — never
	// Descriptors). Harbor ships the production ToolCatalog; the
	// runtime engine phase wires a ToolCatalogView adapter.
	Catalog ToolCatalogView

	// Memory is the declared-policy memory view. RESERVED for future
	// planner drivers that pull memory mid-run — no shipped planner
	// concrete reads it (the react planner consumes the pre-fetched
	// `MemoryBlocks` instead; see the type godoc + SDK friction audit
	// §3). Assemblers populate `MemoryBlocks`; wiring this view is
	// optional until a driver consumes it.
	Memory MemoryView

	// Skills is the skills subsystem's search/get surface. RESERVED
	// for future planner drivers that search skills mid-run — no
	// shipped planner concrete reads it (the react planner consumes
	// the pre-fetched `SkillsContext` instead). Assemblers populate
	// `SkillsContext`; wiring this view is optional until a driver
	// consumes it.
	Skills SkillLookup

	// Artifacts is the artifact store. Heavy outputs MUST round-trip
	// through ArtifactRef; the planner is the
	// caller responsible for upgrading inline bytes to refs at the
	// LLM edge.
	Artifacts artifacts.ArtifactStore

	// InputArtifacts carry the operator-uploaded
	// multimodal inputs the run consumes on its FIRST planner turn —
	// pre-resolved by the run loop (no async I/O inside the planner).
	// The planner's first-turn user-content builder routes each entry
	// by MIME: `image/*` materializes as `llm.ImagePart{DataURL}` with
	// inline base64 bytes (Path 1); everything else stays as
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
	// replay policy. When nil, the planner uses
	// the agent's configured `config.PlannerConfig.ReasoningReplay`.
	// When non-nil, this value wins — letting a tenant-specific or
	// run-specific policy override the per-agent default (e.g. an
	// operator disabling replay for a cost-sensitive run). The Runtime
	// populates it from the per-run options; the planner reads only.
	ReasoningReplay *ReasoningReplayMode

	// LLMOverrides carries the per-run, run-start-resolved LLM-parameter
	// overrides the Runtime applies to this run's LLM requests: the model
	// name, sampling temperature, per-call token ceiling, reasoning-effort
	// hint, and an ADDITIVE extra-instructions block appended to the
	// agent's system prompt. Nil means "no overrides" — the planner uses
	// the agent/config defaults (the bound `llm.model`, the provider
	// defaults, the agent's base system prompt).
	//
	// The Runtime resolves these ONCE at run start from the effective
	// override layers (an admin-set tenant default today; a per-session
	// next-turn override above it later) and pins the snapshot here for
	// the whole run — a swap at any layer lands on the NEXT run, never
	// mid-flight (the same next-turn-only invariant the compiled-artifact
	// concurrent-reuse contract requires: a running planner's parameters
	// are fixed for the run; per-run state lives on rc, never on the
	// shared planner artifact). The planner reads only.
	LLMOverrides *LLMOverrides

	// MemoryBlocks carries the pre-fetched, identity-scoped memory
	// blobs the planner injects into the system prompt as UNTRUSTED-
	// framed `<read_only_*_memory>` sections. The
	// Runtime populates this from the MemoryStore per its declared
	// scoping policy; the planner only renders. Nil means "no memory
	// to inject" — the wrappers are omitted entirely. The Runtime is
	// responsible for filtering the blob to the run's identity BEFORE
	// it reaches this field; the prompt builder never re-applies
	// identity filtering (RFC §6.6).
	MemoryBlocks *MemoryBlocks

	// SkillsContext carries pre-retrieved skill bodies the runtime
	// resolved for this run. The planner renders
	// them into a single UNTRUSTED-framed `<skills_context>` section.
	// Nil/empty means "no skills to inject" — the section is omitted.
	// The element type is `any` for V1: callers may supply
	// `planner.Skill` values, maps, or operator-defined structs; the
	// renderer compact-JSON-encodes whatever is passed. The runtime,
	// not the planner, decides which skills land here (RFC §6.7).
	SkillsContext []any

	// OnReasoning is a per-step callback the Planner invokes with the
	// provider-side reasoning trace captured by the LLM call (
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
	// Concurrent-reuse: the callback closure is captured per
	// run on the runloop's stack; the planner reads it from rc, never
	// from itself. N concurrent runs see N independent closures.
	OnReasoning func(string)

	// OnAssistantContent is a per-step callback the Planner invokes
	// with the natural-language content the assistant emitted
	// alongside its tool_call on this step. The Runtime sets it on
	// each per-step RunContext so the runloop copies the preamble
	// onto `trajectory.Step.AssistantPreamble` when it appends the
	// step; the prompt builder then replays it as the assistant
	// message's content on subsequent turns so the model retains
	// its narrative thread.
	//
	// Under native tool-calling the response carries both a
	// `tool_calls` block AND a natural-language `content` field.
	// The Decision sum only encodes the tool_call; this callback
	// is the side-channel that ferries `content` onto the step.
	//
	// Same shape as OnReasoning: nil-safe, per-run closure,
	// invoked by the planner after each LLM call.
	OnAssistantContent func(string)

	// OnChunk is the per-step streaming callback the Planner invokes
	// per token delta from the LLM provider. The Runtime
	// sets it on each per-step RunContext so the runloop publishes
	// `llm.completion.chunk` events. May be nil — a planner without
	// streaming wired skips the emission silently.
	//
	// Concurrent-reuse: same pattern as OnReasoning — per-run
	// closure on the stack, never on the shared planner artifact.
	OnChunk func(delta string, done bool, kind ChunkKind)

	// DiscoveredTools is the per-run list of
	// tool names the LLM discovered via meta-tools during this run.
	// The React planner reads this to add discovered tools to
	// subsequent turns' Tools[] declarations. Stack-local-per-run
	// never on the planner struct.
	DiscoveredTools []string

	// PendingToolCalls carries remaining
	// serialized native ToolCalls when the LLM emits N>1 calls in
	// one response (AC-19 serialization fallback). The planner
	// consumes PendingToolCalls before consulting the LLM again.
	// Stack-local-per-run.
	PendingToolCalls []ToolCallDeferred

	// OnPendingToolCalls (— AC-19 + AC-19a) is the
	// per-step callback the planner invokes BEFORE returning a
	// Decision to surface the post-step `PendingToolCalls` queue back
	// to the runloop. rc is passed BY VALUE to Next; without this
	// bridge any append to `rc.PendingToolCalls` inside Next dies
	// with the planner's stack frame, and the AC-19 multi-ToolCall
	// serialisation fallback becomes dead code. The runloop captures
	// a stack-local slice via the closure (per-run, never on
	// the planner artifact) and writes it back into `spec.Base` so
	// the next iteration's value-copy carries the queue forward.
	//
	// Nil callback is a no-op (tests that exercise Next directly
	// without a runloop). Operators should never set it; the
	// runloop owns the closure.
	OnPendingToolCalls func([]ToolCallDeferred)

	// SessionArtifacts carries the pre-resolved,
	// identity-scoped session-artifact manifest the planner renders into
	// a read-only `<session_artifacts>` prompt block, so the model stays
	// aware of uploads and tool-materialised artifacts across turns and
	// can `artifact_fetch` any of them by ref.
	//
	// The run loop lists `ArtifactStore.List` scoped to the run's
	// `(tenant, user, session)` triple each turn and maps each ref into
	// a metadata-only entry (the run-loop pre-resolution pattern —
	// the planner does NO I/O; it reads only from rc). Nil/empty means
	// "no manifest": the planner omits the `<session_artifacts>` block
	// entirely (no fabricated rows). Newest-first; the renderer caps the
	// rendered rows and appends an explicit "+K more" line on overflow
	// (never a silent truncation, CLAUDE.md §17.6).
	//
	// Concurrent-reuse: the manifest is per-run state on rc,
	// never on the shared planner artifact.
	SessionArtifacts []ArtifactManifestEntry

	// OutputSchema is the optional, opt-in run-level output schema the
	// caller supplied (via the assemble.WithOutputSchema run option).
	// When non-nil the run is a structured-output run: the terminal
	// answer must validate against this schema. A generation-steering
	// planner (the ReAct driver) renders OutputSchema.Raw() into its
	// terminal completion's ResponseFormat and installs a
	// schema-Validator so the corrective retry-with-feedback loop
	// engages; every planner's terminal Finish is additionally validated
	// against this schema at the runtime edge (planner-agnostic, no
	// capability ceremony). Nil means "no schema" — the run is plain
	// text exactly as before.
	//
	// The schema is compiled ONCE per run at the runtime edge and the
	// compiled validator threads here; the planner reads only. Per-run
	// state on rc, never on the shared planner artifact.
	OutputSchema *OutputSchemaValidator
}

// LLMOverrides is the per-run bundle of run-start-resolved LLM-parameter
// overrides carried on [RunContext.LLMOverrides]. Every field is optional
// (a nil pointer / zero-value means "no override for this dimension — use
// the agent/config default"). The Runtime resolves the bundle once at run
// start and the planner reads it when building each LLM request; the
// bundle is immutable for the run.
//
//   - Model overrides the model name the request asks for (validated at
//     set time against the configured `ModelProfiles`).
//   - Temperature / MaxTokens / ReasoningEffort override the corresponding
//     `llm.CompleteRequest` fields.
//   - ExtraInstructions is trusted, tenant-level additive guidance rendered
//     into `<additional_guidance>`; UserPersonalization is the one-run,
//     non-admin contribution rendered separately with structural escaping in
//     `<user_personalization>`. Neither replaces the base prompt.
//   - SystemPromptOverride is a full REPLACE of the agent's base system
//     prompt (the session layer's affordance, distinct from the additive
//     ExtraInstructions). When both are set, the base prompt is replaced
//     AND the additive guidance still appends. An empty string is a valid
//     override (it clears the base prompt for that run); nil = no replace.
//   - BasePromptLayer is the durable, operator-owned base prompt layer
//     resolved from the agent's active config at run start. When set it is
//     the run's base system prompt (overriding the agent's configured
//     default base); nil = inherit the configured default. A session
//     SystemPromptOverride takes precedence over it for its single message.
//   - UserPromptLayer is the durable, optional user-instruction layer
//     resolved from the agent's active config at run start. When set it
//     composes ABOVE the base in a distinctly-framed, lower-trust position
//     — it can extend the operator's guidance but never precede, replace,
//     or weaken the base. A session SystemPromptOverride replaces the whole
//     base+user spine for its single message.
//   - ExtraSystemBlocks are the durable, operator-authored NAMED blocks
//     resolved from the agent's active config at run start. They render
//     VERBATIM, in declared order, into the operator-trusted additive
//     position, each behind a plain-text `[name]` label, and they survive a
//     SystemPromptOverride. Nil or empty renders nothing (no empty
//     wrapper). Order is SEMANTIC — it is the render order.
type LLMOverrides struct {
	Model                *string
	Temperature          *float64
	MaxTokens            *int
	ReasoningEffort      *string
	ExtraInstructions    *string
	UserPersonalization  *string
	SystemPromptOverride *string
	BasePromptLayer      *string
	UserPromptLayer      *string
	ExtraSystemBlocks    []NamedBlock
}

// NamedBlock is one named unit of additive, operator-authored prompt text
// carried on [LLMOverrides.ExtraSystemBlocks]. It exists so N independent
// capability sources can each contribute — and later remove — exactly
// their own text, addressed by name.
//
// The Body renders VERBATIM (unescaped) in the operator-trusted additive
// guidance position. That trust comes from the WRITE DOOR — the durable
// section behind it is admin-only, the same tier that writes the whole
// prompt spine — not from any escaping here. A block must therefore never
// carry user-authored or model-authored text; recalled content belongs in
// the UNTRUSTED-framed [MemoryBlocks] tiers.
//
// The Name is a plain-text label for a human reading a transcript. It is
// never emitted as an XML tag, so the prompt's structural taxonomy is
// never a function of config data. It is NOT a security boundary: to the
// model, two blocks from two capabilities are one contiguous run of
// trusted guidance.
type NamedBlock struct {
	Name string
	Body string
}

// ToolCallDeferred is a pending native tool-call the planner will
// dispatch on the next step (AC-19 serialization fallback).
type ToolCallDeferred struct {
	Name   string
	Args   json.RawMessage
	CallID string
}

// ChunkKind is a sealed enum for the streaming-chunk kind.
// Values: ChunkContent (model output text), ChunkReasoning (thinking
// trace — renders).
type ChunkKind string

const (
	ChunkContent   ChunkKind = "content"
	ChunkReasoning ChunkKind = "reasoning"
)

// MemoryBlocks carries the two memory tiers the ReAct planner injects
// into its system prompt with UNTRUSTED anti-prompt-injection framing.
// Both fields are optional: a nil tier is omitted
// from the prompt entirely (no empty wrapper is rendered).
//
// The fields are typed `any` deliberately. The Runtime's MemoryStore
// and memory strategies produce free-form
// structured blobs whose shape is policy-dependent — a struct, a map,
// a slice of entries. The prompt builder compact-JSON-encodes whatever
// is supplied; a value `json.Marshal` rejects fails loudly with
// [ErrMemoryBlockUnserializable] rather than degrading to an empty
// wrapper.
//
// Identity contract, stated PER PROVENANCE because the tiers have more
// than one producer:
//
//   - STORE-DERIVED content — anything read out of a MemoryStore, a
//     StateStore or any other identity-scoped substrate — MUST have
//     already been filtered to the run's `(tenant, user, session)` scope
//     before it is placed here. The prompt builder never re-applies
//     identity filtering; it renders exactly what it is handed
//     (RFC §6.6, CLAUDE.md §6).
//   - CALLER-SUPPLIED content is not store-derived, so there is no
//     filtering to perform. It arrives in a request body under the
//     caller's own verified triple and is admitted only into the run
//     minted for that same triple. The contract above governs store
//     READS; this path performs none, and content flows in, never out.
//
// This is a sharpening, not a relaxation: nothing that was filtered
// before stops being filtered.
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
// prior step's captured reasoning trace into the next turn's prompt.
// The predecessor never replayed; Harbor's stance
// is never-replay default for ALL models, with a per-agent operator
// opt-in. V1 ships exactly two modes — no `provider_native` mode,
// because Bifrost's docs do not address signed-thinking-block round-
// trips.
type ReasoningReplayMode string

// Reasoning replay modes. The zero value is deliberately the
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
// planner step. The per-run `RunContext.ReasoningReplay`
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
// ToolCatalog. The view exposes schemas only — never
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
	// step. The map shape is intentionally opaque — the
	// production MemoryView adapter (later wave) defines the keying
	// convention. Empty map + nil error is the no-memory case.
	Snapshot(ctx context.Context) (map[string]any, error)
}

// SkillLookup is the planner-facing read view over the skills subsystem.
// Harbor ships the production surface; Harbor declares the planner-
// facing shape so the planner package compiles without importing
// internal/skills (parallel fork at).
type SkillLookup interface {
	// Search returns up to `limit` skills matching `query`. Empty
	// slice + nil error is the no-match case.
	Search(ctx context.Context, query string, limit int) ([]SkillResult, error)

	// Get returns the full skill by id, or (nil, nil) on miss.
	Get(ctx context.Context, id string) (*Skill, error)
}

// Skill is the planner-facing projection of a skill record. The
// production internal/skills package defines the full
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
// Harbor ships a minimal struct — the unified pause/resume primitive
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
	// trajectory summariser (seam; consumer —).
	// Zero means no token-budget enforcement; the trajectory
	// grows unbounded (until the context-window safety net
	// backstops it).
	//
	// The runtime's CompressionRunner reads this field: the steering
	// RunLoop calls [CompressionRunner.MaybeCompress] at each step
	// boundary when TokenBudget > 0 and a runner is configured on the
	// run spec. When the trajectory's estimate exceeds the budget the
	// configured [Summariser] (production: the LLM-backed
	// TrajectorySummariser in internal/llm/summarizer) produces a
	// [TrajectorySummary] that replaces the raw step history in
	// subsequent prompt builds (RFC §6.2). One
	// compression per run at V1.1.x (the runner's Summary != nil
	// idempotence). Production wiring: the `planner.token_budget`
	// config knob projects here via the per-task run-loop drivers.
	// Compression is a runtime concern; the planner sees only the
	// compacted view via the trajectory summary.
	TokenBudget int
}

// InputArtifactView is a pre-resolved multimodal input artifact the
// planner consumes on its first turn. The run
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
	// Disposition is the RESOLVED attachment disposition:
	// how this artifact is handed to the model. The run loop
	// resolves it via [ResolveDisposition] + [EffectiveDisposition]
	// (per-attachment caller hint > per-agent policy map > runtime
	// default); a headless library consumer constructing its own
	// views sets it directly — the consumer IS the top-precedence
	// layer. The zero value selects the pre-84b per-MIME dispatch in
	// the materializer, so existing consumers are byte-for-byte
	// unchanged.
	Disposition AttachmentDisposition
}

// ArtifactManifestEntry is one metadata-only row in the session-artifact
// manifest. The run loop builds the slice from
// `ArtifactStore.List` scoped to the run's `(tenant, user, session)`
// triple and sets it on `RunContext.SessionArtifacts`; the ReAct planner
// renders the rows into the read-only `<session_artifacts>` prompt block
// so the model can `artifact_fetch` any listed `Ref`.
//
// The shape carries NO bytes — content stays in the store and is fetched
// on demand via the `artifact_fetch` meta-tool (heavy-content
// discipline). `Provenance` is the run-loop-resolved origin string
// (e.g. "user_upload", "tool: web_search", "flow: ...") — see the
// run loop's provenance resolver.
type ArtifactManifestEntry struct {
	// Ref is the content-addressed artifact identifier the model passes
	// to `artifact_fetch` to read the artifact's bytes.
	Ref string
	// Filename is the operator-supplied or producer-supplied original
	// filename, if known. May be empty.
	Filename string
	// MIME is the artifact's IANA media type (metadata for the model).
	MIME string
	// SizeBytes is the artifact's byte length (metadata, not the bytes).
	SizeBytes int64
	// Provenance is the human-readable origin of the artifact, resolved
	// by the run loop from the artifact's `Source` map (canonical
	// `source` key else the tool/producer/flow name else "unknown").
	Provenance string
}

// PlanningNudges are caller-provided nudges the planner MAY honour.
// The Runtime hard caps win in every case.
//
// An earlier phase renamed this type from `PlanningHints` to free that name
// for the richer runtime-supplied [PlanningHints] struct rendered into
// the `<planning_constraints>` prompt section. PlanningNudges
// stays the type of [RunContext.Hints] — the legacy parallel/transport
// nudge surface; [PlanningHints] is the new operator-steering surface.
type PlanningNudges struct {
	// MaxParallel hints the maximum CallParallel branch count the
	// planner should produce. The Runtime's system cap
	// (absolute_max_parallel = 50, per RFC §6.2) wins.
	// Zero means no hint.
	MaxParallel int
	// PreferTransport hints the planner should prefer tools of the
	// given transport kind when multiple tools satisfy the same
	// goal. Empty means no preference.
	PreferTransport string
}

// RepairCounters carries the per-run, across-step failure counters
// that drive the ReAct planner's escalating repair guidance (
// ). Each counter tracks one class of LLM-output-format
// failure the runtime had to repair:
//
//   - FinishRepair  — a `_finish` action that failed schema
//     validation (malformed finish args, missing answer).
//   - ArgsRepair    — a tool-call action whose args failed the tool's
//     schema validation.
//   - MultiAction   — the LLM emitted more than one JSON action block
//     in a single turn (multi-action / multi-JSON emission).
//
// The runtime increments the matching counter when the repair
// pipeline had to fix an output, and resets it when a clean turn
// lands at that surface (a clean finish resets FinishRepair; a clean
// single-action-with-valid-args turn resets ArgsRepair AND
// MultiAction). The ReAct prompt builder reads the counters per turn
// and merges escalating `reminder → warning → critical` guidance into
// the system prompt for that turn only — closing the across-step
// feedback loop the per-step repair leaves open.
//
// **Concurrent-reuse.** RepairCounters lives on the
// per-run [RunContext], NEVER on the `ReActPlanner` struct. The
// runtime constructs one RepairCounters per run and threads the same
// pointer through every per-step RunContext; the shared planner
// artifact stays immutable. A counter field on the planner would be a
// §13-forbidden mutable-state-on-a-compiled-artifact bug.
//
// **Parallel-branch failures do NOT increment these counters.** A
// `parallel` plan whose branches fail at tool execution is a
// tool-execution failure, not an LLM-output-format failure — the
// counters track only the latter (non-goal; see
// repair_guidance.go).
type RepairCounters struct {
	// FinishRepair counts consecutive `_finish` actions that failed
	// validation. Reset to 0 on a clean finish.
	FinishRepair int
	// ArgsRepair counts consecutive tool-call actions whose args
	// failed schema validation. Reset to 0 on a clean single action.
	ArgsRepair int
	// MultiAction counts consecutive turns that emitted more than one
	// JSON action block. Reset to 0 on a clean single action.
	MultiAction int
}

// BudgetHints carries the optional budget caps a runtime may surface
// to the planner through [PlanningHints.Budget].
// Every field is a pointer: nil means "no cap for this dimension", so
// a partial BudgetHints renders only the dimensions it pins. The
// caps are advisory prompt content — the runtime's Governance
// subsystem and [Budget] enforce the hard caps; the
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
// section of the system prompt. It is the
// operator/runtime steering surface: tenant-specific policy, or
// guiding the planner around a known-bad path, expressed as prompt
// content rather than Go code.
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
	// PauseMaxStepsExceeded — the runtime's bounded-tranche cap was
	// reached: RunLoop parked the run at its per-tranche MaxSteps
	// boundary, checkpointing the cumulative trajectory, so an
	// operator can RESUME the SAME run for a fresh tranche of
	// planner steps. Emitted by the RunLoop (the tranche park), never
	// by a planner: it is the runtime mechanism's own pause cause —
	// the same-process exact-run continuation surface — distinct
	// from the four planner reasoning reasons above. The typed
	// reason is what lets a RESUME of this pause be told apart from
	// a human-pause resume (only the former grants a fresh tranche).
	PauseMaxStepsExceeded PauseReason = "max_steps"
)

// IsValidPauseReason reports whether r is one of the five canonical
// pause reasons. Used by the conformance pack to verify
// RequestPause.Reason is well-formed.
func IsValidPauseReason(r PauseReason) bool {
	switch r {
	case PauseApprovalRequired, PauseAwaitInput, PauseExternalEvent, PauseConstraintsConflict, PauseMaxStepsExceeded:
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
	// constraint conflict). Harbor emits this from the repair
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
