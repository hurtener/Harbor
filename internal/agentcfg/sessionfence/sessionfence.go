// Package sessionfence defines the shared StateStore slots that fence a
// session-scoped agent-owned record against session erasure.
package sessionfence

import (
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
)

const (
	erasureAuditSession    = "<erasure-audit>"
	erasurePendingPrefix   = "session.erasure.pending."
	erasureTombstonePrefix = "session.erasure.tombstone."
)

// PendingSlot returns the erasure-in-progress fence slot for id.
func PendingSlot(id identity.Quadruple) (identity.Quadruple, string, error) {
	if err := identity.Validate(id.Identity); err != nil {
		return identity.Quadruple{}, "", fmt.Errorf("session fence pending slot: %w", err)
	}
	return auditScope(id), erasurePendingPrefix + id.SessionID, nil
}

// TombstoneSlot returns the terminal erasure fence slot for id.
func TombstoneSlot(id identity.Quadruple) (identity.Quadruple, string, error) {
	if err := identity.Validate(id.Identity); err != nil {
		return identity.Quadruple{}, "", fmt.Errorf("session fence tombstone slot: %w", err)
	}
	return auditScope(id), erasureTombstonePrefix + id.SessionID, nil
}

func auditScope(id identity.Quadruple) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: id.TenantID, UserID: id.UserID, SessionID: erasureAuditSession}}
}
