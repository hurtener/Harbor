package planner

// Wake-on-resolution contract (D-032).
//
// When a planner concrete returns SpawnTask WITHOUT retain-turn (i.e.
// SpawnTask.Spec.RetainTurn == false), it MUST consume
// `tasks.TaskRegistry.WatchGroup(sessionID, groupID)` to learn when
// the group resolves. Three wake patterns sit on top of that single
// mechanism — documented in `internal/tasks/groups.go` package godoc:
//
//  1. Push: subscribe to WatchGroup; the runtime engine consumes the
//     closed channel as a wake event and re-invokes the planner with
//     the typed `tasks.GroupCompletion` payload surfaced through
//     RunContext.Trajectory.Background.
//  2. Poll: skip WatchGroup; call `tasks.TaskRegistry.ListGroups` /
//     `Get(taskID)` periodically and return to the main loop when the
//     group's Status is terminal. Suits deterministic interleaving.
//  3. Hybrid: the main planner subscribes via WatchGroup (push); a
//     sidecar polls intermediate state and emits user-visible
//     progress events between push deliveries.
//
// D-032 keeps the TaskRegistry NEUTRAL — no `WakeMode` field, no
// `Supports*` capability protocol. The choice is a planner-concrete
// concern. The planner-side `WakeMode` enum below + the optional
// `WakeAware` interface exist so:
//
//   - Each concrete declares its mode in ONE canonical place.
//   - The conformance pack (Phase 49) asserts the round-trip for the
//     declared mode (SpawnTask → group completes → planner re-enters
//     → reads MemberOutcome).
//   - Observability + the Console can surface the mode without
//     introspecting concrete-private fields.
//
// `WakeAware` is OPTIONAL — a planner that never spawns
// non-retain-turn tasks (e.g. the Phase 42 stub finish.Planner) can
// implement it returning `WakePush` (the safe default) or skip it
// entirely. The conformance pack falls back to `WakePush` when the
// concrete does not implement `WakeAware`.

// WakeMode names a planner concrete's chosen wake-on-resolution
// strategy. The constants match the canonical names documented at
// `internal/tasks/groups.go`.
type WakeMode string

// Wake modes (D-032 — settled).
const (
	// WakePush — the planner subscribes to WatchGroup; the runtime
	// re-invokes Next on group resolution. Lowest latency, lowest
	// LLM cost (no in-flight polls). Phase 45 ReAct uses WakePush.
	WakePush WakeMode = "push"

	// WakePoll — the planner skips WatchGroup and re-checks group
	// status deterministically on its own cadence. No subscription
	// required. Suits deterministic / workflow planners (Phase 48+).
	WakePoll WakeMode = "poll"

	// WakeHybrid — the main planner subscribes via WatchGroup (push)
	// AND a sidecar (small LLM / templater) polls intermediate state
	// to emit user-facing progress events between push deliveries.
	WakeHybrid WakeMode = "hybrid"
)

// IsValidWakeMode reports whether m is one of the three canonical
// modes. The conformance pack uses this to validate WakeAware
// implementations.
func IsValidWakeMode(m WakeMode) bool {
	switch m {
	case WakePush, WakePoll, WakeHybrid:
		return true
	default:
		return false
	}
}

// String returns the canonical string form of the wake mode.
func (m WakeMode) String() string {
	return string(m)
}

// WakeAware is the OPTIONAL interface a planner concrete may
// implement to declare its non-retain-turn wake strategy. The
// conformance pack (Phase 49) asserts the round-trip for the
// declared mode; observability + the Console surface the value.
//
// A planner that does not implement WakeAware is treated as WakePush
// by the conformance pack (the safe default — no polling burden).
//
// Implementations MUST return a constant value for the lifetime of
// the planner instance. WakeMode is identity, not capability — a
// concrete picks one mode at construction time.
type WakeAware interface {
	WakeMode() WakeMode
}

// ResolveWakeMode returns the effective WakeMode for a planner
// instance: the planner's own WakeMode() if it implements WakeAware,
// otherwise WakePush as the documented default.
//
// Callers (the conformance pack, observability emitters) use this
// helper to avoid scattering type assertions.
func ResolveWakeMode(p Planner) WakeMode {
	if wa, ok := p.(WakeAware); ok {
		return wa.WakeMode()
	}
	return WakePush
}
