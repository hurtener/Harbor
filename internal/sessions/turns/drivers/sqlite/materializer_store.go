package sqlite

import (
	"context"
	"database/sql"
	"errors"
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
        SELECT holder, lease_expires, cursor, generation FROM turn_materializer WHERE name = ?`
	takeLeaseSQL = `
        UPDATE turn_materializer SET holder = ?, lease_expires = ?, generation = ? WHERE name = ?`
	writeFenceSQL = `
        SELECT 1 FROM turn_materializer WHERE name = ? AND holder = ? AND generation = ?`
	releaseLeaseSQL = `
        UPDATE turn_materializer SET holder = '', lease_expires = 0
        WHERE name = ? AND holder = ?`
	saveCursorSQL = `
        UPDATE turn_materializer SET cursor = MAX(cursor, ?)
        WHERE name = ? AND holder = ? AND lease_expires > ? AND (? < 0 OR generation = ?)`
	listRepairSQL = `
        SELECT tenant, user, session, turn_id FROM turn_rows
        WHERE identity_repair = 1
          AND (tenant, user, session, turn_id) > (?, ?, ?, ?)
        ORDER BY tenant, user, session, turn_id
        LIMIT ?`
	listPendingSQL = `
        SELECT tenant, user, session, turn_id FROM turn_rows
        WHERE pending_complete = 1
          AND (tenant, user, session, turn_id) > (?, ?, ?, ?)
        ORDER BY tenant, user, session, turn_id
        LIMIT ?`
	markPendingSQL = `
        UPDATE turn_rows SET pending_complete = CASE WHEN sealed THEN 0 ELSE 1 END
        WHERE tenant = ? AND user = ? AND session = ? AND turn_id = ?`
	pruneRoutesSQL = `
        DELETE FROM turn_routes WHERE tenant = ? AND user = ? AND session = ? AND root_task_id = ?`
	deleteRoutesScopeSQL = `
        DELETE FROM turn_routes WHERE tenant = ? AND user = ? AND session = ?`
	saveRouteSQL = `
        INSERT INTO turn_routes (tenant, user, session, task_id, parent_task_id, run_id, root_task_id, event_seq)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT (tenant, user, session, task_id) DO UPDATE SET
            parent_task_id = excluded.parent_task_id,
            run_id         = CASE WHEN excluded.run_id <> '' THEN excluded.run_id ELSE turn_routes.run_id END,
            root_task_id   = CASE WHEN excluded.root_task_id <> '' THEN excluded.root_task_id ELSE turn_routes.root_task_id END,
            event_seq      = MAX(turn_routes.event_seq, excluded.event_seq)`
	loadTaskRouteSQL = `
        SELECT task_id, parent_task_id, run_id, root_task_id, event_seq FROM turn_routes
        WHERE tenant = ? AND user = ? AND session = ? AND task_id = ?`
	loadRunRouteSQL = `
        SELECT task_id, parent_task_id, run_id, root_task_id, event_seq FROM turn_routes
        WHERE tenant = ? AND user = ? AND session = ? AND run_id = ?
        ORDER BY event_seq DESC LIMIT 1`
	clearPendingSQL = `
        UPDATE turn_rows SET pending_complete = 0
        WHERE tenant = ? AND user = ? AND session = ? AND turn_id = ?`
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
		cur        string
		expires    int64
		cursor     int64
		generation int64
	)
	if err := tx.QueryRowContext(ctx, readMaterializerSQL, materializerName).Scan(&cur, &expires, &cursor, &generation); err != nil {
		return turns.MaterializerLease{}, fmt.Errorf("sessions/turns/sqlite: read lease: %w", err)
	}
	now := d.clock().UnixNano()
	if cur == "" || cur == holder || expires <= now {
		if cur != holder {
			generation++ // the lease changes hands
		}
		cur = holder
		if _, err := tx.ExecContext(ctx, takeLeaseSQL, holder, now+ttl.Nanoseconds(), generation, materializerName); err != nil {
			return turns.MaterializerLease{}, fmt.Errorf("sessions/turns/sqlite: take lease: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return turns.MaterializerLease{}, fmt.Errorf("sessions/turns/sqlite: commit lease: %w", err)
	}
	return turns.MaterializerLease{Held: cur == holder, Holder: cur, Generation: uint64(generation), Cursor: uint64(cursor)}, nil //nolint:gosec // G115: the cursor / generation columns only ever hold non-negative counters
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
	gen := int64(-1)
	if f, ok := turns.WriteFenceFrom(ctx); ok {
		if f.Holder != holder {
			return turns.ErrLeaseNotHeld
		}
		gen = int64(f.Generation) //nolint:gosec // G115: generations are small counters
	}
	res, err := d.db.ExecContext(ctx, saveCursorSQL, int64(cursor), materializerName, holder, d.clock().UnixNano(), gen, gen) //nolint:gosec // G115: event sequences stay far below MaxInt64
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
	return d.listFlagged(ctx, listRepairSQL, after, limit)
}

// ListPendingCompletions implements turns.MaterializerStore.
func (d *driver) ListPendingCompletions(ctx context.Context, after turns.IdentityRepairKey, limit int) ([]turns.IdentityRepairKey, error) {
	return d.listFlagged(ctx, listPendingSQL, after, limit)
}

func (d *driver) listFlagged(ctx context.Context, query string, after turns.IdentityRepairKey, limit int) ([]turns.IdentityRepairKey, error) {
	if err := d.guard(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, nil
	}
	rows, err := d.db.QueryContext(ctx, query,
		after.Identity.TenantID, after.Identity.UserID, after.Identity.SessionID, string(after.TurnID), limit)
	if err != nil {
		return nil, fmt.Errorf("sessions/turns/sqlite: list flagged rows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]turns.IdentityRepairKey, 0, limit)
	for rows.Next() {
		var k turns.IdentityRepairKey
		var turnID string
		if err := rows.Scan(&k.Identity.TenantID, &k.Identity.UserID, &k.Identity.SessionID, &turnID); err != nil {
			return nil, fmt.Errorf("sessions/turns/sqlite: scan flagged row: %w", err)
		}
		k.TurnID = turns.TurnID(turnID)
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sessions/turns/sqlite: iterate flagged rows: %w", err)
	}
	return out, nil
}

// MarkPendingCompletion implements turns.MaterializerStore.
func (d *driver) MarkPendingCompletion(ctx context.Context, id identity.Identity, turnID turns.TurnID) error {
	if err := d.guard(ctx); err != nil {
		return err
	}
	if err := identity.Validate(id); err != nil {
		return turns.ErrIdentityRequired
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sessions/turns/sqlite: begin mark pending tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // a committed tx makes this a no-op
	if err := checkWriteFence(ctx, tx); err != nil {
		return err
	}
	fenced, err := scopeFenced(ctx, tx, id)
	if err != nil {
		return err
	}
	if fenced {
		return turns.ErrErasureFenced
	}
	res, err := tx.ExecContext(ctx, markPendingSQL, id.TenantID, id.UserID, id.SessionID, string(turnID))
	if err != nil {
		return fmt.Errorf("sessions/turns/sqlite: mark pending: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("sessions/turns/sqlite: mark pending rowcount: %w", err)
	} else if n == 0 {
		return fmt.Errorf("%w: %q", turns.ErrTurnNotFound, turnID)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sessions/turns/sqlite: commit mark pending: %w", err)
	}
	return nil
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
	if err := checkWriteFence(ctx, tx); err != nil {
		return turns.TurnRow{}, false, err
	}
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

// checkWriteFence enforces the lease write fence ctx carries (if any)
// inside tx (the driver's single-writer immediate transaction serializes
// it against a takeover).
func checkWriteFence(ctx context.Context, tx queryer) error {
	f, ok := turns.WriteFenceFrom(ctx)
	if !ok {
		return nil
	}
	var one int
	err := tx.QueryRowContext(ctx, writeFenceSQL, materializerName, f.Holder, int64(f.Generation)).Scan(&one) //nolint:gosec // G115: generations are small counters
	if errors.Is(err, sql.ErrNoRows) {
		return turns.ErrLeaseNotHeld
	}
	if err != nil {
		return fmt.Errorf("sessions/turns/sqlite: lease write fence: %w", err)
	}
	return nil
}

// SaveTaskRoute implements turns.MaterializerStore.
func (d *driver) SaveTaskRoute(ctx context.Context, id identity.Identity, r turns.TaskRoute) error {
	if err := d.guard(ctx); err != nil {
		return err
	}
	if err := identity.Validate(id); err != nil {
		return turns.ErrIdentityRequired
	}
	if r.TaskID == "" {
		return fmt.Errorf("%w: route task id is empty", turns.ErrInvalidInput)
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sessions/turns/sqlite: begin save route tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // a committed tx makes this a no-op
	if err := checkWriteFence(ctx, tx); err != nil {
		return err
	}
	fenced, err := scopeFenced(ctx, tx, id)
	if err != nil {
		return err
	}
	if fenced {
		return turns.ErrErasureFenced
	}
	if _, err := tx.ExecContext(ctx, saveRouteSQL, id.TenantID, id.UserID, id.SessionID,
		r.TaskID, r.ParentTaskID, r.RunID, r.RootTaskID, int64(r.EventSeq)); err != nil { //nolint:gosec // G115: event sequences stay far below MaxInt64
		return fmt.Errorf("sessions/turns/sqlite: save route: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sessions/turns/sqlite: commit save route: %w", err)
	}
	return nil
}

// LoadTaskRoute implements turns.MaterializerStore.
func (d *driver) LoadTaskRoute(ctx context.Context, id identity.Identity, taskID string) (turns.TaskRoute, bool, error) {
	return d.loadRoute(ctx, loadTaskRouteSQL, id, taskID)
}

// LoadRunRoute implements turns.MaterializerStore.
func (d *driver) LoadRunRoute(ctx context.Context, id identity.Identity, runID string) (turns.TaskRoute, bool, error) {
	if runID == "" {
		return turns.TaskRoute{}, false, nil
	}
	return d.loadRoute(ctx, loadRunRouteSQL, id, runID)
}

func (d *driver) loadRoute(ctx context.Context, query string, id identity.Identity, key string) (turns.TaskRoute, bool, error) {
	if err := d.guard(ctx); err != nil {
		return turns.TaskRoute{}, false, err
	}
	if err := identity.Validate(id); err != nil {
		return turns.TaskRoute{}, false, turns.ErrIdentityRequired
	}
	var r turns.TaskRoute
	var seq int64
	err := d.db.QueryRowContext(ctx, query, id.TenantID, id.UserID, id.SessionID, key).
		Scan(&r.TaskID, &r.ParentTaskID, &r.RunID, &r.RootTaskID, &seq)
	if errors.Is(err, sql.ErrNoRows) {
		return turns.TaskRoute{}, false, nil
	}
	if err != nil {
		return turns.TaskRoute{}, false, fmt.Errorf("sessions/turns/sqlite: load route: %w", err)
	}
	r.EventSeq = uint64(seq) //nolint:gosec // G115: non-negative sequences
	return r, true, nil
}
