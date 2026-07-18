# Phase 185 — Batch decision + AC-21 supersession (projector)

## Summary

Adds a fourth sealed `Decision` shape, `Batch{Tools, Spawns, Join}`, so one
native multi-tool-call LLM response can mix catalog-tool branches with
non-retain-turn task spawns — closing the gap where a model batching
`_spawn_task` alongside an ordinary tool call today fails the run loud at
step 0. AC-21 narrows to AC-21′: `_finish` and `_await_task` keep the
standalone guard; `_spawn_task` becomes batchable. This phase ships the
Decision shape, the projector partitioning, the trajectory invocation-count
rule, and the corrected `_spawn_task` prompt description; the executor that
dispatches `Batch` lands in the same wave's next phase.

## RFC anchor

- RFC §6.2
- RFC §6.4

## Briefs informing this phase

- brief 16
- brief 02
- brief 07
- brief 15

## Brief findings incorporated

- brief 16 §1: "AC-21 is the one place still enforcing the old one-intent
  world, and models trained on 'send a single message with multiple tool
  calls' conventions trip it in production (observed live: a model batching
  `_spawn_task` with a catalog tool fails the run at step 0 with
  `ErrInvalidDecision`)." This phase is the direct fix.
- brief 16 §4: the `Batch` shape recommendation — "A fourth sealed shape, not
  a widening of `CallParallel` (whose `len(Branches)` is load-bearing for
  tool-count accounting — a spawn is explicitly NOT a tool invocation,
  `runloop.go:880-888`)" — adopted verbatim as `Batch{Tools []CallTool;
  Spawns []SpawnTask; Join *JoinSpec}`.
- brief 16 §4: "a degenerate one-branch Batch is never constructed (the
  projector prefers the plain shape — one representation per semantic)" —
  adopted as a conformance-tested invariant (`Batch` requires
  `len(Tools)+len(Spawns) ≥ 2`).
- brief 16 §4: "`_finish` and `_await_task` KEEP the standalone guard (a
  terminal decision and a single-target block have no coherent multi-call
  semantics — and an await-in-batch would create a same-step data
  dependency on a sibling's not-yet-existing `task_id`); `_spawn_task`
  becomes batchable with tools and with other spawns" — adopted as AC-21′
  and as this phase's explicit non-goal boundary for await-in-batch.
- brief 16 §2f: "Harbor's current `_spawn_task` description ('Use to launch
  parallel work…', `discovered_tools.go:305-307`) actively INVITES the
  co-occurrence AC-21 rejects — the prompt and the validator disagree
  today, which is itself a defect independent of the redesign." This phase
  rewrites the reserved-control descriptions to teach the corrected
  contract instead of just fixing the validator.
- brief 02 §2 / brief 07 §4: the sealed `Decision` sum and the
  `CallParallel` atomic-setup / per-branch-outcome model this phase's
  `Batch.Tools` deliberately mirrors rather than reinvents.
- brief 15 §1-2: native tool-calling surfaces a structured `ToolCalls`
  array per response instead of one JSON action — the structural precondition
  that makes multi-call batching (and therefore this defect) possible in
  the first place; `Batch` is the shape that finishes closing the gap the
  native-tool-calling migration opened.

## Findings I'm departing from (if any)

- D-169 item 5 (`docs/decisions.md`) closed the 107d silent-tail-drop bug by
  making reserved control names standalone-only and stated: "A future
  one-turn batch-spawn, if wanted, is a dedicated `_spawn_tasks` meta-tool
  taking an array — never reserved names as `CallParallel` branches." This
  phase deliberately departs from that closing note: instead of a second
  `_spawn_tasks` array-taking meta-tool, it keeps the existing singular
  `_spawn_task` reserved name and lets the projector partition N native
  `_spawn_task` calls (plus catalog-tool calls) out of one response into
  `Batch.Spawns` / `Batch.Tools`. Brief 16 §2b's cross-agent evidence
  (opencode dispatches its spawn tool through the exact same multi-call
  path as ordinary tools, with no special-casing) is the reason: a second
  meta-tool would duplicate the array-vs-repeated-call ambiguity brief 16
  §3 flags as a non-transferable pattern (tura's macro-tool `step` tags),
  and would still need its own standalone-vs-batchable rule. This
  supersession is recorded as decision D-322 in the same PR, per CLAUDE.md
  §15/§16 (a settled decision does not get silently re-litigated).
- brief 16 §5's `MaxBatchSpawns` hard breadth cap is NOT enforced by this
  phase. The cap needs an operator-configured value threaded through
  dispatch (the same shape as `planner.absolute_max_spawn_depth`,
  `internal/config/config.go:1694`), which requires the executor this
  phase's non-goals explicitly exclude. See Risks/open questions.

## Goals

- Ship `planner.Batch` as the fourth sealed `Decision` shape with a
  constructor that fails loud on the invariants brief 16 identifies
  (non-degenerate, every spawn non-retain-turn).
- Narrow AC-21 to AC-21′: only `_finish` / `_await_task` are standalone;
  `_spawn_task` is batchable with catalog tools and with other spawns.
- Make `DecisionInvocationCount` (and therefore `CountToolInvocations` and
  the Console tool-count read) correct for `Batch`.
- Fix the `_spawn_task` prompt-vs-validator disagreement brief 16 §2f
  flags, teaching the model the corrected batching contract.
- Register `Batch` in the planner conformance pack as the primitive every
  future concrete `Planner` must be able to round-trip, ahead of its first
  dispatch consumer landing later in the same wave.

## Non-goals

- **Executor dispatch of `Batch`** — how the Runtime actually runs
  `Batch.Tools` concurrently and registers `Batch.Spawns`. That is the next
  phase in this wave (186); this phase ships the shape and its projection,
  not its execution.
- **`MaxBatchSpawns` operator cap enforcement** — needs the dispatch path
  186 ships; deferred there (see Risks/open questions).
- **Meta-tools for mid-flight task observation/cancel** (`_task_status`,
  `_cancel_task`, brief 16 §5) — a later phase (187).
- **Await-in-batch.** `_await_task` stays standalone. Brief 16 §4's stated
  reason is the rule this phase encodes and does not revisit: a batched
  await would create a same-step data dependency on a sibling spawn's
  not-yet-existing `task_id` — the sibling hasn't been dispatched yet when
  the batch is constructed, so there is no `task_id` for the await to name.
- **The conversational wake-message notification** (brief 16 §5's
  opencode-inspired narrative layer on top of `WatchGroup`) — a future
  phase touching `internal/runtime/notifications`, not this one.
- **`RequestPause` / `Finish` batching** — out of scope; both stay
  single-shape terminal/control decisions, matching AC-21′.

## Acceptance criteria

- [ ] `internal/planner/decision.go` defines `Batch{Tools []CallTool;
      Spawns []SpawnTask; Join *JoinSpec}` as Harbor's fourth sealed
      `Decision` shape (`isDecision()` marker), documented as NOT a
      widening of `CallParallel` — tool-count accounting treats `Tools`
      and `Spawns` as separately counted.
- [ ] A `Batch` constructor (or equivalent package-level validating
      function used by every producer) fails loud, wrapping
      `planner.ErrInvalidDecision`, when: (a) `len(Tools)+len(Spawns) < 2`
      (degenerate — callers must construct the plain `CallTool` /
      `SpawnTask` / `CallParallel` shape instead), or (b) any
      `Spawns[i].Spec.RetainTurn == true`.
- [ ] `internal/planner/react/projector.go`'s AC-21 guard narrows to AC-21′:
      only `FinishToolName` and `AwaitTaskToolName` trigger the
      standalone-co-occurrence rejection; `SpawnTaskToolName` no longer
      does.
- [ ] `projectResponse` partitions a native multi-call response's
      `ToolCalls` by name into catalog-tool calls and `_spawn_task` calls;
      a response containing at least one `_spawn_task` call alongside ≥1
      other call (catalog tool or another `_spawn_task`) projects to
      `Batch`, never to `CallParallel` with a synthetic spawn branch and
      never to two decisions.
- [ ] Existing single-call fast paths (one `CallTool`, one `_finish`, one
      `_spawn_task`, one `_await_task`) are unchanged byte-for-byte in
      behavior (regression-tested against the pre-existing projector unit
      tests).
- [ ] The projector NEVER constructs a one-branch-total `Batch`: an
      N-call response with exactly one `_spawn_task` and zero other calls
      projects to plain `SpawnTask` (today's path, unchanged); an N-call
      response with zero `_spawn_task` calls projects to `CallParallel`
      (unchanged); only ≥2 combined branches with ≥1 spawn present produce
      `Batch`. A conformance test asserts this invariant directly (one
      representation per semantic).
- [ ] `FailFast` disagreement across the spawns auto-grouped inside one
      `Batch` (when ≥2 spawns share no explicit `GroupID`) is rejected
      loud at projection time — the projector surfaces
      `planner.ErrInvalidDecision` naming the conflicting values, matching
      brief 16 §4's "`FailFast` disagreement across auto-grouped spawns
      fails the batch loud."
- [ ] `internal/planner/trajectory.go`'s `DecisionInvocationCount` gains a
      `Batch` case returning `len(d.Tools)` (spawns contribute zero,
      matching `SpawnTask`'s existing zero-count rule); `*Batch` nil-checks
      per the existing pointer-case pattern.
- [ ] `Trajectory.Serialize` round-trips a `Batch`-carrying step:
      `Serialize → Deserialize → Serialize` is byte-stable (D-049's
      contract), and a `Batch` containing a non-serializable value in any
      branch surfaces `ErrUnserializable` with the offending field path —
      never a silent drop.
- [ ] `reservedPlannerControlDeclarations()`
      (`internal/planner/react/discovered_tools.go`) rewrites the
      `_spawn_task` description to teach the batching contract (may
      co-occur with catalog tools and other spawns in one response) and
      keeps `_await_task`'s description unchanged in spirit but explicit
      that it must be sent alone.
- [ ] `internal/planner/conformance/conformance.go` registers `Batch` in
      the `Sealed_DecisionSum` compile-time assertion and in every
      `switch dec.(type)` exhaustiveness site the pack currently lists (the
      `Sanity_NextReturnsDecision` shape-allowlist and the
      `WakeMode_RoundTrip` terminal-shape allowlist).
- [ ] `scripts/smoke/phase-185.sh` passes: it is a unit-test-class smoke
      (source assertions, not a live server) per the Smoke script
      additions section below.
- [ ] Coverage on `internal/planner` and `internal/planner/react` meets
      the stated targets.

## Files added or changed

- `internal/planner/decision.go` — `Batch` type + validating constructor.
- `internal/planner/decision_test.go` — constructor validation unit tests.
- `internal/planner/trajectory.go` — `DecisionInvocationCount` `Batch`
  case.
- `internal/planner/trajectory_test.go` — invocation-count + serialization
  round-trip tests for `Batch`.
- `internal/planner/react/projector.go` — AC-21′ guard narrowing,
  `ToolCalls` partitioning, `Batch` construction path.
- `internal/planner/react/projector_test.go` — partition-table tests,
  degenerate-batch-never-constructed test, `FailFast`-disagreement test,
  AC-21′ standalone-guard tests for `_finish` / `_await_task`.
- `internal/planner/react/discovered_tools.go` — rewritten `_spawn_task`
  reserved-control description.
- `internal/planner/react/discovered_tools_test.go` — description-text
  assertions (the prompt teaches the batching contract; no residual text
  implying `_spawn_task` must be sent alone).
- `internal/planner/conformance/conformance.go` — `Batch` added to the
  sealed-sum assertion and both `switch` allowlists.
- `internal/planner/conformance/conformance_test.go` (or existing file) —
  any new pack-level assertions the `Batch` registration needs.
- `docs/decisions.md` — new `## D-322` entry recording the `Batch` shape,
  the AC-21′ narrowing, and the explicit supersession of D-169 item 5's
  "a dedicated `_spawn_tasks` meta-tool" closing note.
- `scripts/smoke/phase-185.sh` — new smoke script (unit-test-class
  assertions per AGENTS.md §4.2).

## Public API surface

```go
// internal/planner/decision.go

// Batch groups zero-or-more catalog-tool branches with zero-or-more
// task spawns projected from ONE native multi-call LLM response. A
// fourth sealed Decision shape — not a widening of CallParallel.
type Batch struct {
    Tools  []CallTool  // catalog branches; joined per Join (nil → JoinAll)
    Spawns []SpawnTask // every entry's Spec.RetainTurn MUST be false
    Join   *JoinSpec   // governs ONLY Tools; ignored/must-be-nil when Tools is empty
}

func (Batch) isDecision() {}

// NewBatch validates and constructs a Batch, failing loud (wrapping
// planner.ErrInvalidDecision) on a degenerate batch (< 2 combined
// branches) or any retain-turn spawn.
func NewBatch(tools []CallTool, spawns []SpawnTask, join *JoinSpec) (Batch, error)

// SpawnTask gains an additive CallID field mirroring CallTool.CallID:
// the provider-assigned tool-call identifier of the native _spawn_task
// call, so the batch executor's call-id-keyed observation (phase 186)
// can answer every tool_call_id, including spawn calls. Empty for
// programmatic (non-native) spawn emissions, exactly like
// CallTool.CallID today; the projector stamps it during partition.
//
//    type SpawnTask struct { /* existing fields */ ; CallID string }
```

```go
// internal/planner/trajectory.go — extended switch case
func DecisionInvocationCount(action any) int {
    // ... existing cases ...
    case Batch:
        return len(d.Tools)
    case *Batch:
        if d == nil { return 0 }
        return len(d.Tools)
}
```

## Test plan

- **Unit:**
  - `decision_test.go`: `NewBatch` rejects `len(Tools)+len(Spawns) < 2`;
    rejects any `Spawns[i].Spec.RetainTurn == true`; accepts the minimum
    valid shapes (1 tool + 1 spawn; 0 tools + 2 spawns; 2 tools + 0
    spawns — the last one documented as "prefer plain `CallParallel`
    instead" and asserted the PROJECTOR never does this, not that the
    constructor forbids it).
  - `trajectory_test.go`: `DecisionInvocationCount` returns `len(Tools)`
    for `Batch` and `*Batch`; zero for a nil `*Batch`; zero contribution
    from `Spawns` regardless of count.
  - `projector_test.go`: the partition table — every combination of {0,1,
    N} catalog-tool calls × {0,1,N} `_spawn_task` calls × presence/absence
    of `_finish`/`_await_task` in the tail — asserts the correct
    `Decision` shape (`CallTool` / `CallParallel` / `SpawnTask` / `Batch`
    / `ErrInvalidDecision`); AC-21′ guard messages name the offending
    tool and the call count; `FailFast` disagreement across auto-grouped
    spawns fails loud with both conflicting values named.
  - `discovered_tools_test.go`: the `_spawn_task` description text no
    longer contains "parallel work" framed as if `_spawn_task` runs
    alone, and does state it may co-occur with other calls; the
    `_await_task` description still says to await one task_id, sent
    alone.
- **Integration:** `internal/planner/react/react_test.go` drives a full
  `ReActPlanner.Next(ctx, rc)` call with a scripted mock LLM response
  containing two `_spawn_task` calls and one catalog-tool call in a
  single native multi-call response, using the REAL projector, the REAL
  `discovered_tools` declarations (so the mock's declared schema and the
  parsed args agree), and the REAL `Trajectory` — asserting the resulting
  `Batch` decision's shape, that appending it as a `Step.Action` and
  calling `Trajectory.Serialize` round-trips byte-stable, and that a
  forced-unserializable `Args` payload on one `Tools` branch surfaces
  `ErrUnserializable` naming that branch's field path (the ≥1 failure
  mode this seam requires per AGENTS.md §17.3). This is the in-package
  wiring test between the projector, `decision.go`, and `trajectory`;
  the cross-subsystem dispatch wiring (executor consuming `Batch`) is
  186's integration test, not this phase's — 186 is the primitive's
  first consumer in this wave per CLAUDE.md §13, landing on the same
  `Batch` shape this phase freezes.
- **Conformance:** `internal/planner/conformance/conformance.go`'s
  `Sealed_DecisionSum` subtest compiles `var _ planner.Decision =
  planner.Batch{}`; the `Sanity_NextReturnsDecision` and
  `WakeMode_RoundTrip` shape-allowlist switches accept `planner.Batch`
  without failing; a new conformance-pack assertion (or a
  `projector_test.go`-local test if the pack's scenario factories don't
  naturally reach a `Batch`-producing mock response) pins "the projector
  never constructs a degenerate one-branch `Batch`" as a pack-level
  invariant so any future concrete planner sharing the projector inherits
  the guard.
- **Concurrency / leak:** N/A as a NEW artifact — `Batch` is an immutable
  value type constructed fresh per `Next` call, not a shared/reusable
  artifact under D-025. The existing `ConcurrentReuse_D025` conformance
  scenario (N=64 concurrent `Next` calls against one shared `ReActPlanner`)
  already covers this phase's projector changes because it exercises the
  SAME `Next` entry point; no new concurrency surface is introduced.

## Smoke script additions

`scripts/smoke/phase-185.sh` is unit-test-class (source/build assertions,
not a live-server smoke, matching the `PREFLIGHT_REQUIRES: static-only`
convention used by phase 184's script where a check has no HTTP surface):

- `go build ./internal/planner/...` succeeds (the `Batch` shape compiles
  and the sealed sum still closes).
- `go test ./internal/planner/... ./internal/planner/react/... ./internal/planner/conformance/... -run 'Batch|AC21|DecisionInvocationCount' -race` passes.
- Grep assertion: `internal/planner/decision.go` declares `type Batch
  struct` and `func (Batch) isDecision()`.
- Grep assertion: `internal/planner/react/projector.go` no longer lists
  `SpawnTaskToolName` inside the standalone AC-21 guard's reserved-name
  switch (the guard's remaining case list is exactly `FinishToolName,
  AwaitTaskToolName`).
- Grep assertion: the `_spawn_task` description string in
  `discovered_tools.go` does not contain the pre-fix phrase "Use to
  launch parallel work" (regression guard against re-introducing the
  prompt/validator disagreement brief 16 §2f flagged) and does mention
  that it may accompany other calls.
- `skip` (not `fail`) if `internal/planner/decision.go` doesn't yet define
  `Batch` — the standard 404-equivalent for a not-yet-built surface,
  consistent with the phase-184 script's pattern.

## Coverage target

- `internal/planner`: 85%
- `internal/planner/react`: 85%

## Dependencies

- 184 (prior wave shipped; sequencing predecessor)
- 42 (`planner.Decision` sealed sum origin, D-047)
- 45 (React planner + AC-21's predecessor single-call reduction, D-051)
- 47 (`CallParallel` executor + `SpawnTask`/`AwaitTask` emission, D-056 —
  `Batch.Tools` reuses this dispatch contract; `Batch.Spawns` reuses this
  spawn contract)
- 107c/107d (native tool-calling projection + AC-21 itself, D-169)
- 107e (`SpawnTask`/`AwaitTask` dispatch wiring, D-170 — the
  `absolute_max_spawn_depth` cap this phase's Risks section references)

## Risks / open questions

- **`MaxBatchSpawns` has no home yet.** Brief 16 §5 recommends a hard,
  operator-configurable breadth cap on spawns-per-batch with whole-batch
  loud rejection (mirroring `CallParallel`'s `absolute_max_parallel=50`
  system cap, RFC §6.2 / phase 47). This phase's `Batch` constructor
  validates STRUCTURAL invariants only (non-degenerate, non-retain-turn);
  the numeric cap needs a config value threaded through dispatch the same
  way `planner.absolute_max_spawn_depth` is (`internal/config/config.go`),
  which lives in 186's executor. If 186 lands without it, breadth is
  bounded only by depth-cap-adjacent reasoning (brief 16 §6: "sibling
  spawns in one Batch share the parent's depth and are NOT mutually
  limited") — an operator-visible gap to flag in 186's plan, not silently
  dropped here.
- **D-169 item 5 supersession needs careful wording in the new D-322
  entry** so a future reader hits the supersession note on D-169 first,
  not a second contradictory "settled" decision. The PR must cross-link
  both entries (CLAUDE.md §15 doesn't silently overwrite the old entry —
  it stays as history, D-322 explains the departure).
- **Conformance pack scenario coverage for `Batch`.** The existing
  `ScenarioFactory` pattern (e.g. `ScenarioParallelAtomicity`'s
  CallParallel-preferred skip-with-reason) may need a similar optional
  factory hook for a `Batch`-producing mock response if the harness's
  default scripted content map can't naturally trigger the shape; if the
  pack's existing scenario infrastructure is reused as-is, note that
  explicitly in the PR rather than silently skipping `Batch` coverage in
  the shared pack.

## Glossary additions

- **`Batch` decision** — the fourth sealed `planner.Decision` shape
  (`Tools []CallTool; Spawns []SpawnTask; Join *JoinSpec`) a projector
  constructs when one native multi-call LLM response mixes catalog-tool
  calls with `_spawn_task` calls. Not a widening of `CallParallel`
  (`Batch.Tools` and `Batch.Spawns` are counted separately for
  tool-invocation accounting); every `Spawns` entry has
  `Spec.RetainTurn == false`. Phase 185, D-322.
- **AC-21′** — the narrowed successor to AC-21 (D-169 item 5): only
  `_finish` and `_await_task` are standalone reserved control names that
  reject co-occurrence with any other tool-call in one response;
  `_spawn_task` is batchable with catalog tools and with other spawns via
  the `Batch` decision. Phase 185, D-322.
- **Degenerate batch** — a would-be `Batch` with fewer than two combined
  `Tools`+`Spawns` branches. The projector never constructs one — it
  prefers the plain single-shape `Decision` (`CallTool` / `SpawnTask` /
  `CallParallel`) instead, per the "one representation per semantic"
  invariant (brief 16 §4). Phase 185, D-322.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A: this phase touches only planner-package decision shape, projection, and trajectory counting; no identity-scoped storage or event path changes.
- [ ] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** N/A: `Batch` is an immutable value type constructed fresh per `Next` call, not a shared/reusable artifact; the existing `ConcurrentReuse_D025` conformance scenario already covers the shared `Next` entry point this phase's projector changes flow through.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See the Test plan Integration bucket above — `internal/planner/react/react_test.go` wires the real projector + real trajectory + real discovered-tools declarations end-to-end under a scripted LLM boundary, with `ErrUnserializable` as the failure mode.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (D-322, superseding D-169 item 5's closing note)
