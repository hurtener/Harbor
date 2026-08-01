# Phase 231 — Deterministic reliability closure

## Summary

Closes the accepted test-flake backlog without retries, longer sleeps, or weaker assertions. Tests coordinate through explicit barriers and liveness signals. The Linux PTY workflow is an unconditional release gate with failure-only process/goroutine diagnostics. The #604 recurrence exposed three harness defects and one production modal race: the harness sent legacy/internal function-key values after Kitty CSI-u negotiation, treated a cell-diff byte stream as a terminal screen, and required a transient toast that the renderer may validly coalesce; a superseded same-scope inspection could also close a newly opened action modal. The corrected path uses real CSI-u functional-key codepoints, persisted-state acknowledgements before full visual repaints, canonical Runtime outcomes, and discards stale same-scope inspection results without changing current focus.

The frozen corrective tree passed 100 race-instrumented repetitions of the full authenticated PTY workflow in a two-CPU Go 1.26 Linux container (`ok`, 392.617s), after an independent 20-repetition constrained calibration and 10 local race repetitions.

## RFC anchor

- RFC §5.4.
- RFC §6.4.

## Briefs informing this phase

- brief 06
- brief 07
- brief 11
- brief 12

## Brief findings incorporated

- brief 06 §6: lifecycle and event tests assert observable behavior and ordering.
- brief 07 §11: cancellation and parallel tests use explicit outcomes and leak checks.
- brief 11: the TUI remains a Protocol client with one owned event stream.
- brief 12: deployment/client lifecycles clean up deterministically.

## Findings I'm departing from (if any)

- None.

## Goals

- Fix #615, #627, and #610 causally; correct #604's reachable harness defect, remove its quarantine, and independently prove #480, #598, #599, #532, and #507 resolved.

## Non-goals

- No timeout inflation, pass-by-rerun, permanent quarantine, or unrelated TUI feature work.

## Acceptance criteria

- [x] Tool failure simulation is deterministic per invocation, never scheduler-dependent.
- [x] OAuth refresh singleflight holds all callers behind one observable flight and asserts one refresh.
- [x] OAuth choreography distinguishes the tool pause from the planner/run pause, waits until both exist, and resumes the run pause exactly once after callback persistence.
- [x] PTY waits react to output/exit/deadline and emit actionable Linux process/goroutine evidence.
- [x] A second retry command while the first retry is dispatching is deterministically rejected without a second start or queued-intent mutation; the PTY sends the canonical single command.
- [x] A focused real-PTY decoder test covers F1–F9 and fails when the Kitty wire translation is removed; the unquarantined workflow then passes at least 100 two-CPU Linux `-race` repetitions.
- [x] A superseded inspection in the current identity/generation cannot close a newer action modal; a cross-generation or cross-identity result still invalidates it.
- [ ] Stale issues have exact targeted `-race -count=100` and CI/full-suite evidence.

## Files added or changed

- `internal/tools/auth/`, tool/dispatch test harnesses
- `internal/tui/app/` same-scope stale-inspection focus handling
- `test/integration/` OAuth and TUI PTY tests
- `scripts/smoke/phase-231.sh`

## Public API surface

- None; test-only synchronization is immutable after construction and production behavior changes only when #604 evidence identifies a causal defect.

## Test plan

- **Unit:** barriers, process-exit diagnostics, render/input invalidation branch, and CSI-u wire translation.
- **Integration:** OAuth callback and real PTY workflow.
- **Conformance:** repeated targeted issue proofs.
- **Concurrency / leak:** `-race -count=100`, two-CPU Linux PTY stress, joined goroutines.

## Smoke script additions

- Execute every named issue regression and reject no-test/skip outcomes.

## Coverage target

- Touched production packages do not regress; test-only packages execute every new branch.

## Dependencies

- 223, 229.

## Risks / open questions

- Kitty functional-key codepoints are distinct from Bubble Tea's internal `tea.KeyF*` enum. The focused real-PTY decoder test is the guard; raw cell-diff bytes are never treated as a screen-state acknowledgement.

## Glossary additions

- None.

## Pre-merge checklist

- [ ] Drift, mirror, CI preflight, exact repetitions, Linux stress, integration, and leak gates pass
