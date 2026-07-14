# Phase 180 — TUI projection and reconciliation core

## Summary

Build the pure Go state model that turns Harbor Protocol snapshots and events
into a stable ordered conversation/runtime projection. This phase contains no
Bubble Tea code; it closes hydration, replay, race, partiality, and cross-client
fixture correctness before rendering begins.

## RFC anchor

- RFC §3.1
- RFC §3.3
- RFC §4.1
- RFC §4.2
- RFC §5.1
- RFC §5.2
- RFC §5.3
- RFC §5.4
- RFC §5.5

## Briefs informing this phase

- brief 05
- brief 06
- brief 07
- brief 11

## Brief findings incorporated

- brief 06 §1 and §5: one canonical event stream plus snapshots feeds every
  external client; no TUI-private projection endpoint.
- brief 06 §4: replay cursors and dropped-event signals must become explicit
  client state rather than invisible loss.
- brief 05 §sessions/tasks: identity and lifecycle records remain isolated by
  the full triple across concurrent sessions.
- brief 11 §PG-1: conversation and task views are projections over Runtime-owned
  state, never a client shadow source of truth.

## Findings I'm departing from (if any)

Brief 11 assumes a browser Console reducer. This phase produces a pure Go
reducer and language-neutral fixtures so the native TUI can reach equivalent
behavior without sharing Svelte implementation code.

## Goals

- Produce deterministic conversation/runtime state from history, task/session/
  pause snapshots, and ordered events.
- Make stale snapshots, replay gaps, retention, and partial values explicit.
- Share canonical fixtures with the Console to prevent projection drift.
- Prove concurrent sessions cannot cross-talk.

## Non-goals

- No terminal rendering, keybindings, composer, or Bubble Tea dependency.
- No new Protocol transcript method or TUI-only endpoint.
- No local persistence beyond test fixtures.
- No coding-agent concepts.

## Acceptance criteria

- [x] `internal/tui/projection` is pure Go with no Bubble Tea/Lip Gloss import.
- [x] Hydration loads tail-first `state.history`, joins `tasks.list/get`,
      `sessions.inspect`, and `pause.list`, then establishes the live cursor
      without snapshot-over-live rollback.
- [x] Reducer ordering is deterministic by session/sequence and repairs terminal
      lifecycle events whose opening event was retained away.
- [x] Duplicate/out-of-order events are idempotent; stale snapshot generations
      cannot resurrect deleted/resolved/terminal state.
- [x] `session.reopened` invalidates stale closed state; `session_erased` creates
      a terminal tombstone that reconnect never retries as an ordinary session.
- [x] `counters_partial`, history/aggregate truncation, scoped retention,
      unavailable capabilities, and bounded tool analytics remain representable.
- [x] Unknown event/tool/result payloads become safe generic blocks and never
      disappear or panic the reducer.
- [x] High-rate content/reasoning deltas expose a batchable update stream while
      lifecycle/intervention changes remain immediate.
- [x] Language-neutral JSON fixtures cover all canonical block families and run
      through both Go and Console reducer tests with equivalent normalized output.
- [x] N≥100 sessions reduce concurrently against shared immutable fixtures with
      no identity bleed, cancellation cross-talk, race, or goroutine leak.

## Files added or changed

- `internal/tui/projection/`
- `internal/tui/testdata/projection/`
- production Console history/live reducer contract tests and the concrete
  `PauseToken` intervention-correlation correction those fixtures exposed
- `test/integration/tui_projection_test.go`
- `scripts/smoke/phase-180.sh`

## Public API surface

```go
type Reducer struct{}

func (r *Reducer) Hydrate(SnapshotBundle) (Projection, error)
func (r *Reducer) Apply(Projection, WireEvent) (Projection, ChangeSet, error)
func Reconcile(Projection, SnapshotBundle) (Projection, ChangeSet, error)
```

The values remain internal until the rendering consumer proves the required
surface; no public SDK projection is introduced.

## Test plan

- **Unit:** every event/block family, ordering, dedupe, stale generations,
  tombstones, lifecycle repair, partiality, unknown payloads, batch boundaries.
- **Integration:** real authenticated Protocol server and production in-memory
  drivers; hydrate, stream, close/reopen, erase, approval resolution, replay gap.
- **Conformance:** shared JSON fixture corpus consumed by Go and Console tests.
- **Concurrency / leak:** N≥100 session projections and cancellation races under
  `-race`, with goroutine baseline restored.

## Smoke script additions

- Run projection and shared-fixture tests under `-race`.
- Assert the projection package imports no terminal framework.
- Assert every registered canonical event has a typed or generic fixture path.

## Coverage target

- `internal/tui/projection`: 95%

## Dependencies

- 179
- 124
- 125
- 161
- 162
- 163
- 165
- 174
- 175
- 176
- 177
- 178

## Risks / open questions

- Flat events plus task/history joins can drift from a future canonical turn
  read model; measured fixture drift is the trigger for a later RFC.
- The reducer must retain enough provenance to explain incomplete state without
  retaining raw secret-bearing tool arguments/results.

## Glossary additions

- conversation reducer
- snapshot generation fence

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] Cross-session isolation test passes
- [x] Concurrent-reuse test passes with N≥100 under `-race`
- [x] Real-driver integration covers identity and ≥1 failure mode under `-race`
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: N/A; no departure
