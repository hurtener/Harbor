package pauseresume

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
)

// identityFromContext reads the resuming caller's identity triple from
// ctx. It prefers a full Quadruple (Identity + RunID) and falls back
// to a bare Identity. A context carrying neither — or one whose triple
// is incomplete — fails closed with ErrIdentityRequired (CLAUDE.md §6
// rule 9 + D-001: identity is mandatory; there is no opt-out knob).
//
// Used by Resume to validate the resuming scope against the pause's
// recorded scope. Request takes its identity from PauseRequest.Identity
// directly rather than ctx — the pause is recorded under the run's
// identity, which the caller passes explicitly.
func identityFromContext(ctx context.Context) (identity.Identity, error) {
	if q, ok := identity.QuadrupleFrom(ctx); ok {
		if err := identity.Validate(q.Identity); err != nil {
			return identity.Identity{}, fmt.Errorf("%w: %w", ErrIdentityRequired, err)
		}
		return q.Identity, nil
	}
	if id, ok := identity.From(ctx); ok {
		if err := identity.Validate(id); err != nil {
			return identity.Identity{}, fmt.Errorf("%w: %w", ErrIdentityRequired, err)
		}
		return id, nil
	}
	return identity.Identity{}, fmt.Errorf("%w: context carries no identity", ErrIdentityRequired)
}

// runIDFromContext reads the per-execution RunID from ctx when a full
// Quadruple is present. Returns "" when ctx carries only a bare
// Identity (or no identity at all) — an empty RunID is acceptable for
// a session-scoped pause (e.g. a pre-run approval gate), matching
// state.StateStore's "empty RunID is allowed for session-scoped
// state" contract.
func runIDFromContext(ctx context.Context) string {
	if q, ok := identity.QuadrupleFrom(ctx); ok {
		return q.RunID
	}
	return ""
}
