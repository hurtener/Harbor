// Package inprocess is Harbor's default in-process TaskRegistry
// driver. It wraps the shared task state machine in
// internal/tasks/engine with an ephemeral persistence backend: every
// lifecycle transition is written through the configured
// state.StateStore (so live state is observable to a co-located
// reader), but records are NOT reloaded on open — a process restart
// starts with an empty registry. Operators who need task records to
// survive a restart select the durable driver
// (internal/tasks/drivers/durable) instead.
//
// The driver is the reference the conformance suite runs against; the
// engine it wraps is the same one every other driver wraps, so all
// drivers share one verified lifecycle implementation.
package inprocess

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tasks/engine"
)

// New constructs the in-process TaskRegistry. Production callers go
// through tasks.Open; the constructor is exported for tests that want
// to skip the registry dispatch.
//
// A non-nil StateStore is required: the ephemeral backend writes every
// transition through it. Bus and Redactor are validated by the engine.
func New(deps tasks.Dependencies) (tasks.TaskRegistry, error) {
	if deps.Store == nil {
		return nil, fmt.Errorf("tasks/inprocess: New requires a non-nil StateStore")
	}
	return engine.New(deps.Bus, deps.Redactor, ephemeralBackend{store: deps.Store})
}

func init() {
	tasks.Register("inprocess", New)
}

// ephemeralBackend persists task / group / patch records through a
// state.StateStore for live observability but never reloads them:
// Hydrate returns an empty Snapshot, so a fresh driver starts empty.
//
// Each record kind writes to a single fixed Kind slot per identity
// (tasks.LifecycleKind / GroupKind / PatchKind), so successive saves
// overwrite — the ephemeral backend keeps no per-record history. The
// durable driver uses per-record keys precisely so its records survive
// and can be replayed; the in-process driver deliberately does not.
type ephemeralBackend struct {
	store state.StateStore
}

var _ engine.Backend = ephemeralBackend{}

func (b ephemeralBackend) SaveTask(ctx context.Context, rec engine.TaskRecord) error {
	bytesPayload, err := json.Marshal(rec.Task)
	if err != nil {
		return fmt.Errorf("tasks/inprocess: marshal task: %w: %w", tasks.ErrUnserializable, err)
	}
	if err := b.store.Save(ctx, state.StateRecord{
		ID:       state.NewEventID(),
		Identity: rec.Task.Identity,
		Kind:     tasks.LifecycleKind,
		Bytes:    bytesPayload,
	}); err != nil {
		return fmt.Errorf("tasks/inprocess: state save (task): %w", err)
	}
	return nil
}

func (b ephemeralBackend) SaveGroup(ctx context.Context, g *tasks.TaskGroup) error {
	bytesPayload, err := json.Marshal(g)
	if err != nil {
		return fmt.Errorf("tasks/inprocess: marshal group: %w: %w", tasks.ErrUnserializable, err)
	}
	if err := b.store.Save(ctx, state.StateRecord{
		ID:       state.NewEventID(),
		Identity: identity.Quadruple{Identity: g.SessionID},
		Kind:     tasks.GroupKind,
		Bytes:    bytesPayload,
	}); err != nil {
		return fmt.Errorf("tasks/inprocess: state save (group): %w", err)
	}
	return nil
}

func (b ephemeralBackend) SavePatch(ctx context.Context, p *tasks.Patch) error {
	bytesPayload, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("tasks/inprocess: marshal patch: %w: %w", tasks.ErrUnserializable, err)
	}
	if err := b.store.Save(ctx, state.StateRecord{
		ID:       state.NewEventID(),
		Identity: identity.Quadruple{Identity: p.SessionID},
		Kind:     tasks.PatchKind,
		Bytes:    bytesPayload,
	}); err != nil {
		return fmt.Errorf("tasks/inprocess: state save (patch): %w", err)
	}
	return nil
}

// DeleteTask is a no-op: the ephemeral backend never reloads records,
// so there is nothing to compensate for a rolled-back spawn. (It also
// cannot safely delete the single shared task.lifecycle slot, which by
// then may hold a different task's last write.)
func (b ephemeralBackend) DeleteTask(_ context.Context, _ *tasks.Task) error {
	return nil
}

// Hydrate returns an empty Snapshot: the in-process driver never
// reloads records, so a restart starts with an empty registry.
func (b ephemeralBackend) Hydrate(_ context.Context) (engine.Snapshot, error) {
	return engine.Snapshot{}, nil
}
