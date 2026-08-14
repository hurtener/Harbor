package materializer

import (
	"context"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
)

// sessionState is the materializer's per-session in-memory working
// state. It is NOT a second durable store and NOT a warehouse: the
// projector Store remains the authoritative row source; this state
// exists only to preserve the materializer's own derived accumulators
// (the FULL cumulative activity / reasoning / usage / input feeds —
// which the row cannot retain past its bounded windows) and the
// run/task routing index, across the events of one materializer
// lifetime. After a process restart the state is rebuilt by re-paging
// the durable event source from sequence zero (idempotent: every
// projector mutation at or below a row's last-applied sequence is a
// no-op), so the state never needs its own persistence.
type sessionState struct {
	// id is the session's isolation triple.
	id identity.Identity
	// checkpoint mirrors the projector Store's durable per-session
	// checkpoint (last-applied runtime event sequence). It is loaded
	// from the store when the session state is created and advanced
	// monotonically after every applied event. Events at or below it
	// are applied to the in-memory state only (their row mutations
	// already landed in a previous lifetime) — restart catch-up is
	// cheap and idempotent.
	checkpoint uint64
	// fenced marks the session as permanently fenced (an erasure has
	// converged): every further event for it is skipped without
	// touching the store. The fence is never lifted.
	fenced bool
	// runs maps a run id (the envelope Quadruple.RunID) to the task id
	// the run executed — built from task.spawned events. Tool /
	// planner / app / pause / usage events route through it to the
	// owning turn; a child run's events fold into its parent's turn.
	runs map[string]string
	// tasks maps a task id to its parent task id ("" = a root task).
	// Task lifecycle events route through it to the root foreground
	// turn; walking it folds child-task events into the root turn.
	tasks map[string]string
	// turns holds one entry per materialized turn, keyed by TurnID.
	turns map[turns.TurnID]*turnState
}

// turnState is the materializer's per-turn derived working state: the
// accumulators the projector row cannot retain (bounded windows) plus
// the routing/consistency markers. The projector row is authoritative
// for everything the row carries; this state is the FULL cumulative
// feed that keeps the row's positions, totals, and snapshots stable
// across incremental updates and restarts.
type turnState struct {
	// taskID is the root foreground task id (the row key).
	taskID string
	// runID is the root run id (empty = unavailable, never equated
	// with taskID).
	runID string
	// sealed marks the row as sealed (terminal): no further mutation is
	// possible, so later events for the turn are skipped (the turn has
	// converged; the projector refuses writes to sealed rows).
	sealed bool
	// pendingComplete records that a `task.completed` was observed but
	// the complete seal was refused because the answer source had not
	// converged (no answer-carrying canonical event was seen). The
	// materializer retries the seal at the end of every materialize
	// pass so a late-arriving answer source converges the row without
	// any manual rebuild.
	pendingComplete bool
	// activity is the FULL cumulative content-free tool-dispatch feed
	// for the turn, oldest first. The projector's inline window is
	// bounded, but the exact turn-level totals and the per-row
	// immutable positions depend on the full feed, so the materializer
	// retains it and feeds it wholesale on every activity-affecting
	// update.
	activity []turns.ActivityRow
	// usage is the cumulative per-measure usage accumulator fed to the
	// projector wholesale on every usage-affecting update.
	usage turns.Usage
	// reasoning is the FULL ordered derived reasoning feed (index +
	// closed kind), fed wholesale on every planner-decision attach.
	reasoning []turns.ReasoningStep
	// nextReasoningIndex is the next chronological reasoning index to
	// stamp (strictly increasing, gap-tolerant).
	nextReasoningIndex int
	// inputs is the accumulated input attachment metadata list (from
	// task.input_disposition.resolved), deduplicated by artifact id.
	inputs []turns.Attachment
}

// newSessionState creates the per-session state and seeds the
// checkpoint mirror from the projector store.
func (m *Materializer) newSessionState(ctx context.Context, id identity.Identity) (*sessionState, error) {
	cp, err := m.proj.Checkpoint(ctx, id)
	if err != nil {
		return nil, err
	}
	return &sessionState{
		id:         id,
		checkpoint: cp,
		runs:       map[string]string{},
		tasks:      map[string]string{},
		turns:      map[turns.TurnID]*turnState{},
	}, nil
}

// rootTurn resolves the turn an event with the given run id folds
// into: walk run → task → parent chain to the root foreground task.
// ok=false when the run is unknown or the chain is malformed (a cycle
// is impossible — task ids are ULIDs and parents are always spawned
// before children — but the walk is bounded defensively).
func (s *sessionState) rootTurn(runID string) (turns.TurnID, *turnState, bool) {
	taskID, ok := s.runs[runID]
	if !ok {
		return "", nil, false
	}
	for depth := 0; depth < 128; depth++ {
		parent, hasParent := s.tasks[taskID]
		if !hasParent {
			// The task is not registered in this session's index (a
			// legacy event or a task whose spawn was never observed) —
			// it cannot be routed.
			return "", nil, false
		}
		if parent == "" {
			// Root task: its turn, if materialized.
			ts, ok := s.turns[turns.TurnID(taskID)]
			if !ok {
				return "", nil, false
			}
			return turns.TurnID(taskID), ts, true
		}
		taskID = parent
	}
	return "", nil, false
}

// taskTurn resolves the turn a task lifecycle event (which names its
// TaskID in the payload) belongs to: walk the task → parent chain to
// the root foreground task. This is the task-event twin of rootTurn.
func (s *sessionState) taskTurn(taskID string) (turns.TurnID, *turnState, bool) {
	for depth := 0; depth < 128; depth++ {
		parent, hasParent := s.tasks[taskID]
		if !hasParent {
			return "", nil, false
		}
		if parent == "" {
			ts, ok := s.turns[turns.TurnID(taskID)]
			if !ok {
				return "", nil, false
			}
			return turns.TurnID(taskID), ts, true
		}
		taskID = parent
	}
	return "", nil, false
}

// splitIdentity validates the envelope triple and returns the session
// identity plus a presence flag. The materializer never fabricates an
// identity: an event with an incomplete triple is skipped (fail
// closed, CLAUDE.md §6).
func splitIdentity(q identity.Quadruple) (identity.Identity, bool) {
	id := identity.Identity{TenantID: q.TenantID, UserID: q.UserID, SessionID: q.SessionID}
	if id.TenantID == "" || id.UserID == "" || id.SessionID == "" {
		return identity.Identity{}, false
	}
	return id, true
}

// runIDFromIdentity reads the run id off the envelope quadruple — the
// per-execution scope inside the session. Empty means the run id is
// unavailable (never equated with a task id).
func runIDFromIdentity(q identity.Quadruple) string { return q.RunID }

// hasPipe reports whether s contains the reserved cursor separator.
// Turn ids ride the opaque page cursor encoding; a pipe would make the
// row's own cursor undecodable. The projector already refuses such ids
// at Append — this is the routing-side mirror so a malformed task id
// never even reaches the projector.
func hasPipe(s string) bool { return containsRune(s, '|') }

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}
