package conformancetest

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/state"

	// Test-scoped driver carve-out (CLAUDE.md §13): the in-memory
	// StateStore driver backs the fence slots of the smoke driver.
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// smokeDriver is the in-package mini Store that smoke-runs the suite:
// a mutex-guarded map store (unbounded retention) with fence
// enforcement against the shared StateStore. It is a TEST-GRADE
// implementation living in a _test.go file — never a production
// default — and exists so the suite itself is exercised by CI.
type smokeDriver struct {
	mu          sync.Mutex
	rows        map[smokeKey]turns.TurnRow
	seqs        map[string]uint64
	checkpoints map[string]uint64
	st          state.StateStore
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

func newSmokeDriver(t *testing.T) (*smokeDriver, state.StateStore) {
	t.Helper()
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("open inmem state: %v", err)
	}
	return &smokeDriver{
		rows:        map[smokeKey]turns.TurnRow{},
		seqs:        map[string]uint64{},
		checkpoints: map[string]uint64{},
		st:          st,
	}, st
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

func (d *smokeDriver) fenceErr(fence turns.Fence) error {
	for _, slot := range []turns.Slot{fence.PendingAbsent, fence.TombstoneAbsent} {
		_, err := d.st.Load(context.Background(), slot.Identity, slot.Kind)
		if err == nil {
			return turns.ErrErasureFenced
		}
		if !errors.Is(err, state.ErrNotFound) {
			return err
		}
	}
	return nil
}

func (d *smokeDriver) AppendTurnIf(ctx context.Context, id identity.Identity, row turns.TurnRow, fence turns.Fence) (turns.TurnRow, error) {
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
	if err := d.fenceErr(fence); err != nil {
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

func (d *smokeDriver) mutate(ctx context.Context, id identity.Identity, turnID turns.TurnID, expectedVersion int, row turns.TurnRow, sealed bool, fence turns.Fence) (turns.TurnRow, error) {
	if err := d.fenceErr(fence); err != nil {
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

func (d *smokeDriver) UpdateTurnIf(ctx context.Context, id identity.Identity, turnID turns.TurnID, expectedVersion int, row turns.TurnRow, fence turns.Fence) (turns.TurnRow, error) {
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
	return d.mutate(ctx, id, turnID, expectedVersion, row, false, fence)
}

func (d *smokeDriver) SealTurnIf(ctx context.Context, id identity.Identity, turnID turns.TurnID, expectedVersion int, row turns.TurnRow, fence turns.Fence) (turns.TurnRow, error) {
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
	return d.mutate(ctx, id, turnID, expectedVersion, row, true, fence)
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

func (d *smokeDriver) ListTurns(ctx context.Context, id identity.Identity, before *turns.Cursor, limit int) ([]turns.TurnRow, *turns.Cursor, bool, error) {
	if err := d.closedErr(); err != nil {
		return nil, nil, false, err
	}
	if err := d.identityErr(id); err != nil {
		return nil, nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, false, err
	}
	if limit < 1 {
		return nil, nil, false, turns.ErrInvalidInput
	}
	d.mu.Lock()
	defer d.mu.Unlock()
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
	if len(candidates) <= limit {
		return candidates, nil, false, nil
	}
	page := candidates[:limit]
	last := page[len(page)-1]
	return page, &turns.Cursor{Seq: last.Sequence, TurnID: last.TurnID}, false, nil
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
	Run(t, func() (turns.Store, state.StateStore, func()) {
		d, st := newSmokeDriver(t)
		return d, st, func() { _ = d.Close(context.Background()) }
	})
}

// Assert the smoke driver satisfies the interface at compile time.
var _ turns.Store = (*smokeDriver)(nil)
