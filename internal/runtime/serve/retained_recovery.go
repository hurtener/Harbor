package serve

import (
	"context"
	"errors"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/identity"
	sessionmemory "github.com/hurtener/Harbor/internal/memory/session"
	sessionsprotocol "github.com/hurtener/Harbor/internal/sessions/protocol"
	"github.com/hurtener/Harbor/internal/state"
)

// retainedContextReconciler is the thin served consumer of the same primitive
// used by embedded runs. It owns no mutable state or new recovery machinery.
type retainedContextReconciler struct {
	store    state.StateStore
	redactor audit.Redactor
	turns    int
}

func (r retainedContextReconciler) ReconcileContext(ctx context.Context, id identity.Identity, run string) error {
	err := sessionmemory.ReconcileRetainedRun(ctx, r.store, r.redactor, identity.Quadruple{Identity: id, RunID: run}, r.turns, nil)
	switch {
	case errors.Is(err, sessionmemory.ErrRetainedContextUnsettled):
		return sessionsprotocol.ErrContextUnsettled
	case errors.Is(err, sessionmemory.ErrRetainedContextUnavailable):
		return sessionsprotocol.ErrContextUnavailable
	default:
		return err
	}
}
