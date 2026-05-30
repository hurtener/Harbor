# Phase 25a — durable-memory-strategies

## Summary

Make `truncation` and `rolling_summary` work on the SQLite and Postgres memory
drivers (today they implement `strategy=none` only and reject the rest), and
thread the `Summarizer` through the `memory.Open` registry path so the dev
binary stops special-casing `rolling_summary` with a direct `inmem.New(...)`
call. The strategy algorithms already live in a driver-agnostic
`internal/memory/strategy/` executor package that persists through an injected
`state.StateStore`; the inmem driver is a thin shell over it. This phase makes
the SQL drivers delegate to the SAME executors, so all three drivers gain all
three strategies with one code path — no algorithm is reimplemented in SQL.

## RFC anchor

- RFC §6.6
- RFC §9

## Briefs informing this phase

- brief 02
- brief 13

## Brief findings incorporated

- brief 02 §"fail loudly": memory must not silently degrade. `rolling_summary`
  configured without a real `Summarizer` MUST fail loud at open (as
  `strategy.New` already does), never fall back to a stub. This phase keeps that
  invariant on the registry path it newly enables.
- brief 13 §"memory injection": the run loop's session-scoped `GetLLMContext`
  (83f) injects memory each turn; recall quality depends on the strategy keeping
  meaningful context. `rolling_summary` (recent window + running summary) is the
  strategy that holds recall across a long chat — making it durable closes the
  "memory resets on restart" gap operators hit.

## Findings I'm departing from (if any)

None. This phase completes the Phase 24/25 intersection (strategies × SQL
drivers) that shipped only against inmem; it does not redesign the executors.

## Goals

- The SQLite and Postgres memory drivers delegate to `strategy.StrategyExecutor`
  (like inmem), injecting their `state.StateStore` dependency, so `truncation`
  and `rolling_summary` persist durably via the StateStore.
- `memory.Deps` gains a `Summarizer memory.Summarizer` field; `memory.Open`
  routes it to the driver factories, which pass it into the executor `Deps`.
- The `cmd/harbor/cmd_dev.go` rolling_summary special-case (the direct
  `inmem.New(...)` + the "only inmem" rejection) is removed: one
  `memory.Open(ctx, cfg, Deps{State, Bus, Summarizer})` call serves every driver
  + strategy. The summariser still defaults to the agent's configured LLM
  (`llmsummarizer.New(llmClient)`) — no separate summariser model, no hardwiring.
- Fail loud, unchanged: `rolling_summary` without a `Summarizer` errors at open
  on every driver; no stub default (CLAUDE.md §13).
- The memory conformance suite runs all three strategies against all three
  drivers (the SQL driver tests stop pinning `StrategyNone` and supply a
  `Summarizer` for the rolling_summary subtests).

## Non-goals

- Vision-aware summarisation (Phase 99) — the rolling_summary placeholder for
  image turns is unchanged.
- New memory strategies beyond the three that exist.
- A Console surface for memory strategy — out of scope.
- Changing the executor algorithms (recent-window size `FullZoneTurns=4`, token
  estimator, health FSM) — reused as-is.

## Acceptance criteria

- [ ] **AC-1** `memory.Deps` gains `Summarizer memory.Summarizer` (optional;
  required only when the configured strategy is `rolling_summary`). `memory.Open`
  validates it for that strategy and routes it to the driver factory.
- [ ] **AC-2** The SQLite memory driver supports `truncation` + `rolling_summary`
  by delegating to `strategy.StrategyExecutor` with its injected
  `state.StateStore`; the `ErrStrategyNotImplemented` rejection
  (`sqlite.go:121-124`) and `TestSQLite_New_RejectsTruncationStrategy` are removed.
- [ ] **AC-3** The Postgres memory driver does the same; its reject path +
  `TestPostgres_New_RejectsTruncationStrategy` are removed.
- [ ] **AC-4** The memory conformance suite passes for `{none, truncation,
  rolling_summary}` × `{inmem, sqlite, postgres}` — the sqlite/postgres driver
  test files loop all three strategies and inject a stub `Summarizer` for
  rolling_summary (mirroring `inmem_test.go`).
- [ ] **AC-5** Durability: a `rolling_summary` (and `truncation`) store on SQLite,
  after `Close` + reopen against the same DSN, returns the prior summary + recent
  turns from `GetLLMContext` (a restart-rehydration test — the StateStore-backed
  reload path).
- [ ] **AC-6** `rolling_summary` without a `Summarizer` fails loud at
  `memory.Open` on ALL three drivers (no stub fallback) — explicit test.
- [ ] **AC-7** `cmd/harbor/cmd_dev.go` no longer special-cases rolling_summary:
  one `memory.Open` call; the summariser is built from the configured LLM and
  passed in `Deps`; the "only inmem" error is deleted.
- [ ] **AC-8** Cross-driver byte-stable snapshots hold for rolling_summary: the
  snapshot record carries the summary (reconcile the exported `memory.Record`
  with the internal `memoryStateRecord{Summary}` so the SQL drivers don't drop it).
- [ ] **AC-9** Goroutine-leak baseline: the rolling_summary recovery-loop
  goroutine is joined on `Close` for the SQL drivers too (free if delegating to
  the executor, whose `Close` already joins it) — the conformance leak assertion
  passes for all drivers.
- [ ] **AC-10** `scripts/smoke/phase-25a.sh` runs the conformance suite across the
  three drivers (classification `unit-tests`) and asserts the SQL drivers no
  longer reject non-none strategies.

## Files added or changed

- `internal/memory/registry.go` — `Deps.Summarizer`; `Open` routes it;
  `validateDeps` requires it for rolling_summary.
- `internal/memory/drivers/sqlite/sqlite.go` — delegate to
  `strategy.StrategyExecutor`; remove the strategy rejection; pass
  `deps.State` + `deps.Summarizer` into the executor.
- `internal/memory/drivers/postgres/postgres.go` — same.
- `internal/memory/drivers/inmem/inmem.go` — factory reads `deps.Summarizer`
  (so the registry path wires rolling_summary on inmem too).
- `internal/memory/drivers/sqlite/sqlite_test.go` +
  `postgres_test.go` — loop all three strategies; delete the reject tests; add
  the restart-rehydration test.
- `internal/memory/wire.go` / `internal/memory/strategy/none.go` — reconcile the
  snapshot record so rolling_summary's `Summary` survives (AC-8).
- `cmd/harbor/cmd_dev.go` — collapse the memory wiring to one `memory.Open` call.
- `scripts/smoke/phase-25a.sh` (**NEW**).
- `docs/decisions.md` — D-174.
- `docs/plans/README.md` — Status; reflect that 24/25 strategies×SQL is now
  closed.

## Public API surface

- `memory.Deps` gains `Summarizer memory.Summarizer` (additive; existing callers
  that pass `{State, Bus}` still compile — zero value is nil, valid for
  none/truncation).
- No change to the `MemoryStore` interface or the `Strategy` constants or the
  `MemoryConfig` YAML (already accepts all three strategies + drivers).

## Test plan

- **Unit:** the SQL drivers' delegation wiring (executor constructed with the
  right `state.StateStore` + `Summarizer`); the snapshot-record reconciliation.
- **Integration:** the restart-rehydration test (AC-5) — write turns under
  rolling_summary on a real SQLite DSN, close, reopen, assert summary + recent
  turns return; identity propagated; the failure mode = reopen with a nil
  Summarizer fails loud. Under `-race`.
- **Conformance:** the existing suite, now run for all three strategies on
  sqlite + postgres (AC-4) — this is the binding gate.
- **Concurrency / leak:** the conformance leak subtest (goroutine baseline after
  Close) for rolling_summary on the SQL drivers (AC-9); the memory store is a
  reusable artifact — the existing concurrent-reuse coverage extends to the SQL
  drivers under the new strategies.

## Smoke script additions

- `scripts/smoke/phase-25a.sh` (classification `unit-tests`): run the memory
  conformance suite for the SQL drivers across the three strategies; assert (via
  `go test`) the SQL drivers accept truncation + rolling_summary.

## Coverage target

- `internal/memory/drivers/sqlite`: 85%.
- `internal/memory/drivers/postgres`: 85%.
- `internal/memory`: maintain ≥ current (registry + Deps change).

## Dependencies

- 23 (memory subsystem core), 24 (memory strategies — the executors), 25 (SQL
  memory drivers — the persistence shells this phase upgrades), 15/16 (SQLite +
  Postgres StateStore drivers — the durable backing the executors persist to).

## Risks / open questions

- **`memory.Record` (exported) vs `memoryStateRecord{Summary}` (internal)
  divergence** (AC-8): if the SQL drivers marshal snapshots via the exported
  record they drop the summary. Reconcile to one shape that carries `Summary`,
  preserving cross-driver byte-stability (a Phase 25 acceptance criterion).
- **§13 primitive-with-consumer / stub-as-default:** `Deps.Summarizer` is a new
  primitive; `cmd_dev` is its production consumer — they land together. The
  registry default MUST NOT be a stub summariser; rolling_summary without one
  fails loud (AC-6).
- **Executor cache vs StateStore source-of-truth:** the executors cache per-key
  in `sync.Map` and load-once from the StateStore. Durability rides on the
  StateStore writes; the restart-rehydration test (AC-5) is the proof the reload
  path works against a real SQL StateStore.
- **Migrations:** delegating to the executor (which persists at
  `Kind="memory.state"` via the StateStore) means the SQL memory drivers' own
  `memory_state` table may become vestigial. Decide at implementation time
  whether to keep it (back-compat, strategy=none rows) or migrate — forward-only,
  never edit a merged migration (CLAUDE.md §9).

## Glossary additions

- Extend the `rolling_summary` / memory-strategy glossary notes to record that
  all three strategies now run on all three drivers via the shared executor, and
  that the `Summarizer` resolves through `memory.Open` (defaulting to the agent
  LLM in `harbor dev`).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — memory is identity-scoped; the conformance suite asserts per-identity isolation across the new strategies.
- [ ] **Concurrent-reuse test passes** — the SQL memory stores under truncation/rolling_summary run the conformance concurrency + leak subtests (N concurrent, goroutine baseline after Close).
- [ ] **Integration test exists** — restart-rehydration on a real SQLite DSN under rolling_summary, ≥1 failure mode (nil Summarizer fails loud), under `-race`.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
