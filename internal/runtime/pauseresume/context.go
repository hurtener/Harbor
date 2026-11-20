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
// rule 9: identity is mandatory; there is no opt-out knob).
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

// Decider names the verified principal whose control decided a pause:
// the identity triple the deciding request authenticated as (the acting
// user when a session participant decides inside another user's
// session) plus the issuer-signed display label, when the bearer carried
// one. It is attribution metadata only — never an authorisation input
// and never an isolation key; Resume still scopes by the pause's own
// triple. The zero value means "no verified principal": a timeout, a
// runtime-restart resolution, or an in-process resolver that supplied
// none.
type Decider struct {
	TenantID  string
	UserID    string
	SessionID string
	Speaker   string
}

// IsZero reports whether no principal is named.
func (d Decider) IsZero() bool { return d == Decider{} }

type deciderKey struct{}

// WithDecider seats the deciding principal on ctx. The steering edge
// seats it from the verified caller of an approve / reject / resume
// control before resolving the pause, so Coordinator.Resume and the
// approval gate can attribute the decision. A zero Decider leaves ctx
// unchanged.
func WithDecider(ctx context.Context, d Decider) context.Context {
	if d.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, deciderKey{}, d)
}

// DeciderFrom returns the deciding principal seated on ctx, or the zero
// Decider when none was seated.
func DeciderFrom(ctx context.Context) Decider {
	d, _ := ctx.Value(deciderKey{}).(Decider)
	return d
}

// writerFromContext returns the session participant seated on ctx, or ""
// when none is seated.
func writerFromContext(ctx context.Context) string {
	if p, ok := identity.ParticipantFrom(ctx); ok {
		return p.UserID
	}
	return ""
}
