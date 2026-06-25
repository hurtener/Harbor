# Research Brief 16 — Agent-pattern foundations

> **Status:** Pre-RFC research, internal.
> **Date:** 2026-06-25

## Why this brief exists

Anthropic has published a run of agent-design write-ups over the past year — "Building effective agents," "A harness for every task," "The advisor strategy," "Building effective human-agent teams," "Steering Claude Code," "Agent identity & access model," "Enterprise managed auth," and "Artifacts in Claude Code." Each names a pattern that an agent framework can plausibly copy. This brief maps those eight write-ups onto Harbor's existing foundations to decide, for each, what is **already covered** by a shipped primitive, what is a **genuine gap** worth building, and where copying the obvious pattern would **dilute Harbor's DNA** — the sealed `Decision` sum, the mandatory identity tuple, the headless Runtime, fail-loudly, and typed audit/replay.

The brief is organized around five pillars (composition, the consultation primitive, the human-agent boundary, identity & access, and artifacts-as-deliverables) and three binding DNA boundaries (B1, B2, B3). One pillar — the consultation primitive — is the only net-new thing this brief argues to **build**; the rest are DOCUMENT, GUARD, or a small additive spec.

As with every brief under `docs/research/`, this is authoritative for **context, not design**. It argues *shapes* and pressure-tests them against the code, but every adoption decision — whether a primitive ships, in which version, and with what surface — is RFC-territory and **out of this brief's scope**. The Go shapes below are research sketches for the RFC, not final API.

## The composition spectrum (framing)

"Building effective agents" draws one load-bearing line: **workflows** (an LLM plus tools on predefined code paths) versus **agents** (the LLM directs its own process in a loop), with the standing advice to *add complexity only when it demonstrably improves outcomes*. Harbor already encodes the same split: the Runtime owns mechanism, the Planner owns the loop (RFC §3.2; `internal/planner/planner.go:64-66`, where `Next` is a single method). The reframe this brief sharpens is that "serving different agent types" is **compositions over one planner plus the existing composition layer**, not a zoo of planner concretes.

Each Anthropic workflow pattern maps onto a Harbor layer that already exists at the primitive level:

| "Building effective agents" pattern | Harbor surface (verified) | Layer |
|---|---|---|
| Prompt-chaining | engine node graph (`internal/runtime/engine/`) — nodes wired in Go, each transforms→next | Runtime kernel (flows) |
| Routing | `PredicateRouter` (`routers/predicate.go:52`), `UnionRouter` (`routers/union.go:31`), `RoutePolicy` (struct `routers/policy.go:38`; the `policy.go:1-16` package doc enumerates all three) | Runtime kernel |
| Parallelization — sectioning | `CallParallel{Join: JoinAll}` (`internal/planner/decision.go:66-98`) + `concurrency.MapConcurrent` | Planner decision / kernel |
| Parallelization — voting | `CallParallel{Join: JoinN}` threshold (`decision.go:107-114`) + `concurrency.JoinK` | Planner decision / kernel |
| Orchestrator-workers | `SpawnTask` / `AwaitTask` (`decision.go:117-171`) | Planner decision |
| Evaluator-optimizer | **absent** — no verifier/critic anywhere | gap (Pillar 2) |
| The autonomous agent loop | the `Planner` (`react` concrete) | Planner |

Every cell except the evaluator-optimizer row resolves to real code. But two qualifications govern the rest of the brief. First, none of `routers` / `concurrency` / `engine` is exposed as a *declarative thing an operator picks*: flows are constructed in Go, routers wrap as nodes via `AsNode`, and there is no typed catalog of harnesses or selection verb. The composition layer is **runtime-kernel plumbing**, not an operator-facing authoring surface. Second, CLAUDE.md §3 lists `internal/runtime/playbooks/` ("composable subflows"), but that directory does not exist; the shipped subflow primitive is `engine.CallSubflow` (`internal/runtime/engine/subflow.go:23-40`), which builds a fresh child engine per parent envelope. Any framing that treats "playbooks" as a present-tense catalog member overclaims. These two facts matter directly for boundary B2 below: the surface it governs is one Harbor would have to **build**, not one it already has.

## 1. Composition spectrum & dynamic harnesses

### 1.1 The anti-zoo reframe is correct and code-supported

The master plan rolls PlanExecute, Workflow, Graph, Supervisor, MultiAgent, and HumanApproval under one post-V1 entry (`docs/plans/README.md:1297`, "Deps: 49"), gated explicitly on *"V1 evidence that the interface holds"* (RFC §2.2 NG3; RFC §12). V1 ships only `react`, `deterministic`, and the `finish` stub. The right dividing line has three arms:

- **behavior-on-the-loop** (verify, advise, plan-track, failure-policy, memory-candidate) → a primitive on the *one* planner, **no new opcode**;
- **genuinely-different control structure** (a non-LLM tree-walk) → a real planner concrete;
- **multi-actor orchestration** → the composition layer plus a reducer.

Applying that line to the six named concretes vindicates the anti-zoo stance more strongly than the framing alone:

- **Deterministic** — already shipped; a scripted step/tree walker with **no LLM** (its import-graph guard rejects `internal/llm`; conformance skips `MalformedLLM_Salvage` with a reason). It is the existence proof that a non-LLM `Decision` shape works (RFC §12 Q-6). Correctly a concrete. ✓
- **HumanApproval** — **not a concrete.** Human approval is already a `Decision`: `RequestPause{Reason: PauseApprovalRequired}` (`decision.go:185-190`). Any planner emits it. A "HumanApproval planner" would be dead weight — this is policy.
- **Supervisor / MultiAgent** — **not concretes in any deep sense.** A supervisor *is* a planner emitting `SpawnTask`/`AwaitTask` for workers: policy over existing primitives. The genuinely-missing piece is the fan-out reducer of §1.2, a *composition-layer* primitive, not a planner class.
- **Graph** — **most likely the composition layer.** A DAG-walking "Graph planner" is what `engine` + `routers` + `concurrency` already do deterministically. If nodes are LLM-driven, each runs a `react` sub-step; if deterministic, it is a flow. A monolithic `GraphPlanner` would duplicate the engine.
- **Workflow** — **the composition layer by definition.** A "Workflow planner" walking a pre-authored flow is either the engine itself or a thin adapter the `deterministic` concrete already covers in spirit.
- **PlanExecute** — **borderline; the cheap honest version is a ReAct upgrade.** "Generate a plan, then execute steps" is plan-tracking *behavior on the loop* (a plan artifact in the trajectory plus a next-step selection policy). It earns a separate concrete only if the plan is a typed DAG with deterministic execution — at which point it has become Graph/Workflow again.

So of the six, exactly **one** (Deterministic, already shipped) is unambiguously a genuine control structure; the rest are planner *policies* or *the composition layer wearing a planner costume*. This is strong, code-grounded support for boundary **B3**, and suggests the RFC could profitably re-scope the post-V1 planner entry from "additional concretes" toward "planner *behaviors* + a composition-layer reducer." Whether to re-scope is RFC-territory.

### 1.2 A pillar-local gap: the fan-out synthesizer/reducer

Orchestrator-workers in "Building effective agents" ends with a *synthesizer combining the results*. Harbor has the fan-out (`SpawnTask`, `CallParallel`) and the fan-in joins (`JoinAll` / `JoinFirstSuccess` / `JoinN`; `JoinKeyed` is documented-future, `decision.go:105`). It does **not** have a principled reducer. `ParallelObservation` (`internal/planner/parallel_observation.go:22-57`) merely *decomposes* the N branch outcomes back into N `role:"tool"` messages in branch-index order; the synthesis, if any, happens implicitly on the *next planner step*. That is fine for a ReAct loop, but the *deterministic* (composition-layer) expression of orchestrator-workers — fan out, then a dedicated combine node — has no first-class reducer. `concurrency.JoinK` reads K envelopes and cancels the rest; it does not combine them.

This reducer, if the RFC wants it, is squarely a **composition-layer primitive on the engine / concurrency / router layer — never planner state and never a new `Decision` opcode.** It is *not* the consultation primitive Pillar 2 builds. The honest count is therefore two pillar-distinct gaps adjacent to composition: the evaluator/consultation (P2's) and this typed reducer.

### 1.3 The dynamic-workflows boundary (B2): selection over a typed catalog

"A harness for every task" lets Claude Code **write its own JS orchestration file at runtime** — functions that spawn and coordinate subagents (classify-and-act, fan-out-and-synthesize, adversarial verification, tournament selection, a "quarantine" pattern isolating untrusted agents from privileged ops). Harbor must **not copy the mechanism.** Runtime-authored executable orchestration detonates three invariants at once:

1. **The sealed `Decision` sum.** Adding shapes requires editing the unexported `isDecision()` marker (`decision.go:27-29`); runtime-authored code emits arbitrary control flow the six-shape contract cannot type or audit.
2. **Typed audit/replay.** Every step is a typed `Trajectory` entry with fail-loud serialization and typed `planner.*` events; arbitrary JS has no typed replay surface.
3. **The identity quadruple + fail-loud guarantees.** A runtime-authored harness would be handed identity and credentials it could route anywhere — the opposite of mandatory-identity, no-silent-degradation (CLAUDE.md §6, §7).

The Harbor-native expression is **selection over a typed harness catalog**, and the existing `RoutePolicy{ExplicitTarget}` override (`routers/policy.go:30-44`) — the "planner-driven path" where a deterministic step names the next node — is the closest existing seam.

```go
// Research sketch for the RFC — NOT final API.
//
// A harness is SELECTED from a pre-registered, typed catalog; it is never
// authored at runtime. The runtime executes the selected harness through the
// existing engine/router/concurrency layer, so audit, replay, and identity
// gating are unchanged. A selected harness compiles DOWN to an engine flow —
// it never widens the Decision sum and never adds an opcode.
type HarnessRef struct {
    Name string // resolves against a registered, typed catalog
}

type HarnessSelector interface {
    // Select fails loudly when Name does not resolve against the catalog —
    // an unknown harness is a typed error, never a silent no-op, mirroring
    // the driver-factory error pattern (registered names listed on miss).
    Select(rc RunContext) (HarnessRef, error)
}
```

Two honesty flags. The **"quarantine" pattern has a clean Harbor home** worth stating as a positive: isolating an untrusted subagent is exactly the isolation quadruple plus a visibility-filtered `ToolCatalogView` — a spawned task runs within `(tenant, user, session)` and sees only the tools its identity may call (`planner.go:526-537`). Harbor expresses quarantine *structurally*, not as a runtime guard. And the **substrate B2 governs does not yet exist**: there is no harness catalog, no selection verb, and the "compile a skill to a playbook" path is unbuilt (no `playbooks` package; the skills subsystem ships search/get views, `planner.go:551-562`, not a compiler). B2 is therefore a **forward guardrail on a surface to be designed**, not a fence around shipped code.

## 2. The consultation primitive — verification + advisor

This is the one pillar whose verdict is **BUILD**, not DOCUMENT. The other four map existing surfaces onto Anthropic framings; this one argues a net-new primitive. What follows argues a *shape* and proves it costs no new `Decision` opcode and no mutable planner state. Whether and when it ships is RFC-territory.

### 2.1 The gap is real and the RFC already named it

Harbor ships no verifier, critic, evaluator, or advisor. The grep is clean: every `escalat*` hit under `internal/planner/` is the **output-format repair** ladder (`reminder → warning → critical` over `RepairCounters`, `internal/planner/react/repair_guidance.go:46-64`) — schema-repair telemetry, not reasoning-quality consultation. RFC §12 lists "**Reflection / critique loops** in the reference planner. Optional per concrete; not on V1's critical path," filed beside "additional planner concretes … wait on V1 evidence that the interface holds." So the gap is acknowledged and deliberately deferred — exactly the posture a pre-RFC brief should pressure-test, because three Anthropic patterns converge on it:

- **Evaluator-optimizer** ("Building effective agents"): one LLM generates, another gives iterative feedback until criteria are met.
- **The advisor strategy** ("The advisor strategy"): a cheap executor *drives* and *escalates* to a frontier advisor on demand "without decomposition, a worker pool, or orchestration logic." The advisor returns "a plan, a correction, or a stop signal," reads shared context, and **never calls tools or produces user-facing output**. It is a peer, not a gatekeeper, operating *mid-execution on demand*.
- **Doer-Verifier** ("Building effective human-agent teams"): one agent checks another's output; autonomy expands with demonstrated reliability.

### 2.2 Verify and advise are two modes of ONE primitive

The advisor and the verifier differ on **when** they fire and **what authority** they carry, not on mechanism:

| | Advisor | Verifier |
|---|---|---|
| Fires | mid-execution, on demand (stuck / low-confidence) | at a checkpoint, on a candidate artifact |
| Authority | peer — guidance the driver MAY fold in | gate — its verdict CAN block a transition |
| Returns | plan \| correction \| stop | pass \| fail+reason |
| Self-loops? | no (advice consumed once) | yes (evaluator-optimizer is a verify-loop) |

Both are a **second, typically-more-expensive reasoning call that produces no side effect and no user-facing output, whose result the driving planner folds into its own next decision.** That single sentence is the primitive. Modeling them as one `Consultor` seam with a `Mode` discriminator (the §4.4 interface+factory+registry pattern — `internal/planner` is already a registry of policy) keeps the surface honest: an `llm_judge` verifier and a frontier `advisor` are the same call with different prompts and fold-back rules.

### 2.3 Why this needs NO seventh Decision opcode

The `Decision` sum is sealed (`isDecision()` marker, `decision.go:27-29`) and pinned to six shapes by D-047; adding a seventh is a real architectural act B3 warns against. Consultation needs none, because every advisor output already lands on an existing shape:

- **stop signal** → `Finish` (`decision.go:201-207`). `FinishNoPath` already models "couldn't satisfy the goal" (`planner.go:896-897`) and is what `maxStepsExceeded` returns today (`react/react.go:809-810`).
- **a plan** → folded into this turn's prompt as planning content. `PlanningHints` (`planner.go:814-846`) is the exact surface — `Constraints`, `PreferredOrder`, `PreferredTools` render into `<planning_constraints>`. The advisor's returned plan is `PlanningHints`-shaped content for the planner's *next* main LLM call.
- **a correction** → guidance text injected into the immediate re-plan prompt. It does **not** round-trip through `ControlSignals.InjectedContext` (`planner.go:608-610`) — that inbox is for *runtime/operator* steering observed across steps; the advisor's correction is internal to the planner and consumed *within the same `Next` call*.
- **human verification** (the `human` mode) → `RequestPause{Reason: PauseApprovalRequired}` (`decision.go:185-190`; `planner.go:858`). Doer-Verifier's *human* checker is just the unified pause/resume primitive, satisfying CLAUDE.md §7.4 rather than reinventing pause.

So the primitive is a **policy plus a helper the planner calls inside `Next`**, whose result manifests as one of the six decisions the planner already returns. The `Decision` contract is untouched.

### 2.4 The tier-up does NOT ride `RunContext.LLMOverrides`

A subtle but load-bearing correction. `LLMOverrides` is **resolved once at run start and immutable for the whole run** — "a swap at any layer lands on the NEXT run, never mid-flight … the same next-turn-only invariant the compiled-artifact concurrent-reuse contract requires" (`planner.go:244-251`). A planner that mutated `rc.LLMOverrides` mid-`Next` to tier up would violate exactly the immutability the field exists to guarantee, and the D-025 concurrent-reuse contract.

The correct mechanism: the advisor call is a **separate internal `llm.CompleteRequest` the planner issues with an overridden `Model` field**, through the same `p.client` it already holds (the client routes by model name). The cheap executor model is the agent's bound `llm.model`; the advisor model is a *consultation-level* knob (`ConsultationPolicy.AdvisorModel`), not the per-run override bundle (`LLMOverrides.Model`, `planner.go:405`, is the *run-level* dial). This matches the advisor strategy's actual claim — the executor drives cheap and *escalates a distinct call* to the frontier model — and sidesteps the immutability contract cleanly.

### 2.5 The state lives nowhere on the planner

D-025 forbids mutable state on a compiled artifact; the `ReActPlanner` is shared across N goroutines and "per-run state lives in `ctx` + `RunContext`, never on the receiver" (`planner.go:31-34`). Harbor already has the template: `RepairCounters`, a runtime-owned pointer threaded through every per-step `RunContext` (`planner.go:752-795`). Consultation copies that exactly.

```go
// research sketch for the RFC, not final API.

// ConsultCounters bounds and records per-run consultation activity.
// Runtime-owned, threaded through every per-step RunContext exactly like the
// output-format repair counters — the shared planner artifact holds no
// consultation state, so the concurrent-reuse contract still holds.
type ConsultCounters struct {
    Advises  int // mid-execution advisor escalations consumed so far
    Verifies int // checkpoint verifications run so far
    Rejects  int // consecutive verify rejections at the active checkpoint
}
```

### 2.6 The checkpoints and the seam

Four checkpoints are natural in Harbor's loop: **before `Finish`** (verify the terminal answer — the cheapest consumer, §2.8); **before a side-effecting tool** (gate on the catalog's side-effect class, `planner.go:530-531`); **after a spawned task resolves** (verify a `MemberOutcome` when `AwaitTask` re-enters `Next`, `decision.go:163-169` — Doer-Verifier over delegated work); and **when stuck** (advise mode, top-of-`Next`, gated on a stuck signal).

```go
// research sketch for the RFC, not final API.

type Checkpoint string

const (
    BeforeFinish      Checkpoint = "before_finish"       // verify terminal answer
    BeforeSideEffect  Checkpoint = "before_side_effect"  // gate write/irreversible tool
    AfterSpawnResolve Checkpoint = "after_spawn_resolve" // verify delegated work
    OnStuck           Checkpoint = "on_stuck"            // advisor escalation, mid-run
)

// ConsultMode is the verify/advise discriminator. The human mode never reaches
// a Consultor — it short-circuits to a pause decision (see fold-back rules).
type ConsultMode string

const (
    ModeAdvise             ConsultMode = "advise"               // peer guidance, may fold in
    ModeVerifyDeterministic ConsultMode = "verify_deterministic" // pure-code predicate
    ModeVerifyToolCheck    ConsultMode = "verify_tool_check"    // a registered tool re-checks
    ModeVerifyLLMJudge     ConsultMode = "verify_llm_judge"     // a tier-up LLM grades the work
    ModeVerifyHuman        ConsultMode = "verify_human"         // routes through RequestPause
)

// Consultor is the §4.4-style policy seam: one interface, drivers for
// deterministic / tool_check / llm_judge / advisor. The human mode never
// reaches a Consultor. A Consultor produces NO side effect and NO user-facing
// output; it reads the run's shared context and returns a verdict the driving
// planner folds into its own next Decision.
type Consultor interface {
    Consult(ctx context.Context, in ConsultInput) (ConsultResult, error)
}

type ConsultInput struct {
    Checkpoint Checkpoint
    Mode       ConsultMode
    Goal       string
    Candidate  any         // the Finish payload, the tool args, the MemberOutcome
    Trajectory *Trajectory // shared context, read-only
}

// ConsultResult folds onto EXISTING Decision shapes — no seventh opcode.
type ConsultResult struct {
    Verdict Verdict // Pass | Fail | Stop

    // Plan / Correction are advisor outputs rendered as planning-constraint
    // prompt content for the immediate re-plan. They are NOT a new Decision and
    // NOT a RunContext.LLMOverrides mutation.
    Plan       *PlanningHints
    Correction string

    StopReason FinishReason // set when Verdict == Stop
}

type Verdict string

const (
    VerdictPass Verdict = "pass"
    VerdictFail Verdict = "fail" // re-plan with Correction, bounded by ConsultCounters AND Budget
    VerdictStop Verdict = "stop" // -> Finish{StopReason}
)
```

### 2.7 Fold-back, observability, and the BLOCKER on recording

Fold-back, all onto existing shapes: `Pass` at `BeforeFinish` → return the candidate `Finish` unchanged. `Fail` → do **not** finish; fold `Correction`/`Plan` into the prompt and re-plan *within the same `Next`* (the planner already makes its LLM call inside `Next`, `react/react.go:658`, so a bounded re-plan is one more `Complete`). On exhaustion the honest terminal is `Finish{FinishNoPath}`, never a silently-shipped unverified answer. `Stop` → `Finish{StopReason}`. `ModeVerifyHuman` → `RequestPause{PauseApprovalRequired}`, batched at decision granularity (Pillar 3), never per-tool.

**The hardest correctness constraint is recording, not error-handling.** A consultation issues a reasoning call (and possibly a bounded re-plan) *inside* `Next` whose result shapes the returned `Decision`. The append-only `Trajectory` is the planner's *only* memory of its own work (`planner.go:130-135`, D-025); a hidden consult that influenced the decision but never landed in the trajectory makes deterministic replay diverge and breaks "if it's not written down, it doesn't exist." That would erode the exact typed-audit/replay invariant this brief wields against B2. Therefore: **every consultation — pass and fail, advise and verify — MUST be recorded as a typed `Trajectory` entry and emit a canonical `planner.consultation_*` event**, so consults are replayable and Console-visible, not merely error-emitting.

### 2.8 The cheapest honest consumer: wire it into ReAct (§13)

CLAUDE.md §13 is binding: the wave introducing a primitive must also introduce a consumer that exercises it end-to-end with a test. The cheapest consumer that exercises **both modes** is the reference planner: **escalate-on-stuck** (advise mode, top of `Next` after the cancel/circuit-breaker checks, `react/react.go:508-523`, folding into this turn's prompt build at `:598-606`) and **verify-before-finish** (verify mode, when `projectResponse` yields a terminal answer, `:653-666`). This ships a **better default agent** — a cheaper executor that escalates to a frontier model when stuck and grades its own answer before finishing, the advisor strategy verbatim — and **defers** the question of whether Plan-Execute / Graph ever need separate concretes. Reflection becomes a *behavior on the one planner*, the dividing line §1 draws.

### 2.9 Fail-loudly: the consultor's own errors

A `Consultor.Consult` error is **never** swallowed into an implicit `Pass`; the ReAct planner already propagates LLM-call errors verbatim (`react/react.go:657-661`) and consultation must not regress that. **Fail-open vs fail-closed is a DECLARED policy field, never an implicit default.** A `BeforeSideEffect` verifier that cannot run fails **closed**; an `OnStuck` advisor that errors MAY fail **open** (the advisor is a peer, not a gate) — but only with an explicit `OnError: Proceed` value AND a `planner.consultation_failed` event. Either way the failure is observable beside `EventTypePlannerError` / `EventTypePlannerRepairExhausted` (`internal/planner/events.go:40-50`). Critically, the fail/re-plan loop is bounded **not by `ConsultCounters` alone**: an evaluator-optimizer loop of extra LLM calls inside one `Next` can blow past the run's cost/deadline envelope while staying under the iteration count, so the loop MUST also tie into `Budget.CostCap` / `Deadline` / `TokenBudget` (`planner.go:619-660`).

```go
// research sketch for the RFC, not final API.

type ConsultErrorPolicy string

const (
    ConsultFailClosed ConsultErrorPolicy = "fail_closed" // block the transition; surface
    ConsultFailOpen   ConsultErrorPolicy = "fail_open"   // proceed WITHOUT advice; surface
)

type ConsultationPolicy struct {
    AdvisorModel string                     // the tier-up model (NOT rc.LLMOverrides)
    Checkpoints  map[Checkpoint]ConsultMode
    MaxAdvises   int                        // bounds the evaluator-optimizer loop (count)
    MaxRejects   int                        // bounds verify-before-finish re-plans (count)
    OnError      ConsultErrorPolicy         // explicit; no silent swallow
    // The re-plan loop is ALSO bounded by the run's Budget (cost/deadline/tokens),
    // never by counts alone.
}
```

## 3. The human–agent boundary & the steering surface

"Building effective human-agent teams" splits the human's relationship to a working agent into two postures: **human-in-the-loop** (HITL — review before execution) and **human-on-the-loop** (HOTL — kept informed, can intervene, agent executes within declared bounds). "Steering Claude Code" enumerates seven configuration mechanisms along three axes: *when instructions load*, *how they persist*, and *what authority they carry* (deterministic hooks vs model-mediated guidance). The thesis here: Harbor already owns the **mechanism** for both — one unified pause/resume primitive (RFC §3.3) plus the nine-type steering taxonomy (RFC §6.3) — and the work is to **name the modes and make the posture a declared policy**, not to build runtime surface.

### 3.1 HITL and HOTL are two modes of ONE boundary

**HITL — the agent waits.** Two distinct producers, one primitive: the planner can return `RequestPause{Reason: approval_required}` (one of four sealed reasons — `planner.go:855-868`: `approval_required`, `await_input`, `external_event`, `constraints_conflict`); and the **tool-side approval gate** (`internal/tools/approval/approval.go`) parks a call via `Coordinator.Request(ApprovalRequired)` *before* the tool fires, by consulting a construction-time `ApprovalPolicy.ShouldApprove` (`internal/tools/approval/policies.go`) — **not** by asking the LLM. The mode is *already* a declared policy at this seam. Both converge on the one `pauseresume.Coordinator` and re-export the same four reasons (`internal/runtime/pauseresume/pauseresume.go:86-106`). A rejected gate is **terminal** — `REJECT` ends the run with `Finish{constraints_conflict}` (D-071).

**HOTL — the agent proceeds, the human watches and nudges.** This is the steering inbox, whose Settled taxonomy is **nine** control types (`internal/runtime/steering/taxonomy.go:13-41`, RFC §6.3): `INJECT_CONTEXT`, `REDIRECT`, `CANCEL`, `PRIORITIZE`, `PAUSE`, `RESUME`, `APPROVE`, `REJECT`, `USER_MESSAGE` — not the five the casual reading suggests. The planner never touches the inbox; the Runtime drains it once per planner-step boundary and projects the result onto `RunContext.Control` (`planner.go:206-216`, `ControlSignals` at `:598-617`). The planner *observes* steering as advisory input.

The two postures differ only in **whether the boundary blocks**: HITL parks and waits; HOTL keeps running and interventions land as advisory `Control` on the next step. The substrate is identical — the single strongest argument for treating mode as a knob rather than a fork.

### 3.2 The mode should be a declared policy — and half of it already is

`tools.SideEffect` already classifies every tool — `pure`, `read`, `write`, `external`, `stateful` (`internal/tools/tools.go:69-73`) — and the approval gate already takes a policy at construction (`AlwaysApprovePolicy` is explicitly forbidden as a production default per §13). What is missing is the *unification*: a single per-agent, per-side-effect-class declaration of boundary posture, projected once at run start into the run's immutable snapshot. The natural home is the **agent-config control plane** (D-234), whose every field is projected at run start in alignment with D-025; the authorization gate is already specified (capability-changing edits require an elevated scope, D-235; authority derives from the verified ctx, never the request body, D-219).

```go
// research sketch for the RFC, not final API.
//
// BoundaryPosture is a per-agent declaration of when the human-agent boundary
// BLOCKS (in-the-loop) vs merely OBSERVES (on-the-loop), keyed by the tool's
// declared side-effect class. Projected once at run start into the run's
// immutable snapshot, exactly like the prompt layer and per-tool policy.
type BoundaryPosture struct {
    Default       Mode                   // ModeOnLoop | ModeInLoop
    PerSideEffect map[tools.SideEffect]Mode
    // Approver is one of the closed runtime scope set (admin / console-fleet);
    // boundary posture NEVER introduces a new scope, and authority derives from
    // the verified ctx, never the request body.
    Approver auth.Scope
}
```

This is additive and DNA-safe: it composes the existing `SideEffect` taxonomy, the `ApprovalPolicy` seam, and the agent-config projection; it adds no runtime mechanism. The default for `write`/`external`/`stateful` should lean **in-the-loop** — fail-loudly (RFC §3.4) argues against silently proceeding on consequential effects when no posture is declared. Note that `Approver` is constrained to Harbor's **closed two-scope vocabulary** (`auth.ScopeAdmin` + `auth.ScopeConsoleFleet`, D-079, re-affirmed against per-subsystem-scope pressure in D-105/D-106/D-108); a free-form scope field would invite per-agent custom scopes the runtime does not recognize.

### 3.3 Batch approvals at decision granularity — and keep the bus complete

"Treat human attention as scarce" says batch questions and limit interruption. The drain-between-steps invariant is the substrate (`internal/runtime/steering/steering.go` package doc), so the **Decision** is the natural batching unit; `CallParallel{JoinAll}` (`decision.go:73-115`) is the sharpest case — a planner fanning out N calls in one decision should surface **one** approval prompt, not N. Today `RunGuarded` fires per call, so a decision-level batch is a real gap between the granularity Harbor *emits decisions at* and the granularity it *gates at*. Two cautions: **batching collides with REJECT-is-terminal (D-071)** — the conservative read is approve-all-or-reject-all, with finer partial approval deferred; and **batch the *interrupts*, never the *events*** — "limit visibility" must not be read as "emit fewer events," because the HOTL posture depends entirely on a complete event bus (§3.4). Thinning the bus to "save attention" would quietly violate work-in-public, audit redaction, and Console-as-Protocol-client.

### 3.4 "If it's not written down, it doesn't exist" is an invariant here

Work-in-public is architectural, not disciplinary: the **append-only trajectory** is the planner's only memory (`planner.go:130-135`, D-025); the **canonical event bus** carries identity-filtered lifecycle events (steering emits `control.received` / `control.applied`; the gate emits `tool.approval_requested` / `tool.approved` / `tool.rejected`); and the **Console is a pure Protocol client** (RFC §5, §7; D-061/D-062) that *cannot* read internals, so everything an operator sees is by construction on the wire. HOTL is only possible because of this. North-star proactivity maps too: `RunContext.Goal` (redirectable via `REDIRECT`) is the north star, and the declared `Budget` caps (`planner.go:619-660`) plus §3.2's posture are the on-the-loop envelope. The one work-in-public element with no Harbor primitive — a "weekly report including lessons & missteps" — is self-assessment, exactly the reflection/critique capability Harbor lacks; it is Pillar 2's consultation primitive plus a product-side report generator, not a steering concern.

### 3.5 The steering-layering model as the onboarding map

Anthropic's three axes translate almost one-to-one, and the table is the recommended mental model for "configuring an agent type" — composition over existing layers, not new machinery:

| Anthropic mechanism | Axes (load × persist × authority) | Harbor analog |
|---|---|---|
| CLAUDE.md / rules | session-start · memoized · model-mediated | agent-config **base operator prompt layer** (D-234) |
| append-system-prompt | session-start · memoized · model-mediated | the **user-instruction layer ABOVE the operator base** — composition order is the security boundary (D-235) |
| skills | on-demand · re-injected · model-mediated | the **skills subsystem** (`RunContext.Skills`, `planner.go:551-555`) |
| subagents | child context · model-mediated | **`SpawnTask`/`AwaitTask`** child runs (`decision.go:9-26`); child inherits the quadruple via ctx |
| (runtime steering) | on-demand · re-injected · model-mediated | `INJECT_CONTEXT` / `USER_MESSAGE` / `REDIRECT` on `RunContext.Control` |
| hooks | lifecycle · **deterministic (no LLM)** | approval policy + governance pre-checks + compression short-circuit — see §3.6 |

The ephemeral column also has a home: `RunContext.LLMOverrides` and `PlanningHints` are per-run, run-start-resolved, seen only by this run.

### 3.6 Deterministic "hooks": the mechanism exists, fragmented — don't copy the shell-out

The "hooks = possible gap" instinct is half-right. The code shows the **mechanism already exists, fragmented**: `ApprovalPolicy.ShouldApprove` (a `PreToolUse` analog), the governance cost/rate-limit/max-tokens checks (`internal/governance/{cost,ratelimit,maxtokens}.go`, firing before the model call), and `CompressionRunner.MaybeCompress` at the step boundary (a `PreCompact` analog, `planner.go:643-660`). The gap is twofold: these gates are not *unified or documented* as one deterministic-authority layer, and there is a real temptation to copy Claude Code's *generic shell-out hook surface*. Harbor must not. A hook that shells out on `PreToolUse` would bypass audit redaction (§7), sit outside the typed nine-event taxonomy, and break deterministic replay — the same hazard as runtime-authored orchestration (B2). The Harbor-native answer is **typed policy seams per lifecycle point**, optionally surfaced as composable agent-config fields, never a generic executable-hook escape hatch. Recommendation: DOCUMENT the existing deterministic layer as a named concept; do not build a shell-hook mechanism.

## 4. Identity & access — the agent-as-principal tension

This is the pillar where the Anthropic source and Harbor's DNA point in opposite directions, and the most valuable move is to show the opposition is *apparent*. "Agent identity & access model" moves Claude to **agent-as-principal** — "it has its own account in each system it touches" — motivated by genuinely multiplayer cases (a shared channel with no single user whose permissions are "right"). Harbor's isolation boundary is the deliberate inverse: the tuple `(tenant, user, session)` (+ `run`), with `agent_id` explicitly *not* an isolation principal (D-059; `internal/identity/identity.go:24-39`). The reframe: keep the tuple as the isolation boundary, full stop, and adopt only the *credential mechanics* as an orthogonal seam.

### 4.1 The isolation boundary does not move (B1)

`identity.Identity` is `(TenantID, UserID, SessionID)`; `identity.Quadruple` adds `RunID` (`identity.go:26-39`). Identity is mandatory and fails closed — `Validate` rejects any empty component, `MustFrom` panics on absence (`identity.go:60-72, 99-105`) — with no opt-out knob (CLAUDE.md §6 rule 9). D-059 settles the agent model: an agent is a runtime *entity* with a three-ID model (`agent_id` / `incarnation` / `version_hash`) that *runs within* `(tenant, user, session)` and does not widen the boundary. This is exactly the question the article's design pressures, already answered in the negative for the isolation axis, and load-bearing for three product properties. **B1** is binding (stated in full under DNA boundaries below).

### 4.2 The credential mechanics — three of four already exist

The article's *mechanics* are sound and DNA-safe because they live one layer below isolation, at the tool/credential edge. Three of four already ship:

1. **Stored independently.** Tool-side OAuth tokens live in `TokenStore`, a typed wrapper over the `state.StateStore` seam, AES-256-GCM-encrypted at rest with an operator KEK that fails the boot loud when missing (`internal/tools/auth/auth.go:45-68`; `tokenstore.go:50-68`; D-083). Access and refresh tokens encrypt under separate Kind prefixes (`tokenstore.go:20-37`).
2. **Mapped to the agent's config, not inferred.** `OAuthConfig.BindingScope` is a *declared* field — `ScopeUser` (keyed `(tenant, user_id, source)`) or `ScopeAgent` (an admin-configured service-account, keyed `(tenant, agent_id, source)`), "never inferred from runtime state" (`auth.go:119-163`). This *is* the article's "per-channel distinct agent identity"; crucially the `ScopeAgent` composite key carries `agent_id` on the token *value*, never as an isolation-tuple element.
3. **Injected at the network boundary at request time.** The HTTP driver stamps the credential as the request is built — `req.Header.Set("Authorization", ...)` (`internal/tools/drivers/http/auth.go:124-139`) — sourced from the independent store via `auth_ref`; templates referencing the `Auth` namespace are rejected at load (CLAUDE.md §7.3; `docs/plans/phase-27-tools-http.md:28`). No-passthrough (§7.3) is the article's "credentials never flow through to the model" as a Harbor invariant.

The fourth — **outbound to non-authorized hosts blocked** — Harbor does *not* yet have. There is no per-request egress host-allowlist in the tool path; the closest control is config-time (adding a stdio MCP server is allowlist/approval-gated, D-235 §1), a gate on *which servers exist*, not *which hosts a credentialed request may reach*. The egress block is an honest additive gap (§4.5).

### 4.3 Compound authorization — anchor on D-219, not RFC 8693

The forward direction — *compound authorization = agent credential-scope ∩ verified user/session scope* — is real and half-built. The **runtime-authority gate (D-219)** derives caller authority from the verified ctx identity + JWT scope claims, never the request body (it fixed a steering escalation where the body's `Scope` was rubber-stamped); the agent-config control plane extends the same gate to capability mutation (D-235). The **credential-binding gate (BindingScope)** decides *whose token authorizes the upstream call*. The intersection: a tool call succeeds only when the verified `(tenant, user, session)` scope is permitted to address the source *and* a credential is bound for it.

```go
// research sketch for the RFC, not final API.
//
// CredentialScope is what a stored token authorizes UPSTREAM. It is orthogonal
// to the isolation tuple and never widens it.
type CredentialScope struct {
    Binding BindingScope // user | agent — who the upstream sees
    // Subject is a CREDENTIAL-LOOKUP KEY ONLY (a user_id or an agent
    // registration id). INVARIANT: Subject never appears in any WHERE clause,
    // event filter, or driver scope. It is not an isolation principal.
    Subject  string
    Source   ToolSourceID
    Upstream []string // scopes the upstream granted (informational)
}

// AuthorizeOutbound is the compound gate. Authority for "may this caller address
// this source" comes ONLY from the verified ctx (the runtime scope), mirroring
// the steering-authority rule. The credential binding answers the orthogonal
// "is there a token for this source." Both must hold; neither reads the request
// body for authority.
func AuthorizeOutbound(ctx context.Context, src ToolSourceID) (CredentialScope, error)
```

**Anchor carefully:** D-219 is the right anchor; **RFC 8693 token-exchange is not a current seam**. CLAUDE.md §7.3 names it as the *policy stance* for no-passthrough, but no exchange flow is implemented — the OAuth path uses authorization-code/PKCE + fresh per-identity acquisition, and 8693 is explicitly deferred (`docs/plans/phase-115-production-jwt-jwks-serve.md:51`; `phase-85b-mcp-http-oauth.md:40`; brief 09 line 525). Compound authorization should be specified on the *existing* BindingScope + D-219 substrate, with 8693 named only as a possible future realization.

### 4.4 Enterprise managed auth is product — and the framework seam already shipped

"Enterprise managed auth" (Okta/IdP, provision-once-scope-by-group, role inheritance, short token lifetimes, "a connector only ever connects through the IdP") is almost entirely a *product* concern, and Harbor has drawn the line — twice. **The framework verifies through the operator's IdP; it never provisions.** D-220 shipped a JWKS-backed `KeySet` (`internal/protocol/auth/jwks.go`) behind the existing `Validator` interface and a `harbor serve` subcommand; asymmetric-only allowlist is enforced via `jwt.WithValidMethods` (D-079); the closed two-scope set is the entire vocabulary the runtime knows. "Short token lifetimes" is an IdP configuration consumed for free. **The IdP-provisioning half is explicitly a named non-goal** — the "Enterprise-Managed Authorization extension" is "enterprise-tier, separate future work" (`phase-85b-mcp-http-oauth.md:39`). So the framework rule is mostly *already satisfied*, not aspirational: the runtime MUST NOT grow an IdP client, a group model, or a provisioning surface. The article's "comprehensive logging" and "revoking the identity ends access everywhere" likewise map to existing invariants (provenance captures the triple, CLAUDE.md §6 rule 6; deleting an agent-bound token at `(tenant, agent_id, source)` ends that agent's upstream access).

### 4.5 Where Harbor genuinely lags — additive, DNA-safe gaps

Three honest gaps, none threatening B1: (1) **outbound egress-host blocking** (the missing fourth mechanic) — open question is which layer owns it (per-driver vs one runtime egress gate) and how it composes across HTTP/MCP/A2A uniformly; critically its policy must derive from declared agent-config, never per-request body, or it becomes the per-user ad-hoc auth path §7.3 forbids. (2) **Just-in-time credential grants** — Harbor has JIT *acquisition* (`ErrAuthRequired` parks via pause/resume, resume reattaches the token, D-083); the article's stronger JIT *grant* (an identity-aware overlay = §4.3's compound authorization) is the genuine extension. (3) **The multiplayer/shared-principal case** the article is built around has *no V1 Harbor analog* — D-219 §3 deliberately withholds a `ScopeSessionUser` tier until a non-admin token carries a verified session principal — and that is fine; the article's DMs "stay user-delegated," which is the `ScopeUser` model.

## 5. Artifacts as versioned deliverables

This is the smallest pillar deliberately. The other four argue *behavior*; this one is about a noun. The work is to separate two things English forces to share one word, then confirm closing the gap is purely additive at the Protocol layer.

### 5.1 Two concepts wearing one word

"Artifacts in Claude Code" describes a **deliverable**: a live, shareable visual page that "updates itself as your session works," behind **one link that stays constant across updates**, where "every publish is a new version at the same link, with version history so you can restore, and a gallery." Two load-bearing properties: a *stable reference whose target changes over time*, and *version history with restore*.

Harbor's `artifacts` subsystem is a different thing sharing the name: a **content-addressed blob store** — "the mandatory routing target for any output above the heavy-output threshold" (`internal/artifacts/artifacts.go:1-3`; RFC §6.10). Its job is the **context-window safety net** (D-026): heavy output and oversized `DataURL`s materialize into an `ArtifactRef` and the message is rewritten before persistence and emission (RFC §6.9-§6.10), and at the LLM edge any raw heavy part that isn't already an `ArtifactStub` trips `ErrContextLeak` loudly (defined at `internal/llm/errors.go:34`, tripped at `internal/llm/safety.go:100`; `internal/llm/safety_test.go:57`). So Harbor has the **storage-by-reference** half, not the **versioned self-updating deliverable** half.

### 5.2 What the interface does and does not support — exactly

`ArtifactStore` is eight mandatory methods, no `Supports*` ceremony (`artifacts.go:139-180`): `PutBytes`, `PutText`, `Get`, `GetRef`, `Exists`, `Delete`, `List`, `Close`. The identity model is `ArtifactScope{TenantID, UserID, SessionID, TaskID}`, mandatory at the boundary (`artifacts.go:55-73`). The decisive fact is the **ID rule**: IDs are content-addressed — `{namespace}_{sha256_hex[:12]}` — and "re-uploading identical bytes returns the existing ref" (`artifacts.go:86-88`). Corollaries:

- **No stable reference across content updates.** A content-addressed ID is a *function of the bytes*; change one byte and the ID changes. Constancy-of-link and content-addressing are opposites. (This is exactly what a context-safety dedup store wants.)
- **No version history.** A grep for `version` / `revision` / `history` / `gallery` / `supersede` returns nothing. `GetRef` resolves one ref; `List` returns scope-filtered refs in unspecified order. Nothing relates *v2* to the ref it superseded.
- **No "latest" pointer and no restore.** `Delete` is idempotent removal; there is no set-current, no chain, no rollback.
- **The `ArtifactSource` enum is producer-typed, not version-typed** — `tool` / `planner` / `user_upload` / `system` (`internal/protocol/types/artifacts.go:40-51`).

The Protocol surface is four methods (`artifacts.list/put/get_ref` plus admin `artifacts.delete`, `internal/protocol/methods/methods.go:499-526`) with **Protocol-owned wire types** — the file's own doc calls a 1:1 internal mapping "an RFC §5.1 reject-on-sight smell" (`types/artifacts.go:24`). That is the right shape for the gap: a versioning layer can be added to the *wire contract* without leaking store internals.

**The happy accident:** the content-addressed store is the *ideal substrate* for versioning, not an obstacle. Each published version is naturally a distinct immutable deduplicated blob. What is missing is **one layer up**: a content-independent *logical handle*, an *ordered list* of refs it has pointed at, and a *current* pointer. Versions are free; the grouping and the stable name are the gap.

### 5.3 The additive gap, specified at the Protocol layer

The Runtime mints versions; any Protocol client renders and browses. The additive surface is a logical-handle layer *over* the existing store — it modifies neither the eight-method interface nor the D-026 offload path.

```go
// research sketch for the RFC, not final API — illustrates the logical-handle
// layer ABOVE the existing content-addressed store.

// A Deliverable is a stable, identity-scoped name for evolving visual content.
// Its identity is independent of any version's bytes: the link stays constant
// while the content it resolves to changes. INVARIANT: a deliverable is resolved
// ONLY under its own (tenant,user,session) scope (or an elevated scope claim);
// "shareable" never means identity-bypassing. It is a named pointer inside the
// tuple, never an isolation principal.
type Deliverable struct {
    ID       string        // stable handle; the shareable reference
    Scope    ArtifactScope // (tenant, user, session, task) — minted by the Runtime
    Kind     string        // e.g. "walkthrough", "dashboard", "checklist"
    Current  int           // index into Versions; the "live" revision
    Versions []Version     // append-only, ordered oldest->newest
}

// A Version pins one published revision to one immutable, already-stored
// content-addressed blob. No bytes live here; the ref already does.
type Version struct {
    Ref       ArtifactRef
    Label     string
    Publisher ArtifactSource
    CreatedAt time.Time
}
```

The Protocol additions mirror the existing four-method shape: `deliverables.publish` (append a version, advance current), `deliverables.get` (resolve handle → current or named version), `deliverables.list` (the gallery, scope-filtered), `deliverables.restore` (re-point current — a metadata write; the bytes are immutable and already stored). A `deliverable.published` canonical event lets any client observe "the link's content changed" without polling and without reading internals. **Self-updating is an emission concern, not new machinery:** "updates itself as the session works" = the runtime calls `deliverables.publish` as it progresses and the event streams. No new pause/resume, no new steering, no new opcode.

### 5.4 DNA safety

Walking the non-negotiables: **multi-isolation holds** — the handle is scoped by the same tuple, minted by the Runtime, never an `agent_id`-style principal, never resolved across principals (the "shareable" framing in the source means cross-principal sharing; in Harbor it must not — a deliverable link resolves only under its own scope or an elevated scope claim). **The Console stays a pure Protocol client** — the gallery, version browser, and restore button consume `deliverables.*` exactly as the Console consumes `artifacts.*` today (D-061). **The Planner stays swappable** — publishing is a runtime/tool-side action, not a `Decision` opcode. **Fail-loudly is preserved** — a missing handle is a typed not-found, a restore to an absent version a typed error, never a silent "return the latest anyway." **Primitive-with-consumer (§13/D-062)** is satisfiable: the honest first consumer is the Console gallery landing in the same wave as `deliverables.*`, rendering the cheapest real deliverable (a session-progress checklist or a topology explainer the runtime already has data for).

The one thing to *not* copy: the source's deliverables are "interactive." Harbor's job stops at **substrate** — minting handles, ordering versions, surfacing them, offloading heavy bytes. *How* a client renders an interactive page is a Console/renderer concern. The moment the Runtime grows an opinion about HTML, iframes, or a rendering runtime, it has stopped being headless.

## DNA boundaries (binding constraints)

**B1 — Agent-as-principal must never become the isolation principal.** `agent_id` is a registration identity (D-059) and may bind credentials (§4.2); it MUST NOT appear in any `WHERE` clause, event filter, or driver scope as an isolation key. The isolation boundary is and stays `(tenant, user, session)` + `run`. A design that lets an agent's own "account" widen, replace, or short-circuit the tuple is a multi-isolation violation, not an enhancement — even in the multiplayer case the article motivates. *Rationale:* multi-isolation from V1, one user safely in many concurrent sessions, and no package-level identity state are the three product properties this protects.

**B2 — Dynamic workflows are SELECTION over a typed catalog, never runtime-authored executable orchestration.** A harness is chosen from a pre-registered, typed catalog and **compiles DOWN to an engine flow** the runtime already knows how to run; selection fails loudly on an unknown name. A planner MUST NOT emit a richer "run-this-sub-orchestration" object — that would pressure the sealed six-shape `Decision` sum (D-047) and is the obvious move B2 forbids: **no seventh opcode for sub-orchestration.** *Rationale:* runtime-authored code detonates the sealed sum, typed audit/replay, and mandatory-identity/fail-loudly all at once. (The substrate this governs is unbuilt; B2 is a forward guardrail.)

**B3 — "Different agent types" = one planner + composition + steering layers; a new `Planner` concrete is justified ONLY by a genuinely-different control structure.** Policy-on-the-loop (verify, advise, plan-track, approval) and multi-actor orchestration (supervisor, multi-agent, graph, workflow) MUST NOT become planner concretes — they are behaviors on the one loop or the composition layer. New concretes are allowed for control structures that genuinely differ from both ReAct and the engine — the shipped non-LLM tree-walk (`deterministic`) is the existence proof and a legitimate concrete. *Rationale:* proliferating near-duplicate concretes weakens the "Planner is swappable via ONE interface" property instead of deepening one.

## Verdict — build / document / defer

| Pillar | Status | Action |
|---|---|---|
| P1 — Composition | Covered at the primitive level; two pillar-local gaps (the consultation primitive, and a typed fan-out reducer) | DOCUMENT the spectrum + the anti-zoo dividing line; flag the reducer as a composition-layer (engine/concurrency/router) primitive for the RFC; the consultation gap is built in P2 |
| P2 — Consultation primitive | **Genuine net-new gap** (no verifier/critic/advisor exists; RFC §12 defers it) | **BUILD** — one `Consultor` seam (verify + advise modes), state on `RunContext`, results folding onto existing decisions, every consult recorded in the trajectory + a canonical event. Wire into the ReAct planner as its own first consumer |
| P3 — Human-agent boundary | Have the primitives (pause/resume, nine-type steering, four pause reasons, construction-time approval policy, side-effect classes, agent-config projection) | DOCUMENT — name HITL/HOTL as two modes of one boundary; declared per-side-effect posture; decision-level batched approval; document the deterministic-gate layer (resist the shell-hook) |
| P4 — Identity & access | Have the seams; the DNA tension is already resolved | DOCUMENT + GUARD — keep the tuple (B1); credential mechanics are 3/4 built; spec compound authorization on D-219 + BindingScope (not RFC 8693) |
| P4 — Enterprise IdP auth | Framework seam already shipped (D-220); provisioning is a named non-goal | Product, not framework — keep the seams clean; the runtime never provisions |
| P5 — Artifacts as deliverables | Additive gap (storage substrate present; versioned self-updating deliverable absent) | SPEC versioning — a logical-handle layer + `deliverables.*` Protocol surface over the content-addressed store |

The cheapest honest **first consumer** of the one thing this brief argues to build is wiring the consultation primitive into the existing ReAct planner — escalate-on-stuck (advise) plus verify-before-finish (verify). That satisfies CLAUDE.md §13's primitive-with-consumer rule, ships a better default agent, and defers the question of whether any additional planner concrete is ever needed.

## Open questions for the RFC

1. Should the post-V1 planner entry be re-scoped from "additional concretes" to "planner behaviors-on-the-loop + a composition-layer reducer," given that five of its six named entries are policy or composition, not control structures? (B3.)
2. Is a typed result-reducer/synthesizer for fan-out worth a first-class composition-layer primitive (engine/concurrency), or does the next-planner-step implicit synthesis suffice for V1.x? Should `JoinKeyed` (documented-future, `decision.go:105`) back a deterministic fan-out-and-synthesize?
3. Where does the harness catalog live — does it require building the missing `internal/runtime/playbooks` package, or is it a thin registry over engine flows + `RoutePolicy`? Does "compile a skill to a playbook" need a new skills→flow compiler, and is that V1.x or post-V1?
4. Does the consultation stop/verify-fail terminal reuse `FinishNoPath`, or does the `FinishReason` enum gain a `FinishVerificationFailed` value? Where does the "stuck" signal come from — reusing `RepairCounters` conflates output-format failures with reasoning stalls, and the planner has no confidence score today.
5. Is the `Consultor` a planner-package seam or a runtime-side service the planner calls through a `RunContext` view (keeping the tier-up LLM call out of the planner's import graph)? Should the advisor model be agent-config or per-tenant policy resolved at run start (the latter risks re-importing the immutability subtlety §2.4 corrects)? Does verify-before-side-effect need the tool side-effect class formalized as a typed enum?
6. Should boundary posture (HITL/HOTL per side-effect class) be a first-class agent-config field or layered onto the construction-time `ApprovalPolicy`? For `CallParallel{JoinAll}`, does a decision-level batched approval surface one gate or N, and what are the partial-approval semantics given REJECT-is-terminal (D-071)? What is the default posture when an agent declares none — should write/external/stateful default to in-the-loop?
7. Is a unified, operator-composable deterministic-authority layer (approval + governance + compression) warranted, or do the per-subsystem typed policies suffice?
8. Should Harbor adopt an outbound egress-host allowlist, and at which layer (per-driver vs one runtime egress gate) so HTTP/MCP/A2A share one enforcement point? Where does its policy live so it derives from declared agent-config, never per-request body?
9. When/if RFC 8693 lands, is compound authorization realized as an exchanged on-behalf-of token, or does it stay a two-gate check over BindingScope + verified ctx? How does the deferred session-user safe subset compose a non-admin principal's authority with agent-bound credentials? Does a future shared/multiplayer session need a distinct identity model, and is that runtime or product?
10. Is a deliverable a new top-level subsystem or a thin logical-handle layer inside `internal/artifacts`? How does `artifacts.delete` cascade/tombstone against an append-only version chain that pins immutable blobs forever, and who pays storage cost? Is versioning V1-additive or post-V1? Is `Kind` opaque to the Runtime?

## What comes next

The two genuine gaps this brief isolates — the **consultation primitive** (P2, the one BUILD) and **artifact versioning** (P5, an additive spec) — are the candidates for targeted RFC-section additions: a new reference-planner consultation section anchored on the sealed `Decision` sum and the `RunContext`-owned state pattern, and a versioned-deliverable layer specified at the Protocol surface over the existing content-addressed store. Everything else in this brief is DOCUMENT or GUARD against drift. The adoption, sequencing, and final API of all of it is RFC-territory; this brief has argued only the shapes and where copying the obvious pattern would cost Harbor its DNA.
