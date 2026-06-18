package durable

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tasks/engine"
)

// StateStore Kind prefixes for the durable task driver. Each record
// gets its own slot, keyed by the record's own ID, so a session's
// many tasks/groups/patches do not clobber one another (the contrast
// with the in-process driver's single fixed-Kind slot per identity).
// The prefixes are disjoint from the durable event log
// (events.durable.*) and the in-process driver's task.lifecycle /
// task.group / task.patch Kinds, so the three coexist in one shared
// StateStore without collision.
const (
	taskKindPrefix  = "task.durable.task/"
	groupKindPrefix = "task.durable.group/"
	patchKindPrefix = "task.durable.patch/"
)

// persistedTask is the durable wire shape for a task slot: the task
// record plus the engine's idempotency content hash (hex-encoded).
// The hash is carried explicitly because it is computed over the
// PRE-redaction task content and cannot be re-derived from the
// post-redaction fields the task itself stores.
type persistedTask struct {
	Task        *tasks.Task `json:"task"`
	ContentHash string      `json:"content_hash,omitempty"`
}

// backend is the StateStore-backed engine.Backend. Records survive a
// restart; Hydrate replays them on open.
type backend struct {
	store state.StateStore
}

var _ engine.Backend = (*backend)(nil)

// SaveTask persists a task to its own per-ID slot. A marshal failure
// is raised loudly as tasks.ErrUnserializable (never a silent drop).
func (b *backend) SaveTask(ctx context.Context, rec engine.TaskRecord) error {
	if rec.Task == nil {
		return fmt.Errorf("tasks/durable: SaveTask received a nil task")
	}
	pt := persistedTask{
		Task:        rec.Task,
		ContentHash: hex.EncodeToString(rec.ContentHash[:]),
	}
	payload, err := json.Marshal(pt)
	if err != nil {
		return fmt.Errorf("tasks/durable: marshal task: %w: %w", tasks.ErrUnserializable, err)
	}
	return b.save(ctx, sessionScope(rec.Task.Identity), taskKindPrefix+string(rec.Task.ID), payload)
}

// SaveGroup persists a group to its own per-ID slot.
func (b *backend) SaveGroup(ctx context.Context, g *tasks.TaskGroup) error {
	if g == nil {
		return fmt.Errorf("tasks/durable: SaveGroup received a nil group")
	}
	payload, err := json.Marshal(g)
	if err != nil {
		return fmt.Errorf("tasks/durable: marshal group: %w: %w", tasks.ErrUnserializable, err)
	}
	return b.save(ctx, identity.Quadruple{Identity: g.SessionID}, groupKindPrefix+string(g.ID), payload)
}

// SavePatch persists a patch to its own per-ID slot. The patch ID is
// session-scoped, and the storage Identity carries the session, so two
// sessions reusing one patch ID land in distinct slots.
func (b *backend) SavePatch(ctx context.Context, p *tasks.Patch) error {
	if p == nil {
		return fmt.Errorf("tasks/durable: SavePatch received a nil patch")
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("tasks/durable: marshal patch: %w: %w", tasks.ErrUnserializable, err)
	}
	return b.save(ctx, identity.Quadruple{Identity: p.SessionID}, patchKindPrefix+p.ID, payload)
}

// DeleteTask removes a task's per-ID slot. Used to compensate a spawn
// whose group wiring failed after the task was already persisted, so a
// restart does not resurrect it. Deleting an absent slot is not an
// error (the StateStore Delete contract is idempotent).
func (b *backend) DeleteTask(ctx context.Context, t *tasks.Task) error {
	if t == nil {
		return fmt.Errorf("tasks/durable: DeleteTask received a nil task")
	}
	if err := b.store.Delete(ctx, sessionScope(t.Identity), taskKindPrefix+string(t.ID)); err != nil {
		return fmt.Errorf("tasks/durable: state delete (task %s): %w", t.ID, err)
	}
	return nil
}

// save writes one record under a fresh EventID so the (Identity, Kind)
// slot is overwritten with the latest state on every call.
func (b *backend) save(ctx context.Context, scope identity.Quadruple, kind string, payload []byte) error {
	if err := b.store.Save(ctx, state.StateRecord{
		ID:       state.NewEventID(),
		Identity: scope,
		Kind:     kind,
		Bytes:    payload,
	}); err != nil {
		return fmt.Errorf("tasks/durable: state save (%s): %w", kind, err)
	}
	return nil
}

// Hydrate replays every persisted task / group / patch so the engine
// can rebuild its in-memory state on open. It scans by Kind prefix
// across identities (a boot-time maintenance scan); each record's
// bytes carry its own full identity, so cross-identity scanning never
// leaks one tenant's records into another's scope — the engine keys
// everything by the record's own identity.
func (b *backend) Hydrate(ctx context.Context) (engine.Snapshot, error) {
	var snap engine.Snapshot
	scope := state.ListScope{MaintenanceScoped: true}

	taskRecs, err := b.store.ListKind(ctx, scope, taskKindPrefix)
	if err != nil {
		return engine.Snapshot{}, fmt.Errorf("tasks/durable: list task records: %w", err)
	}
	for _, rec := range taskRecs {
		var pt persistedTask
		if uerr := json.Unmarshal(rec.Bytes, &pt); uerr != nil {
			return engine.Snapshot{}, fmt.Errorf("tasks/durable: unmarshal task record %q: %w", rec.Kind, uerr)
		}
		if pt.Task == nil {
			continue
		}
		var hash [32]byte
		if pt.ContentHash != "" {
			decoded, derr := hex.DecodeString(pt.ContentHash)
			if derr != nil {
				return engine.Snapshot{}, fmt.Errorf("tasks/durable: decode content hash for %q: %w", rec.Kind, derr)
			}
			copy(hash[:], decoded)
		}
		snap.Tasks = append(snap.Tasks, engine.TaskRecord{Task: pt.Task, ContentHash: hash})
	}

	groupRecs, err := b.store.ListKind(ctx, scope, groupKindPrefix)
	if err != nil {
		return engine.Snapshot{}, fmt.Errorf("tasks/durable: list group records: %w", err)
	}
	for _, rec := range groupRecs {
		var g tasks.TaskGroup
		if uerr := json.Unmarshal(rec.Bytes, &g); uerr != nil {
			return engine.Snapshot{}, fmt.Errorf("tasks/durable: unmarshal group record %q: %w", rec.Kind, uerr)
		}
		gCopy := g
		snap.Groups = append(snap.Groups, &gCopy)
	}

	patchRecs, err := b.store.ListKind(ctx, scope, patchKindPrefix)
	if err != nil {
		return engine.Snapshot{}, fmt.Errorf("tasks/durable: list patch records: %w", err)
	}
	for _, rec := range patchRecs {
		var p tasks.Patch
		if uerr := json.Unmarshal(rec.Bytes, &p); uerr != nil {
			return engine.Snapshot{}, fmt.Errorf("tasks/durable: unmarshal patch record %q: %w", rec.Kind, uerr)
		}
		pCopy := p
		snap.Patches = append(snap.Patches, &pCopy)
	}

	return snap, nil
}

// sessionScope drops the RunID so a task's storage slot is scoped to
// its session triple. The full identity (including RunID) is preserved
// in the persisted bytes; this only governs the StateStore key.
func sessionScope(q identity.Quadruple) identity.Quadruple {
	return identity.Quadruple{Identity: q.Identity}
}
