package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
)

// materializerName keys the single coordination row of the turn
// materializer.
const materializerName = "turns"

// Compile-time assertion that *driver satisfies turns.MaterializerStore.
var _ turns.MaterializerStore = (*driver)(nil)

// AcquireMaterializerLease implements turns.MaterializerStore. The
// lease expiry is evaluated against the DATABASE clock (now()), so the
// instances sharing the store never disagree about expiry through
// clock skew. One conditional upsert takes a free / expired / own
// lease; a read-back reports the outcome and the persisted cursor.
func (d *driver) AcquireMaterializerLease(ctx context.Context, holder string, ttl time.Duration) (turns.MaterializerLease, error) {
	if err := d.check(ctx); err != nil {
		return turns.MaterializerLease{}, err
	}
	if holder == "" || ttl <= 0 {
		return turns.MaterializerLease{}, fmt.Errorf("%w: lease needs a holder and a positive ttl", turns.ErrInvalidInput)
	}
	if _, err := d.db.ExecContext(ctx, `
        INSERT INTO turn_materializer (name, holder, lease_expires, generation)
        VALUES ($1, $2, now() + ($3::bigint * interval '1 microsecond'), 1)
        ON CONFLICT (name) DO UPDATE
            SET holder = EXCLUDED.holder, lease_expires = EXCLUDED.lease_expires,
                generation = CASE WHEN turn_materializer.holder = EXCLUDED.holder
                                  THEN turn_materializer.generation
                                  ELSE turn_materializer.generation + 1 END
            WHERE turn_materializer.holder = ''
               OR turn_materializer.holder = EXCLUDED.holder
               OR turn_materializer.lease_expires <= now()`,
		materializerName, holder, ttl.Microseconds(),
	); err != nil {
		return turns.MaterializerLease{}, fmt.Errorf("turns/postgres: acquire materializer lease: %w", err)
	}
	var (
		cur        string
		cursor     int64
		generation int64
	)
	if err := d.db.QueryRowContext(ctx, `
        SELECT holder, cursor, generation FROM turn_materializer WHERE name = $1`,
		materializerName,
	).Scan(&cur, &cursor, &generation); err != nil {
		return turns.MaterializerLease{}, fmt.Errorf("turns/postgres: read materializer lease: %w", err)
	}
	return turns.MaterializerLease{Held: cur == holder, Holder: cur, Generation: fromDBSeq(generation), Cursor: fromDBSeq(cursor)}, nil
}

// ReleaseMaterializerLease implements turns.MaterializerStore.
func (d *driver) ReleaseMaterializerLease(ctx context.Context, holder string) error {
	if err := d.check(ctx); err != nil {
		return err
	}
	if _, err := d.db.ExecContext(ctx, `
        UPDATE turn_materializer SET holder = '', lease_expires = 'epoch'
        WHERE name = $1 AND holder = $2`,
		materializerName, holder,
	); err != nil {
		return fmt.Errorf("turns/postgres: release materializer lease: %w", err)
	}
	return nil
}

// SaveMaterializerCursor implements turns.MaterializerStore: a
// lease-fenced, monotonic cursor write in one statement.
func (d *driver) SaveMaterializerCursor(ctx context.Context, holder string, cursor uint64) error {
	if err := d.check(ctx); err != nil {
		return err
	}
	gen := int64(-1)
	if f, ok := turns.WriteFenceFrom(ctx); ok {
		if f.Holder != holder {
			return turns.ErrLeaseNotHeld
		}
		gen = dbSeq(f.Generation)
	}
	res, err := d.db.ExecContext(ctx, `
        UPDATE turn_materializer SET cursor = GREATEST(cursor, $3)
        WHERE name = $1 AND holder = $2 AND lease_expires > now()
          AND ($4 < 0 OR generation = $4)`,
		materializerName, holder, dbSeq(cursor), gen,
	)
	if err != nil {
		return fmt.Errorf("turns/postgres: save materializer cursor: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("turns/postgres: save materializer cursor rowcount: %w", err)
	}
	if n == 0 {
		return turns.ErrLeaseNotHeld
	}
	return nil
}

// ListIdentityRepairCandidates implements turns.MaterializerStore: a
// keyset page over the partial turn_rows_identity_repair index.
func (d *driver) ListIdentityRepairCandidates(ctx context.Context, after turns.IdentityRepairKey, limit int) ([]turns.IdentityRepairKey, error) {
	return d.listFlagged(ctx, "identity_repair", after, limit)
}

// listFlagged pages the rows whose boolean flag column is set (keyset over
// the flag's partial index). flag is one of the two fixed column names.
func (d *driver) listFlagged(ctx context.Context, flag string, after turns.IdentityRepairKey, limit int) ([]turns.IdentityRepairKey, error) {
	if err := d.check(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, nil
	}
	var query string
	switch flag {
	case "identity_repair":
		query = listFlaggedSQL("identity_repair")
	case "pending_complete":
		query = listFlaggedSQL("pending_complete")
	default:
		return nil, fmt.Errorf("turns/postgres: unknown flag %q", flag)
	}
	rows, err := d.db.QueryContext(ctx, query,
		after.Identity.TenantID, after.Identity.UserID, after.Identity.SessionID, string(after.TurnID), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("turns/postgres: list %s rows: %w", flag, err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]turns.IdentityRepairKey, 0, limit)
	for rows.Next() {
		var k turns.IdentityRepairKey
		var turnID string
		if err := rows.Scan(&k.Identity.TenantID, &k.Identity.UserID, &k.Identity.SessionID, &turnID); err != nil {
			return nil, fmt.Errorf("turns/postgres: scan %s row: %w", flag, err)
		}
		k.TurnID = turns.TurnID(turnID)
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("turns/postgres: iterate %s rows: %w", flag, err)
	}
	return out, nil
}

// listFlaggedSQL builds the keyset query for one of the fixed flag columns
// (never caller input).
func listFlaggedSQL(flag string) string {
	return `
        SELECT tenant_id, user_id, session_id, turn_id FROM turn_rows
        WHERE ` + flag + `
          AND (tenant_id, user_id, session_id, turn_id) > ($1, $2, $3, $4)
        ORDER BY tenant_id, user_id, session_id, turn_id
        LIMIT $5`
}

// RepairTurnIdentity implements turns.MaterializerStore: the fence
// check, the row read under its lock, the shared ApplyIdentityFill
// rule, and the conditional write all run in one transaction.
func (d *driver) RepairTurnIdentity(ctx context.Context, id identity.Identity, turnID turns.TurnID, fill turns.IdentityFill) (turns.TurnRow, bool, error) {
	if err := d.check(ctx); err != nil {
		return turns.TurnRow{}, false, err
	}
	if err := d.checkIdentity(id); err != nil {
		return turns.TurnRow{}, false, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return turns.TurnRow{}, false, fmt.Errorf("turns/postgres: repair begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // best-effort; the surfaced error is the original one
	if err := checkWriteFence(ctx, tx); err != nil {
		return turns.TurnRow{}, false, err
	}

	var fenced bool
	err = tx.QueryRowContext(ctx, `
        SELECT fenced FROM turn_sessions
        WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3
        FOR UPDATE`,
		id.TenantID, id.UserID, id.SessionID,
	).Scan(&fenced)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return turns.TurnRow{}, false, fmt.Errorf("turns/postgres: repair fence check: %w", err)
	}
	if fenced {
		return turns.TurnRow{}, false, fmt.Errorf("%w: session %q is erasure-fenced", turns.ErrErasureFenced, id.SessionID)
	}
	var raw string
	err = tx.QueryRowContext(ctx, `
        SELECT row_json FROM turn_rows
        WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3 AND turn_id = $4
        FOR UPDATE`,
		id.TenantID, id.UserID, id.SessionID, string(turnID),
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return turns.TurnRow{}, false, fmt.Errorf("%w: turn %q", turns.ErrTurnNotFound, turnID)
	}
	if err != nil {
		return turns.TurnRow{}, false, fmt.Errorf("turns/postgres: repair load row: %w", err)
	}
	cur, err := decodeRowString(raw)
	if err != nil {
		return turns.TurnRow{}, false, err
	}
	next, changed, settled, err := turns.ApplyIdentityFill(cur, fill)
	if err != nil {
		return turns.TurnRow{}, false, err
	}
	stored := raw
	if changed {
		next.Version = cur.Version + 1
		b, err := json.Marshal(next)
		if err != nil {
			return turns.TurnRow{}, false, fmt.Errorf("turns/postgres: repair encode row: %w", err)
		}
		stored = string(b)
		if _, err := tx.ExecContext(ctx, `
            UPDATE turn_rows SET
                effective_agent_id = $5,
                version            = $6,
                row_json           = $7,
                identity_repair    = $8
            WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3 AND turn_id = $4`,
			id.TenantID, id.UserID, id.SessionID, string(turnID),
			next.Agent.ID, next.Version, stored, !settled,
		); err != nil {
			return turns.TurnRow{}, false, fmt.Errorf("turns/postgres: repair write: %w", err)
		}
	} else if settled {
		if _, err := tx.ExecContext(ctx, `
            UPDATE turn_rows SET identity_repair = false
            WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3 AND turn_id = $4`,
			id.TenantID, id.UserID, id.SessionID, string(turnID),
		); err != nil {
			return turns.TurnRow{}, false, fmt.Errorf("turns/postgres: repair settle: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return turns.TurnRow{}, false, fmt.Errorf("turns/postgres: repair commit: %w", err)
	}
	row, err := decodeRowString(stored)
	if err != nil {
		return turns.TurnRow{}, false, err
	}
	return row, changed, nil
}

// checkWriteFence enforces the lease write fence ctx carries (if any)
// inside tx: the lease row must still name the fence's holder at its
// generation. The row is share-locked, so a takeover (which updates it)
// serializes after this transaction, and a write that commits after a
// takeover is impossible.
func checkWriteFence(ctx context.Context, tx *sql.Tx) error {
	f, ok := turns.WriteFenceFrom(ctx)
	if !ok {
		return nil
	}
	var one int
	err := tx.QueryRowContext(ctx, `
        SELECT 1 FROM turn_materializer
        WHERE name = $1 AND holder = $2 AND generation = $3
        FOR SHARE`,
		materializerName, f.Holder, dbSeq(f.Generation),
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return turns.ErrLeaseNotHeld
	}
	if err != nil {
		return fmt.Errorf("turns/postgres: lease write fence: %w", err)
	}
	return nil
}

// MarkPendingCompletion implements turns.MaterializerStore.
func (d *driver) MarkPendingCompletion(ctx context.Context, id identity.Identity, turnID turns.TurnID) error {
	if err := d.check(ctx); err != nil {
		return err
	}
	if err := d.checkIdentity(id); err != nil {
		return err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("turns/postgres: mark pending begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // best-effort; the surfaced error is the original one
	if err := checkWriteFence(ctx, tx); err != nil {
		return err
	}
	var fenced bool
	err = tx.QueryRowContext(ctx, `
        SELECT fenced FROM turn_sessions
        WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3
        FOR UPDATE`,
		id.TenantID, id.UserID, id.SessionID,
	).Scan(&fenced)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("turns/postgres: mark pending fence check: %w", err)
	}
	if fenced {
		return fmt.Errorf("%w: session %q is erasure-fenced", turns.ErrErasureFenced, id.SessionID)
	}
	var sealed bool
	err = tx.QueryRowContext(ctx, `
        UPDATE turn_rows SET pending_complete = NOT sealed
        WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3 AND turn_id = $4
        RETURNING sealed`,
		id.TenantID, id.UserID, id.SessionID, string(turnID),
	).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: turn %q", turns.ErrTurnNotFound, turnID)
	}
	if err != nil {
		return fmt.Errorf("turns/postgres: mark pending: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("turns/postgres: mark pending commit: %w", err)
	}
	return nil
}

// ListPendingCompletions implements turns.MaterializerStore.
func (d *driver) ListPendingCompletions(ctx context.Context, after turns.IdentityRepairKey, limit int) ([]turns.IdentityRepairKey, error) {
	return d.listFlagged(ctx, "pending_complete", after, limit)
}

// SaveTaskRoute implements turns.MaterializerStore.
func (d *driver) SaveTaskRoute(ctx context.Context, id identity.Identity, r turns.TaskRoute) error {
	if err := d.check(ctx); err != nil {
		return err
	}
	if err := d.checkIdentity(id); err != nil {
		return err
	}
	if r.TaskID == "" {
		return fmt.Errorf("%w: route task id is empty", turns.ErrInvalidInput)
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("turns/postgres: save route begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // best-effort; the surfaced error is the original one
	if err := checkWriteFence(ctx, tx); err != nil {
		return err
	}
	var fenced bool
	err = tx.QueryRowContext(ctx, `
        SELECT fenced FROM turn_sessions
        WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3
        FOR SHARE`,
		id.TenantID, id.UserID, id.SessionID,
	).Scan(&fenced)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("turns/postgres: save route fence check: %w", err)
	}
	if fenced {
		return fmt.Errorf("%w: session %q is erasure-fenced", turns.ErrErasureFenced, id.SessionID)
	}
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO turn_routes (tenant_id, user_id, session_id, task_id, parent_task_id, run_id, root_task_id, event_seq)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
        ON CONFLICT (tenant_id, user_id, session_id, task_id) DO UPDATE SET
            parent_task_id = EXCLUDED.parent_task_id,
            run_id         = CASE WHEN EXCLUDED.run_id <> '' THEN EXCLUDED.run_id ELSE turn_routes.run_id END,
            root_task_id   = CASE WHEN EXCLUDED.root_task_id <> '' THEN EXCLUDED.root_task_id ELSE turn_routes.root_task_id END,
            event_seq      = GREATEST(turn_routes.event_seq, EXCLUDED.event_seq)`,
		id.TenantID, id.UserID, id.SessionID, r.TaskID, r.ParentTaskID, r.RunID, r.RootTaskID, dbSeq(r.EventSeq),
	); err != nil {
		return fmt.Errorf("turns/postgres: save route: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("turns/postgres: save route commit: %w", err)
	}
	return nil
}

// LoadTaskRoute implements turns.MaterializerStore.
func (d *driver) LoadTaskRoute(ctx context.Context, id identity.Identity, taskID string) (turns.TaskRoute, bool, error) {
	return d.loadRoute(ctx, `
        SELECT task_id, parent_task_id, run_id, root_task_id, event_seq FROM turn_routes
        WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3 AND task_id = $4`, id, taskID)
}

// LoadRunRoute implements turns.MaterializerStore.
func (d *driver) LoadRunRoute(ctx context.Context, id identity.Identity, runID string) (turns.TaskRoute, bool, error) {
	if runID == "" {
		return turns.TaskRoute{}, false, nil
	}
	return d.loadRoute(ctx, `
        SELECT task_id, parent_task_id, run_id, root_task_id, event_seq FROM turn_routes
        WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3 AND run_id = $4
        ORDER BY event_seq DESC LIMIT 1`, id, runID)
}

func (d *driver) loadRoute(ctx context.Context, query string, id identity.Identity, key string) (turns.TaskRoute, bool, error) {
	if err := d.check(ctx); err != nil {
		return turns.TaskRoute{}, false, err
	}
	if err := d.checkIdentity(id); err != nil {
		return turns.TaskRoute{}, false, err
	}
	var r turns.TaskRoute
	var seq int64
	err := d.db.QueryRowContext(ctx, query, id.TenantID, id.UserID, id.SessionID, key).
		Scan(&r.TaskID, &r.ParentTaskID, &r.RunID, &r.RootTaskID, &seq)
	if errors.Is(err, sql.ErrNoRows) {
		return turns.TaskRoute{}, false, nil
	}
	if err != nil {
		return turns.TaskRoute{}, false, fmt.Errorf("turns/postgres: load route: %w", err)
	}
	r.EventSeq = fromDBSeq(seq)
	return r, true, nil
}
