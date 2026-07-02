# Phase 147 — Events conformance suite: the shared multi-driver home + the duplicated-scenario fold

## Summary

Builds `internal/events/conformancetest` — the canonical shared correctness suite every `events.EventBus` driver must pass (CLAUDE.md §11's conformance-suite rule, applied to the one multi-driver subsystem that never got its home) — and folds the verified duplicated scenarios out of the inmem and durable drivers' per-package tests into it: fence semantics (the D-274-hardened erasure fence's bus-side contract), Bounds/Window history reads, replay cursor semantics, subscribe scope validation, and close lifecycle. This is the v1.9 wave audit's deferred NIT 7 ("no shared events conformance home both drivers run; creating one is a structural refactor — deferred") paid down as its own phase. Pure test refactor: ZERO production code change; both drivers invoke the suite from their own `_test.go` in the same PR, so the §13 primitive-with-consumer rule is satisfied by construction. D-277.

## RFC anchor

- RFC §6.13 (the typed event bus — the one contract both drivers implement; the suite pins its driver-observable semantics)
- RFC §6.9 (sessions — the erasure cascade whose bus-side fence contract is among the folded scenarios)

## Briefs informing this phase

- brief 06

## Brief findings incorporated

- brief 06 §6 ("Tests required"): "**Late-subscriber replay tests**: subscribe-from-cursor with both ring-buffer and durable backends." The brief demanded the SAME replay scenarios against both backends from the start; what shipped instead was two hand-copied test files that drifted independently (the durable driver never gained inmem's `Window_ReplayDisabled` scenario; inmem never gained durable's `Window_EmptySession` / `Window_ReachesHead`). The suite is the mechanical home that makes "both backends" one artifact instead of a copy-paste discipline.
- brief 06 §6: "**Cross-tenant isolation tests**: subscriber for tenant A receives zero events emitted by tenant B; `admin` scope can bypass; assertion on the audit event for the bypass." Cross-identity isolation is a mandatory, unconditional suite member for every driver (CLAUDE.md §6 rule 10) — never a per-driver copy that one driver can silently lack. (The admin-bypass + audit-emit half stays driver-local in this tranche — see Non-goals.)
- brief 06 §5 ("Sharp edges"): "**Two-channel split** … every dashboard, replay tool, and Console feature has to fuse them. Lesson: unify on one bus from t=0." One bus contract means one correctness definition. Two per-driver forks of the same scenario are the test-side re-run of the two-channel split: each fork evolves alone and the contract stops having a single authoritative statement.

## Findings I'm departing from (if any)

None. (Brief 06 §6 also demands OTel-propagation, CLI-golden and hot-reload tests — those live in their own shipped phases (56, 63, 65) and are out of this phase's scope, which is not a departure.)

## Goals

- **A shared conformance home for the events subsystem, mirroring the settled precedent exactly.** Package `internal/events/conformancetest`, exported `func Run(t *testing.T, factory Factory)` — the same naming and shape as `internal/identity/conformancetest`, `internal/state/conformancetest` (`Run` at conformancetest.go:73) and `internal/memory/conformancetest` (`Run` at conformancetest.go:127). NOT `RunConformance`; the package name IS the qualifier (`conformancetest.Run`). The suite lives in a subpackage so `internal/events` never imports `testing` (the identity/state/memory rationale, verbatim).
- **A Harness-shaped Factory, because the scenarios span three bus configurations.** The state suite's bare `func() (state.StateStore, func())` works when every subtest wants one default store. The events fold scope needs three distinct driver configurations — default replay-capable, replay-disabled (`ErrReplayUnavailable`), and bounded-retention (`ErrCursorTooOld` on overrun) — so the memory precedent's `Harness` fits better, carrying named CONSTRUCTORS rather than one pre-built bus (see Public API surface). All three constructors are mandatory: no `Supports*` capability flags, no skip-if-absent ceremony (§4.4) — both V1 drivers can express all three configurations today (inmem: `ReplayBufferSize` 0 / small; durable: best-effort ring mode with `ReplayBufferSize` 0 / small, exactly what `TestDurable_NoStateStore_RingZero_ReplayUnavailable` and `TestDurable_BestEffort_CursorTooOld` already construct).
- **Fold the verified duplicated scenarios; lose nothing.** Every folded old test's assertions map to a named suite scenario — the mapping table below is binding, and the plan's acceptance criteria include the no-coverage-loss check. Scenario subtest names are pinned stable strings (the `ConformanceScenario` discipline from the planner pack, D-058) so both drivers' pass/fail boards stay comparable.
- **Identity scoping is a mandatory suite member** (§6 rule 10): empty-triple rejection on Publish / Subscribe / Replay / Bounds, cross-tenant live-subscribe isolation, cross-identity replay isolation, and the fence's cross-session non-bleed all run unconditionally against every driver.
- **The genuinely divergent scenario is parameterized by CONFIGURATION, not capability.** The one real semantic divergence: the durable driver in durable mode has unbounded persisted retention and can never return `ErrCursorTooOld`; only its documented best-effort ring mode can. The suite expresses this as the `NewBoundedRetentionBus` constructor (each driver supplies its bounded-retention configuration) — a config choice the driver already documents, not an optional-capability declaration. Capability-interface presence (`events.Replayer` / `events.HistoryReplayer` / `events.Fencer`) is ASSERTED, never skipped: a driver missing one FAILS the suite loudly (both V1 drivers implement the full set — `internal/events/events.go`'s `Fencer` doc pins "Both V1 bus drivers (inmem, durable) implement it").
- **Zero production code change.** The diff is confined to the new suite package, the two drivers' `_test.go` files (deletions + the two `conformance_test.go` consumers), the smoke script, and docs. The suite package's `conformancetest.go` is a non-test `.go` file by necessity (drivers import it) but contains no production behavior — the state/memory precedent.

## Non-goals

- **Folding the near-duplicate drop-policy / redaction / reaper / admin-audit scenarios** (`TestPublish_DropOldestEmitsBusDropped` ↔ `TestDurable_Subscriber_DropOldest_EmitsBusDropped`, `TestPublish_RedactionFailure_EmitsSibling` ↔ `TestDurable_RedactionFailure_EmitsSiblingAndReturnsError`, the reaper pair, the admin-subscribe-audit pair). These are real duplication, but their harnesses are timing- and config-sensitive (drop windows, idle timeouts, per-driver clock wiring) and folding them is a second, riskier tranche. Named follow-up once this tranche proves stable — deferred deliberately, not silently (recorded in D-277).
- **Any production change**: no change to `events.EventBus`, the capability interfaces, either driver, or `internal/config`. If the suite surfaces a genuine driver divergence, that becomes a same-PR §17.6 fix and an explicitly documented §4.3 deviation from this phase's zero-production-change property — never a silently skipped scenario.
- **Driver-specific tests stay put** (see the "stays driver-specific" list below). In particular each driver KEEPS its own D-025 concurrent-reuse test — §5's contract is per-artifact and per-package by design.
- **No `sdk/` or `harbortest` export of the suite.** It is contributor-facing test infrastructure, same posture as the state/memory suites and the in-tree Protocol conformance suite (D-210).
- **No third bus driver** ships here; the suite is the landing pad for one.

## Fold mapping (binding — the no-coverage-loss table)

Suite scenario names are pinned. "gain" marks a driver that lacked the scenario before this phase — folding widens its coverage; a gain-side failure is a real finding handled per §17.6, not a reason to drop the scenario.

| Suite scenario | inmem (old test, file:line) | durable (old test, file:line) |
|---|---|---|
| `Publish_IdentityMandatory_Rejected` | `TestPublish_RejectsMissingIdentity` inmem_test.go:204 | gain |
| `Subscribe_EmptyTripleNonAdmin_Rejected` | `TestSubscribe_RejectsEmptyTripleNonAdmin` inmem_test.go:214 | `TestDurable_Subscribe_RejectsEmptyTripleNonAdmin` durable_test.go:308 |
| `Replay_EmptyTripleNonAdmin_Rejected` | `TestReplay_RejectsEmptyFilter` replay_test.go:309 | `TestDurable_Replay_RejectsEmptyTripleNonAdmin` durable_test.go:317 |
| `Bounds_EmptyTriple_ErrIdentityScopeRequired` | `TestInmem_Bounds_EmptyTriple_ErrIdentityScopeRequired` inmem_test.go:1051 | `TestDurable_Bounds_EmptyTriple_ErrIdentityScopeRequired` durable_test.go:623 |
| `Subscribe_CrossTenant_Isolation` | `TestPublish_CrossTenantIsolation` inmem_test.go:282 | gain |
| `Replay_CrossIdentity_Isolation` | `TestReplay_CrossTenant_Isolation` replay_test.go:535 | `TestDurable_Replay_CrossSessionIsolation` durable_test.go:269 (suite runs BOTH axes: tenant + session) |
| `Replay_HeadCursor_ReturnsNil` | `TestReplay_HeadCursor_ReturnsNilNil` replay_test.go:131 | `TestDurable_ReplayCursorAtHead_ReturnsNil` durable_test.go:206 |
| `Replay_Disabled_ErrReplayUnavailable` | `TestReplay_DisabledByConfig_ErrReplayUnavailable` replay_test.go:229 | `TestDurable_NoStateStore_RingZero_ReplayUnavailable` durable_test.go:388 |
| `Replay_RetentionOverrun_ErrCursorTooOld` | `TestReplay_RingOverrun_ErrCursorTooOld` replay_test.go:254 | `TestDurable_BestEffort_CursorTooOld` durable_test.go:404 |
| `Bounds_ReportsHeadAndTail` | `TestInmem_Bounds_ReportsHeadTail` inmem_test.go:918 | `TestDurable_Bounds_ReportsHeadAndTail` durable_test.go:599 |
| `Bounds_EmptySession_ErrNoHistory` | `TestInmem_Bounds_NoMatch_ErrNoHistory` inmem_test.go:1041 | `TestDurable_Bounds_EmptySession_ErrNoHistory` durable_test.go:614 |
| `Window_TailFirst_MostRecentK_OldestFirst` | `TestInmem_Window_TailFirst_OldestFirst` inmem_test.go:1059 | `TestDurable_Window_TailFirst_MostRecentK_OldestFirst` durable_test.go:632 |
| `Window_BeforeCursor_ScrollsUp` | `TestInmem_Window_BeforeCursor` inmem_test.go:1072 | `TestDurable_Window_BeforeCursor_ScrollsUp` durable_test.go:652 |
| `Window_EmptySession_Empty` | gain | `TestDurable_Window_EmptySession_Nil` durable_test.go:668 |
| `Window_ReachesHead` | gain | `TestDurable_Window_ReachesHead` durable_test.go:680 |
| `Window_ReplayDisabled_ErrReplayUnavailable` | `TestInmem_Window_ReplayDisabled_ErrReplayUnavailable` inmem_test.go:1085 | gain |
| `Fence_DropsLateEventsAndEmptiesHistory` (incl. Unfence restore + cross-session non-bleed) | `TestInmem_Fence_DropsAndEmptiesHistory` inmem_fence_test.go:16 | `TestDurable_Fence_DropsLateEventsAndEmptiesHistory` durable_fence_test.go:18 — the suite folds the BUS-READ half (Bounds/Window read empty after a fenced publish); the old test's STORE-level persistence half (`store.Load(…, "events.durable.head")` → `ErrNotFound`) survives in `internal/sessions/erasure_fence_test.go` (its `events.durable.head` assertions), which is now the ONLY home of the durable persist-path fence guard — a sessions-test refactor must not orphan it |
| `Fence_AfterClose_ErrBusClosed` | `TestInmem_Fence_AfterClose` inmem_fence_test.go:78 | `TestDurable_Fence_AfterClose` durable_fence_test.go:70 |
| `AfterClose_OpsRejected_ErrBusClosed` (Publish + Subscribe + Replay + Bounds/Window) | `TestPublish_AfterClose_ReturnsBusClosed` inmem_test.go:525 + `TestReplay_AfterClose_ErrBusClosed` replay_test.go:586 | `TestDurable_ClosedBus_RejectsOps` durable_test.go:473 |
| `Close_Idempotent` | `TestClose_Idempotent` inmem_test.go:540 | `TestDurable_Close_Idempotent` durable_test.go:493 |

**Stays driver-specific (deliberately NOT folded):** durable recovery/restart (`recovery_test.go` — restart-against-the-same-store semantics the inmem driver cannot express; D-255), `OpenWith` shared-store (`openwith_test.go`), persist-failure fail-loud (`TestDurable_PersistFailure_SurfacesLoudly` durable_test.go:447), cancellation-bounds-persistence (durable_test.go:530, :560), exact replay-across-restart (`TestDurable_ReplayAcrossRestart_NoGaps` durable_test.go:225), the D-025 concurrent-reuse tests (each driver keeps its own: `TestBus_ConcurrentReuse_ReuseContract`, `TestReplay_ConcurrentReuse_ReuseContract`, `TestConcurrentReuse_DurableBus`, `TestConcurrentReuse_DurableBus_RecoveredInstance`), inmem ring-truncation reporting (`TestInmem_Bounds_WrappedRing_ReportsTruncated` inmem_test.go:1015), the inmem goroutine-leak `TestMain` harness (`leak_test.go`), and — for this tranche — the drop-policy / redaction / reaper / admin-audit near-duplicates (see Non-goals).

## Acceptance criteria

- [x] `internal/events/conformancetest` exists: package `conformancetest`, exported `Run(t *testing.T, factory Factory)` and the `Harness` type below — naming mirrors the identity/state/memory precedent exactly (NOT `RunConformance`). The package imports `internal/events` + `internal/identity` + stdlib only — never a concrete driver (grep-asserted in the smoke; the §13 import-direction discipline).
- [x] Both drivers consume the suite in the same PR: `internal/events/drivers/inmem/conformance_test.go` (`TestInmem_Conformance`) and `internal/events/drivers/durable/conformance_test.go` (`TestDurable_Conformance`) each supply a full three-constructor `Harness`; the durable harness wires a REAL inmem StateStore (no mock at the seam, §17.3). §13 primitive-with-consumer holds by construction; no separate self-applied suite test is added (the two real consumers land with the suite — a deliberate, documented simplification of the state precedent's `TestRun_SelfApplied`, which existed because that suite shipped before its second consumer).
- [x] Every row of the fold mapping table is realized: the suite scenario exists with the pinned name, its assertions cover the old test's assertions (reviewed row-by-row against the table), and the old duplicated test functions are DELETED from the driver packages (smoke grep-absent). `inmem_fence_test.go` and `durable_fence_test.go` are removed entirely.
- [x] The identity-scoping scenarios (`Publish_IdentityMandatory_Rejected`, the three empty-triple rejections, `Subscribe_CrossTenant_Isolation`, `Replay_CrossIdentity_Isolation`, the fence non-bleed) run unconditionally for both drivers (§6 rule 10) — no skip path exists in the suite for them.
- [x] Capability presence is asserted fail-loud: a factory whose default bus does not type-assert to `events.Replayer` + `events.HistoryReplayer` + `events.Fencer` fails the suite with a named error — no capability-gated skips (§4.4). Realized as the dedicated `Capabilities_ReplayerHistoryReplayerFencer_Present` subtest plus the `mustReplayer`/`mustHistoryReplayer`/`mustFencer` helpers every scenario uses (each `t.Fatalf`s, never `t.Skip`s, on a missed assertion).
- [x] Zero production code change: the PR's diff touches only `internal/events/conformancetest/`, `_test.go` files under `internal/events/drivers/`, `scripts/smoke/phase-147.sh`, and docs (plans/decisions/glossary/README). Asserted by review + the smoke's grep legs. Held exactly: all 20 scenarios, including the 6 coverage-GAIN cells, passed against both drivers with no driver-side fix required.
- [x] `go test -race -count=1 ./internal/events/...` and `-count=2` are green (the §17.6 test-idempotency lesson — folded scenarios must not depend on left-over state), and no suite scenario synchronizes via `time.Sleep` (§17.4 — channel reads with bounded timeouts only).
- [x] Statement coverage on `internal/events/drivers/inmem` and `internal/events/drivers/durable` does not regress from the pre-fold baseline (recorded in the PR description). inmem ~93.5–93.8%→~93.8–94.3% (run-to-run noisy from timing-dependent reaper/drop-window branches; no pairing regresses), durable 88.3%→88.6% (stable) — both improved slightly, no regression.
- [x] `scripts/smoke/phase-147.sh` flips from skeleton to real assertions (the unit-test leg + the folded-names-gone grep + the both-drivers-invoke grep + the no-concrete-driver-import grep).
- [x] D-277 lands in `docs/decisions.md`; "events conformance suite" lands in `docs/glossary.md`; the master plan row + detail block land in `docs/plans/README.md` — same PR.

## Files added or changed

- `internal/events/conformancetest/conformancetest.go` — the suite (new)
- `internal/events/drivers/inmem/conformance_test.go` — inmem consumer (new)
- `internal/events/drivers/durable/conformance_test.go` — durable consumer (new)
- `internal/events/drivers/inmem/inmem_fence_test.go` — deleted (folded)
- `internal/events/drivers/durable/durable_fence_test.go` — deleted (folded)
- `internal/events/drivers/inmem/inmem_test.go`, `replay_test.go` — folded tests removed; driver-specific tests + shared helpers (`mkID`, `mkEvent`, `defaultCfg`) remain
- `internal/events/drivers/durable/durable_test.go` — folded tests removed; driver-specific tests + helpers remain
- `scripts/smoke/phase-147.sh` — new (skeleton in this plan's PR; real assertions with the implementation)
- `docs/decisions.md` (D-277), `docs/glossary.md`, `docs/plans/README.md` (row + detail block)

## Public API surface

Internal test-infrastructure surface (no `sdk/` export; "public" to driver packages only):

```go
package conformancetest // internal/events/conformancetest

// Harness bundles the driver-specific bus constructors the suite needs.
// All three are MANDATORY — no optional-capability ceremony (§4.4): both
// V1 drivers express all three configurations. Constructors register
// cleanup via t.Cleanup (the drivers' existing newReplayBus/newDurableBus
// shape); the Harness carries constructors rather than a pre-built bus
// because the scenarios span three bus CONFIGURATIONS, which one
// pre-built field cannot express.
type Harness struct {
    // NewBus returns a fresh bus in the driver's default replay-capable
    // configuration. The suite type-asserts events.Replayer,
    // events.HistoryReplayer and events.Fencer on it and FAILS loudly
    // (never skips) when an assertion misses.
    NewBus func(t *testing.T) events.EventBus

    // NewReplayDisabledBus returns a fresh bus configured with replay
    // OFF (ReplayBufferSize 0; the durable driver in its documented
    // best-effort mode), so Replay / Window fail with
    // ErrReplayUnavailable.
    NewReplayDisabledBus func(t *testing.T) events.EventBus

    // NewBoundedRetentionBus returns a fresh bus whose replay retention
    // is exactly capacity events (inmem: ring capacity; durable:
    // best-effort ring mode), so publishing past capacity evicts the
    // oldest sequences and a stale cursor fails with ErrCursorTooOld.
    NewBoundedRetentionBus func(t *testing.T, capacity int) events.EventBus
}

// Factory builds a fresh Harness; called once per top-level subtest.
type Factory func() Harness

// Run executes the canonical EventBus driver correctness suite.
func Run(t *testing.T, factory Factory)
```

## Test plan

- **Unit:** N/A as a separate bucket — this phase IS test code. The suite's own correctness is proven by its two real consumers running green (and by the fold mapping review: every old assertion has a new home).
- **Integration:** the durable consumer's harness wires the durable bus over a REAL `internal/state/drivers/inmem` StateStore — the events↔state seam stays exercised with real drivers, identity propagation (the isolation scenarios), and failure modes (`ErrCursorTooOld`, `ErrReplayUnavailable`, `ErrBusClosed`, empty-triple rejections) under `-race`. In-package boundary shape per §17.2; no new `test/integration/` file (the shipped wave E2Es already compose the bus at the system level).
- **Conformance:** the phase's entire deliverable — `conformancetest.Run` over both V1 drivers, 20 pinned scenarios (table above).
- **Concurrency / leak:** unchanged and deliberately untouched — each driver keeps its own D-025 concurrent-reuse tests and the inmem `leak_test.go` TestMain goroutine gate; the suite adds no long-lived component. Both `-count=1` and `-count=2` runs gate idempotency.

## Smoke script additions

`scripts/smoke/phase-147.sh` (`# PREFLIGHT_REQUIRES: unit-tests`) — skeleton parks with `skip` until the surface lands, then asserts:

1. `go test -race -count=1 ./internal/events/...` green (the suite via both consumers + all remaining driver-specific tests).
2. `assert_grep_present` — `conformancetest.Run` appears in BOTH `internal/events/drivers/inmem/conformance_test.go` and `internal/events/drivers/durable/conformance_test.go` (the two mandated consumers).
3. `assert_grep_absent` — the folded old test names (spot list from the mapping table, e.g. `TestInmem_Fence_DropsAndEmptiesHistory`, `TestDurable_Fence_DropsLateEventsAndEmptiesHistory`, `TestInmem_Bounds_ReportsHeadTail`, `TestDurable_Close_Idempotent`) no longer exist anywhere under `internal/events/drivers/` — the fold actually removed the duplication.
4. `assert_grep_absent` — `internal/events/conformancetest/conformancetest.go` contains no `internal/events/drivers` import (the suite never imports a concrete driver).
5. `assert_grep_present` — representative driver-specific tests survived (e.g. `TestDurable_PublishAfterRestart_NoSequenceCollision`, `TestConcurrentReuse_DurableBus`, `TestBus_ConcurrentReuse_ReuseContract`) — the fold did not over-reach.

## Coverage target

- `internal/events/drivers/inmem`: no regression from pre-fold baseline (85% floor per the shipped events phases)
- `internal/events/drivers/durable`: no regression from pre-fold baseline (85% floor)
- `internal/events/conformancetest`: no numeric target — test-support package whose non-test `.go` is dominated by assertion bodies (the documented posture of `internal/state/conformancetest` and `internal/planner/conformance`, per the Phase 49/62 precedent)

## Dependencies

- 05 (event taxonomy + inmem `EventBus` + isolation — the inmem consumer)
- 06 (replay + ring buffer + cursor — the `Replayer` scenarios)
- 57 (durable event-log driver — the durable consumer)
- 125 (`state.history` windowed replay — the `HistoryReplayer` Bounds/Window scenarios, D-254)
- 130 (session erasure — the cascade the `Fencer` capability serves, D-262; the fence itself landed in the v1.7 band close-out and its fail-loud hardening is D-274 item 2)

## Risks / open questions

- **A coverage-GAIN scenario fails on the driver that lacked it.** The six "gain" cells widen one driver's asserted surface (e.g. durable never asserted `Publish` identity-mandatory rejection or live cross-tenant subscribe isolation). Both drivers share `internal/events`' filter/validation machinery, so passes are expected — but a genuine divergence is a real finding: per §17.6 it is fixed in the SAME PR, and the PR documents the resulting §4.3 deviation from this phase's zero-production-change property. It is never resolved by dropping or skipping the scenario.
- **Name collision on `-run` filters.** `TestDurable_Conformance` already exists in `internal/tasks/drivers/durable` and `internal/distributed/drivers/durable` (and one of them carries a known pre-existing timing flake noted in the v1.9 checkpoint). Package-scoped `go test ./internal/events/...` invocations (as the smoke does) are unambiguous; repo-wide `-run TestDurable_Conformance` sweeps all three — acceptable, but the smoke and docs always scope by package path.
- **Helper entanglement.** The folded tests share helpers (`mkID`, `mkEvent`, `publishN`, `filterFor`, config builders) with tests that stay. The suite ships its own minimal helpers internally; the driver packages keep theirs for their remaining tests. Some transient duplication of trivial helpers between suite and driver tests is accepted — it is not the scenario duplication this phase exists to remove.
- **Fence publish-path synchronization is timing-adjacent.** The fence contract ("an in-flight Publish either completes before Fence returns … or observes the fence and is dropped") must be asserted without `time.Sleep` synchronization (§17.4). The folded tests are already synchronous on this path; the suite preserves that shape.
- **Second-tranche pull.** Reviewers may ask to fold the drop-policy/redaction/reaper/admin-audit pairs "while we're here." Resist: that tranche is timing-sensitive and separately risky; it is the named follow-up in D-277, not scope creep in this one.

## Glossary additions

- **events conformance suite** — the canonical shared correctness suite (`internal/events/conformancetest`) every `events.EventBus` driver must pass: 20 pinned scenarios covering publish/subscribe identity scoping, replay cursor semantics, Bounds/Window history reads, erasure-fence semantics, and close lifecycle, consumed by each driver's `conformancetest.Run(t, factory)` call. The events sibling of `internal/state/conformancetest` and `internal/memory/conformancetest`; genuinely configuration-dependent scenarios (replay-disabled, bounded retention) are parameterized via the `Harness`'s mandatory constructors, never via optional-capability flags. D-277.

(Added to `docs/glossary.md` in the same PR.)

## Pre-merge checklist

- [x] `make drift-audit` passes (1101 OK, 0 WARN, 0 FAIL)
- [ ] `make preflight` passes — NOT run locally per the dispatch instructions for this phase (commit uses `HARBOR_PREFLIGHT_SKIP=1`); CI runs the full gate. `make vet test build` (which builds the Console + `bin/harbor` and runs the full `-race` suite) passed locally, as did the phase-147 smoke script standalone and `go test -race ./...` across all 147 packages.
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve (drift-audit)
- [x] Coverage on touched packages ≥ stated target (no regression on either driver package; baselines recorded in the PR). inmem ~93.5–93.8%→~93.8–94.3% (run-to-run noisy from timing-dependent branches; no pairing regresses), durable 88.3%→88.6% (stable).
- [x] If multi-isolation paths changed: cross-session isolation test passes (no production isolation path changes; the suite's isolation scenarios themselves are the strengthened gate)
- [x] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. N/A — test-only refactor; no new reusable artifact. Each driver's existing D-025 tests are deliberately kept driver-local and untouched.
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. Satisfied in-package: the durable consumer wires the real durable bus over a real inmem StateStore; identity scenarios + multiple failure modes run under `-race`.
- [x] If new vocabulary: glossary updated ("events conformance suite")
- [x] If a brief finding was departed from: justified above + decisions.md entry filed (None departed from)
