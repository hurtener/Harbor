# Phase 86 — durable-distributed-bus

## Summary

Ships the first **durable `MessageBus` driver** so cross-worker bus traffic survives a Runtime restart and fans out across instances. It is a new driver under `internal/distributed/drivers/durable` that persists every published `BusEnvelope` through the existing `state.StateStore` (the in-mem / SQLite / Postgres triad) and projects it onto the local `events.EventBus` — mirroring the Phase 57 durable *events* driver and the Phase 87 durable *tasks* driver. A background poller scans the shared store for envelopes published by OTHER instances (or left by a crash) and projects them onto the local bus, giving at-least-once cross-instance delivery. It passes the existing `internal/distributed/conformancetest.RunBus` suite verbatim. The in-process `loopback` driver stays the default; `durable` is opt-in via `distributed.bus_driver`. Realises D-009 (durable distributed backend deferred to post-V1).

The backend is **StateStore-backed (Postgres-as-queue when the shared store is Postgres)**; NATS / Redis Streams are explicitly deferred to later phases in the same post-V1 distributed set (they need new dependencies → an RFC §10 PR first).

## RFC anchor

- RFC §6.12
- RFC §12

## Briefs informing this phase

- brief 05

## Brief findings incorporated

- brief 05 Q-4 (resolved in RFC §6.12): "`MessageBus.Publish` is at-least-once; handlers must be idempotent on `(TaskID, Edge, EventID)`." The durable driver's persist→project path is at-least-once: a re-delivery on a poll replay or a crash-between-persist-and-project re-projects the same envelope, and consumers dedupe on the triple. The driver never promises exactly-once.
- brief 05 §"distributed" + RFC §6.12 "V1 ships contracts only": the contract (`MessageBus`, `BusEnvelope`) and the `loopback` driver shipped at Phase 22; Phase 86 lands behind the **unchanged** `MessageBus` interface — no caller (planner, runloop, any `BusEnvelope` publisher) changes. The conformance suite is the gate every driver inherits (D-031).
- brief 05 §5 (the StateStore triad as the durable substrate): persistent lifetimes that survive restart "match the StateStore driver triad." The durable bus reuses that triad rather than growing a parallel persistence seam — identity isolation + the three drivers + conformance parity come for free (the same reasoning the Phase 57 durable events driver and the Phase 87 durable tasks driver used).

## Findings I'm departing from (if any)

RFC §6.12 / §12 name "NATS, Redis Streams, Postgres-as-queue" as the post-V1 durable-bus driver set. Phase 86 ships **only** the StateStore-backed driver (which IS Postgres-as-queue when the shared store is Postgres) and **defers NATS / Redis Streams** to later phases in the set. Justification: (a) StateStore-backed is the lower-risk, DRY path already proven twice (Phase 57 durable events, Phase 87 durable tasks) and reuses the existing `pgx` / `modernc.org/sqlite` dependencies — no new dependency, so no RFC §10 PR is required to land it; (b) NATS / Redis each pull a new client dependency (`nats.go` / `go-redis`), which RFC §10 requires be added via an RFC PR first — out of scope for this phase. This departure is filed as **D-229**.

## Goals

- A `durable` `MessageBus` driver that persists every `BusEnvelope` through a `state.StateStore` and projects it onto the local `events.EventBus`, so bus traffic is replayable after a process restart.
- **Cross-instance fan-out**: a background poller projects envelopes published by other instances sharing the same (Postgres) store onto the local bus, deduped so an instance never re-projects an envelope it already delivered.
- Pass the existing `internal/distributed/conformancetest.RunBus` suite verbatim (the D-031 gate every bus driver inherits).
- Opt-in selection via `distributed.bus_driver: durable` + StateStore wiring, with the `loopback` driver unchanged as `DefaultDriver`.
- Full `(tenant, user, session)` identity scoping and at-least-once `(TaskID, Edge, EventID)` idempotency preserved across restart and across instances.
- Fail loud at boot when `distributed.bus_driver: durable` is set but no `StateStore` is wired (the Phase 57 / Phase 87 posture; §13 "no silent stub default").

## Non-goals

- **NATS / Redis Streams drivers.** Deferred to later phases in the post-V1 distributed set; each needs a new dependency (RFC §10 PR first).
- **Postgres `LISTEN`/`NOTIFY` push.** The StateStore contract exposes no change-notification primitive, so cross-instance delivery is **poll-based** at this phase. A Postgres-only `LISTEN`/`NOTIFY` fast-path (lower latency) is a documented future optimization behind the same driver seam — not this phase.
- **Exactly-once delivery.** The contract is at-least-once (RFC §6.12); consumers dedupe on `(TaskID, Edge, EventID)`. The durable driver does not attempt exactly-once.
- **Changing the `MessageBus` interface or any caller.** The interface is frozen (Phase 22); this phase is a driver only.
- **A new persistence seam or migration.** No new driver registry, no new SQL migration — the driver rides the `state.StateStore` keyed-slot contract (the durable events driver's head+entry scheme).
- **`RemoteTransport` / A2A work.** `RemoteTransport` (RFC §6.12) is a separate contract; the A2A wire driver is Phase 29. Untouched here.

## Acceptance criteria

- [ ] `internal/distributed/drivers/durable` implements `distributed.MessageBus`, self-registers under `"durable"` from its `init()`, and is reachable via `distributed.OpenBusDriver("durable", …)`.
- [ ] The durable driver passes `internal/distributed/conformancetest.RunBus` verbatim (no suite changes, no skips): at-least-once delivery to local EventBus subscribers, mandatory identity (`ErrIdentityRequired`), `Publish`-after-`Close` → `ErrBusClosed`, the 128-worker concurrent-publish no-race run, and the goroutine-leak-after-`Close` check.
- [ ] **Restart-replay:** publish N envelopes → `Close` the driver → open a NEW driver instance against the SAME `StateStore` → the poller re-projects the persisted envelopes onto the new instance's EventBus (gap-free; identity-scoped).
- [ ] **Cross-instance fan-out:** two durable bus instances over ONE shared `StateStore`; an envelope published on instance A is projected onto instance B's EventBus (deduped — B never sees it twice; A never re-projects its own).
- [ ] Persist/publish stays idempotent on `(TaskID, Edge, EventID)` — a re-delivered or poll-replayed envelope neither double-writes a store record nor is double-projected by the instance that already delivered it.
- [ ] A non-serializable envelope payload fails loudly with a wrapped error (never a silent drop) — the §5 fail-loud contract.
- [ ] `distributed.bus_driver: durable` selects the driver; an empty/`loopback` driver still resolves the loopback default unchanged; the durable driver **fails loud** at boot when no `StateStore` is wired.
- [ ] Example config + `docs/CONFIG.md` document the new `distributed.bus_driver: durable` + StateStore-wiring; backward-compatible (new optional fields / shared-store wiring).
- [ ] Concurrent-reuse test: N≥100 concurrent `Publish` against one shared durable bus under `-race` — no data races, no context bleed, no cross-cancellation, no goroutine leaks (poller joined on `Close`).

## Files added or changed

- `internal/distributed/drivers/durable/durable.go` — the driver (`New(deps distributed.Dependencies) (distributed.MessageBus, error)` + `init()` self-registration), the publish→persist→project path, and the background poller (started at `New`, joined at `Close`).
- `internal/distributed/drivers/durable/record.go` — the persisted envelope record shape + StateStore keying scheme (per-session head + per-entry slots, mirroring `internal/events/drivers/durable`).
- `internal/distributed/drivers/durable/*_test.go` — conformance hook, restart-replay, cross-instance fan-out, concurrent-reuse, leak, fail-loud tests.
- `internal/distributed/registry.go` — `Dependencies` gains a `State state.StateStore` field (shared-store wiring; the durable factory reads it, the loopback factory ignores it). The factory signature is unchanged (`BusFactory func(deps Dependencies) (MessageBus, error)`).
- `internal/config/config.go` + `internal/config/validate.go` — `validateDistributed` accepts `bus_driver: durable`; `allowedDistributedBusDrivers` gains `"durable"`.
- `internal/runtime/assemble/assemble.go` — wire the runtime's shared `StateStore` into `distributed.Dependencies.State` at `OpenBus` (the same store already opened for events/tasks). `harbortest/devstack` inherits this via the promoted `assemble.Assemble` (D-197).
- `internal/drivers/prod/prod.go` — add the durable bus driver's blank import to the production aggregator (§4.4 single home).
- `examples/*.yaml` + `docs/CONFIG.md` + `docs/skills/define-the-agent-yaml/SKILL.md` — document `distributed.bus_driver: durable` (§18 surface-in-lockstep).
- `scripts/smoke/phase-86.sh` — static-only assertions (below).
- `docs/glossary.md`, `docs/decisions.md` (D-229), `docs/plans/README.md` (flip 86 → Shipped on merge), `README.md` (status table) — hygiene.

## Public API surface

No change to `distributed.MessageBus` (frozen, Phase 22) and no change to the `BusFactory` signature. The only additive surface is a new field on the existing `distributed.Dependencies` struct:

```go
// internal/distributed
type Dependencies struct {
    EventBus events.EventBus
    Cfg      config.DistributedConfig
    Tools    config.ToolsConfig
    State    state.StateStore // NEW — the shared runtime StateStore; used by the durable driver, ignored by loopback.
}
```

Registration is via the existing `distributed.RegisterBus("durable", factory)` from `init()`; callers resolve it through the unchanged `distributed.OpenBus` / `OpenBusDriver`.

## Test plan

- **Unit:** envelope record (de)serialization round-trip; keying-scheme correctness; the poller cursor/dedup state machine (self-published entries are not re-projected; remote entries are projected once); fail-loud on a non-serializable payload; config validation of `bus_driver: durable`.
- **Conformance:** `TestDurable_Conformance` calls `conformancetest.RunBus(t, factory)` — the binding D-031 gate; the durable driver passes the same suite as loopback.
- **Integration:** `test/integration/durable_bus_test.go` — durable driver over a REAL `StateStore` (inmem + sqlite; Postgres `t.Skip`s with a reason when `HARBOR_PG_DSN` is unset, mirroring the Phase 57 `durable_eventlog_test.go`), real `events.EventBus` on the seam; publish → close → reopen → poller re-projects (restart-replay); two instances over one store → cross-instance fan-out; identity propagation across the triple; ≥1 failure mode (a closed/forced-error StateStore surfaces loudly). Run under `-race`.
- **Concurrency / leak:** N≥100 concurrent `Publish` against one shared durable bus under `-race`; goroutine baseline restored after `Close` (the poller goroutine joined).

## Smoke script additions

`scripts/smoke/phase-86.sh` (static-only — the durable bus is a runtime-internal §4.4 driver, no new Protocol/REST surface):

- The `internal/distributed/drivers/durable` package exists and declares `func New(`.
- It self-registers under `"durable"` (`RegisterBus("durable"` present).
- It persists through a `state.StateStore` (the reused triad, no new seam).
- A conformance hook exists (`conformancetest.RunBus` referenced in the driver's tests).
- The production aggregator (`internal/drivers/prod`) blank-imports the durable bus driver.
- `internal/config` accepts `distributed.bus_driver: durable` (validator references it).

## Coverage target

- `internal/distributed/drivers/durable`: 85%
- `internal/config` (touched lines): no regression below the package's current target.

## Dependencies

- 22 (the `MessageBus` / `BusEnvelope` contract + `(TaskID, Edge, EventID)` idempotency + the conformance suite + the `loopback` driver)
- (prerequisite, already shipped) StateStore triad (07 / 15 / 16), the events bus (05), identity (01). NOT 87 — the durable tasks driver is independent (a sibling StateStore-backed driver in a different subsystem).

## Risks / open questions

- **Shared store (via `Dependencies.State`) vs owned store (via `DistributedConfig.StateDriver`/`StateDSN`).** Recommend **shared**: `tasks.Open` and the events deps-aware path both reuse the runtime's shared `StateStore`, and Phase 87 settled shared-store for the durable tasks driver (D-228); the durable bus mirrors it (one `State` field on `Dependencies`, wired in `assemble.go`, fail-loud if nil). The owned-store path (a dedicated DB via `StateDriver`/`StateDSN`, the events `Register` precedent) is the alternative if an operator wants the bus log on a separate database. **Settle in D-229 at implementation.**
- **Poller cursor + self-dedup model.** The instance that publishes an envelope projects it locally immediately (the loopback path); the poller must not re-project that same envelope. Model: a persisted per-instance consumer cursor (monotonic sequence) advanced by both publish (own entries) and poll (remote entries), plus an in-memory recently-projected `EventID` set to suppress the self-double-project within a window. At-least-once means a crash between project and cursor-advance MAY re-project a small tail on the next open — acceptable (consumers dedupe). Pin the exact algorithm in D-229; the acceptance criteria bound the observable behavior (no self double-project, remote projected once, gap-free replay).
- **Poll interval / latency.** Poll-based delivery trades latency for the no-new-dependency / works-on-sqlite-and-Postgres property. Default interval is an operator-tunable config knob (documented default, e.g. 1s); a Postgres `LISTEN`/`NOTIFY` push fast-path is a recorded future optimization behind the same seam.
- **`ListKind` cost / scan bound.** Cross-instance polling enumerates entries via the head-record sequence list (per session) or a maintenance-scoped `ListKind` over the entry prefix. Confirm the scheme stays bounded per session and that the maintenance-scoped scan is the right elevation (the Phase 87 durable tasks `Hydrate` precedent). A retention/GC policy for delivered entries (the durable event log accumulates the same way) is a documented scaling follow-up, not this phase.
- **§13 primitive-with-consumer.** Phase 86 is a DRIVER for an already-shipped contract (Phase 22 landed `MessageBus` + `loopback` + the conformance consumer). Like Phase 57 (durable events) and Phase 87 (durable tasks), the driver's "consumer" is the conformance suite + the existing `events.EventBus` subscribers of `distributed.bus_envelope`; the cross-instance delivery is exercised end-to-end by the integration test (two instances, one store). No new unconsumed primitive is introduced.

## Glossary additions

- **durable distributed bus driver** — the StateStore-backed `MessageBus` driver (`internal/distributed/drivers/durable`, Phase 86) that persists every published `BusEnvelope` through the `StateStore` and projects it onto the local `events.EventBus`, with a background poller that delivers cross-instance envelopes; opt-in via `distributed.bus_driver: durable`, fail-loud when no `StateStore` is wired. StateStore-backed (Postgres-as-queue when the shared store is Postgres); NATS / Redis Streams are deferred. Distinct from the durable *event log* (Phase 57, an `events.EventBus`) and the durable *TaskService* (Phase 87, a `TaskRegistry`). RFC §6.12, §12, D-229.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes (doc-only planning PR may skip the local preflight via `HARBOR_PREFLIGHT_SKIP=1` with justification; CI still gates — this plan PR carries no code)
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes (the durable bus is identity-scoped — binding at implementation)
- [ ] **Concurrent-reuse test passes** — N≥100 against one shared durable bus under `-race` (the driver IS a reusable artifact — binding at implementation, NOT N/A).
- [ ] **Integration test exists** — `test/integration/durable_bus_test.go`, real StateStore + EventBus on the seam, identity propagation, restart-replay + cross-instance + write-error failure modes, under `-race` (Dependencies names shipped phase 22 — binding at implementation, NOT N/A).
- [ ] If new vocabulary: glossary updated (durable distributed bus driver)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (D-229 — the StateStore-backed-only / defer-NATS-Redis departure)
