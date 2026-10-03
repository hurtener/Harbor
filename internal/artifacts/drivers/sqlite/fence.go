package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/artifacts"
)

// FenceScope permanently serializes erasure against every byte Put.
func (d *driver) FenceScope(ctx context.Context, s artifacts.ArtifactScope) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if d.closed.Load() {
		return artifacts.ErrStoreClosed
	}
	_, err := d.db.ExecContext(ctx, `INSERT INTO artifact_scope_fences(tenant,user,session,fenced) VALUES(?,?,?,1) ON CONFLICT(tenant,user,session) DO UPDATE SET fenced=1`, s.TenantID, s.UserID, s.SessionID)
	if err != nil {
		return fmt.Errorf("artifacts/sqlite: fence scope: %w", err)
	}
	return nil
}

// ScopeFenced reads the durable owner tombstone without broadening scope.
func (d *driver) ScopeFenced(ctx context.Context, s artifacts.ArtifactScope) (bool, error) {
	if err := s.Validate(); err != nil {
		return false, err
	}
	if d.closed.Load() {
		return false, artifacts.ErrStoreClosed
	}
	var fenced bool
	err := d.db.QueryRowContext(ctx, `SELECT fenced FROM artifact_scope_fences WHERE tenant=? AND user=? AND session=?`, s.TenantID, s.UserID, s.SessionID).Scan(&fenced)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("artifacts/sqlite: read fence: %w", err)
	}
	return fenced, nil
}
func scopeOpenTx(ctx context.Context, tx *sql.Tx, s artifacts.ArtifactScope) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO artifact_scope_fences(tenant,user,session,fenced) VALUES(?,?,?,0) ON CONFLICT(tenant,user,session) DO NOTHING`, s.TenantID, s.UserID, s.SessionID); err != nil {
		return fmt.Errorf("artifacts/sqlite: acquire scope: %w", err)
	}
	var fenced bool
	if err := tx.QueryRowContext(ctx, `SELECT fenced FROM artifact_scope_fences WHERE tenant=? AND user=? AND session=?`, s.TenantID, s.UserID, s.SessionID).Scan(&fenced); err != nil {
		return fmt.Errorf("artifacts/sqlite: check scope: %w", err)
	}
	if fenced {
		return artifacts.ErrScopeFenced
	}
	return nil
}

var _ artifacts.ScopeFencer = (*driver)(nil)
