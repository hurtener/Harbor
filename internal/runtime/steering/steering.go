// Package steering ships Harbor's per-run steering inbox + the
// nine-event control taxonomy + the Protocol-edge validation /
// sanitisation pass (RFC §6.3, brief 02 §2-§4).
//
// # What Phase 52 ships
//
// Steering is a Runtime capability surfaced over the Protocol.
// Planners observe accumulated `Control` signals via `RunContext`;
// they NEVER touch the inbox. The Runtime owns the inbox. Phase 52
// lands the data structures and the edge enforcement:
//
//   - The nine-type control taxonomy (`ControlType` + the canonical
//     `ControlEvent` record) — `INJECT_CONTEXT`, `REDIRECT`, `CANCEL`,
//     `PRIORITIZE`, `PAUSE`, `RESUME`, `APPROVE`, `REJECT`,
//     `USER_MESSAGE`.
//   - The per-run `Inbox` owned by the Runtime: an enqueue + drain
//     surface, per-run, identity-scoped. A process-wide `Registry`
//     mints / looks up / retires per-run inboxes.
//   - The Protocol-edge `Validate` pass: the RFC §6.3 payload bounds
//     (depth ≤ 6, ≤ 64 keys, ≤ 50 list items, ≤ 4096 chars / string,
//     ≤ 16 KiB total) — enforced loud, never silently truncated.
//   - The per-event `Scope` check: each control type declares the
//     minimum caller scope per RFC §6.3; a mismatch is rejected loud
//     with `ErrScopeMismatch` (the Protocol projection maps that to
//     403 + an audit emit).
//
// # What Phase 53 adds (the run-loop wiring)
//
// Phase 52 shipped the primitive (taxonomy + inbox + validation +
// scope). Phase 53 ships `RunLoop` — the per-run planner-step loop
// that is the §13 first consumer of BOTH this primitive AND the
// Phase 50 `pauseresume.Coordinator`:
//
//   - `RunLoop.Run` drives a `planner.Planner` to a terminal
//     `planner.Finish`, draining the per-run `Inbox` exactly ONCE per
//     step boundary (`Inbox.Drain` — never mid-tool-call, the
//     drain-between-steps invariant from brief 02 §6).
//   - The nine control events' side effects are applied (`apply.go`):
//     CANCEL hard/soft, PAUSE/RESUME/APPROVE/REJECT onto the unified
//     `pauseresume.Coordinator`, INJECT_CONTEXT/REDIRECT/USER_MESSAGE
//     projected onto `RunContext.Control` (the planner sees ONLY this
//     — never the `Inbox`), PRIORITIZE onto the `tasks.TaskRegistry`.
//   - A planner's `RequestPause` decision routes through
//     `Coordinator.Request`; `Inbox.WaitForEvent` blocks the loop
//     (no busy-spin) until a RESUME / APPROVE arrives, which routes
//     through `Coordinator.Resume`.
//   - Per-session applied-control history is capped
//     (`controlHistory`, `MaxControlHistory`, newest-wins ring).
//   - `control.received` / `control.applied` lifecycle events are
//     emitted (`events.go`).
//
// See D-071 and the phase-53 plan's "§13 primitive-with-consumer —
// discharged here" section.
//
// # Pause-family controls converge on the unified primitive
//
// `PAUSE` / `RESUME` / `APPROVE` / `REJECT` are taxonomy entries here
// — Phase 52 validates them and scope-checks them. Phase 53 wires
// their side effects onto the ONE pause/resume primitive
// (`internal/runtime/pauseresume`, Phase 50) — Phase 52 does NOT
// reinvent pause coordination (CLAUDE.md §7 rule 4).
//
// # Fail loudly
//
// There is no silent-degradation path (RFC §3.4, CLAUDE.md §13 +
// §5). An oversize / over-deep payload is REJECTED with a wrapped
// `ErrPayloadInvalid`, never truncated to fit. A missing identity
// triple fails closed with `ErrIdentityRequired`. A scope mismatch
// fails closed with `ErrScopeMismatch`. An unknown control type is
// rejected with `ErrUnknownControlType`.
//
// # Concurrent reuse (D-025)
//
// The process-wide `Registry` is a compiled artifact: immutable
// after construction, with the per-run inbox map behind a
// documented-invariant `sync.Mutex`. A per-run `Inbox` is itself
// concurrent-safe — N Protocol-edge goroutines may `Enqueue` while
// the run loop `Drain`s — its queue is mutex-guarded. Per-run state
// never leaks across runs: each `Inbox` is keyed by the run's
// identity quadruple and holds only that run's events.
// concurrent_test.go pins N≥100 under -race.
package steering

import "time"

// Clock is the minimal time source the inbox uses for the
// per-event `EnqueuedAt` stamp. Tests inject a controllable clock so
// no test sleeps for synchronisation (CLAUDE.md §11). Production code
// uses the real-time `systemClock`.
type Clock interface {
	Now() time.Time
}

// systemClock is the production wall-clock Clock.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
