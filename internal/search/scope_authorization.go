package search

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// ScopeDecision is the immutable result of evaluating one request's three
// identity axes against its verified caller and admin-tier claim.
type ScopeDecision struct {
	Elevated          bool
	CrossTenant       bool
	CrossUser         bool
	MultiSessionFanIn bool
}

// EvaluateScope evaluates and gates all identity axes once. It performs no
// audit emission; per-index callers use Deps.AuthorizeScope so permission and
// accountability stay coupled, while search.query reuses this pure edge gate
// before it fans out to the independently-audited indexes.
func EvaluateScope(ctx context.Context, checker ScopeChecker, caller identity.Identity, req types.SearchRequest) (ScopeDecision, error) {
	decision := ScopeDecision{
		CrossTenant:       CrossTenantRequested(caller.TenantID, req),
		CrossUser:         CrossUserRequested(caller.UserID, req),
		MultiSessionFanIn: CrossSessionFanInRequested(req),
	}
	decision.Elevated = checker(ctx)
	if decision.Elevated {
		return decision, nil
	}
	if decision.CrossTenant {
		return ScopeDecision{}, ErrCrossTenantRequiresAdmin
	}
	if decision.CrossUser {
		return ScopeDecision{}, ErrCrossUserRequiresAdmin
	}
	if decision.MultiSessionFanIn {
		return ScopeDecision{}, ErrCrossSessionRequiresAdmin
	}
	return decision, nil
}

// AuthorizeScope gates a per-index request and, for the sessions/tasks/
// artifacts indexes, emits exactly one canonical audit.admin_scope_used event
// before returning a granted widening. The events index is excluded because
// its Replayer already emits the same event when its Admin fan-in is used.
func (d Deps) AuthorizeScope(ctx context.Context, index types.SearchIndex, caller identity.Identity, req types.SearchRequest) (ScopeDecision, error) {
	decision, err := EvaluateScope(ctx, d.AdminScope, caller, req)
	if err != nil {
		return ScopeDecision{}, err
	}
	if index == types.SearchIndexEvents || !decision.widensRows(req) {
		return decision, nil
	}

	// The payload describes the granted target, while the event envelope
	// remains the verified actor. A blank target component is the canonical
	// wildcard/fan-in spelling: recording only the first member would make a
	// multi-principal read look narrower than it was. Tenant elision is not a
	// wildcard, so EffectiveTenantSet supplies the caller's tenant there.
	tenant := singleScopeValue(EffectiveTenantSet(caller.TenantID, req))
	user := singleScopeValue(req.Filter.UserIDs)
	session := singleScopeValue(req.Filter.SessionIDs)
	// Run every audit-visible field through the configured redactor before
	// publishing, even though AdminScopeUsedPayload is a SafePayload. This
	// mirrors the other producers of the canonical notice and preserves the
	// project-wide "every audit payload" boundary.
	redacted, err := d.Redactor.Redact(ctx, map[string]any{
		"actor_tenant":   caller.TenantID,
		"actor_user":     caller.UserID,
		"actor_session":  caller.SessionID,
		"target_tenant":  tenant,
		"target_user":    user,
		"target_session": session,
	})
	if err != nil {
		return ScopeDecision{}, fmt.Errorf("%w: index %q: redact admin-scope audit: %w", ErrAuditFailed, index, err)
	}
	redactedMap, ok := redacted.(map[string]any)
	if !ok {
		return ScopeDecision{}, fmt.Errorf("%w: index %q: redactor returned %T, want map[string]any", ErrAuditFailed, index, redacted)
	}
	tenant, err = auditString(redactedMap, "target_tenant")
	if err != nil {
		return ScopeDecision{}, fmt.Errorf("%w: index %q: %w", ErrAuditFailed, index, err)
	}
	user, err = auditString(redactedMap, "target_user")
	if err != nil {
		return ScopeDecision{}, fmt.Errorf("%w: index %q: %w", ErrAuditFailed, index, err)
	}
	session, err = auditString(redactedMap, "target_session")
	if err != nil {
		return ScopeDecision{}, fmt.Errorf("%w: index %q: %w", ErrAuditFailed, index, err)
	}
	ev := events.Event{
		Type:     events.EventTypeAdminScopeUsed,
		Identity: identity.Quadruple{Identity: caller},
		Payload: events.AdminScopeUsedPayload{
			Tenant:  tenant,
			User:    user,
			Session: session,
		},
	}
	if err := d.Audit(ctx, ev); err != nil {
		return ScopeDecision{}, fmt.Errorf("%w: index %q: %w", ErrAuditFailed, index, err)
	}
	return decision, nil
}

// widensRows reports actual tenant/user expansion for the three indexes whose
// admin path uses WidenedUserSet. An elevated elided user axis is a wildcard
// there and therefore is a widening even though CrossUserRequested is false.
func (d ScopeDecision) widensRows(req types.SearchRequest) bool {
	return d.Elevated && (d.CrossTenant || d.CrossUser || len(req.Filter.UserIDs) == 0)
}

func singleScopeValue(values []string) string {
	if len(values) == 1 {
		return values[0]
	}
	return ""
}

func auditString(redacted map[string]any, key string) (string, error) {
	value, ok := redacted[key]
	if !ok {
		return "", fmt.Errorf("redactor dropped audit field %q", key)
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("redactor changed audit field %q to %T", key, value)
	}
	return text, nil
}
