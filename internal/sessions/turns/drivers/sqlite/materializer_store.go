package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
)

// materializerName keys the single coordination row of the turn
// materializer.
const materializerName = "turns"

const (
	flagIdentityRepairSQL = `
        UPDATE turn_rows SET identity_repair = 1
        WHERE tenant = ? AND user = ? AND session = ? AND turn_id = ?`
	clearIdentityRepairSQL = `
        UPDATE turn_rows SET identity_repair = 0
        WHERE tenant = ? AND user = ? AND session = ? AND turn_id = ?`
	ensureMaterializerSQL = `
        INSERT OR IGNORE INTO turn_materializer (name) VALUES (?)`
	readMaterializerSQL = `
        SELECT holder, lease_expires, cursor FROM turn_materializer WHERE name = ?`
	takeLeaseSQL = `
        UPDATE turn_materializer SET holder = ?, lease_expires = ? WHERE name = ?`
	releaseLeaseSQL = `
        UPDATE turn_materializer SET holder = '', lease_expires = 0
        WHERE name = ? AND holder = ?`
	saveCursorSQL = `
        UPDATE turn_materializer SET cursor = MAX(cursor, ?)
        WHERE name = ? AND holder = ? AND lease_expires > ?`
	listRepairSQL = `
        SELECT tenant, user, session, turn_id FROM turn_rows
        WHERE identity_repair = 1
          AND (tenant, user, session, turn_id) > (?, ?, ?, ?)
        ORDER BY tenant, user, session, turn_id
        LIMIT ?`
)

// Compile-time assertion that *driver satisfies turns.MaterializerStore.
var _ turns.MaterializerStore = (*driver)(nil)

func (d *driver) guard(ctx context.Context) error {
	if d.closed.Load() {
		return fmt.Errorf("sessions/turns/sqlite: %w", turns.ErrStoreClosed)
	}
	return ctx.Err()
}

// AcquireMaterializerLease implements turns.MaterializerStore. The
// read-decide-write runs in one immediate transaction (the driver's
// single-writer serialization), so two acquirers can never both win.
func (d *driver) AcquireMaterializerLease(ctx context.Context, holder string, ttl time.Duration) (turns.MaterializerLease, error) {
	if err := d.guard(ctx); err != nil {
		return turns.MaterializerLease{}, err
	}
	if holder == "" || ttl <= 0 {
		return turns.MaterializerLease{}, fmt.Errorf("%w: lease needs a holder and a positive ttl", turns.ErrInvalidInput)
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return turns.MaterializerLease{}, fmt.Errorf("sessions/turns/sqlite: begin lease tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // the caller receives the original error
	if _, err := tx.ExecContext(ctx, ensureMaterializerSQL, materializerName); err != nil {
		return turns.MaterializerLease{}, fmt.Errorf("sessions/turns/sqlite: ensure lease row: %w", err)
	}
	var (
		cur     string
		expires int64
		cursor  int64
	)
	if err := tx.QueryRowContext(ctx, readMaterializerSQL, materializerName).Scan(&cur, &expires, &cursor); err != nil {
		return turns.MaterializerLease{}, fmt.Errorf("sessions/turns/sqlite: read lease: %w", err)
	}
	now := d.clock().UnixNano()
	if cur == "" || cur == holder || expires <= now {
		cur = holder
		if _, err := tx.ExecContext(ctx, takeLeaseSQL, holder, now+ttl.Nanoseconds(), materializerName); err != nil {
			return turns.MaterializerLease{}, fmt.Errorf("sessions/turns/sqlite: take lease: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return turns.MaterializerLease{}, fmt.Errorf("sessions/turns/sqlite: commit lease: %w", err)
	}
	return turns.MaterializerLease{Held: cur == holder, Holder: cur, Cursor: uint64(cursor)}, nil //nolint:gosec // G115: the cursor column only ever holds non-negative sequences
}

// ReleaseMaterializerLease implements turns.MaterializerStore.
func (d *driver) ReleaseMaterializerLease(ctx context.Context, holder string) error {
	if err := d.guard(ctx); err != nil {
		return err
	}
	if _, err := d.db.ExecContext(ctx, releaseLeaseSQL, materializerName, holder); err != nil {
		return fmt.Errorf("sessions/turns/sqlite: release lease: %w", err)
	}
	return nil
}

// SaveMaterializerCursor implements turns.MaterializerStore.
func (d *driver) SaveMaterializerCursor(ctx context.Context, holder string, cursor uint64) error {
	if err := d.guard(ctx); err != nil {
		return err
	}
	res, err := d.db.ExecContext(ctx, saveCursorSQL, int64(cursor), materializerName, holder, d.clock().UnixNano()) //nolint:gosec // G115: event sequences stay far below MaxInt64
	if err != nil {
		return fmt.Errorf("sessions/turns/sqlite: save cursor: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sessions/turns/sqlite: save cursor rowcount: %w", err)
	}
	if n == 0 {
		return turns.ErrLeaseNotHeld
	}
	return nil
}

// ListIdentityRepairCandidates implements turns.MaterializerStore: a
// keyset page over the partial turn_rows_identity_repair index.
func (d *driver) ListIdentityRepairCandidates(ctx context.Context, after turns.IdentityRepairKey, limit int) ([]turns.IdentityRepairKey, error) {
	if err := d.guard(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, nil
	}
	rows, err := d.db.QueryContext(ctx, listRepairSQL,
		after.Identity.TenantID, after.Identity.UserID, after.Identity.SessionID, string(after.TurnID), limit)
	if err != nil {
		return nil, fmt.Errorf("sessions/turns/sqlite: list identity repair candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]turns.IdentityRepairKey, 0, limit)
	for rows.Next() {
		var k turns.IdentityRepairKey
		var turnID string
		if err := rows.Scan(&k.Identity.TenantID, &k.Identity.UserID, &k.Identity.SessionID, &turnID); err != nil {
			return nil, fmt.Errorf("sessions/turns/sqlite: scan identity repair candidate: %w", err)
		}
		k.TurnID = turns.TurnID(turnID)
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sessions/turns/sqlite: iterate identity repair candidates: %w", err)
	}
	return out, nil
}

// RepairTurnIdentity implements turns.MaterializerStore: fence check,
// row read, the shared ApplyIdentityFill rule, and the conditional
// write in one transaction. Only the row DTO changes (the child
// activity / App tables are untouched by an identity fill).
func (d *driver) RepairTurnIdentity(ctx context.Context, id identity.Identity, turnID turns.TurnID, fill turns.IdentityFill) (turns.TurnRow, bool, error) {
	if err := d.guard(ctx); err != nil {
		return turns.TurnRow{}, false, err
	}
	if err := identity.Validate(id); err != nil {
		return turns.TurnRow{}, false, turns.ErrIdentityRequired
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return turns.TurnRow{}, false, fmt.Errorf("sessions/turns/sqlite: begin repair tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // the caller receives the original error
		}
	}()
	fenced, err := scopeFenced(ctx, tx, id)
	if err != nil {
		return turns.TurnRow{}, false, err
	}
	if fenced {
		return turns.TurnRow{}, false, turns.ErrErasureFenced
	}
	cur, found, err := loadTurn(ctx, tx, id, turnID)
	if err != nil {
		return turns.TurnRow{}, false, err
	}
	if !found {
		return turns.TurnRow{}, false, fmt.Errorf("%w: %q", turns.ErrTurnNotFound, turnID)
	}
	next, changed, settled, err := turns.ApplyIdentityFill(cur, fill)
	if err != nil {
		return turns.TurnRow{}, false, err
	}
	out := cur
	if changed {
		next.Version = cur.Version + 1
		if err := writeRowAndChildren(ctx, tx, id, next); err != nil {
			return turns.TurnRow{}, false, err
		}
		out = next
	}
	if settled {
		if _, err := tx.ExecContext(ctx, clearIdentityRepairSQL, id.TenantID, id.UserID, id.SessionID, string(turnID)); err != nil {
			return turns.TurnRow{}, false, fmt.Errorf("sessions/turns/sqlite: clear identity repair: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return turns.TurnRow{}, false, fmt.Errorf("sessions/turns/sqlite: commit repair: %w", err)
	}
	committed = true
	row, err := cloneRowViaJSON(out)
	if err != nil {
		return turns.TurnRow{}, false, err
	}
	return row, changed, nil
}
