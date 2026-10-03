package postgres

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
	_, err := d.db.ExecContext(ctx, `INSERT INTO artifact_scope_fences(tenant,"user",session,fenced) VALUES($1,$2,$3,TRUE) ON CONFLICT(tenant,"user",session) DO UPDATE SET fenced=TRUE`, s.TenantID, s.UserID, s.SessionID)
	if err != nil {
		return fmt.Errorf("artifacts/postgres: fence scope: %w", err)
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
	err := d.db.QueryRowContext(ctx, `SELECT fenced FROM artifact_scope_fences WHERE tenant=$1 AND "user"=$2 AND session=$3`, s.TenantID, s.UserID, s.SessionID).Scan(&fenced)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("artifacts/postgres: read fence: %w", err)
	}
	return fenced, nil
}
func scopeOpenTx(ctx context.Context, tx *sql.Tx, s artifacts.ArtifactScope) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO artifact_scope_fences(tenant,"user",session,fenced) VALUES($1,$2,$3,FALSE) ON CONFLICT(tenant,"user",session) DO NOTHING`, s.TenantID, s.UserID, s.SessionID); err != nil {
		return fmt.Errorf("artifacts/postgres: acquire scope: %w", err)
	}
	var fenced bool
	if err := tx.QueryRowContext(ctx, `SELECT fenced FROM artifact_scope_fences WHERE tenant=$1 AND "user"=$2 AND session=$3 FOR UPDATE`, s.TenantID, s.UserID, s.SessionID).Scan(&fenced); err != nil {
		return fmt.Errorf("artifacts/postgres: check scope: %w", err)
	}
	if fenced {
		return artifacts.ErrScopeFenced
	}
	return nil
}

var _ artifacts.ScopeFencer = (*driver)(nil)
