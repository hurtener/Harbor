package engine

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"
)

// Backend is the persistence seam the engine writes through. It is
// the ONLY difference between TaskRegistry drivers: the engine owns
// the lifecycle state machine; a Backend owns where records live and
// whether they survive a restart.
//
// Two implementations ship:
//
//   - the in-process driver's ephemeral backend, whose Hydrate
//     returns an empty Snapshot (records never reload across a
//     restart); and
//   - the durable driver's StateStore-backed backend, whose records
//     are keyed per-record and whose Hydrate replays them on open.
//
// Contract:
//
//   - Save* MUST be idempotent on the record's identity: a re-save of
//     the same task/group/patch overwrites in place (the engine
//     re-persists the full record on every mutation, and the recovery
//     sweep re-persists a recovered record). A backend that appended
//     instead of overwriting would accumulate stale duplicates.
//   - Save* MAY block on I/O. The engine calls Save* while holding its
//     lock, so a slow backend serializes mutations on that engine
//     instance — acceptable for single-instance durability.
//   - A Save* that cannot serialize a record (e.g. a non-serializable
//     payload) MUST return an error, never silently drop it
//     (CLAUDE.md §5 fail-loud).
//   - Hydrate is called exactly once, from [New], before the engine is
//     handed to any caller. It returns every persisted record so the
//     engine can rebuild its indices. An error fails New loudly.
type Backend interface {
	// SaveTask persists a task record (the task plus the engine's
	// idempotency content hash, which is not recoverable from the
	// post-redaction task alone).
	SaveTask(ctx context.Context, rec TaskRecord) error
	// DeleteTask removes a task's persisted record. The engine calls it
	// to COMPENSATE a half-completed spawn (a task persisted before its
	// group wiring failed), so a restart never resurrects a spawn the
	// caller was told failed. A backend that never reloads (ephemeral)
	// may no-op. Deleting an absent record is not an error.
	DeleteTask(ctx context.Context, t *tasks.Task) error
	// SaveGroup persists a task-group record.
	SaveGroup(ctx context.Context, g *tasks.TaskGroup) error
	// SavePatch persists a patch record.
	SavePatch(ctx context.Context, p *tasks.Patch) error
	// Hydrate returns every persisted record so [New] can rebuild the
	// engine's in-memory state. An ephemeral backend returns an empty
	// Snapshot.
	Hydrate(ctx context.Context) (Snapshot, error)
}

// TaskRecord is the durable unit for a task: the task itself plus the
// engine's idempotency content hash. The hash is the SHA-256 of the
// task's PRE-redaction content (see spawnRequestContentHash); it is
// carried out-of-band because it cannot be re-derived from the
// post-redaction Task fields a backend would otherwise persist.
type TaskRecord struct {
	Task        *tasks.Task
	ContentHash [32]byte
}

// Snapshot is the full set of persisted records a Backend returns from
// Hydrate. The engine rebuilds its task / group / patch maps and the
// derived indices (idempotency, children, member→group) from it.
type Snapshot struct {
	Tasks   []TaskRecord
	Groups  []*tasks.TaskGroup
	Patches []*tasks.Patch
}

// hydrate rebuilds the engine's in-memory state from the backend's
// persisted records. Called once from New, before the engine is
// shared, so it runs without the lock (no concurrent access is
// possible yet).
//
// Tasks are loaded first so the group-completion cache rebuild below
// can read member outcomes from e.tasks.
func (e *Engine) hydrate(ctx context.Context) error {
	snap, err := e.backend.Hydrate(ctx)
	if err != nil {
		return fmt.Errorf("tasks/engine: hydrate: %w", err)
	}

	for _, rec := range snap.Tasks {
		t := rec.Task
		if t == nil {
			continue
		}
		e.tasks[t.ID] = t
		if t.IdempotencyKey != "" {
			e.idemIdx[idemKeyFor(t.Identity.Identity, t.IdempotencyKey)] = idempotencyRecord{
				TaskID:      t.ID,
				ContentHash: rec.ContentHash,
			}
		}
		if t.ParentTaskID != nil && *t.ParentTaskID != "" {
			e.children[*t.ParentTaskID] = append(e.children[*t.ParentTaskID], t.ID)
		}
		// Seed the progress rate-window baseline from the persisted
		// snapshot so a restarted engine does not treat the first
		// post-restart message-only update as "outside the window".
		// Best-effort by design: a snapshot that was durably recorded
		// but coalesced (never published) right before the restart is
		// treated as emitted, which can at most suppress one message-
		// only publication — a real phase/fraction change always
		// publishes.
		if t.Progress != nil {
			e.lastProgressEmit[t.ID] = t.Progress.ReportedAt
		}
	}

	for _, g := range snap.Groups {
		if g == nil {
			continue
		}
		e.groups[g.ID] = g
		for _, mid := range g.Members {
			e.taskGroup[mid] = g.ID
		}
		// Rebuild the completion cache for already-resolved groups so a
		// post-restart WatchGroup late-subscriber still receives a primed
		// channel. Reason is not persisted on the group, so a recovered
		// completion for a previously-cancelled group carries an empty
		// Reason — a documented, best-effort fidelity gap.
		if isGroupTerminal(g.Status) {
			resolvedAt := g.UpdatedAt
			if g.ResolvedAt != nil {
				resolvedAt = *g.ResolvedAt
			}
			e.groupCompletions[g.ID] = tasks.GroupCompletion{
				GroupID:     g.ID,
				SessionID:   g.SessionID,
				OwnerTaskID: g.OwnerTaskID,
				FinalStatus: g.Status,
				ResolvedAt:  resolvedAt,
				Members:     e.collectMemberOutcomesLocked(g),
			}
		}
	}

	for _, p := range snap.Patches {
		if p == nil {
			continue
		}
		e.patches[patchKey{SessionID: p.SessionID.SessionID, PatchID: p.ID}] = p
	}

	// Replay progress publications that were durably staged before a publish
	// failure. The exact update id is retained in the payload, so recovery never
	// substitutes a newer snapshot or silently treats the event as acknowledged.
	for _, rec := range snap.Tasks {
		if rec.Task == nil || rec.Task.PendingProgress == nil {
			continue
		}
		idctx, err := identity.With(ctx, rec.Task.Identity.Identity)
		if err != nil {
			return fmt.Errorf("tasks/engine: progress replay identity: %w", err)
		}
		if err := e.publish(idctx, rec.Task, tasks.EventTypeTaskProgress, *rec.Task.PendingProgress); err != nil {
			return fmt.Errorf("tasks/engine: replay progress %q: %w", rec.Task.ID, err)
		}
		rec.Task.PendingProgress = nil
		if err := e.backend.SaveTask(idctx, rec); err != nil {
			return fmt.Errorf("tasks/engine: acknowledge progress %q: %w", rec.Task.ID, err)
		}
		if rec.Task.Progress != nil {
			e.lastProgressEmit[rec.Task.ID] = rec.Task.Progress.ReportedAt
		}
	}

	return nil
}
