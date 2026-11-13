// cmd/harbor/session_ensurer.go — adapts the concrete sessions.Registry
// to the protocol.SessionEnsurer seam.
//
// The Protocol ControlSurface owns the create-on-first-use behaviour on
// `start` but must not import the sessions package (it depends only on
// the error-only `protocol.SessionEnsurer` interface). This adapter
// lives at the cmd/harbor wiring boundary — the one place allowed to
// know both the concrete registry and the Protocol surface — and
// translates the registry's lifecycle sentinels into the protocol-side
// sentinels the surface's error mapper understands.
package serve

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/sessions"
)

// SessionEnsurerAdapter wraps a *sessions.Registry as a
// protocol.SessionEnsurer. Immutable after construction; the wrapped
// registry is itself concurrency-safe, so the adapter is too.
type SessionEnsurerAdapter struct {
	reg *sessions.Registry
}

// NewSessionEnsurerAdapter builds the adapter. A nil registry would be a
// wiring bug; the caller (cmd_dev bootDevStack) always passes the
// constructed registry.
func NewSessionEnsurerAdapter(reg *sessions.Registry) *SessionEnsurerAdapter {
	return &SessionEnsurerAdapter{reg: reg}
}

// EnsureSession implements protocol.SessionEnsurer. It calls the
// registry's create-on-first-use EnsureOpen and translates the
// registry's sentinels into the protocol-side sentinels so the
// ControlSurface's mapSessionEnsureError reaches a stable Protocol code.
// An unclassified error is wrapped (not swallowed) so the surface's
// catch-all maps it to CodeRuntimeError (CLAUDE.md §5 — fail loud).
//
// A session participant acting inside another user's partition never
// creates a session: the session must already exist (open or closed) in
// that partition, otherwise ErrSessionParticipantNoSession.
func (a *SessionEnsurerAdapter) EnsureSession(ctx context.Context, ident identity.Identity) error {
	if p, ok := identity.ParticipantFrom(ctx); ok && p.IsGuest(ident) {
		if err := a.reg.RequireExisting(ctx, ident); err != nil {
			if errors.Is(err, sessions.ErrSessionNotFound) {
				return fmt.Errorf("%w: %w", protocol.ErrSessionParticipantNoSession, err)
			}
			return fmt.Errorf("session ensure: %w", err)
		}
	}
	_, err := a.reg.EnsureOpen(ctx, ident)
	switch {
	case err == nil:
		// Fresh create OR a re-activated closed session (RFC §6.9 amended):
		// EnsureOpen returns the resumed session, no error.
		return nil
	case errors.Is(err, sessions.ErrReopenAfterErase):
		return fmt.Errorf("%w: %w", protocol.ErrSessionReopenAfterErase, err)
	case errors.Is(err, sessions.ErrSessionIDReuse):
		return fmt.Errorf("%w: %w", protocol.ErrSessionIDReuse, err)
	case errors.Is(err, identity.ErrIdentityIncomplete):
		// Pass the identity sentinel through unwrapped so the surface maps
		// it to CodeIdentityRequired.
		return err
	default:
		return fmt.Errorf("session ensure: %w", err)
	}
}

// Compile-time assertion: the adapter satisfies the Protocol seam.
var _ protocol.SessionEnsurer = (*SessionEnsurerAdapter)(nil)
