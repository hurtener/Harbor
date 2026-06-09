# Harbor SDK friction audit — full-seam fan-out (2026-06-09)

> Status: findings record. Produced by a 10-investigator parallel audit (one per
> seam, read-only, every claim cited `file:line`) followed by an adversarial
> verification pass: every blocker/major finding got an independent skeptic agent
> instructed to refute it. **Of 34 blocker/major findings, 0 were refuted**; 3
> were downgraded in severity with reasons noted. 53 agents total. Companion to
> `docs/notes/phase-84bcd-sdk-lens-review.md` (the 84-band prior art whose three
> settled findings — cmd-only run loop, the `internal/` import boundary, the
> 84b/c/d amendments — were excluded from re-reporting here).
>
> The lens: RFC §1 claims Harbor "ships as a Go module plus a single static
> binary." Harbor must hold as an embeddable Go SDK even though the
> Protocol/Console path is the house-favored client. The audit hunted for
> capabilities whose only real wiring is `cmd/harbor` or the Protocol.

## 0. Verdict in one paragraph

The **bottom layers are genuinely SDK-shaped** — store factories with §4.4
seams, `memory.Open`+`Deps`, `llm.Open`'s by-construction safety client,
`steering.NewRunLoop`, `pauseresume.New`, `planner.Resolve` — the investigators
repeatedly called individual packages "the model headless seam." The friction is
not in the primitives; it is **stratified one layer up**, in five repeating
patterns: (1) a **package-main stratum** of production semantics that lives only
in `cmd/harbor` with an already-diverged devstack mirror; (2) a large class of
**primitives with zero production consumers anywhere** — not even the dev binary
— several of them behind config knobs that validate cleanly and then silently do
nothing; (3) **Protocol auth vocabulary leaked into in-process runtime control**
(approval resolution); (4) the **external-module boundary**, where the product's
own scaffold output cannot compile and the test kit is presented as the runtime
library; and (5) **config duality**, where five subsystems' config→snapshot
projections are unexported package-main helpers that have already shipped one
silent-field-drop bug and carry a second live one today. The audit also surfaced
**three outright product bugs** (not just SDK friction) — see §1.

Per-seam reachability: runloop/tasks **partial** · planner/RunContext
**partial** · LLM stack **partial** · tools/dispatch **partial** ·
pause/steering **partial** · memory/skills **partial** · stores/events **yes** ·
config duality **partial** · external surface **no** · governance/telemetry
**partial**.

## 1. Correctness bugs found incidentally (fix independent of any SDK program)

These fail on every path, including `harbor dev` — surfaced because the audit
asked "who actually drives this?"

**B1 — Approval-gated `CallTool` deadlocks the run loop; no working HITL
choreography exists anywhere.** The run-loop goroutine calls
`ToolExecutor.ExecuteDecision` synchronously (`internal/runtime/steering/runloop.go:602`);
the approval wrapper blocks inside `Invoke` via `gate.RunGuarded`
(`internal/tools/catalog/catalog.go:425` → `internal/tools/approval/gate.go:177-303`);
the inbox is drained only at step boundaries (`runloop.go:381`), so the D-097
APPROVE/REJECT bridge (`internal/runtime/steering/apply.go:225-279`) can never
fire mid-step. A planner-dispatched gated tool hangs until ctx cancellation.
HITL is one of the four canonical reasons the unified pause primitive exists
(`pauseresume.go:91-106`). *Fix:* dispatch `ExecuteDecision` on a per-step
goroutine and keep draining the inbox while it's in flight (or ship an exported
`ApprovalResolver` the RunLoop spawns per run), exercised by an E2E whose gated
tool is dispatched **by the planner through the executor**, not a side goroutine.

**B2 — Session GC can reap RUNNING sessions: the `RunningProbe` seam is filled
by nothing, anywhere.** The no-op default returns `(false, nil)`
(`internal/sessions/sessions.go:103-120`), the sweeper auto-starts in
`sessions.New` (`registry.go:113`), and both reference assemblies omit
`WithGCPolicy` (`cmd/harbor/cmd_dev.go:791`, `harbortest/devstack/devstack.go:749`)
despite the TaskRegistry being in scope. The RFC §6.9 "GC never reaps a RUNNING
session" invariant is unenforced on every path — silent at the 24h-IdleTTL
timescale. *Fix:* exported `sessions.TaskRunningProbe(reg tasks.TaskRegistry)
RunningProbe`, wired in both assemblies in the same PR (§17.6), with a
RUNNING-survives-GC integration test.

**B3 — Live devstack planner-config drift.** Production maps `ExtraGuidance` /
`ReasoningReplay` / `MaxToolExamplesPerTool` / `ParallelToolCalls`
(`cmd/harbor/cmd_dev.go:1741-1752`); the devstack mirror maps only
`Driver`/`MaxSteps`/`Extra` (`devstack.go:1259-1264`) — despite its own "MUST
track production field-for-field" comment. A devstack-assembled stack silently
drops four planner knobs **today**. This is the D-155 silent-field-drop bug
shape recurring, in the exact place D-155's fix comment predicted it would.

Honorable mention: `tools.http_manifests` is documented, exemplified
(`examples/harbor.yaml:442-449`), validated (`internal/config/validate.go:823`)
— and consumed by **nothing** repo-wide; populated `governance.identity_tiers`
yields posture display and **zero enforcement** (`governance.SetFactory`'s only
caller is a test, `internal/governance/registry_test.go:97`). Both are
fail-loud violations: clean validation, silent no-op.

## 2. Pattern 1 — the package-main stratum (and the D-094 mirror tax)

The settled finding was "the run loop lives in cmd." The audit inventoried
what *else* does. These are production semantics, not CLI plumbing — the
investigators confirmed `cmd_dev_runloop.go`/`cmd_dev_executor.go` import only
`internal/` packages and plain config values, so promotion is mechanical.

| # | What lives only in `package main` | Evidence anchor | Smallest fix |
|---|---|---|---|
| P1 | **The only full `steering.ToolExecutor`** — D-026 heavy-result→artifact promotion (`projectForLLM`), `CallParallel` aggregation, `SpawnTask`/`AwaitTask` driving with depth caps | `cmd_dev_executor.go:61,157-170,457-517,224-415` | Export as `internal/runtime/dispatch.NewToolExecutor(catalog, artifacts, tasks, opts...)`; cmd + devstack become callers |
| P2 | **RunContext population**: `projectMemoryBlocks`, `projectSkillsContext`, `extractSkillKeywords` (the FTS5/BM25 query shaping, D-156), `extractAssistantAnswer`, `resolveInputArtifacts` policy — only `BuildArtifactManifest` was ever promoted | `cmd_dev_runloop.go:996-1145,856-911` vs `internal/planner/artifact_manifest.go` | Promote the five siblings next to `BuildArtifactManifest` (the proven precedent) |
| P3 | **Task-FSM bridge contract**: `Finish.Reason`→`TaskError.Code` mapping, the Phase-106 answer envelope `{answer, finish_reason, tool_calls_seen}` — a cmd↔cmd implicit wire contract (`cmd_dev_executor.go:417-443` parses what `cmd_dev_runloop.go:777-789` marshals) | `cmd_dev_runloop.go:717-817` | Exported `AnswerEnvelope` type + error-code constants in `internal/tasks` or `internal/planner` *(verifier downgraded major→ lower: subsumed by P1's promotion)* |
| P4 | **Event-emission closures**: the identity-stamping `Emit` + `OnChunk` constructors, with the in-code comment recording 280+ bus-rejected chunks/task when wired wrong | `cmd_dev_runloop.go:601-660` | `events.IdentityStampingEmitter(bus,q,logger)` + `llm.NewChunkPublisher(...)` (~20-line pure functions) |
| P5 | **Catalog→planner view** (identity+scope filter; per-run, never cached — the cross-tenant warning lives in a package-main comment) | `cmd_dev_catalog_view.go:31-70` | `tools.NewPlannerView(cat, filter)` |
| P6 | **MCP attach lifecycle** (config→Connect→Discover→Register incl. `ToolPolicy` projection) — devstack's copy **already drops policy projection silently** | `cmd_dev.go:2051-2198` vs `devstack.go:1921+` | Exported `mcp.Attach(...)` helper incl. policy projection |
| P7 | **OAuth provider assembly** (KEK resolve → sealer → token store → provider factory loop) | `cmd_dev.go:1637-1720` | Exported `auth.BuildProviders(ctx, cfg.Tools, deps)` |
| P8 | **The config→stack fan-out itself** (~700 ordered lines: stores → bus → llm → memory → skills → tasks → catalog → coordinator → planner, with closers) — the only other copy demands `*testing.T` (`devstack.Assemble:444`; the error-returning `tryAssemble:467` is unexported) | `cmd_dev.go:396-771` | Promote `tryAssemble`'s shape to an exported, error-returning `Assemble(ctx, cfg, opts)` both wrap |
| P9 | **The complete production blank-import block** (~30 lines, only in `main.go:27-95`) | `cmd/harbor/main.go` | One aggregator package (`internal/drivers/prod`) cmd and embedders both import |

**The mirror tax, quantified.** The D-094 devstack mirror — the official
external test surface — has materially diverged: its executor is CallTool-only
and skips D-026 promotion by its own admission (`devstack.go:2103-2143`),
`MarkComplete` passes an empty `TaskResult{}` (`:1639`, no answer envelope), it
wires neither `Emit` nor `OnChunk`, has no background-task driving, no
SearchCache (so the 107c `tool_search`/`skill_search` meta-tools return
honest-empty, `devstack.go:666`), and carries the B3 planner drift. An external
consumer building on devstack gets an agent whose first >32KB tool result trips
`ErrContextLeak` (`internal/llm/safety.go:99`) — the *opposite* failure of
production. The kit validates weaker semantics than production ships; every
promotion in the table above deletes a mirror copy.

A detail worth savoring: `internal/planner/react/prompt.go:1163-1213`
pattern-matches the heavy-truncation map shape and **cites
`cmd_dev_executor.go::heavyTruncationSummary` as its source of truth** — an
`internal/` package documenting `package main` as its shape contract. The
dependency arrow already points the wrong way; promotion just makes it honest.

## 3. Pattern 2 — primitives with zero production consumers (the D-149 class, one step worse)

The Phase 83f / D-149 failure class is "the seam exists, production wiring
doesn't fill it." The audit found a band of seams that not even the dev binary
fills:

| Primitive | State | Evidence | Disposition recommendation |
|---|---|---|---|
| **Governance enforcement** (cost/rate/MaxTokens) | `SetFactory` never called outside one test; enforcer constructors consumed only by `wave7b_test.go:239`; populated `identity_tiers` = posture-only | `internal/governance/registry.go:43`, `cmd_dev.go:1145` | Export `governance.SubsystemFromConfig(cfg, state, bus)`; call `SetFactory` from both assemblies when tiers non-empty; until then `validateGovernance` must warn posture-only |
| **Trajectory summarisation** (Phase 46) | `planner.Summariser` has only test impls; `MaybeCompress` has zero call sites; `Budget.TokenBudget` is a dead field whose godoc promises runtime behavior | `internal/planner/compression.go:36-44,135,187`, `planner.go:563-573` | Ship the LLM-backed summariser + a `MaybeCompress` call in the RunLoop gated on `TokenBudget>0` — or mark the seam deferred in godoc + decisions.md so consumers stop designing against it |
| **Tool OAuth completion** | The pause *producer* is live; `CompleteFlow` (the resume half) has zero callers; the "Harbor Protocol callback handler" its godoc names **does not exist** (`auth.go:189`); a bare Resume re-parks immediately | `internal/tools/auth/provider.go:424-625` | Exported `auth.CallbackHandler(providers) http.Handler`, mounted by cmd and mountable headless; fix the godoc |
| **Durable pauses** | `WithCheckpointStore` has zero production consumers; RunLoop checkpoints `Trajectory: nil` (`runloop.go:670`) so a rehydrated pause can't restore planner state | `pauseresume/coordinator.go:82-84`, `cmd_dev.go:670` | Thread the run's trajectory into `requestPause`; configure the store in both assemblies; pause→restart→resume test |
| **Pause GC/expiry** | `DecisionTimeout` is vocabulary with no producer ("Phase 50 does not yet emit this"); the only checkpoint deletion is inside `Resume`; cancel-while-paused orphans records forever | `pauseresume/decision.go:45-51`, `coordinator.go:234-244` | `WithMaxParkDuration` + an exported sweeper over the existing List/Resume surface |
| **Phase 38/41 skills tools + generator** | `skills/tools.Register` (capability filter, redaction, token budgeter) and `generator.Register` called by nothing; `main.go:76-90` still promises "Phase 60+ bootstrap will call Register"; production registers a **thinner parallel implementation** (`internal/tools/builtin/skill_search.go`) — the §13 two-implementations smell | `skills/tools/tools.go:203`, `cmd_dev.go:655-661` | Pick one canonical surface: either builtin delegates to the rich handlers, or a decisions.md entry formally supersedes Phase 38 and the stale promise is deleted |
| **Skills.md ingestion** | Importer is exported but test-only; the documented `harbor skill import` verb **does not exist** (`root.go:93-101`; `docs/skills/configure-memory-and-skills/SKILL.md:94,114,138` documents it — §18 drift, live) | `skills/importer/importer.go:189` | Ship the verb calling an exported `ImportAndStore` helper, or excise the fictional verb from the SKILL.md in the same PR |
| **Phase 39 skills Directory** | `NewDirectory` consumers are tests only; the production path bypasses it with raw `store.Search` | `skills/directory.go:172`, `cmd_dev_runloop.go:545` | Wire it or formally supersede it — "a headless consumer cannot tell which retrieval surface Harbor stands behind" |
| **HTTP-manifest + A2A tool legs** | `LoadManifest`/`RegisterManifest` and `a2a.New` have zero construction sites; `cfg.Tools.HTTPManifests` is a validated dead knob; `main.go:120`'s a2a blank import is a **no-op** (package has no `init()`) | `tools/drivers/http/manifest.go:119,267`, `config.go:603` | Wire both where MCP attach lands, or delete the dead knobs + decoy import and document headless-only |
| **Canonical telemetry Logger + runtime.error chain** | `telemetry.New` (redactor-mandatory, identity-stamped) has zero non-test callers — cmd boots **bare slog** (`cmd_dev.go:258`); `engine.WithRunErrorHandler`'s godoc describes "production wiring" that doesn't exist (`engine/options.go:114-127`) | `telemetry/logger.go:87` | Wire `telemetry.New` + the bus emitter in cmd/devstack; correct the godoc |
| **OTel tracing (Phase 55)** | `NewTracer` never constructed on any production path; exporters blank-imported for nothing (`main.go:101-104`); metrics got `BridgeBusToMetrics`, traces got no bridge | `telemetry/tracing.go:205,310` | `telemetry.BridgeBusToTracer(ctx, bus, tracer, filter)`, symmetric with metrics |

The §13 primitive-with-consumer rule, read against the current tree, is the
through-line: these all shipped with test consumers and the production consumer
"landed later" — i.e., never. The SDK lens makes it visible because the headless
consumer asks the question CI doesn't: *who calls this?*

## 4. Pattern 3 — Protocol vocabulary in runtime control paths

`ApprovalGate.ResolveApproval` hard-requires `internal/protocol/auth` scope
claims on ctx (`gate.go:13,353-355` → `ErrApprovalScopeRequired`), and
`ResolveApproval` is the **only** sender on the resolve channel — a direct
`Coordinator.Resume` leaves the `RunGuarded` waiter hung. The runtime's own
steering bridge has to self-elevate via `protocolauth.WithScopes`
(`steering/apply.go:373-374`) to call its own gate — the tell that the check
sits one layer too low. The verifier downgraded this from blocker-adjacent to a
calibrated major: the workaround is exported and the steering-inbox path hides
the elevation — but that path can't fire for gated tools until B1 is fixed.
*Fix:* make the privilege check an injected seam (defaulting to protocolauth at
the Protocol edge, identity-tuple-check for direct construction), or adopt the
existing precedent `internal/runtime/registry.WithControlScope`. Otherwise the
direction check came back clean: core runtime imports of `internal/protocol/types`
are pure-data projection vocabulary (defensible per "the Protocol is the
canonical contract"), and the `<area>/protocol` subpackages are honest one-way
adapters — `internal/memory` core imports only `identity`. Record the rule:
**runtime may import protocol *types* (data), never protocol auth/methods/
transports (behavior)** — auth is the one standing violation.

## 5. Pattern 4 — the external-module boundary (reachable_headless: **no**)

The external-surface investigator ran empirical probes (throwaway external
module + `replace` directive — which does **not** lift the `internal/` rule):

- **`harbor scaffold` output cannot compile the moment it declares a tool.**
  The template emits an external module (`go.mod.tmpl:1`) whose `agent.go`
  imports `internal/tools`, `internal/tools/builtin`,
  `internal/tools/drivers/inproc` (`agent.go.tmpl:27,30,33`). Verified by
  reproduction: `go mod tidy` fails with "use of internal package … not
  allowed". The only scaffold smoke (`phase-67.sh:125-139`) builds the
  **toolless** template, so CI never sees it. The product's own golden path is
  broken for its advertised audience. *Immediate gate:* extend phase-67.sh to
  scaffold `--from-config` with one built-in + one custom tool and `go build` it.
- **harbortest's advertised surface is type-poisoned externally.** Only
  zero-Deps `RunOnce` + `AssertNoLeaks` + EventLog reads compile from outside.
  `AssertSequence` wants `[]events.EventType`; `NewFaultInjector` wants
  `tools.ToolCatalog`; `Deps.Bus/Redactor/Identity` are internal types with no
  external constructors (`assertions.go:24`, `simulate.go:50`,
  `runonce.go:31-47`) — while `doc.go:42-48`, the recipe, and the README all
  claim full external importability. And an external `Agent` under `RunOnce`
  **cannot emit events or read identity at all** (the bus/identity accessors
  are internal), so the captured EventLog is structurally empty and
  `AssertNoLeaks` passes vacuously — the test kit tests nothing about an
  external agent. *Fix:* re-home the kit's parameter vocabulary into kit-owned
  public types (`harbortest.Identity{...}`, string-based `AssertSequence`,
  `harbortest.NewBus()/NewCatalog()`, Bus/Identity-from-context helpers).
- **devstack is importable but unusable externally**: `Assemble` takes
  `*testing.T` + `*internal/config.Config`; its required blank imports are all
  `internal/` paths; the error-returning core is unexported (re-exported only
  in `export_test.go`). It is a test fixture wearing the assembly-entry-point's
  clothes.
- **The recipes actively teach the broken pattern**: `define-a-tool.md:48-85`
  hands an explicitly-external reader (`scaffold-an-agent.md:40-42` prescribes
  the replace directive) `internal/` import blocks the toolchain rejects.
- **README.md:93-96** says "Build an agent against the runtime library:" and
  imports… `harbortest`. The test kit is the runtime library's stand-in because
  it is the only thing that *can* be.

**Facade promotion inventory** (the verified-complete list of what the
templates, recipes, and devstack already pretend is public — an RFC-level
decision per the Phase 71 precedent, not a phase plan footnote): (1) identity
(Identity, Quadruple, With/WithRun/From); (2) events vocabulary (EventBus,
Event, EventType, Filter, Open, WithBus) + a programmatic config/options
surface; (3) tools (ToolCatalog, NewCatalog, ToolDescriptor, RegisterFunc,
builtin); (4) llm (LLMClient, Open, ConfigSnapshot + public driver import
path); (5) store ifaces + factories + public driver-registration paths;
(6) planner/tasks/steering + the promoted run loop; (7) the error-returning
assembly entry. Note this dovetails with Pattern 1: **promotion out of
`package main` is the prerequisite for promotion out of `internal/`** — you
cannot facade what currently lives in a binary.

## 6. Pattern 5 — config duality

- **Five subsystems** (llm, memory, skills, planner, governance) decouple via
  snapshot types but keep the config→snapshot projection as unexported
  package-main helpers, duplicated in devstack — the mechanism behind shipped
  bug D-155 and live bug B3. *Fix:* one exported `FromConfig` per owning
  package (`llm.SnapshotFromConfig`, `memory.SnapshotFromConfig`,
  `planner.ConfigFromYAML`, `skills.SnapshotFromConfig`,
  `governance.ConfigFromOperator`), both assemblies converted, duplicates
  deleted.
- **Planner-adjacent knobs bypass the planner boundary**: `skills_context_max`,
  `planning_hints`, `absolute_max_spawn_depth`, `granted_scopes` never reach
  `planner.PlannerConfig` — they're projected onto RunContext/executor params
  only inside cmd's run loop, with the default constants duplicated in three
  places; `config.go:1082-1088`'s godoc even admits "only consumed by the dev
  binary's per-task run loop." Settable in YAML, invisible to the SDK path.
- **Defaults are loader-only**: `defaults()` is unexported (`loader.go:165`);
  a hand-built Config gets none, and factories are inconsistent (events fails
  loud on zero values; sessions self-defaults). *Fix:* export
  `config.Defaults()` and/or adopt factory-side `withDefaults` uniformly.
- **`config.Validate` unconditionally demands JWT identity config**
  (`validate.go:33-36,124-133`) — Protocol-server ceremony dragged into
  headless config use. *Fix:* a validation profile (`ValidateCore`) or the
  documented one-liner in a recipe.
- Dead knobs: `tools.http_manifests` (§1), `governance.identity_tiers`
  enforcement (§3 table).

## 7. The recipes gap (every seam, same finding)

All ten investigators independently filed "no headless recipe" — the only
assembly prior art is `bootDevStack` (1,100 lines, package main), devstack
(test-framed, stale), and three integration tests. The recipe family to add as
the fixes land: `embed-harbor-headless.md` (the substrate + fan-out),
`embed-the-llm-client.md` (incl. the wrapper blank-import gotcha),
`steer-and-resume-a-run.md` (pause/steer choreography incl. what's not yet
supported), `use-memory-and-skills-from-go.md`,
`observe-an-embedded-runtime.md` (audit→bus→logger→metrics order + blank
imports). The runloop investigator's closing line stands for all of them:
*"until the promotions land, the recipe cannot honestly be written — which is
itself the cleanest signal of this seam's friction."*

One LLM-specific trap deserves its own callout: **`llm.Open` silently degrades
when the wrapper hooks aren't seated.** The corrections/downgrade/retry/
governance chain is installed only by `cmd/harbor/main.go`'s blank imports
(`main.go:51-68`); `llm.Open` skips any nil hook with no warning
(`registry.go:457-505` — the godoc admits it), and **devstack itself composes
the client without corrections/downgrade/retry** (its documented
"Required blank imports" list omits them, `devstack.go:426-439`) — invisible
under the mock driver, real against live providers. *Fix:* warn/fail when a
hook is nil and its Disable flag is false; better, one importable
`internal/llm/llmstack` leaf whose `init` seats the production set, imported by
cmd, devstack, and embedders alike.

## 8. Prioritized program

**Wave A — correctness (independent of SDK goals, fix-now):** B1 HITL deadlock
(+ the planner-dispatched E2E), B2 RunningProbe wiring, B3 devstack planner
drift + executor/envelope/Emit/SearchCache parity (or fast-track the Wave B
promotions that delete the mirror), fail-loud on the two dead config knobs,
`llm.Open` nil-hook warning, fix the lying godocs (`engine/options.go:114`,
`auth.go:189`, `compression.go:36`, the fictional `harbor skill` verbs per §18).

**Wave B — the great re-homing (mechanical, high-leverage):** the §2 table —
executor → `internal/runtime/dispatch`; the five RunContext projection helpers
→ next to `BuildArtifactManifest`; answer envelope + error codes → exported
types; Emit/OnChunk constructors; catalog view; MCP attach (+ policy
projection); OAuth provider assembly; the five `FromConfig` projections; the
fan-out (`tryAssemble` promoted to an exported, error-returning `Assemble`);
the blank-import aggregator. Every item deletes a devstack mirror copy;
collectively they reduce D-094 from "hand-maintained mirror" to "thin caller."

**Wave C — finish or formally defer the half-shipped primitives (§3 table):**
each needs either its first production consumer or a decisions.md carve-out +
honest godoc. The skills-surface decision (builtin vs Phase-38 rich tools) and
the Directory decision are the two that block a coherent headless skills story.

**Wave D — the external facade (RFC-level):** the §5 promotion inventory, the
harbortest vocabulary fixes, the scaffold-with-tools smoke gate, the recipe
family, and the README sentence made true. Wave B is the prerequisite; Wave D
is where "ships as a Go module" stops being aspirational.

## 9. Cross-references

Verification notes: 3 severity downgrades — P3 (subsumed by P1), and the two
protocolauth findings (workaround exists + exported; see §4). Several verifiers
*strengthened* findings beyond what investigators filed (a third mirror copy of
`extractSkillKeywords`; a third fictional CLI reference in the SKILL.md; the
react-prompt→cmd shape-contract citation; empirical external-module
reproductions). Full structured output (62 findings, every evidence line, every
verdict): `sdk-friction-audit-raw.json` (a session working artifact, not
committed to the repo).

Prior art: `docs/notes/phase-84bcd-sdk-lens-review.md` (84-band),
D-149/Phase 83f (the seam-without-wiring class), D-155 (config-projection
drift), D-094 (the mirror), §17.6 (fix-both-sides), §13
(primitive-with-consumer, two-implementations, silent degradation), §18
(skill-doc drift). The patterns here are those rules' violations, found by
asking each seam the question the SDK consumer asks: *can I reach this without
the binary?*
