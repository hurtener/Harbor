package skills

import (
	"context"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// EmitIdentityRejected publishes the `skill.identity_rejected`
// event on `bus` and returns the wrapped `ErrIdentityRequired`.
// Drivers call this from every method that detected an identity-
// rejection so the audit emit path is consistent across
// implementations.
//
// The bus's `ValidateEvent` requires a fully-populated identity
// triple; the partial / empty triple the caller supplied is
// preserved where present and substituted with the
// `missingIdentitySentinel` string elsewhere, so the rejection
// event itself is bus-publishable. The audit-visible payload's
// `Reason` field names the component(s) that were actually missing.
//
// `operation` is the rejected method name ("Upsert", "Get", "Search",
// "Delete", "List", "Close").
//
// If Publish fails, the wrapped error names both the rejection
// cause and the bus failure — callers MUST NOT silently drop either.
func EmitIdentityRejected(ctx context.Context, bus events.EventBus, q identity.Quadruple, operation string) error {
	reason := identityRejectionReason(q)
	payload := SkillIdentityRejectedPayload{
		Operation: operation,
		Reason:    reason,
	}

	emitID := q
	if emitID.TenantID == "" {
		emitID.TenantID = missingIdentitySentinel
	}
	if emitID.UserID == "" {
		emitID.UserID = missingIdentitySentinel
	}
	if emitID.SessionID == "" {
		emitID.SessionID = missingIdentitySentinel
	}

	ev := events.Event{
		Type:       EventTypeSkillIdentityRejected,
		Identity:   emitID,
		OccurredAt: time.Now(),
		Payload:    payload,
	}
	if pubErr := bus.Publish(ctx, ev); pubErr != nil {
		return fmt.Errorf("%w: %s (audit emit failed: %v)",
			ErrIdentityRequired, reason, pubErr)
	}
	return fmt.Errorf("%w: %s", ErrIdentityRequired, reason)
}

// identityRejectionReason names the missing components on q.
// Deterministic ordering so tests can pin the string.
func identityRejectionReason(q identity.Quadruple) string {
	missing := make([]string, 0, 3)
	if q.TenantID == "" {
		missing = append(missing, "tenant_id")
	}
	if q.UserID == "" {
		missing = append(missing, "user_id")
	}
	if q.SessionID == "" {
		missing = append(missing, "session_id")
	}
	switch len(missing) {
	case 0:
		return "identity components missing (none detected)"
	case 1:
		return missing[0] + " empty"
	case 2:
		return missing[0] + " and " + missing[1] + " empty"
	default:
		return missing[0] + ", " + missing[1] + " and " + missing[2] + " empty"
	}
}
