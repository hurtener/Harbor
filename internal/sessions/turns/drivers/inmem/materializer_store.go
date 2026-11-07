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
	holder  string
	expires time.Time
	cursor  uint64
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
		d.lease.holder = holder
		d.lease.expires = now.Add(ttl)
	}
	return turns.MaterializerLease{Held: d.lease.holder == holder, Holder: d.lease.holder, Cursor: d.lease.cursor}, nil
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
	if cursor > d.lease.cursor {
		d.lease.cursor = cursor
	}
	return nil
}

// ListIdentityRepairCandidates implements turns.MaterializerStore.
func (d *driver) ListIdentityRepairCandidates(ctx context.Context, after turns.IdentityRepairKey, limit int) ([]turns.IdentityRepairKey, error) {
	if err := d.guard(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, nil
	}
	d.mu.RLock()
	keys := make([]turns.IdentityRepairKey, 0, len(d.repair))
	for k := range d.repair {
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
