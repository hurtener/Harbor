package inmem

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
)

// WithClock injects the clock the single-writer lease expiry is
// evaluated against (default time.Now). Tests use it to expire a lease
// deterministically.
func WithClock(clock func() time.Time) Option {
	return func(o *options) { o.clock = clock }
}

// leaseState is the in-memory single-writer lease + resume cursor. Like
// every in-memory record it is lost on restart (the projection then
// rebuilds from sequence zero).
type leaseState struct {
	holder     string
	expires    time.Time
	cursor     uint64
	generation uint64
}

// fenceLocked enforces the lease write fence ctx carries (if any).
// Callers hold d.mu.
func (d *driver) fenceLocked(ctx context.Context) error {
	if f, ok := turns.WriteFenceFrom(ctx); ok && (d.lease.holder != f.Holder || d.lease.generation != f.Generation) {
		return turns.ErrLeaseNotHeld
	}
	return nil
}

// Compile-time assertion that *driver satisfies turns.MaterializerStore.
var _ turns.MaterializerStore = (*driver)(nil)

func (d *driver) guard(ctx context.Context) error {
	if err := d.closedErr(); err != nil {
		return err
	}
	return ctx.Err()
}

// AcquireMaterializerLease implements turns.MaterializerStore.
func (d *driver) AcquireMaterializerLease(ctx context.Context, holder string, ttl time.Duration) (turns.MaterializerLease, error) {
	if err := d.guard(ctx); err != nil {
		return turns.MaterializerLease{}, err
	}
	if holder == "" || ttl <= 0 {
		return turns.MaterializerLease{}, fmt.Errorf("%w: lease needs a holder and a positive ttl", turns.ErrInvalidInput)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.clock()
	if d.lease.holder == "" || d.lease.holder == holder || !now.Before(d.lease.expires) {
		if d.lease.holder != holder {
			d.lease.generation++ // the lease changes hands
		}
		d.lease.holder = holder
		d.lease.expires = now.Add(ttl)
	}
	return turns.MaterializerLease{Held: d.lease.holder == holder, Holder: d.lease.holder, Generation: d.lease.generation, Cursor: d.lease.cursor}, nil
}

// ReleaseMaterializerLease implements turns.MaterializerStore.
func (d *driver) ReleaseMaterializerLease(ctx context.Context, holder string) error {
	if err := d.guard(ctx); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lease.holder == holder {
		d.lease.holder = ""
		d.lease.expires = time.Time{}
	}
	return nil
}

// SaveMaterializerCursor implements turns.MaterializerStore.
func (d *driver) SaveMaterializerCursor(ctx context.Context, holder string, cursor uint64) error {
	if err := d.guard(ctx); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lease.holder != holder || !d.clock().Before(d.lease.expires) {
		return turns.ErrLeaseNotHeld
	}
	if f, ok := turns.WriteFenceFrom(ctx); ok && (f.Holder != holder || f.Generation != d.lease.generation) {
		return turns.ErrLeaseNotHeld
	}
	if cursor > d.lease.cursor {
		d.lease.cursor = cursor
	}
	return nil
}

// ListIdentityRepairCandidates implements turns.MaterializerStore.
func (d *driver) ListIdentityRepairCandidates(ctx context.Context, after turns.IdentityRepairKey, limit int) ([]turns.IdentityRepairKey, error) {
	return d.listFlagged(ctx, d.repair, after, limit)
}

// ListPendingCompletions implements turns.MaterializerStore.
func (d *driver) ListPendingCompletions(ctx context.Context, after turns.IdentityRepairKey, limit int) ([]turns.IdentityRepairKey, error) {
	return d.listFlagged(ctx, d.pending, after, limit)
}

func (d *driver) listFlagged(ctx context.Context, flags map[rowKey]bool, after turns.IdentityRepairKey, limit int) ([]turns.IdentityRepairKey, error) {
	if err := d.guard(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, nil
	}
	d.mu.RLock()
	keys := make([]turns.IdentityRepairKey, 0, len(flags))
	for k := range flags {
		if _, ok := d.rows[k]; !ok {
			continue
		}
		key := turns.IdentityRepairKey{
			Identity: identity.Identity{TenantID: k.tenant, UserID: k.user, SessionID: k.session},
			TurnID:   turns.TurnID(k.turnID),
		}
		if after.Less(key) {
			keys = append(keys, key)
		}
	}
	d.mu.RUnlock()
	sort.Slice(keys, func(i, j int) bool { return keys[i].Less(keys[j]) })
	if len(keys) > limit {
		keys = keys[:limit]
	}
	return keys, nil
}

// MarkPendingCompletion implements turns.MaterializerStore.
func (d *driver) MarkPendingCompletion(ctx context.Context, id identity.Identity, turnID turns.TurnID) error {
	if err := d.guard(ctx); err != nil {
		return err
	}
	if err := d.identityErr(id); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.fenceLocked(ctx); err != nil {
		return err
	}
	if st := d.stateRead(id); st != nil && st.fenced {
		return turns.ErrErasureFenced
	}
	k := rowKeyOf(id, turnID)
	row, ok := d.rows[k]
	if !ok {
		return fmt.Errorf("%w: %q", turns.ErrTurnNotFound, turnID)
	}
	if !row.Sealed {
		d.pending[k] = true
	}
	return nil
}

// RepairTurnIdentity implements turns.MaterializerStore.
func (d *driver) RepairTurnIdentity(ctx context.Context, id identity.Identity, turnID turns.TurnID, fill turns.IdentityFill) (turns.TurnRow, bool, error) {
	if err := d.guard(ctx); err != nil {
		return turns.TurnRow{}, false, err
	}
	if err := d.identityErr(id); err != nil {
		return turns.TurnRow{}, false, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.fenceLocked(ctx); err != nil {
		return turns.TurnRow{}, false, err
	}
	if st := d.stateRead(id); st != nil && st.fenced {
		return turns.TurnRow{}, false, turns.ErrErasureFenced
	}
	k := rowKeyOf(id, turnID)
	cur, ok := d.rows[k]
	if !ok {
		return turns.TurnRow{}, false, fmt.Errorf("%w: %q", turns.ErrTurnNotFound, turnID)
	}
	next, changed, settled, err := turns.ApplyIdentityFill(cloneRow(cur), fill)
	if err != nil {
		return turns.TurnRow{}, false, err
	}
	if changed {
		next.Version = cur.Version + 1
		d.rows[k] = cloneRow(next)
	}
	if settled {
		delete(d.repair, k)
	}
	return cloneRow(d.rows[k]), changed, nil
}

// SaveTaskRoute implements turns.MaterializerStore.
func (d *driver) SaveTaskRoute(ctx context.Context, id identity.Identity, r turns.TaskRoute) error {
	if err := d.guard(ctx); err != nil {
		return err
	}
	if err := d.identityErr(id); err != nil {
		return err
	}
	if r.TaskID == "" {
		return fmt.Errorf("%w: route task id is empty", turns.ErrInvalidInput)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.fenceLocked(ctx); err != nil {
		return err
	}
	if st := d.stateRead(id); st != nil && st.fenced {
		return turns.ErrErasureFenced
	}
	k := rowKeyOf(id, turns.TurnID(r.TaskID))
	if old, ok := d.routes[k]; ok {
		if r.RunID == "" {
			r.RunID = old.RunID
		}
		if r.RootTaskID == "" {
			r.RootTaskID = old.RootTaskID
		}
		r.EventSeq = max(r.EventSeq, old.EventSeq)
	}
	d.routes[k] = r
	return nil
}

// LoadTaskRoute implements turns.MaterializerStore.
func (d *driver) LoadTaskRoute(ctx context.Context, id identity.Identity, taskID string) (turns.TaskRoute, bool, error) {
	if err := d.guard(ctx); err != nil {
		return turns.TaskRoute{}, false, err
	}
	if err := d.identityErr(id); err != nil {
		return turns.TaskRoute{}, false, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	r, ok := d.routes[rowKeyOf(id, turns.TurnID(taskID))]
	return r, ok, nil
}

// LoadRunRoute implements turns.MaterializerStore.
func (d *driver) LoadRunRoute(ctx context.Context, id identity.Identity, runID string) (turns.TaskRoute, bool, error) {
	if err := d.guard(ctx); err != nil {
		return turns.TaskRoute{}, false, err
	}
	if err := d.identityErr(id); err != nil {
		return turns.TaskRoute{}, false, err
	}
	if runID == "" {
		return turns.TaskRoute{}, false, nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	sk := sessionKeyOf(id)
	var best turns.TaskRoute
	found := false
	for rk, r := range d.routes {
		if rk.tenant == sk.tenant && rk.user == sk.user && rk.session == sk.session && r.RunID == runID && (!found || r.EventSeq > best.EventSeq) {
			best, found = r, true
		}
	}
	return best, found, nil
}
