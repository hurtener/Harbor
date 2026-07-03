// Package identity is the public SDK facade over Harbor's
// internal/identity package — the load-bearing (tenant, user,
// session) isolation triple plus the per-run Quadruple (RFC §3.6,
// §4). Alias-based re-exports only: no behavior lives here.
// What this package omits from the internal surface is deliberately
// private (the curation contract).
package identity

import (
	internal "github.com/hurtener/Harbor/internal/identity"
)

// Identity is the mandatory (TenantID, UserID, SessionID) isolation
// triple. Alias of the internal type; see internal/identity.
type Identity = internal.Identity

// Quadruple carries Identity plus the per-execution RunID. Alias of
// the internal type; see internal/identity.
type Quadruple = internal.Quadruple

// Re-exported sentinel errors callers compare via errors.Is.
var (
	// ErrIdentityMissing — the context carries no Identity and no Quadruple.
	ErrIdentityMissing = internal.ErrIdentityMissing
	// ErrIdentityIncomplete — one or more identity components are empty.
	ErrIdentityIncomplete = internal.ErrIdentityIncomplete
)

// Validate fails closed when any component of the triple is empty.
var Validate = internal.Validate

// With returns a child context carrying the validated Identity.
var With = internal.With

// WithRun returns a child context carrying the validated Identity
// plus the run ID (the Quadruple).
var WithRun = internal.WithRun

// From extracts the Identity from ctx, reporting presence.
var From = internal.From

// MustFrom extracts the Identity from ctx, panicking with
// ErrIdentityMissing when absent (handler-only).
var MustFrom = internal.MustFrom

// QuadrupleFrom extracts the Quadruple from ctx, reporting presence.
var QuadrupleFrom = internal.QuadrupleFrom

// MustQuadrupleFrom extracts the Quadruple from ctx, panicking with
// ErrIdentityMissing when absent (handler-only).
var MustQuadrupleFrom = internal.MustQuadrupleFrom
