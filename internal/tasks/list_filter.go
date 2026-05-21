package tasks

import (
	"errors"
	"fmt"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// Phase 73h (Wave 13 Stage 2.3 / D-128) — the wire-to-runtime
// `tasks.list` filter translator.
//
// The Console Background Jobs page (and the Tasks page) issue a
// `tasks.list` carrying the Protocol-layer `types.TaskFilter` facet
// shape — a flat, JSON-stable wire vocabulary. The runtime-internal
// `tasks.TaskRegistry.List` consumes a NARROWER `tasks.TaskFilter`
// (single-valued `Status` / `Kind` / `ParentID` pointers). The two
// vocabularies are deliberately distinct (CLAUDE.md §8 — the Protocol
// owns its own wire vocabulary; the runtime owns its own).
//
// `ListFilterFromWire` is the single translator between them. It is a
// PURE function — no goroutines, no shared state, no per-call mutation
// of any artifact (D-025). The Protocol `tasks/protocol.Service`
// already applies the rich multi-valued facet filtering (status sets,
// kind sets, group / approval / search) on TOP of the registry result;
// this translator narrows ONLY the facets the registry's single-valued
// `TaskFilter` can pre-filter on, so the registry returns a tighter
// candidate set. A facet the registry cannot express (a multi-status
// set, the group / approval / search facets) is left to the
// Service-layer `filterMatches` pass — translating it here would be a
// silent lossy down-conversion.
//
// # The translation rule
//
//   - A wire `Kinds` slice of EXACTLY ONE kind narrows the registry
//     `Kind` pointer (the Background Jobs page's canonical
//     `Kinds: ["background"]` queue-mode binding hits this path). A
//     `Kinds` slice with two or more kinds (or zero) leaves the
//     registry `Kind` pointer nil — the Service's `filterMatches`
//     applies the multi-kind set.
//   - A wire `Statuses` slice of EXACTLY ONE status narrows the
//     registry `Status` pointer; a multi-status set is left to the
//     Service.
//   - A wire `ParentTaskID` narrows the registry `ParentID` pointer
//     verbatim.
//   - The `GroupID`, `HasPendingApproval`, `Identities`, `Since`,
//     `Until`, `ErrorClasses`, `LatencyAboveMS`, and `Search` facets
//     have no registry-`TaskFilter` counterpart — they stay on the
//     Service-layer pass and this translator does not touch them.
//
// The translator validates the enum values it narrows on: an unknown
// wire status / kind fails loud with ErrInvalidListFilter rather than
// silently producing a registry filter that matches nothing (CLAUDE.md
// §13 — no silent degradation).

// ErrInvalidListFilter is returned by ListFilterFromWire when the wire
// filter names an enum value outside the canonical task-status /
// task-kind sets. Callers compare with errors.Is.
var ErrInvalidListFilter = errors.New("tasks: invalid tasks.list wire filter")

// ListFilterFromWire translates the Protocol-layer `types.TaskFilter`
// into the runtime-internal `tasks.TaskFilter` the `TaskRegistry.List`
// method consumes. It narrows the registry filter ONLY on the facets
// the single-valued registry `TaskFilter` can express (a one-element
// status / kind set, and the parent-task pointer); every richer facet
// is left to the Protocol-layer `filterMatches` pass.
//
// The function is pure: it allocates and returns a fresh
// `tasks.TaskFilter`, reads only its argument, and holds no state. It
// is therefore trivially safe for concurrent use by N goroutines
// (D-025) — there is no reusable artifact to leak.
func ListFilterFromWire(wire *prototypes.TaskFilter) (TaskFilter, error) {
	if wire == nil {
		return TaskFilter{}, nil
	}

	var rf TaskFilter

	// A single-status wire set narrows the registry Status pointer; a
	// multi-status set is left to the Service-layer filterMatches pass.
	if len(wire.Statuses) == 1 {
		st, err := statusFromWire(wire.Statuses[0])
		if err != nil {
			return TaskFilter{}, err
		}
		rf.Status = &st
	} else {
		// Validate every status in a multi-element set so a malformed
		// enum still fails loud here (the Service also validates, but
		// the translator must not pass a structurally invalid filter
		// downstream).
		for _, ws := range wire.Statuses {
			if _, err := statusFromWire(ws); err != nil {
				return TaskFilter{}, err
			}
		}
	}

	// A single-kind wire set narrows the registry Kind pointer. The
	// Background Jobs page's canonical `Kinds: ["background"]`
	// queue-mode binding hits exactly this path — the registry then
	// returns only background tasks and the Service's filterMatches is
	// a no-op on the kind axis.
	if len(wire.Kinds) == 1 {
		k, err := kindFromWire(wire.Kinds[0])
		if err != nil {
			return TaskFilter{}, err
		}
		rf.Kind = &k
	} else {
		for _, wk := range wire.Kinds {
			if _, err := kindFromWire(wk); err != nil {
				return TaskFilter{}, err
			}
		}
	}

	// The parent-task pointer narrows a SpawnTask drill-in verbatim.
	if wire.ParentTaskID != "" {
		pid := TaskID(wire.ParentTaskID)
		rf.ParentID = &pid
	}

	return rf, nil
}

// statusFromWire maps a Protocol wire TaskStatus onto the runtime
// TaskStatus, failing loud on an unknown enum value.
func statusFromWire(ws prototypes.TaskStatus) (TaskStatus, error) {
	switch ws {
	case prototypes.TaskStatusPending:
		return StatusPending, nil
	case prototypes.TaskStatusRunning:
		return StatusRunning, nil
	case prototypes.TaskStatusPaused:
		return StatusPaused, nil
	case prototypes.TaskStatusComplete:
		return StatusComplete, nil
	case prototypes.TaskStatusFailed:
		return StatusFailed, nil
	case prototypes.TaskStatusCancelled:
		return StatusCancelled, nil
	default:
		return "", fmt.Errorf("%w: unknown task status %q", ErrInvalidListFilter, ws)
	}
}

// kindFromWire maps a Protocol wire TaskKind onto the runtime TaskKind,
// failing loud on an unknown enum value.
func kindFromWire(wk prototypes.TaskKind) (TaskKind, error) {
	switch wk {
	case prototypes.TaskKindForeground:
		return KindForeground, nil
	case prototypes.TaskKindBackground:
		return KindBackground, nil
	default:
		return "", fmt.Errorf("%w: unknown task kind %q", ErrInvalidListFilter, wk)
	}
}
