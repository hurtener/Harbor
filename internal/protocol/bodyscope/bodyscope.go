// Package bodyscope is the Harbor Protocol's single body-identity gate.
//
// # The problem it owns
//
// Every Protocol method carries an identity scope in its request body.
// That scope is caller-supplied, so it is an input, not an authority:
// the authority is the verified identity the transport established at
// the request edge (identity.FromVerified). Reconcile is the one place
// the two meet.
//
// # The contract
//
// For a request body scope B and the ctx-verified identity V:
//
//   - B's User and Session equal V's unconditionally. No scope claim
//     widens them; a surface that wants a broader read expresses it in a
//     filter field, never by renaming the caller.
//   - An entirely empty B is backfilled from V. Echoing the verified
//     triple into the body is a client convenience, not a requirement.
//   - B's Tenant equals V's, EXCEPT on a surface whose registered policy
//     declares that component admin-scoped. There, a differing value is
//     permitted only when the caller holds auth.ScopeAdmin or
//     auth.ScopeConsoleFleet, and every permitted crossing publishes an
//     `audit.admin_scope_used` event naming the ctx-verified actor.
//     Without the claim the request fails with CodeScopeMismatch (or the
//     surface's declared deny code, for a surface whose deny path must
//     not confirm that the named scope exists).
//   - When ctx carries NO verified identity there is nothing to
//     reconcile against, so the request fails closed with
//     CodeIdentityRequired. Trusting the body in that case would make a
//     transport that forgot its auth middleware indistinguishable from
//     one that ran it.
//
// # Why the policy is a registry key, not a parameter
//
// Reconcile takes a Surface, not a Policy value: a caller cannot invent
// a posture at the call site, only name one the registry already
// declares. The registry is closed and lockstep-tested — every canonical
// Protocol request type that carries a body identity scope must join to
// a registered Surface, so a new surface is either registered or fails
// `go test`. See registry.go.
//
// # Why the audit sink is a parameter
//
// A surface whose policy permits a tenant crossing MUST pass a non-nil
// Auditor. The permission and the accountability are one argument list
// apart, so the two cannot drift: a tenant-permissive policy with no
// audit sink is refused at run time with CodeRuntimeError, the same
// fail-closed posture the impersonation gate holds. A surface whose
// policy pins the tenant needs no Auditor and may pass nil.
package bodyscope

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// ScopeRef is a mutable handle on a request body's identity triple.
// Wire bodies carry the triple in more than one shape (types.IdentityScope
// on most requests, types.ArtifactScope on the artifacts cluster), so
// Reconcile reads and writes through this handle rather than switching on
// the concrete request type.
//
// Adapters for the canonical shapes are ForIdentityScope and
// ForArtifactScope. A surface with a bespoke body shape supplies its own;
// the handle is three strings, so an adapter is mechanical.
type ScopeRef interface {
	// Triple returns the body's (tenant, user, session).
	Triple() (tenant, user, session string)
	// SetTriple writes the reconciled (tenant, user, session) back into
	// the body so everything downstream reads the reconciled value.
	SetTriple(tenant, user, session string)
}

// identityScopeRef adapts *types.IdentityScope to ScopeRef.
type identityScopeRef struct{ s *types.IdentityScope }

func (r identityScopeRef) Triple() (string, string, string) {
	return r.s.Tenant, r.s.User, r.s.Session
}

func (r identityScopeRef) SetTriple(tenant, user, session string) {
	r.s.Tenant, r.s.User, r.s.Session = tenant, user, session
}

// ForIdentityScope returns a ScopeRef over a request body's
// types.IdentityScope. The pointer must be non-nil.
func ForIdentityScope(s *types.IdentityScope) ScopeRef { return identityScopeRef{s: s} }

// artifactScopeRef adapts *types.ArtifactScope to ScopeRef. The Task
// component is outside the isolation triple and is left untouched.
type artifactScopeRef struct{ s *types.ArtifactScope }

func (r artifactScopeRef) Triple() (string, string, string) {
	return r.s.Tenant, r.s.User, r.s.Session
}

func (r artifactScopeRef) SetTriple(tenant, user, session string) {
	r.s.Tenant, r.s.User, r.s.Session = tenant, user, session
}

// ForArtifactScope returns a ScopeRef over a request body's
// types.ArtifactScope. The pointer must be non-nil.
func ForArtifactScope(s *types.ArtifactScope) ScopeRef { return artifactScopeRef{s: s} }

// Reconcile is the single body-identity gate. It reconciles ref against
// the ctx-verified identity under the policy registered for surface,
// writes the reconciled triple back through ref, and returns the ctx the
// caller must use for everything downstream.
//
// The returned ctx carries an audited tenant elevation when — and only
// when — the policy permitted a tenant crossing and aud recorded it. A
// later Reconcile on the same request (a surface re-running the gate
// behind its transport) reads that marker and does not re-record the
// same crossing.
//
// aud MUST be non-nil when the registered policy permits an identity
// crossing; a nil sink on such a surface is refused with
// CodeRuntimeError rather than silently accepted.
//
// A nil *protoerrors.Error return means the request is reconciled and
// may proceed. Any non-nil return is terminal: the caller writes it and
// does not dispatch.
func Reconcile(ctx context.Context, ref ScopeRef, surface Surface, aud Auditor) (context.Context, *protoerrors.Error) {
	policy, ok := PolicyFor(surface)
	if !ok {
		// An unregistered surface is a construction bug, not a client
		// error: the registry is closed and lockstep-tested. Fail loud.
		return ctx, protoerrors.Newf(protoerrors.CodeRuntimeError,
			"body-scope surface %q is not registered; the Protocol's body-identity policy registry is the single source of surface postures", surface)
	}
	if ref == nil {
		return ctx, protoerrors.Newf(protoerrors.CodeRuntimeError,
			"body-scope surface %q: no identity scope handle supplied", surface)
	}
	if policy.PermitsCrossIdentity() && aud == nil {
		// The permission and the accountability ship together or not at
		// all — the same precondition the impersonation gate holds.
		return ctx, protoerrors.Newf(protoerrors.CodeRuntimeError,
			"body-scope surface %q permits an audited identity crossing but no audit sink is wired; refusing fail-closed", surface)
	}

	verified, ok := identity.FromVerified(ctx)
	if !ok {
		// Nothing to reconcile against. The body is caller-supplied, so
		// trusting it here would make a request that reached the surface
		// without an established identity indistinguishable from one
		// that carried a verified triple.
		return ctx, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"%s: the request carries no verified identity to reconcile its body identity scope against", policy.wireName())
	}

	tenant, user, session := ref.Triple()

	// An entirely empty body triple is the client saying "use my verified
	// identity". Backfill it and stop — there is nothing to disagree with.
	if tenant == "" && user == "" && session == "" {
		ref.SetTriple(verified.TenantID, verified.UserID, verified.SessionID)
		return ctx, nil
	}

	granted := policy.Granted(ctx)
	crossed := false
	for _, c := range []struct {
		name     string
		rule     ComponentRule
		body     *string
		verified string
	}{
		{"tenant", policy.Tenant, &tenant, verified.TenantID},
		{"user", policy.User, &user, verified.UserID},
		{"session", policy.Session, &session, verified.SessionID},
	} {
		value, didCross, perr := policy.reconcileComponent(c.name, c.rule, *c.body, c.verified, granted)
		if perr != nil {
			return ctx, perr
		}
		*c.body = value
		crossed = crossed || didCross
	}

	ref.SetTriple(tenant, user, session)
	if !crossed {
		return ctx, nil
	}

	effective := identity.Identity{
		TenantID:  firstNonEmpty(tenant, verified.TenantID),
		UserID:    firstNonEmpty(user, verified.UserID),
		SessionID: firstNonEmpty(session, verified.SessionID),
	}
	// A permitted crossing is recorded before it is granted. A surface
	// re-running the gate behind the transport that fronted it sees the
	// crossing ALREADY GRANTED TO THIS TENANT and does not duplicate the
	// record. The comparison is against the audited tenant, not a bare
	// "already elevated" flag: a crossing to a second, DIFFERENT tenant
	// is a second decision and gets its own record.
	if elevated, ok := identity.ElevatedTenant(ctx); ok && elevated == effective.TenantID {
		return ctx, nil
	}
	reason := fmt.Sprintf("%s: cross-identity request under an admin-tier scope claim", policy.wireName())
	aud.AdminScopeUsed(ctx, Elevation{
		Surface: surface,
		Actor:   verified,
		Target:  effective,
		Reason:  reason,
	})
	elevated, err := identity.WithElevated(ctx, effective, reason)
	if err != nil {
		return ctx, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"%s: identity scope incomplete: %v", policy.wireName(), err)
	}
	return elevated, nil
}

// firstNonEmpty returns a when it is non-empty, else b. A reconciled
// wildcard component carries no value of its own, so the elevated
// working identity inherits the verified one for that component.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Elevated reports whether ctx carries an audited tenant crossing minted
// by Reconcile. Surfaces consult it to widen a read that would otherwise
// be pinned to the verified tenant.
func Elevated(ctx context.Context) bool { return identity.IsElevated(ctx) }
