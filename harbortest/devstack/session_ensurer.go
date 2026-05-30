// harbortest/devstack/session_ensurer.go — adapts the concrete
// sessions.Registry to the protocol.SessionEnsurer seam (D-171).
//
// Mirrors `cmd/harbor/session_ensurer.go` field-for-field (D-094
// source-of-truth invariant): the production dev boot and the
// production-mirroring fixture wire the SAME create-on-first-use
// behaviour, so an integration test against the devstack exercises the
// exact path `harbor dev` runs.
package devstack

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/sessions"
)

// sessionEnsurer wraps a *sessions.Registry as a
// protocol.SessionEnsurer, translating the registry's lifecycle
// sentinels into the protocol-side sentinels the ControlSurface's error
// mapper understands.
type sessionEnsurer struct {
	reg *sessions.Registry
}

// newSessionEnsurer builds the adapter over a constructed registry.
func newSessionEnsurer(reg *sessions.Registry) *sessionEnsurer {
	return &sessionEnsurer{reg: reg}
}

// EnsureSession implements protocol.SessionEnsurer.
func (a *sessionEnsurer) EnsureSession(ctx context.Context, ident identity.Identity) error {
	_, err := a.reg.EnsureOpen(ctx, ident)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sessions.ErrReopenAfterClose):
		return fmt.Errorf("%w: %w", protocol.ErrSessionReopenAfterClose, err)
	case errors.Is(err, sessions.ErrSessionIDReuse):
		return fmt.Errorf("%w: %w", protocol.ErrSessionIDReuse, err)
	case errors.Is(err, identity.ErrIdentityIncomplete):
		return err
	default:
		return fmt.Errorf("session ensure: %w", err)
	}
}

var _ protocol.SessionEnsurer = (*sessionEnsurer)(nil)
