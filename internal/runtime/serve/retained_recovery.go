package serve

import (
	"context"
	"errors"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
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
	err := runctx.ReconcileRetainedRun(ctx, r.store, r.redactor, identity.Quadruple{Identity: id, RunID: run}, r.turns, nil)
	switch {
	case errors.Is(err, runctx.ErrRetainedContextUnsettled):
		return sessionsprotocol.ErrContextUnsettled
	case errors.Is(err, runctx.ErrRetainedContextUnavailable):
		return sessionsprotocol.ErrContextUnavailable
	default:
		return err
	}
}
