# Phase 83m — warn-cleanup

## Summary

Phase 83m closes the eight WARN-tier items the §17.5 audit + Wave 17
operator validation surfaced. Each item is small individually; the
band ships them together because they share the "the surface works
but a hygiene corner is dead" failure mode. No new operator surface
— this phase is pure production hygiene.

To keep the band reviewable, the work is split into two buckets
(dispatched as parallel worktree agents; integrated by the
coordinator). The buckets are chosen on package boundary so the
agents touch disjoint files.

## RFC anchor

- RFC §6.4 — Tool catalog (items 1 + 6: MCP identity + GrantedScopes).
- RFC §6.5 — LLM client (item 5: per-call timeout).
- RFC §6.6 — Memory (item 4: skills query extraction lives next to the
  per-run skills.Search call).
- RFC §6.2 — Planner subsystem (item 8: reasoning trace round-trip).
- RFC §6.8 — Tasks (item 7: tool_count counter).
- RFC §8 — CLI (items 2 + 3: hot-reload watcher + lifecycle closers).

## Briefs informing this phase

- brief 06
- brief 02

## Brief findings incorporated

- brief 06 §4 — Production hygiene is not optional. Each dead WARN
  surfaces, on its own, as the operator's first bad experience: a
  cross-tenant identity reuse, an infinite reboot loop on the main
  sqlite file, a goroutine leak from an unclosed lifecycle, a tool
  call the planner can't see the trace of. The audit-trail discipline
  the brief argues for is what 83m closes.
- brief 02 §3 — The reasoning channel is a first-class surface;
  capturing it on `trajectory.Step.ReasoningTrace` is what makes
  multi-step ReAct replay-able. Item 8 closes the gap Phase 83e
  documented (`ReasoningReplay=text` mode is currently ineffective
  in production because the runloop's trajectory append leaves
  `Step.ReasoningTrace` empty).

## Findings I'm departing from (if any)

None.

## Goals

Eight surgical fixes, no surface changes operators see except via
correctness improvements.

### Bucket A — cmd/harbor + tool drivers

**Item 1 — MCP DefaultIdentity per-push.** The Phase 83g
`attachDevMCPServer` sets `mcp.Config.DefaultIdentity` to
`{DevTenant, DevUser, DevSession}` at attach time; the driver uses
this for every server-pushed event. A future multi-tenant operator
sees the wrong identity stamped on cross-tenant push events. Fix:
the driver reads identity from `ctx` at push time (when available);
the DefaultIdentity becomes the fallback for transport-side events
that arrive without an inflight call (notifications). The cmd_dev
caller still passes the dev triple but its role narrows to "the
identity for transport-level events" rather than "every call's
identity."

**Item 2 — sqlite-main-file watcher.** Phase 83h's hot-reload fix
extended `dbSidecarSuffixes` to skip `.sqlite-wal` / `.sqlite-shm` /
`.sqlite-journal` / `.db-wal` / etc., but did NOT skip the MAIN
`.sqlite` / `.db` file. Every SQLite commit rewrites the main file
too, which still triggers a hot-reload. The reboot loop is just
slower. Fix: add `.sqlite` / `.db` to the skip list (a SQLite-
managed file should never trigger hot-reload regardless of which
component is rewriting it).

**Item 3 — draftStore + agentRegistry lifecycle closers.** Both
subsystems are constructed in `bootDevStack` but neither's `Close`
is appended to the `closers` chain. A clean shutdown leaves goroutines
and file handles dangling. Fix: append the close functions in the
documented dependency order (after construction, before the next
subsystem opens).

**Item 4 — Skills query keyword extraction.** Today the dev runloop
calls `skills.Search(taskCtx, sessionQ, task.Query, cap)` with the
RAW task query — full sentences, punctuation, articles. The FTS5
ranker in the SQLite skills driver does best with a keyword-shaped
query. Fix: extract keywords (lowercase, drop stopwords + punctuation,
dedupe; cap at ~10 terms) before calling Search. The change lives in
one small helper in `cmd/harbor/cmd_dev_runloop.go`; the devstack
mirror gets the same helper per D-094.

**Item 6 — Catalog GrantedScopes plumb-through.** The
`runtimeCatalogView` filter takes a `granted []string` parameter
but the dev runloop passes `nil`, hard-coded with a `TODO (Phase 83m)`
comment. Fix: read the configured operator scopes from
`cfg.Tools.GrantedScopes` (new yaml field; defaults to nil — backward
compatible) and pass them through. Validator + CONFIG.md update + the
existing doc-drift gate keeps the new field documented.

### Bucket B — internal/llm + tasks + steering + planner

**Item 5 — Per-call LLM timeout.** `internal/llm/safety.go::Complete`
applies `defaultRequestTimeout` (5min) when the ctx has no deadline
— it ignores `c.cfg.Timeout` (the operator-configured value). Fix:
prefer `c.cfg.Timeout` when it's > 0; fall back to
`defaultRequestTimeout` only when the operator left it zero. Single-
line change with a unit test.

**Item 7 — task.tool_count increment.** The wire shape
`prototypes.Task.ToolCount` exists (Phase 73h) but has NO production
producer — every Console renders 0. Fix: add `ToolCount int` to
`tasks.Task`; add `IncrementToolCount(ctx, taskID) error` to the
`TaskRegistry` interface; implement on the inprocess driver under
the existing FSM lock; wire the call from the runloop's CallTool
dispatch path; project the value in
`internal/tasks/protocol/registry_projector.go::projectRow`.

**Item 8 — Reasoning trace round-trip.** Phase 83e captured reasoning
on `llm.CompleteResponse.Reasoning` and on `planner.decision` events,
but the runloop's trajectory append (Phase 83i) leaves
`trajectory.Step.ReasoningTrace` empty — so `ReasoningReplay=text`
mode is structurally ineffective. Fix: extend the planner's Decision
interface (or its non-Finish variants) to carry the per-step reasoning,
and have the runloop copy it into `Step.ReasoningTrace` on append.
Re-validate the `ReasoningReplay=text` round-trip via the existing
83e test surface.

## Non-goals

- **A 9th WARN item not in the list above.** The audit may surface
  more during the §17.5 closeout; those land in their own band.
- **A schema migration for `tasks.Task.ToolCount`.** The field is
  in-memory only (the inprocess driver's state) — no SQLite/Postgres
  migrations exist for it because the V1 inprocess driver is the
  only consumer today.
- **A new operator-visible knob for the per-call LLM timeout.** The
  config field already exists (`llm.timeout`); item 5 just makes
  the wrapper actually use it.

## Acceptance criteria

### Bucket A

- [ ] MCP driver reads identity from `ctx` when present; falls back
      to `DefaultIdentity` only for transport-level pushes (item 1).
- [ ] `cmd/harbor/cmd_dev_hot_reload.go` skips `.sqlite` + `.db` main
      files (item 2).
- [ ] `bootDevStack` appends `draftStore.Close` + `agentRegistry.Close`
      to the closer chain (item 3).
- [ ] `cmd/harbor/cmd_dev_runloop.go` runs the task Query through a
      `extractSkillKeywords()` helper before `skills.Search` (item 4).
- [ ] New `tools.granted_scopes []string` yaml field; validator
      accepts any non-empty list of strings; runloop passes it
      through to `newRuntimeCatalogView` (item 6).
- [ ] Devstack mirrors all of the above per D-094.
- [ ] `docs/CONFIG.md` documents `tools.granted_scopes`; doc-drift
      test passes.

### Bucket B

- [ ] `internal/llm/safety.go::Complete` uses `c.cfg.Timeout` when set
      (item 5).
- [ ] `tasks.Task.ToolCount int` field added; `TaskRegistry.IncrementToolCount`
      method added; inprocess driver implements it; runloop calls it
      on CallTool dispatch; `projectRow` projects it (item 7).
- [ ] `planner.Decision` non-Finish variants carry per-step
      reasoning (or an equivalent runloop accessor exists); runloop
      writes it into `trajectory.Step.ReasoningTrace` on append
      (item 8).
- [ ] Unit tests pass for each item's primary code path.

### Cross-bucket

- [ ] `scripts/smoke/phase-83m.sh` static-asserts the surface
      changes in both buckets.
- [ ] `make drift-audit` + `make check-mirror` + `make preflight`
      all pass.

## Files added or changed

### Bucket A

- `internal/tools/drivers/mcp/mcp.go` + matching test — identity-from-ctx
  push helper.
- `cmd/harbor/cmd_dev.go` — drop the unused `DefaultIdentity` cache;
  append draftStore + agentRegistry closers.
- `cmd/harbor/cmd_dev_hot_reload.go` — extend skip list.
- `cmd/harbor/cmd_dev_runloop.go` — extractSkillKeywords helper +
  GrantedScopes plumb-through.
- `cmd/harbor/cmd_dev_catalog_view.go` — minor doc update.
- `internal/config/config.go` + `validate.go` — `ToolsConfig.GrantedScopes`.
- `harbortest/devstack/devstack.go` — D-094 mirror for items 1, 3, 4, 6.
- `docs/CONFIG.md` — `tools.granted_scopes` entry.

### Bucket B

- `internal/llm/safety.go` + matching test — use cfg.Timeout.
- `internal/tasks/tasks.go` + matching test — Task.ToolCount + interface
  method.
- `internal/tasks/drivers/inprocess/inprocess.go` + matching test —
  IncrementToolCount implementation.
- `internal/tasks/protocol/registry_projector.go` — projectRow projection.
- `internal/runtime/steering/runloop.go` — increment call after CallTool
  dispatch; populate Step.ReasoningTrace from per-step reasoning.
- `internal/planner/planner.go` + react driver — per-step reasoning
  surface (the exact shape is the agent's call; the simplest path is
  a new `Reasoning string` field on each non-Finish Decision variant
  + the react planner populating it from the LLM response).

### Coordinator-owned (post-integration)

- `docs/plans/phase-83m-warn-cleanup.md` — this plan.
- `docs/plans/README.md` — Phase 83m row + flip to Shipped.
- `docs/decisions.md` — D-156.
- `docs/glossary.md` — relevant entries.
- `scripts/smoke/phase-83m.sh`.

## Public API surface

- `tasks.TaskRegistry` gains `IncrementToolCount(ctx, TaskID) error`.
- `planner.Decision` non-Finish variants gain `Reasoning string`
  (exact location is implementor's call).
- `config.ToolsConfig` gains `GrantedScopes []string`.
- `mcp.Config.DefaultIdentity` semantics narrow (still set; now only
  the fallback for transport-side pushes).

## Test plan

- **Unit:** each item's primary code path covered (e.g.
  `TestSafety_UsesCfgTimeout`, `TestIncrementToolCount_HappyPath`,
  `TestExtractSkillKeywords`, `TestRunLoop_PopulatesReasoningTrace`).
- **Integration:** the 83l real-bifrost test continues to pass
  (regression guard for items that touch the runloop); a small new
  integration test asserts the runloop populates `Step.ReasoningTrace`
  end-to-end against the scripted LLM.
- **Conformance:** N/A.
- **Concurrency / leak:** the lifecycle-closer fix is the leak fix;
  the existing goroutine-leak harness should now show fewer dangling
  goroutines on `harbor dev` teardown.

## Smoke script additions

`scripts/smoke/phase-83m.sh` asserts:

- The MCP driver carries an identity-from-ctx helper.
- `dbSidecarSuffixes` (or a successor list) includes `.sqlite` + `.db`.
- `bootDevStack` calls `draftStore.Close` + `agentRegistry.Close` via
  the closer chain (grep for the appends).
- `extractSkillKeywords` exists in the runloop file.
- `internal/config.ToolsConfig.GrantedScopes` field exists.
- `tasks.TaskRegistry` has an `IncrementToolCount` method.
- The runloop calls `IncrementToolCount` on the CallTool dispatch path.
- `Step.ReasoningTrace` is set in the runloop's trajectory append.

## Coverage target

- `cmd/harbor`: 80% (existing).
- `internal/llm`: 85% (existing).
- `internal/tasks`: 85% (existing).
- `internal/runtime/steering`: 85% (existing).
- `internal/tools/drivers/mcp`: 80% (existing).

## Dependencies

- Phase 83g (MCP southbound consumer — item 1).
- Phase 83h (hot-reload watcher base — item 2).
- Phase 83i (RunContext wiring + trajectory append — items 7, 8).
- Phase 83l (real-bifrost integration test — regression guard).

## Risks / open questions

- **`tasks.TaskRegistry` interface widening.** Adding
  `IncrementToolCount` is a non-breaking add to the V1 interface
  (only the inprocess driver implements it today). A future durable
  driver MUST implement it; the conformance suite catches the gap.
- **`planner.Decision` reasoning surface.** The exact shape is the
  agent's call. The simplest path keeps the Decision sum sealed (no
  reasoning field on each variant; instead expose reasoning via a
  side-channel that the runloop reads — e.g. a `LastReasoning`
  accessor on the planner). Either works; D-156 documents which the
  agent picked.
- **Devstack mirror conflicts.** Both buckets touch
  `harbortest/devstack/devstack.go`. The coordinator resolves on
  integration; the conflict surface is narrow (item 1's identity-from-
  ctx helper + item 4's keyword extractor + item 6's scopes pass-through
  — three additive changes, no shared lines).

## Glossary additions

- **DefaultIdentity (MCP fallback)** — `mcp.Config.DefaultIdentity`'s
  narrowed role after Phase 83m. The identity stamped on transport-
  side events (notifications) that arrive without an inflight call.
  Per-call tool invocations read identity from `ctx`.
- **GrantedScopes (catalog filter)** — operator-configured tool-scope
  allowlist (`config.ToolsConfig.GrantedScopes`). Threaded through
  `runtimeCatalogView`'s `CatalogFilter` so tools whose `AuthScopes`
  exceed the granted set are invisible to the planner.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Concurrent-reuse — N/A: 83m is hygiene fixes; no new compiled
      artifacts ship
- [ ] Integration test exists per §17 — 83l is the regression guard;
      83m adds a small reasoning-trace integration assertion
- [ ] Glossary updated
- [ ] If a brief finding was departed from: N/A
