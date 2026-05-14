package steering

import (
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
)

// Scope is the privilege tier a steering caller presents. It is a
// trust-based claim in Phase 52 — cryptographic verification arrives
// with Protocol auth (Phase 61), mirroring the events package's
// Admin-claim posture (RFC §6.3 + events.Filter.Admin). The Protocol
// edge derives the Scope from the caller's JWT scope claim before
// calling CheckScope.
type Scope string

// The three canonical caller scopes (RFC §6.3 per-event scope
// mapping). They form a total order: ScopeSessionUser < ScopeOwnerUser
// < ScopeAdmin. A caller presenting a higher scope satisfies any
// requirement a lower scope satisfies.
const (
	// ScopeSessionUser — a user authenticated into the session the
	// run belongs to. The weakest steering scope. Sufficient for
	// INJECT_CONTEXT and USER_MESSAGE.
	ScopeSessionUser Scope = "session_user"
	// ScopeOwnerUser — the user who owns the agent / run (the
	// "originating user"). Sufficient for REDIRECT and the
	// originating-user-or-admin controls (CANCEL, PAUSE, RESUME,
	// APPROVE, REJECT).
	ScopeOwnerUser Scope = "owner_user"
	// ScopeAdmin — an administrator. Sufficient for every control
	// type, including PRIORITIZE (admin-only) and any cross-tenant
	// steering.
	ScopeAdmin Scope = "admin"
)

// scopeRank maps a Scope to its position in the total order. A higher
// rank satisfies any lower-or-equal requirement.
var scopeRank = map[Scope]int{
	ScopeSessionUser: 1,
	ScopeOwnerUser:   2,
	ScopeAdmin:       3,
}

// IsValidScope reports whether s is one of the three canonical scopes.
func IsValidScope(s Scope) bool {
	_, ok := scopeRank[s]
	return ok
}

// requiredScope maps each control type to the minimum caller Scope
// that may submit it (RFC §6.3 "Steering authn/authz", resolving
// brief 02 Q-3):
//
//   - CANCEL, APPROVE, REJECT, PAUSE, RESUME — "the originating
//     user/admin scope" → ScopeOwnerUser (admin satisfies it by
//     rank).
//   - INJECT_CONTEXT, USER_MESSAGE — "the session-scoped user" →
//     ScopeSessionUser.
//   - PRIORITIZE — "admin" → ScopeAdmin.
//   - REDIRECT — "the user (the agent's owner)" → ScopeOwnerUser.
//
// Cross-tenant steering ("requires admin") is enforced separately in
// CheckScope: when the caller's tenant differs from the run's tenant,
// ScopeAdmin is required regardless of the per-type minimum.
var requiredScope = map[ControlType]Scope{
	ControlInjectContext: ScopeSessionUser,
	ControlUserMessage:   ScopeSessionUser,
	ControlRedirect:      ScopeOwnerUser,
	ControlCancel:        ScopeOwnerUser,
	ControlPause:         ScopeOwnerUser,
	ControlResume:        ScopeOwnerUser,
	ControlApprove:       ScopeOwnerUser,
	ControlReject:        ScopeOwnerUser,
	ControlPrioritize:    ScopeAdmin,
}

// RequiredScope returns the minimum caller Scope that may submit a
// control of type t (RFC §6.3). The bool is false when t is not a
// canonical control type.
func RequiredScope(t ControlType) (Scope, bool) {
	s, ok := requiredScope[t]
	return s, ok
}

// CheckScope enforces the RFC §6.3 per-event scope mapping for one
// steering submission. It fails closed:
//
//   - an unknown control type → ErrUnknownControlType;
//   - an unrecognised caller scope → ErrInvalidScope;
//   - a cross-tenant submission (callerTenant != run tenant) by a
//     non-admin caller → ErrScopeMismatch ("Cross-tenant steering
//     requires admin" — RFC §6.3);
//   - a caller scope below the control type's per-type minimum →
//     ErrScopeMismatch.
//
// callerScope is the (trust-based at Phase 52) Scope the Protocol
// edge derived from the caller's JWT. callerTenant is the tenant the
// caller authenticated under; runIdentity is the run the steering
// targets. CheckScope is pure and holds no state — safe for
// concurrent use (D-025).
func CheckScope(t ControlType, callerScope Scope, callerTenant string, runIdentity identity.Quadruple) error {
	if !IsValidControlType(t) {
		return fmt.Errorf("%w: %q", ErrUnknownControlType, string(t))
	}
	callerRank, ok := scopeRank[callerScope]
	if !ok {
		return fmt.Errorf("%w: %q", ErrInvalidScope, string(callerScope))
	}

	// Cross-tenant steering requires admin, regardless of the
	// per-type minimum (RFC §6.3). An empty callerTenant is treated
	// as a mismatch — the Protocol edge always supplies the
	// authenticated tenant; a blank one is a misconfiguration that
	// fails closed.
	if callerTenant != runIdentity.TenantID && callerScope != ScopeAdmin {
		return fmt.Errorf("%w: cross-tenant steering (caller tenant %q, run tenant %q) requires the admin scope",
			ErrScopeMismatch, callerTenant, runIdentity.TenantID)
	}

	minScope := requiredScope[t]
	if callerRank < scopeRank[minScope] {
		return fmt.Errorf("%w: control %q requires at least scope %q, caller presented %q",
			ErrScopeMismatch, string(t), string(minScope), string(callerScope))
	}
	return nil
}
