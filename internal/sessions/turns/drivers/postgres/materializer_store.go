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
        INSERT INTO turn_materializer (name, holder, lease_expires)
        VALUES ($1, $2, now() + ($3::bigint * interval '1 microsecond'))
        ON CONFLICT (name) DO UPDATE
            SET holder = EXCLUDED.holder, lease_expires = EXCLUDED.lease_expires
            WHERE turn_materializer.holder = ''
               OR turn_materializer.holder = EXCLUDED.holder
               OR turn_materializer.lease_expires <= now()`,
		materializerName, holder, ttl.Microseconds(),
	); err != nil {
		return turns.MaterializerLease{}, fmt.Errorf("turns/postgres: acquire materializer lease: %w", err)
	}
	var (
		cur    string
		cursor int64
	)
	if err := d.db.QueryRowContext(ctx, `
        SELECT holder, cursor FROM turn_materializer WHERE name = $1`,
		materializerName,
	).Scan(&cur, &cursor); err != nil {
		return turns.MaterializerLease{}, fmt.Errorf("turns/postgres: read materializer lease: %w", err)
	}
	return turns.MaterializerLease{Held: cur == holder, Holder: cur, Cursor: fromDBSeq(cursor)}, nil
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
	res, err := d.db.ExecContext(ctx, `
        UPDATE turn_materializer SET cursor = GREATEST(cursor, $3)
        WHERE name = $1 AND holder = $2 AND lease_expires > now()`,
		materializerName, holder, dbSeq(cursor),
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
	if err := d.check(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, nil
	}
	rows, err := d.db.QueryContext(ctx, `
        SELECT tenant_id, user_id, session_id, turn_id FROM turn_rows
        WHERE identity_repair
          AND (tenant_id, user_id, session_id, turn_id) > ($1, $2, $3, $4)
        ORDER BY tenant_id, user_id, session_id, turn_id
        LIMIT $5`,
		after.Identity.TenantID, after.Identity.UserID, after.Identity.SessionID, string(after.TurnID), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("turns/postgres: list identity repair candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]turns.IdentityRepairKey, 0, limit)
	for rows.Next() {
		var k turns.IdentityRepairKey
		var turnID string
		if err := rows.Scan(&k.Identity.TenantID, &k.Identity.UserID, &k.Identity.SessionID, &turnID); err != nil {
			return nil, fmt.Errorf("turns/postgres: scan identity repair candidate: %w", err)
		}
		k.TurnID = turns.TurnID(turnID)
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("turns/postgres: iterate identity repair candidates: %w", err)
	}
	return out, nil
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
