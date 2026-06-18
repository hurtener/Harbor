# Phase 87 — durable-taskservice-backend

## Summary

Ships a **durable `TaskRegistry` driver** so background (and foreground) tasks, task groups, and patches survive a Runtime restart. It is a new driver under `internal/tasks/drivers/durable` that persists every task record through the existing `state.StateStore` (the in-mem / SQLite / Postgres triad) — mirroring the Phase 57 durable *events* driver — and passes the existing `internal/tasks/conformancetest` suite verbatim, plus new restart-recovery tests. The in-process driver stays the default; `durable` is opt-in via `tasks.driver`. Closes D-006 (background-task persistence deferred to post-V1).

## RFC anchor

- RFC §6.8
- RFC §12

## Briefs informing this phase

- brief 05

## Brief findings incorporated

- brief 05 §1: "**TaskRegistry** — orchestration surface for foreground and background work … Harbor unifies foreground and background under a single `TaskID` namespace." The durable driver persists that single namespace; foreground and background records share one durable shape (no split store).
- brief 05 §7 (finding 9) + RFC §12: `MessageBus.Publish` is at-least-once; "handlers must be idempotent on `(TaskID, Edge, EventID)`." The durable driver's persist→publish path stays idempotent on re-delivery and on a recovery-sweep replay, so a restart never double-applies a lifecycle transition or double-emits a `task.*` event.
- brief 05 §7 (finding 5): persistent lifetimes that survive restart "matches the StateStore driver triad." The durable task driver reuses that triad rather than growing a parallel persistence seam — identity isolation + the three drivers + conformance parity come for free (the same reasoning the 109i tool-context capture and the Phase 57 durable events driver used).
- brief 05 §1: "the contracts exist so a distributed backend can land post-V1 without runtime changes." Phase 87 lands behind the unchanged `TaskRegistry` interface — no caller (planner, runloop, steering, Console projection) changes.

## Findings I'm departing from (if any)

RFC §6.8 names "Postgres-as-queue, NATS JetStream" as the illustrative post-V1 durable backend shapes. Phase 87 instead ships a **StateStore-backed** durable driver as the first durable backend. Justification: (a) it is the lower-risk, DRY path already proven by the Phase 57 durable events driver and consistent with the `events.driver: inmem|durable` posture; (b) "durable" here means **single-instance restart-survival**, which is orthogonal to the **distributed queue/bus** concern — that is Phase 86 (durable distributed bus driver), which is explicitly NOT a dependency of 87. A queue-backed `TaskRegistry` driver (Postgres-as-queue / NATS) remains a valid *later* driver behind the same seam. This departure is filed as **D-228**.

## Goals

- A `durable` `TaskRegistry` driver that persists task, group, and patch records through a `state.StateStore`, so `Get` / `List` return the full, identity-scoped record set after a process restart.
- Pass the existing `internal/tasks/conformancetest.Run` suite verbatim (the D-031 gate every task driver inherits).
- A **recovery sweep** on driver open that detects records left non-terminal by a crash (a task `StatusRunning` whose owning run no longer exists) and transitions them to a defined terminal recovery state, emitting the canonical lifecycle event exactly once.
- Opt-in selection via `tasks.driver: durable` + StateStore wiring, with the in-process driver unchanged as `DefaultDriver`.
- Full `(tenant, user, session)` identity scoping and at-least-once idempotency preserved across restart.

## Non-goals

- **Auto re-drive of recovered execution.** Phase 87 recovers the durable *record*; it does NOT resume a crashed task's planner sub-run. Re-driving a recovered task is a runloop/steering concern (the D-097 dead-task lineage) and is deferred. A recovered `Running` task transitions to a terminal recovery state, not back to `Running`.
- **Distributed / multi-instance coordination.** Two Runtimes sharing one durable store and racing on the same task is out of scope — that is Phase 86 (durable bus) + a future queue-backed driver. Phase 87 targets single-instance restart-survival.
- **A new persistence seam or migration.** No new driver registry, no new SQL migration — the driver rides the `state.StateStore` keyed-slot contract.
- **Changing the `TaskRegistry` interface or any caller.** The interface is frozen (Phase 20/21); this phase is a driver only.

## Acceptance criteria

- [ ] `internal/tasks/drivers/durable` implements `tasks.TaskRegistry`, self-registers under `"durable"` from its `init()`, and is reachable via `tasks.OpenDriver("durable", …)`.
- [ ] The durable driver passes `internal/tasks/conformancetest.Run` verbatim (no suite changes, no skips).
- [ ] **Restart-survival test:** spawn tasks + groups + patches → `Close` the driver → open a NEW driver instance against the SAME `StateStore` → `Get` / `List` return every record with identical fields, identity-scoped (cross-tenant / cross-session reads still rejected with `ErrNotFound`).
- [ ] **Recovery sweep:** a record persisted at `StatusRunning` whose run is gone is transitioned to the defined terminal recovery state on the next open, emitting its lifecycle event exactly once (idempotent on a re-open of the same store).
- [ ] Persist/publish stays idempotent on `(TaskID, Edge, EventID)` — a re-delivered or recovery-replayed transition neither double-writes nor double-emits.
- [ ] A non-serializable task payload fails loudly with a wrapped `ErrUnserializable` (never a silent drop / `nil` record) — the §5 fail-loud contract.
- [ ] `tasks.driver: durable` selects the driver; `tasks.driver` empty/`inprocess` still resolves the in-process default unchanged; the durable driver auto-degrades (or fails loud per the chosen posture — see open questions) when no `StateStore` is configured.
- [ ] Example config + `docs/CONFIG.md` document the new `tasks.driver: durable` + StateStore-wiring fields; backward-compatible (new optional fields).
- [ ] Concurrent-reuse test: N≥100 concurrent invocations against one shared durable driver under `-race` — no data races, no context bleed, no cross-cancellation, no goroutine leaks.

## Files added or changed

- `internal/tasks/drivers/durable/durable.go` — the driver (`New(cfg, …, store state.StateStore, opts…) (tasks.TaskRegistry, error)` + `init()` self-registration), mirroring `internal/events/drivers/durable`.
- `internal/tasks/drivers/durable/record.go` — the persisted record shapes + StateStore keying scheme (per-task / per-group / per-patch slots, keyed by identity + `TaskID`).
- `internal/tasks/drivers/durable/recovery.go` — the open-time recovery sweep.
- `internal/tasks/drivers/durable/*_test.go` — conformance hook, restart-survival, recovery, concurrent-reuse, leak tests.
- `internal/config/config.go` + `internal/config/loader.go` — `TasksConfig` gains the StateStore-wiring fields (mirroring `EventsConfig.{StateDriver,StateDSN}`); `Validate` accepts `driver: durable`.
- `internal/drivers/prod/prod.go` — add the durable task driver's blank import to the production aggregator (§4.4 single home).
- `cmd/harbor/` + `harbortest/devstack` — wire the `StateStore` into the durable task driver when `tasks.driver: durable` (the same wiring path the durable events driver uses).
- `examples/*.yaml` + `docs/CONFIG.md` + `docs/skills/define-the-agent-yaml/SKILL.md` — document `tasks.driver: durable` (§18 surface-in-lockstep).
- `scripts/smoke/phase-87.sh` — static-only assertions (below).
- `docs/glossary.md`, `docs/decisions.md` (D-228), `docs/plans/README.md` (flip 87 → Shipped on merge), `README.md` (status table) — hygiene.

## Public API surface

No change to `tasks.TaskRegistry` (frozen, Phase 20/21). New driver constructor, matching the Phase 57 precedent:

```go
// internal/tasks/drivers/durable
func New(cfg config.TasksConfig, store state.StateStore, opts ...Option) (tasks.TaskRegistry, error)
```

Registration is via the existing `tasks.Register("durable", factory)` from `init()`; callers resolve it through the unchanged `tasks.Open` / `tasks.OpenDriver`.

## Test plan

- **Unit:** record (de)serialization round-trip; keying-scheme correctness; recovery-sweep state machine; fail-loud `ErrUnserializable`; config validation of `driver: durable`.
- **Integration:** `test/integration/durable_tasks_test.go` — durable driver over a REAL `StateStore` (sqlite + inmem), real `events.EventBus` on the seam; spawn → close → reopen → records intact; identity propagation across the triple; ≥1 failure mode (recovery sweep on a crash-left `Running` record; a forced StateStore write error surfaces loudly). Run under `-race`.
- **Conformance:** `TestDurable_Conformance` calls `conformancetest.Run(t, factory)` — the binding D-031 gate; the durable driver passes the same suite as the in-process driver.
- **Concurrency / leak:** N≥100 concurrent `Spawn`/`Mark*`/`Get`/`List` against one shared durable driver under `-race`; goroutine baseline restored after `Close`.

## Smoke script additions

`scripts/smoke/phase-87.sh` (static-only — the durable driver is a runtime-internal §4.4 driver, no new Protocol/REST surface):

- The `internal/tasks/drivers/durable` package exists and declares `func New(`.
- It self-registers under `"durable"` (`tasks.Register("durable"` present).
- A conformance hook exists (`conformancetest.Run` referenced in the driver's tests).
- The production aggregator (`internal/drivers/prod`) blank-imports the durable task driver.
- `internal/config` accepts `tasks.driver: durable` (validator references it).

## Coverage target

- `internal/tasks/drivers/durable`: 85%
- `internal/config` (touched lines): no regression below the package's current target.

## Dependencies

- 20 (the `TaskRegistry` per-task surface)
- 22 (distributed contracts — `BusEnvelope` shape + the `(TaskID, Edge, EventID)` idempotency contract)
- (prerequisite, already shipped) StateStore triad (07 / 15 / 16), the events bus (05), identity (01). NOT 86 — single-instance durability is independent of the durable bus.

## Risks / open questions

- **Recovery posture for crash-left `Running` tasks.** What terminal recovery state? Options: a `Failed` with a reserved `runtime_restarted` code, or a dedicated `StatusRecovered`. Leaning `Failed{code: runtime_restarted}` to avoid widening the FSM, but pin it in D-228 at implementation. (Links to the D-097 dead-task gap.)
- **No-StateStore posture.** The durable events driver *auto-degrades* to in-memory when no store is configured. Mirror that, OR fail loud at boot (§13 — "no silent stub default")? Recommend: **fail loud** when `tasks.driver: durable` is explicitly set but no StateStore is wired (an operator asked for durability and must get it), and only auto-degrade is acceptable for the unset/default path which is already `inprocess`. Settle in D-228.
- **Group/patch atomicity across a crash mid-fan-out.** A group resolution that persisted some member transitions but not the group record before a crash must reconcile deterministically on recovery (the idempotency key makes the replay safe; the sweep recomputes group resolution from member terminality).
- **StateStore keyed-slot capacity / list cost.** `List` over many tasks in a session must stay bounded; confirm the keying scheme supports an efficient identity+session scan within the StateStore contract (mirror the durable events `kindEntryPrefix` scheme).

## Glossary additions

- **durable TaskService driver** — the StateStore-backed `TaskRegistry` driver (`internal/tasks/drivers/durable`, Phase 87) whose task/group/patch records survive a Runtime restart; opt-in via `tasks.driver: durable`. Distinct from the durable *events* driver (Phase 57) and the durable *bus* (Phase 86). RFC §6.8, D-228.
- **task recovery sweep** — the open-time pass the durable task driver runs to detect records left non-terminal by a crash and transition them to a terminal recovery state, exactly once. Does NOT re-drive execution (deferred).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes (the durable driver is identity-scoped — binding)
- [ ] **Concurrent-reuse test passes** — N≥100 against one shared durable driver under `-race` (the driver IS a reusable artifact — binding, NOT N/A).
- [ ] **Integration test exists** — `test/integration/durable_tasks_test.go`, real StateStore + EventBus on the seam, identity propagation, restart + write-error failure modes, under `-race` (Dependencies names shipped phases 20/22 — binding, NOT N/A).
- [ ] If new vocabulary: glossary updated (durable TaskService driver, task recovery sweep)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (D-228 — the StateStore-backed-vs-queue departure)
