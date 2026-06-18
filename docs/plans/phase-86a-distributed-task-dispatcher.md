# Phase 86a — distributed-task-dispatcher

## Summary

Ships the **consumer that makes the Phase 86 durable bus load-bearing**: a distributed task dispatcher that wires the `MessageBus` into the runtime (`OpenBus` at assembly), publishes task-lifecycle envelopes (`task.spawned` + terminal transitions) onto the bus, and runs a **fleet RunLoop driver** that picks up a spawned task, **claims it** (so exactly one worker drives it despite the bus's at-least-once cross-instance fan-out), and drives it to completion. The durable bus + durable TaskService (Phases 86 + 87) become a **fleet work queue**: a task spawned on any worker is driven on any worker and survives a restart. It is the distributed evolution of the single-instance per-task RunLoop driver (`cmd/harbor/cmd_dev_runloop.go`), and it closes the §13 "primitive without a consumer" gap the durable bus currently sits in (registered + tested, but unconsumed in production).

The phase also writes down — **at a high level** — the multi-worker deployment topology the dispatcher enables (N stateless Harbor workers behind a shared Postgres `StateStore` + durable bus + durable tasks; EKS / multi-container). Deployment manifests themselves are out of scope; this phase establishes the runtime capability + the operational shape.

## RFC anchor

- RFC §6.8
- RFC §6.12
- RFC §12

## Briefs informing this phase

- brief 05

## Brief findings incorporated

- brief 05 Q-4 + RFC §6.12: "`MessageBus.Publish` is at-least-once; handlers must be idempotent on `(TaskID, Edge, EventID)`." At-least-once delivery means **every** worker's bus poller projects a `task.spawned` envelope onto its local event bus — so a naive "subscribe + drive" consumer would drive the SAME task on every worker (duplicate execution). The dispatcher therefore separates **delivery** (at-least-once, fan-out to all) from **execution** (exactly-once, one driver): a claim/lease gate elects a single driver per task. Idempotency on the triple covers re-delivery; the claim covers concurrent fan-out.
- brief 05 §1: "the contracts exist so a distributed backend can land post-V1 without runtime changes." The dispatcher honours the unchanged `MessageBus` / `TaskRegistry` interfaces — it is wiring + a new fleet driver, not a contract change. The single-instance per-task RunLoop driver is the proven shape; the fleet driver generalises it from "subscribe to the local event bus" to "subscribe to the durable bus's projection + claim before driving."
- brief 05 §5/§7: persistent lifetimes match the StateStore triad; handlers idempotent on `(TaskID, Edge, EventID)`; a worker that dies mid-drive must not strand the task — the Phase 87 recovery sweep (`Failed{runtime_restarted}`) + a claim **lease** (expiry → re-claimable) are the two recovery seams.

## Findings I'm departing from (if any)

None at the brief level. One RFC-posture note: RFC §6.12 settles "V1 ships contracts only; in-process default," and explicitly defers the durable bus + its drivers to post-V1. This phase is the first to **open** a `MessageBus` in the runtime and route real work through it — i.e. the first multi-instance execution path. That is squarely post-V1 and within the contracts the RFC already pins; it does not change the `MessageBus` / `TaskRegistry` interfaces. If the claim/lease needs a new `StateStore` primitive (see Risks), that StateStore-interface addition is an RFC §9 touch and lands as its own RFC PR first.

## Goals

- Wire `distributed.OpenBus(...)` into `internal/runtime/assemble` (passing the shared `StateStore` into `Dependencies.State`, already in place from Phase 86), so a deployment that sets `distributed.bus_driver: durable` actually runs a live bus. `harbortest/devstack` inherits via the promoted `assemble.Assemble`.
- Publish task-lifecycle envelopes (`task.spawned` and terminal transitions) onto the `MessageBus` as the runtime drives tasks, so other workers can observe + claim them.
- A **fleet RunLoop driver** that subscribes to the bus's projected `distributed.bus_envelope` events, and for each `task.spawned`: attempts to **claim** the task; on a successful claim, drives the task's planner sub-run (reusing the Phase 107e per-task RunLoop machinery); on a lost claim, does nothing (another worker owns it).
- **Exactly-one-driver** semantics: under the bus's at-least-once fan-out to N workers, exactly one worker drives each task. A worker that dies mid-drive releases its claim (lease expiry) so another worker re-claims, OR the task is recovered terminal by the Phase 87 sweep — no permanent strand, no double-drive.
- Full `(tenant, user, session)` identity propagation across the worker boundary (the envelope carries it; the claiming worker drives under the task's identity).
- A high-level **multi-worker deployment** design (below): the stateless-workers + shared-Postgres topology, EKS / multi-container shapes, and the singleton-coordination open questions.

## Non-goals

- **NATS / Redis Streams bus drivers.** Still deferred (new dependency → RFC §10 PR). The dispatcher is driver-agnostic; it consumes the `MessageBus` interface, so it works against `loopback` (single-instance, trivially exactly-once) and `durable` (multi-instance).
- **Deployment manifests / IaC.** No Helm chart, Dockerfile, or Terraform here — the phase establishes the runtime capability + documents the topology at a high level. Concrete manifests are a separate ops deliverable.
- **Autoscaling / orchestration policy.** How many workers, HPA rules, etc. are operator concerns.
- **Changing the `MessageBus` / `TaskRegistry` / `events.EventBus` interfaces.** Wiring + a new driver only. (A new `StateStore` claim primitive, IF required, is a separate RFC PR — see Risks.)
- **Re-driving recovered tasks automatically.** Phase 87 recovers a crash-left task's RECORD to terminal; this phase adds the lease so a LIVE worker's death frees the claim for a peer mid-flight, but it does not resurrect a task the sweep already terminated (that lineage stays the D-097 concern).

## Acceptance criteria

- [ ] `internal/runtime/assemble` opens a `MessageBus` (`distributed.OpenBus`) and the runtime publishes `task.spawned` + terminal task-lifecycle envelopes onto it; `loopback` remains the default (single-instance behaviour byte-unchanged).
- [ ] **Cross-worker drive:** a task spawned on worker A (sharing a durable bus + StateStore with worker B) is driven to completion by SOME worker — proven with two in-process runtime stacks over one shared store.
- [ ] **Exactly-one-driver:** under at-least-once fan-out to N workers, each task is driven by exactly one worker — no double-execution (asserted by a per-task drive-count of exactly 1 across the fleet).
- [ ] **Worker-death recovery:** a worker that claims then dies before completing releases the task (lease expiry) so another worker re-claims and drives it (or the Phase 87 sweep terminates it) — no permanent strand.
- [ ] Identity propagates across the worker boundary: the claiming worker drives under the task's `(tenant, user, session)`; cross-tenant isolation holds.
- [ ] `make preflight` boots a `harbor dev` with `distributed.bus_driver: durable` + `state.driver: sqlite` and the dispatcher running; a spawned task drives to completion and `tasks.get` reflects it.
- [ ] Example config + `docs/CONFIG.md` document running the durable bus + dispatcher; the multi-worker deployment topology doc lands.
- [ ] Concurrent-reuse / leak: the fleet driver is a reusable artifact — N≥100 concurrent claims against one shared store under `-race`, no double-claim, no goroutine leak after stop.

## Files added or changed

- `internal/runtime/assemble/assemble.go` — open the `MessageBus` (`distributed.OpenBus` with `Dependencies{EventBus, State, Cfg, Tools}`), add it to the stack, register its `Close`.
- `internal/tasks/...` or `internal/distributed/...` — the task-lifecycle→bus publish path (publish `task.spawned` + terminal envelopes; the exact home — a `TaskRegistry` observer vs an events→bus bridge — is settled at implementation).
- `internal/runtime/dispatcher/` (new, name TBD) — the fleet RunLoop driver: subscribe → claim → drive. Generalises `cmd/harbor/cmd_dev_runloop.go` (which may be refactored to share the claim-aware core).
- `internal/tasks/...` (claim/lease) — the claim primitive (see Risks for the StateStore-CAS open question).
- `internal/config` — config to enable the dispatcher + lease tuning (lease duration, renewal cadence).
- `docs/design/distributed-execution.md` (new) — the high-level multi-worker deployment topology.
- `examples/*.yaml` + `docs/CONFIG.md` + `docs/skills/*` — the fleet-mode config surface (§18).
- `scripts/smoke/phase-86a.sh` — assertions (below).
- `docs/glossary.md`, `docs/decisions.md` (D-230), `docs/plans/README.md` (flip 86a → Shipped on merge), `README.md` — hygiene.

## Public API surface

No interface changes expected to `MessageBus`, `TaskRegistry`, or `events.EventBus`. New surface is internal: the dispatcher package + a claim/lease helper. IF the claim/lease requires a `StateStore` CAS primitive, that is an additive `StateStore` method behind its own RFC PR (RFC §9) — flagged, not assumed.

## Test plan

- **Unit:** the claim/lease state machine (win / lose / renew / expire); the lifecycle→bus publish path; config validation.
- **Integration:** `test/integration/distributed_dispatch_test.go` — TWO runtime stacks (real `StateStore` inmem + sqlite, real durable bus, real `EventBus`) over ONE shared store; spawn a task on A → assert exactly one stack drives it to completion; kill the claiming stack mid-drive → assert a peer re-claims OR the sweep terminates; identity propagation; ≥1 failure mode (store write error during claim). Under `-race`.
- **Concurrency / leak:** N≥100 concurrent claims of distinct tasks against one shared store; assert no double-claim, no goroutine leak after stop.
- **Conformance:** the existing `tasks` + `distributed` conformance suites stay green (this phase is wiring + a consumer, not a driver).

## Smoke script additions

`scripts/smoke/phase-86a.sh`:

- `internal/runtime/assemble` references `distributed.OpenBus` (the bus is wired).
- The dispatcher package exists with the subscribe→claim→drive surface.
- A claim/lease primitive exists.
- `docs/design/distributed-execution.md` exists.
- Live (when the dev surface supports it): boot `harbor dev` with `distributed.bus_driver: durable`, spawn a task, assert it drives to completion via `tasks.get`.

## Coverage target

- The new dispatcher + claim packages: 85%.
- `internal/runtime/assemble` (touched lines): no regression.

## Dependencies

- 86 (the durable `MessageBus` driver — the queue this consumes)
- 87 (the durable `TaskService` — durable task records + the recovery sweep the lease complements)
- (prerequisite, already shipped) the per-task RunLoop driver (Phase 107e), the StateStore triad, events, identity.

## Risks / open questions

- **The claim/lease protocol is the hard core — and the `StateStore` has no native CAS.** `StateStore.Save` is idempotent on `EventID` (same `EventID` + different `Bytes` → `ErrIdempotencyConflict`) but does NOT enforce optimistic concurrency on `Version`. A one-shot claim is expressible with the idempotency contract: write a claim record `task.claim/<taskID>` with a **deterministic** `EventID` derived from the task ID and the claimant's worker ID in `Bytes` — the first writer wins; a concurrent writer with different `Bytes` (a different worker ID) gets `ErrIdempotencyConflict` and backs off. But **lease renewal + expiry** do not fall out of that cleanly (renewing with the same `EventID` + new expiry bytes conflicts with itself; a fresh `EventID` overwrites and loses exclusivity). Options to settle in D-230: (a) a renew-by-overwrite scheme that tolerates the at-least-once double-drive window via `(TaskID, Edge, EventID)` idempotency downstream + the Phase 87 sweep; (b) add an optional `StateStore.CompareAndSwap` primitive (RFC §9 PR first) for true leases; (c) a Postgres-only advisory-lock fast-path behind the §4.4 seam (mirrors the bus's LISTEN/NOTIFY deferral). The plan does NOT pre-commit; it names the options and the safe fallback (at-least-once + idempotent side effects + the sweep).
- **Idempotent side effects under at-least-once.** Exactly-one-driver covers the common path, but a lease-expiry re-claim (or a re-delivery) can drive a task twice. Tool calls / external side effects must be idempotent or guarded — this is the brief 05 idempotency contract surfacing at the execution layer. Document the operator-facing implication.
- **Singleton coordination across the fleet.** Some runtime components assume single-instance: the session GC sweeper (`sessions` GCPolicy) and the pause sweeper (`pauseresume` crash-orphan rescan, D-207) run per-process. In a fleet, N workers each running them is wasteful or unsafe. Open question: run-on-all (idempotent + cheap) vs leader-elected singletons. Likely out of scope for 86a's first cut (document + defer), but it gates a clean multi-worker story.
- **Work-pickup latency = the bus poll interval.** A spawned task is claimed only after a peer's poll tick observes its `task.spawned` envelope (`distributed.bus_poll_interval`, default 1s). Acceptable for background work; the LISTEN/NOTIFY push fast-path (Phase 86's deferred optimization) lowers it.
- **OpenBus wiring is a real runtime change.** Opening a `MessageBus` in `assemble` + publishing lifecycle envelopes is the first production use of the seam. The default (`loopback`) must stay byte-unchanged (single-instance behaviour identical); the durable path is opt-in. Guard with the existing conformance + a no-regression assertion.

### Multi-worker deployment (high level)

The capability this phase unlocks. The shape:

- **N stateless Harbor workers**, identical config, each running the full runtime + the Protocol surface (and optionally `harbor console` on its own). Workers hold no durable state locally.
- **One shared durable substrate: Postgres.** `state.driver: postgres` backs the `StateStore`; `events.driver: durable` + `tasks.driver: durable` + `distributed.bus_driver: durable` all ride that shared store. The Postgres instance (e.g. RDS / Aurora / a `postgres` container) is the single source of truth + the fleet work queue.
- **Work distribution:** a task spawned on any worker is persisted (87) + its `task.spawned` envelope published to the durable bus (86); every worker's poller projects it; the claim/lease elects one driver. A worker death frees the claim (lease) or the sweep terminates the task.
- **Identity / isolation:** unchanged — the `(tenant, user, session)` triple flows through the envelope + the StateStore scoping; cross-tenant isolation is enforced by the same per-record identity filters, fleet-wide.
- **EKS shape:** a `Deployment` of N replica pods (the Harbor worker image), a managed Postgres (RDS/Aurora), a `Service` + `Ingress`/ALB fronting the Protocol surface (any worker can serve a request; sessions are create-on-first-use + StateStore-backed, so requests need not be sticky — confirm at implementation), `/healthz` + `/readyz` per pod for the readiness gate, horizontal scaling on the stateless workers.
- **Multi-container (docker-compose) shape:** N `harbor` services + 1 `postgres` service on a shared network; identical config pointing at the `postgres` host. The local analog of the EKS topology for dev/staging.
- **Open deployment questions (carried into D-230 / a future ops doc):** session affinity (any-worker vs sticky); singleton coordination (GC / pause sweeper — leader election vs run-on-all); Console attachment in a fleet (load-balanced Protocol endpoint vs per-worker); the durable bus's retention/GC under fleet volume (Phase 86 known property).

## Glossary additions

- **distributed task dispatcher** — the fleet RunLoop driver (Phase 86a) that subscribes to the durable `MessageBus`'s projected `task.spawned` envelopes, claims a task (exactly-one-driver lease), and drives its planner sub-run — the consumer that turns the Phase 86 durable bus + Phase 87 durable tasks into a fleet work queue. The distributed evolution of the single-instance per-task RunLoop driver. RFC §6.8 + §6.12, D-230.
- **task claim / lease** — the StateStore-backed gate the dispatcher uses to elect exactly one worker to drive a spawned task despite the bus's at-least-once fan-out to all workers; a lease so a claiming worker's death frees the task for a peer. RFC §6.12, D-230.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes (doc-only planning PR may skip the local preflight via `HARBOR_PREFLIGHT_SKIP=1` with justification; CI gates — this plan PR carries no code)
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target (at implementation)
- [ ] If multi-isolation paths changed: cross-session isolation test passes (the dispatcher drives under the task's identity across the worker boundary — binding at implementation)
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent claims against one shared store under `-race`, no double-claim, no leak (the dispatcher IS a reusable artifact — binding at implementation, NOT N/A).
- [ ] **Integration test exists** — `test/integration/distributed_dispatch_test.go`, two runtime stacks over one shared store, exactly-one-driver, worker-death re-claim, identity propagation, ≥1 failure mode, under `-race` (Deps names shipped phases 86/87 — binding at implementation, NOT N/A).
- [ ] If new vocabulary: glossary updated (distributed task dispatcher, task claim / lease)
- [ ] If a brief finding was departed from / a new StateStore primitive is needed: RFC PR first + decisions.md entry (D-230)
