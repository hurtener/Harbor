package protocol

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// emitAdminAudit publishes EXACTLY ONE canonical audit.admin_scope_used
// event for a granted widening, before the read reaches storage. The
// envelope is anchored on the ACTOR's verified identity (the caller who
// performed the widened read); the payload describes the GRANTED target.
//
// Target semantics (mirroring the established widened-read audit shape):
//
//   - Tenant elision is NOT a wildcard: an empty tenant filter reads the
//     caller's own tenant (the fold), so the payload records the caller's
//     tenant there.
//   - User / session elision IS the canonical wildcard fan-in spelling: a
//     blank component records that the read fanned in across the axis,
//     and recording only the first member of a multi-principal read would
//     make it look narrower than it was.
//
// Every audit-visible field runs through the wired Redactor BEFORE the
// publish (CLAUDE.md §7 rule 6) even though AdminScopeUsedPayload is a
// SafePayload by construction — the redactor pass is mandatory regardless.
// A redaction or emit failure fails the read loud with ErrAuditFailed: the
// read already crossed the identity boundary, so the operator MUST see the
// audit (CLAUDE.md §13 — the admin action is never fully silent).
func (s *Service) emitAdminAudit(ctx context.Context, actor identity.Identity, f Filters) error {
	tenant := singleScopeValue(f.TenantIDs)
	if tenant == "" {
		tenant = actor.TenantID
	}
	user := singleScopeValue(f.UserIDs)
	session := singleScopeValue(f.SessionIDs)

	view := map[string]any{
		"actor_tenant":   actor.TenantID,
		"actor_user":     actor.UserID,
		"actor_session":  actor.SessionID,
		"target_tenant":  tenant,
		"target_user":    user,
		"target_session": session,
	}
	redacted, err := s.redactor.Redact(ctx, view)
	if err != nil {
		return fmt.Errorf("%w: redact admin-scope audit: %w", ErrAuditFailed, err)
	}
	redactedMap, ok := redacted.(map[string]any)
	if !ok {
		return fmt.Errorf("%w: redactor returned %T, want map[string]any", ErrAuditFailed, redacted)
	}
	if tenant, err = auditString(redactedMap, "target_tenant"); err != nil {
		return fmt.Errorf("%w: %w", ErrAuditFailed, err)
	}
	if user, err = auditString(redactedMap, "target_user"); err != nil {
		return fmt.Errorf("%w: %w", ErrAuditFailed, err)
	}
	if session, err = auditString(redactedMap, "target_session"); err != nil {
		return fmt.Errorf("%w: %w", ErrAuditFailed, err)
	}

	ev := events.Event{
		Type:     events.EventTypeAdminScopeUsed,
		Identity: identity.Quadruple{Identity: actor},
		Payload: events.AdminScopeUsedPayload{
			Tenant:  tenant,
			User:    user,
			Session: session,
		},
	}
	if err := s.audit(ctx, ev); err != nil {
		return fmt.Errorf("%w: %w", ErrAuditFailed, err)
	}
	s.logger.InfoContext(ctx, "observability.query widened read audited",
		"actor_tenant", actor.TenantID,
		"actor_user", actor.UserID,
		"actor_session", actor.SessionID,
		"target_tenant", tenant,
		"target_user", user,
		"target_session", session,
	)
	return nil
}
