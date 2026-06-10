// approval_authorizer.go — the Protocol-side resolve authorizer
// (Phase 111f, D-203).
//
// Pre-111f the admin/console:fleet scope check lived INSIDE
// `internal/tools/approval` as a hard `internal/protocol/auth` import
// — the one standing violation of the D-193 direction rule (runtime
// packages import protocol TYPES only, never protocol auth / methods /
// transports). The check moves here, one-way: the server package
// (the Runtime's network surface) adapts the Protocol's verified
// scope claims onto the approval package's injected
// `ResolveAuthorizer` seam. The approval package no longer knows the
// Protocol's auth vocabulary exists.
package server

import (
	"context"
	"fmt"

	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/tools/approval"
)

// ProtocolScopeAuthorizer is the wire-side approval.ResolveAuthorizer:
// a resolving ctx that carries the verified `admin` OR `console:fleet`
// Protocol scope claim (Phase 61 JWT validation → Phase 54 edge) is
// authorized — exactly the acceptance the pre-111f in-gate check
// enforced, so wire behaviour is unchanged. Everything else delegates
// to Next (production wires `approval.NewIdentityAuthorizer()`), which
// is how the in-process steering bridge resolves without the deleted
// protocol-scope self-elevation.
//
// A nil Next is the strict wire-only posture: protocol scopes or
// nothing — fail closed with a wrapped approval.ErrResolveForbidden,
// byte-for-byte the pre-seam gate semantics.
//
// Stateless; safe for concurrent use (D-025).
type ProtocolScopeAuthorizer struct {
	// Next is the fallback authorizer consulted when ctx carries no
	// qualifying Protocol scope. Nil = no fallback (strict).
	Next approval.ResolveAuthorizer
}

// NewProtocolScopeAuthorizer constructs the wire-side authorizer with
// next as the non-Protocol fallback. Production gate assembly
// (`cmd/harbor`, `harbortest/devstack`) passes
// `approval.NewIdentityAuthorizer()` so both resolution paths work:
// Protocol-scoped callers keep today's admin/console:fleet acceptance;
// the runtime steering bridge resolves via the originating identity.
func NewProtocolScopeAuthorizer(next approval.ResolveAuthorizer) ProtocolScopeAuthorizer {
	return ProtocolScopeAuthorizer{Next: next}
}

// AuthorizeResolve implements approval.ResolveAuthorizer.
func (a ProtocolScopeAuthorizer) AuthorizeResolve(ctx context.Context, pending approval.PendingInfo) error {
	if protocolauth.HasScope(ctx, protocolauth.ScopeAdmin) ||
		protocolauth.HasScope(ctx, protocolauth.ScopeConsoleFleet) {
		return nil
	}
	if a.Next != nil {
		return a.Next.AuthorizeResolve(ctx, pending)
	}
	return fmt.Errorf("%w: admin or console:fleet scope required", approval.ErrResolveForbidden)
}
