// Package inmem is Harbor's V1 in-memory StateStore driver. It is
// the test reference for the conformance suite — every later driver
// (SQLite Phase 15, Postgres Phase 16, durable-log Phase 57) inherits
// the same suite verbatim.
//
// Internal model:
//
//   - A primary map keyed on (Quadruple, Kind) holds the active
//     record per slot. A secondary map keyed on EventID resolves
//     idempotency lookups and `LoadByEventID`.
//   - A single `sync.RWMutex` guards both maps. The driver does no
//     I/O so contention is bounded by Go's map throughput; a
//     finer-grained lock structure would be premature.
//   - `Bytes` is deep-copied on Save and on Load to defend against
//     callers mutating the slice they passed in (or the slice they
//     received). Future SQL drivers naturally avoid this issue
//     (rows are independent of the caller's slice).
//   - `Close(ctx)` flips an atomic flag; subsequent calls return
//     `ErrStoreClosed`. There are no driver-owned goroutines to
//     join, so Close is fast.
package inmem

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// New constructs a StateStore directly. Exposed for tests that want
// to skip the registry; production callers go through `state.Open`.
func New(_ config.StateConfig) (state.StateStore, error) {
	return &driver{
		records:  map[indexKey]state.StateRecord{},
		eventIdx: map[state.EventID]indexKey{},
	}, nil
}

func init() {
	state.Register("inmem", func(cfg config.StateConfig) (state.StateStore, error) {
		return New(cfg)
	})
}

// indexKey is the composite primary key. Struct-typed (rather than
// string-concatenated) so tenant IDs containing delimiters can't
// collide.
type indexKey struct {
	Tenant  string
	User    string
	Session string
	Run     string
	Kind    string
}

func keyFor(q identity.Quadruple, kind string) indexKey {
	return indexKey{
		Tenant:  q.TenantID,
		User:    q.UserID,
		Session: q.SessionID,
		Run:     q.RunID,
		Kind:    kind,
	}
}

type driver struct {
	mu       sync.RWMutex
	records  map[indexKey]state.StateRecord
	eventIdx map[state.EventID]indexKey
	closed   atomic.Bool
}

// Save implements state.StateStore.
//
// Idempotency:
//
//  1. If the EventID was seen before AND the previous record's
//     (Identity, Kind) AND Bytes match the new request: no-op.
//  2. If the EventID was seen before AND anything else differs:
//     ErrIdempotencyConflict.
//  3. Else: insert/update the record at (Identity, Kind); update
//     the EventID secondary index. If a previous record at
//     (Identity, Kind) existed under a DIFFERENT EventID, the old
//     EventID is removed from the secondary index.
func (d *driver) Save(_ context.Context, r state.StateRecord) error {
	if d.closed.Load() {
		return state.ErrStoreClosed
	}
	if err := state.ValidateRecord(r); err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	key := keyFor(r.Identity, r.Kind)

	// Idempotency check: same EventID seen before?
	if prevKey, seen := d.eventIdx[r.ID]; seen {
		prev := d.records[prevKey]
		if prevKey != key {
			return fmt.Errorf("%w: EventID %q already routes to a different (Quadruple, Kind)",
				state.ErrIdempotencyConflict, r.ID)
		}
		if !bytes.Equal(prev.Bytes, r.Bytes) {
			return fmt.Errorf("%w: EventID %q already saved with different Bytes",
				state.ErrIdempotencyConflict, r.ID)
		}
		if prev.Version != r.Version {
			return fmt.Errorf("%w: EventID %q already saved with different Version",
				state.ErrIdempotencyConflict, r.ID)
		}
		// Idempotent no-op.
		return nil
	}

	// New EventID. If a record already exists at this slot under a
	// different EventID, evict the old EventID from the secondary
	// index (the slot now belongs to the new EventID).
	if existing, ok := d.records[key]; ok {
		if existing.ID != r.ID {
			delete(d.eventIdx, existing.ID)
		}
	}

	stored := r
	stored.Bytes = cloneBytes(r.Bytes)
	if stored.UpdatedAt.IsZero() {
		stored.UpdatedAt = time.Now()
	}
	d.records[key] = stored
	d.eventIdx[r.ID] = key
	return nil
}

// Load implements state.StateStore.
func (d *driver) Load(_ context.Context, q identity.Quadruple, kind string) (state.StateRecord, error) {
	if d.closed.Load() {
		return state.StateRecord{}, state.ErrStoreClosed
	}
	if err := state.ValidateIdentity(q); err != nil {
		return state.StateRecord{}, err
	}
	if kind == "" {
		return state.StateRecord{}, state.ErrInvalidRecord
	}

	d.mu.RLock()
	defer d.mu.RUnlock()
	rec, ok := d.records[keyFor(q, kind)]
	if !ok {
		return state.StateRecord{}, fmt.Errorf("%w: %s/%s/%s/%s kind=%s",
			state.ErrNotFound, q.TenantID, q.UserID, q.SessionID, q.RunID, kind)
	}
	rec.Bytes = cloneBytes(rec.Bytes)
	return rec, nil
}

// LoadByEventID implements state.StateStore.
func (d *driver) LoadByEventID(_ context.Context, eventID state.EventID) (state.StateRecord, error) {
	if d.closed.Load() {
		return state.StateRecord{}, state.ErrStoreClosed
	}
	if eventID == "" {
		return state.StateRecord{}, state.ErrInvalidRecord
	}

	d.mu.RLock()
	defer d.mu.RUnlock()
	key, ok := d.eventIdx[eventID]
	if !ok {
		return state.StateRecord{}, fmt.Errorf("%w: event_id=%s", state.ErrNotFound, eventID)
	}
	rec, ok := d.records[key]
	if !ok {
		// Secondary points at a slot with no primary record — a
		// driver bug. Surface it loudly.
		return state.StateRecord{}, fmt.Errorf("%w: secondary index points at missing slot for event_id=%s",
			state.ErrNotFound, eventID)
	}
	rec.Bytes = cloneBytes(rec.Bytes)
	return rec, nil
}

// Delete implements state.StateStore.
func (d *driver) Delete(_ context.Context, q identity.Quadruple, kind string) error {
	if d.closed.Load() {
		return state.ErrStoreClosed
	}
	if err := state.ValidateIdentity(q); err != nil {
		return err
	}
	if kind == "" {
		return state.ErrInvalidRecord
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	key := keyFor(q, kind)
	rec, ok := d.records[key]
	if !ok {
		return nil // idempotent
	}
	delete(d.records, key)
	delete(d.eventIdx, rec.ID)
	return nil
}

// Close implements state.StateStore. Idempotent.
func (d *driver) Close(_ context.Context) error {
	d.closed.Store(true)
	return nil
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// Compile-time assertion that driver satisfies state.StateStore.
var _ state.StateStore = (*driver)(nil)
