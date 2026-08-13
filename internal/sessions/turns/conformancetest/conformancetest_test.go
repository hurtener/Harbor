package conformancetest

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
)

// smokeDriver is the in-package mini Store that smoke-runs the suite:
// a mutex-guarded map store (unbounded retention) with a STORE-LOCAL
// erasure fence. It is a TEST-GRADE implementation living in a
// _test.go file — never a production default — and exists so the suite
// itself is exercised by CI. The fence is store-local on purpose: the
// suite pins the contract that a driver fences in its own backend and
// never inspects arbitrary external StateStore slots.
type smokeDriver struct {
	mu          sync.Mutex
	rows        map[smokeKey]turns.TurnRow
	seqs        map[string]uint64
	checkpoints map[string]uint64
	snapshots   map[string]uint64
	fenced      map[string]bool
	closed      bool
}

type smokeKey struct {
	tenant, user, session, turnID string
}

func keyOf(id identity.Identity, turnID turns.TurnID) smokeKey {
	return smokeKey{tenant: id.TenantID, user: id.UserID, session: id.SessionID, turnID: string(turnID)}
}

func sessionKeyOf(id identity.Identity) string {
	return id.TenantID + "|" + id.UserID + "|" + id.SessionID
}

func newSmokeDriver() *smokeDriver {
	return &smokeDriver{
		rows:        map[smokeKey]turns.TurnRow{},
		seqs:        map[string]uint64{},
		checkpoints: map[string]uint64{},
		snapshots:   map[string]uint64{},
		fenced:      map[string]bool{},
	}
}

func (d *smokeDriver) Durable() bool { return false }

func (d *smokeDriver) closedErr() error {
	if d.closed {
		return turns.ErrStoreClosed
	}
	return nil
}

func (d *smokeDriver) identityErr(id identity.Identity) error {
	if err := identity.Validate(id); err != nil {
		return turns.ErrIdentityRequired
	}
	return nil
}

// fencedErr reports ErrErasureFenced when the session's STORE-LOCAL
// fence is set. Caller holds d.mu (FenceSession writes under the same
// lock, so check and write are serialized — a real driver checks in
// the same transaction).
func (d *smokeDriver) fencedErr(id identity.Identity) error {
	if d.fenced[sessionKeyOf(id)] {
		return turns.ErrErasureFenced
	}
	return nil
}

func (d *smokeDriver) AppendTurnIf(ctx context.Context, id identity.Identity, row turns.TurnRow) (turns.TurnRow, error) {
	if err := d.closedErr(); err != nil {
		return turns.TurnRow{}, err
	}
	if err := d.identityErr(id); err != nil {
		return turns.TurnRow{}, err
	}
	if err := ctx.Err(); err != nil {
		return turns.TurnRow{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.fencedErr(id); err != nil {
		return turns.TurnRow{}, err
	}
	k := keyOf(id, row.TurnID)
	if existing, ok := d.rows[k]; ok {
		return existing, nil // idempotent replay no-op
	}
	sk := sessionKeyOf(id)
	d.seqs[sk]++
	row.Sequence = turns.Seq(d.seqs[sk])
	row.TieBreaker = row.TurnID
	row.Version = 1
	d.rows[k] = row
	return row, nil
}

func (d *smokeDriver) mutate(ctx context.Context, id identity.Identity, turnID turns.TurnID, expectedVersion int, row turns.TurnRow, sealed bool) (turns.TurnRow, error) {
	if err := d.fencedErr(id); err != nil {
		return turns.TurnRow{}, err
	}
	k := keyOf(id, turnID)
	current, ok := d.rows[k]
	if !ok {
		return turns.TurnRow{}, turns.ErrTurnNotFound
	}
	if current.Sealed {
		return turns.TurnRow{}, turns.ErrTurnSealed
	}
	if current.Version != expectedVersion {
		return turns.TurnRow{}, turns.ErrStaleVersion
	}
	next := row
	next.TurnID = turnID
	next.Sequence = current.Sequence
	next.TieBreaker = current.TieBreaker
	next.Sealed = sealed
	next.Version = current.Version + 1
	d.rows[k] = next
	return next, nil
}

func (d *smokeDriver) UpdateTurnIf(ctx context.Context, id identity.Identity, turnID turns.TurnID, expectedVersion int, row turns.TurnRow) (turns.TurnRow, error) {
	if err := d.closedErr(); err != nil {
		return turns.TurnRow{}, err
	}
	if err := d.identityErr(id); err != nil {
		return turns.TurnRow{}, err
	}
	if err := ctx.Err(); err != nil {
		return turns.TurnRow{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mutate(ctx, id, turnID, expectedVersion, row, false)
}

func (d *smokeDriver) SealTurnIf(ctx context.Context, id identity.Identity, turnID turns.TurnID, expectedVersion int, row turns.TurnRow) (turns.TurnRow, error) {
	if err := d.closedErr(); err != nil {
		return turns.TurnRow{}, err
	}
	if err := d.identityErr(id); err != nil {
		return turns.TurnRow{}, err
	}
	if err := ctx.Err(); err != nil {
		return turns.TurnRow{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mutate(ctx, id, turnID, expectedVersion, row, true)
}

func (d *smokeDriver) FenceSession(ctx context.Context, id identity.Identity) error {
	if err := d.closedErr(); err != nil {
		return err
	}
	if err := d.identityErr(id); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fenced[sessionKeyOf(id)] = true // idempotent
	return nil
}

func (d *smokeDriver) GetTurn(ctx context.Context, id identity.Identity, turnID turns.TurnID) (turns.TurnRow, error) {
	if err := d.closedErr(); err != nil {
		return turns.TurnRow{}, err
	}
	if err := d.identityErr(id); err != nil {
		return turns.TurnRow{}, err
	}
	if err := ctx.Err(); err != nil {
		return turns.TurnRow{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	row, ok := d.rows[keyOf(id, turnID)]
	if !ok {
		return turns.TurnRow{}, turns.ErrTurnNotFound
	}
	return row, nil
}

func (d *smokeDriver) ListTurns(ctx context.Context, id identity.Identity, before *turns.Cursor, limit int) ([]turns.TurnRow, *turns.Cursor, turns.ListPageInfo, error) {
	var zero turns.ListPageInfo
	if err := d.closedErr(); err != nil {
		return nil, nil, zero, err
	}
	if err := d.identityErr(id); err != nil {
		return nil, nil, zero, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, zero, err
	}
	if limit < 1 {
		return nil, nil, zero, turns.ErrInvalidInput
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	sk := sessionKeyOf(id)
	snapshot := d.snapshots[sk]
	// Opaque-cursor BINDING: session + projection snapshot + a
	// retained boundary row.
	if before != nil {
		if before.SessionID != id.SessionID {
			return nil, nil, zero, fmt.Errorf("%w: cursor names session %q, request is %q",
				turns.ErrCursorForeignSession, before.SessionID, id.SessionID)
		}
		if before.Snapshot != snapshot {
			return nil, nil, zero, fmt.Errorf("%w: cursor snapshot %d, current %d",
				turns.ErrCursorSnapshotStale, before.Snapshot, snapshot)
		}
		if _, ok := d.rows[keyOf(id, before.TurnID)]; !ok {
			return nil, nil, zero, fmt.Errorf("%w: boundary row %q is no longer retained",
				turns.ErrCursorExpired, before.TurnID)
		}
	}
	var rows []turns.TurnRow
	for k, row := range d.rows {
		if k.tenant == id.TenantID && k.user == id.UserID && k.session == id.SessionID {
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Sequence != rows[j].Sequence {
			return rows[i].Sequence > rows[j].Sequence
		}
		return rows[i].TurnID > rows[j].TurnID
	})
	var candidates []turns.TurnRow
	for _, r := range rows {
		if before != nil {
			if r.Sequence > before.Seq || (r.Sequence == before.Seq && r.TurnID >= before.TurnID) {
				continue
			}
		}
		candidates = append(candidates, r)
	}
	info := turns.ListPageInfo{Snapshot: snapshot, Truncated: false}
	if len(candidates) <= limit {
		info.Remaining = 0
		info.CountExact = true
		return candidates, nil, info, nil
	}
	page := candidates[:limit]
	last := page[len(page)-1]
	info.Remaining = len(candidates) - limit
	info.CountExact = true
	next := &turns.Cursor{SessionID: id.SessionID, Snapshot: snapshot, Seq: last.Sequence, TurnID: last.TurnID}
	return page, next, info, nil
}

func (d *smokeDriver) LoadCheckpoint(ctx context.Context, id identity.Identity) (uint64, error) {
	if err := d.closedErr(); err != nil {
		return 0, err
	}
	if err := d.identityErr(id); err != nil {
		return 0, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.checkpoints[sessionKeyOf(id)], nil
}

func (d *smokeDriver) SaveCheckpoint(ctx context.Context, id identity.Identity, seq uint64) error {
	if err := d.closedErr(); err != nil {
		return err
	}
	if err := d.identityErr(id); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	// A fenced (erased) session must never advance its checkpoint — no
	// resurrection after replay / restart.
	if err := d.fencedErr(id); err != nil {
		return err
	}
	sk := sessionKeyOf(id)
	if seq > d.checkpoints[sk] {
		d.checkpoints[sk] = seq // monotonic: never regress
	}
	return nil
}

func (d *smokeDriver) DeleteScope(ctx context.Context, id identity.Identity) (int, error) {
	if err := d.closedErr(); err != nil {
		return 0, err
	}
	if err := d.identityErr(id); err != nil {
		return 0, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	// The STORE-LOCAL FENCE is deliberately NOT removed here: the
	// erasure cascade sets it via FenceSession before DeleteScope, and
	// it must survive the erase so an erased session stays fenced (no
	// resurrection).
	deleted := 0
	for k := range d.rows {
		if k.tenant == id.TenantID && k.user == id.UserID && k.session == id.SessionID {
			delete(d.rows, k)
			deleted++
		}
	}
	sk := sessionKeyOf(id)
	if _, ok := d.checkpoints[sk]; ok {
		delete(d.checkpoints, sk)
		deleted++
	}
	if _, ok := d.seqs[sk]; ok {
		delete(d.seqs, sk)
		deleted++
	}
	// Advance the projection SNAPSHOT generation so any cursor minted
	// before the erase is rejected as stale.
	d.snapshots[sk]++
	return deleted, nil
}

func (d *smokeDriver) Close(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	return nil
}

// TestConformance_Smoke runs the canonical suite against the in-package
// mini driver so CI exercises the suite itself; future driver lanes run
// the same suite against their real drivers.
func TestConformance_Smoke(t *testing.T) {
	Run(t, func() (turns.Store, func()) {
		d := newSmokeDriver()
		return d, func() { _ = d.Close(context.Background()) }
	})
}

// Assert the smoke driver satisfies the interface at compile time.
var _ turns.Store = (*smokeDriver)(nil)
