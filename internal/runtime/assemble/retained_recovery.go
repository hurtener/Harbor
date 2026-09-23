package assemble

import (
	"context"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
)

// ErrRetainedContextUnavailable rejects disabled, expired, corrupt or stale recovery.
var ErrRetainedContextUnavailable = runctx.ErrRetainedContextUnavailable

// ErrRetainedContextUnsettled means a dispatch outcome must be reconciled with
// its owning service rather than retried or inferred from a process restart.
var ErrRetainedContextUnsettled = runctx.ErrRetainedContextUnsettled

// ReconcileRetainedContext explicitly recovers a fully settled source run as
// interrupted evidence for subsequent turns of the same session. Runtime-level
// retained context must be enabled. It fences future source dispatch, but never
// resumes a lost run, repeats a tool, or calls the completion hook. An in-flight
// or uncommitted external operation is refused as an unknown outcome. Already
// sealed context is idempotent while retained; expiry is never extended.
func (s *Stack) ReconcileRetainedContext(ctx context.Context, id identity.Identity, sourceRunID string) error {
	if s == nil || s.Cfg == nil || s.Cfg.Memory.RecentTurnsResolved() <= 0 {
		return runctx.ErrRetainedContextUnavailable
	}
	if err := identity.Validate(id); err != nil {
		return err
	}
	return runctx.ReconcileRetainedRun(ctx, s.State, s.Redactor, identity.Quadruple{Identity: id, RunID: sourceRunID}, s.Cfg.Memory.RecentTurnsResolved(), nil)
}
