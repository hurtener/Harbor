package bodyscope

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
)

// ComponentRule declares how one component of a request body's identity
// triple is reconciled against the ctx-verified identity.
type ComponentRule uint8

const (
	// Pinned — the component MUST equal the verified identity's. An
	// empty component does not equal a populated verified one, so an
	// empty component on a partially-populated body is a mismatch. This
	// is the default and the posture of every component on every surface
	// that has no cross-identity read.
	Pinned ComponentRule = iota
	// PinnedOrEmpty — a populated component MUST equal the verified
	// identity's; an empty one is a wildcard the surface interprets
	// downstream (an artifacts list scoped to a whole tenant, a filter
	// that spans a user's sessions). The wildcard is left empty rather
	// than backfilled, so the surface's own scoping reads it as written.
	PinnedOrEmpty
	// AdminScoped — a component that differs from the verified
	// identity's is permitted ONLY when the caller holds
	// auth.ScopeAdmin or auth.ScopeConsoleFleet, and every permitted
	// divergence is recorded on the audit bus before it is granted. An
	// empty component is a wildcard, as with PinnedOrEmpty.
	AdminScoped
)

// String renders a ComponentRule for test failures and error detail.
func (r ComponentRule) String() string {
	switch r {
	case Pinned:
		return "pinned"
	case PinnedOrEmpty:
		return "pinned-or-empty"
	case AdminScoped:
		return "admin-scoped"
	default:
		return fmt.Sprintf("ComponentRule(%d)", uint8(r))
	}
}

// Policy is one Protocol surface's declared body-identity posture. It is
// a value in a closed registry, never constructed at a call site: the
// whole point of the registry is that a surface's posture is declared in
// one readable table rather than re-derived by whoever writes the next
// handler.
type Policy struct {
	// Surface is the registry key.
	Surface Surface
	// Wire is the operator-facing surface name that appears in the
	// Protocol error message. It names the surface, never the caller's
	// identity or the tenant it asked for.
	Wire string
	// Tenant, User, Session declare the per-component reconciliation
	// rule. The overwhelmingly common shape is all three Pinned.
	Tenant  ComponentRule
	User    ComponentRule
	Session ComponentRule
	// Grants names the claims that permit a crossing on this surface.
	// Empty means the admin-tier pair (`admin` + `console:fleet`) — the
	// common case, and the one the fleet-observation claim was minted
	// for. A surface whose crossing is strictly an administrative act
	// names `admin` alone, so a read-only fleet token cannot take it.
	Grants []auth.Scope
	// ScopeDeniedCode overrides the Protocol code returned when an
	// AdminScoped component diverges and the caller holds no admin-tier
	// claim. Empty means CodeScopeMismatch. A surface whose deny path
	// must not confirm that the named scope exists sets its own code.
	ScopeDeniedCode protoerrors.Code
	// PinnedDeniedCode overrides the Protocol code returned when a Pinned
	// component diverges — the flat refusal, which no claim reopens.
	// Empty means CodeIdentityRequired.
	//
	// A surface sets this when its flat refusal already has a published
	// answer that differs. The gate runs EARLIER than the surface checks
	// it replaces, and moving a refusal earlier must not change the code a
	// client branches on.
	PinnedDeniedCode protoerrors.Code
	// Reason documents why the surface holds this posture. It is the
	// answer a reviewer needs when a policy row looks surprising, and it
	// is the thing that used to live in a copied code comment.
	Reason string
}

// PermitsCrossIdentity reports whether the policy can grant a request
// that names an identity component other than the verified one. A policy
// that can MUST be supplied an Auditor at every Reconcile call site —
// the permission and the accountability travel together.
func (p Policy) PermitsCrossIdentity() bool {
	return p.Tenant == AdminScoped || p.User == AdminScoped || p.Session == AdminScoped
}

// grants returns the claim set that permits a crossing on this surface,
// defaulting to the admin-tier pair.
func (p Policy) grants() []auth.Scope {
	if len(p.Grants) > 0 {
		return p.Grants
	}
	return []auth.Scope{auth.ScopeAdmin, auth.ScopeConsoleFleet}
}

// Granted reports whether ctx carries a claim this surface accepts for a
// crossing. Scopes are write-once at the request edge and absent reads as
// denied, so this is fail-closed by construction.
func (p Policy) Granted(ctx context.Context) bool {
	for _, s := range p.grants() {
		if auth.HasScope(ctx, s) {
			return true
		}
	}
	return false
}

// grantsPhrase renders the surface's claim set for the refusal message,
// so a caller is told which claim it lacks rather than a generic pair.
func (p Policy) grantsPhrase() string {
	names := p.grants()
	if len(names) == 1 {
		return fmt.Sprintf("the `%s` scope claim", names[0])
	}
	out := fmt.Sprintf("the `%s`", names[0])
	for _, n := range names[1 : len(names)-1] {
		out += fmt.Sprintf(", `%s`", n)
	}
	return out + fmt.Sprintf(" or `%s` scope claim", names[len(names)-1])
}

// wireName returns the operator-facing surface name, falling back to the
// registry key so an error is never anonymous.
func (p Policy) wireName() string {
	if p.Wire != "" {
		return p.Wire
	}
	return string(p.Surface)
}

// scopeDeniedCode returns the Protocol code for "this divergence needs an
// admin-tier claim and the caller has none".
func (p Policy) scopeDeniedCode() protoerrors.Code {
	if p.ScopeDeniedCode != "" {
		return p.ScopeDeniedCode
	}
	return protoerrors.CodeScopeMismatch
}

// pinnedDeniedCode returns the Protocol code for a flat refusal — a
// component no claim reopens.
func (p Policy) pinnedDeniedCode() protoerrors.Code {
	if p.PinnedDeniedCode != "" {
		return p.PinnedDeniedCode
	}
	return protoerrors.CodeIdentityRequired
}

// reconcileComponent applies one component's rule. It returns the value
// to write back, whether the component crossed an identity boundary
// under an admin-tier claim, and a terminal Protocol error.
func (p Policy) reconcileComponent(
	name string,
	rule ComponentRule,
	body, verified string,
	granted bool,
) (string, bool, *protoerrors.Error) {
	switch {
	case body == verified:
		return body, false, nil
	case body == "" && rule != Pinned:
		// A surface-interpreted wildcard. Left empty on purpose — the
		// surface's own scoping reads "unset" as "every value I am
		// allowed to see".
		return body, false, nil
	case rule == AdminScoped:
		if !granted {
			return body, false, protoerrors.Newf(p.scopeDeniedCode(),
				"%s: naming a %s other than the verified one requires %s",
				p.wireName(), name, p.grantsPhrase())
		}
		return body, true, nil
	default:
		return body, false, protoerrors.Newf(p.pinnedDeniedCode(),
			"%s: body identity scope does not match the verified identity", p.wireName())
	}
}
