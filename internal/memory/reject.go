package memory

import (
	"context"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// EmitIdentityRejected publishes the `memory.identity_rejected`
// event on `bus` and returns the wrapped sentinel error. Drivers
// call this from every method that detected an identity-rejection
// so the audit emit path is consistent across implementations.
//
// The bus's `ValidateEvent` requires a fully-populated identity
// triple; the partial / empty triple the caller supplied is
// preserved where present and substituted with the
// `missingIdentitySentinel` string elsewhere, so the rejection
// event itself is bus-publishable. The audit-visible payload's
// `Reason` field names the component(s) that were actually missing.
//
// `operation` is the rejected method name ("AddTurn",
// "GetLLMContext", etc.). The reason string is computed from
// whichever components were missing on the supplied quadruple.
//
// Returns the wrapped `ErrIdentityRequired` for the caller to
// surface. If Publish fails, the wrapped error names both the
// rejection cause and the bus failure — callers MUST NOT silently
// drop either.
func EmitIdentityRejected(ctx context.Context, bus events.EventBus, q identity.Quadruple, operation string) error {
	reason := identityRejectionReason(q)
	payload := MemoryIdentityRejectedPayload{
		Operation: operation,
		Reason:    reason,
	}

	// Substitute missing components so ValidateEvent's identity-
	// triple check passes. The Reason field names the truly missing
	// component(s); the Identity field shows the substituted form
	// the bus accepted.
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
		Type:       EventTypeMemoryIdentityRejected,
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

// identityRejectionReason names the missing components on q. Used
// to populate the payload's Reason field and the returned error's
// message. Deterministic ordering so tests can pin the string.
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
		// Defensive: this helper should only be called when at least
		// one component is missing. Returning a stable string keeps
		// the rejection path observable instead of silently degrading.
		return "identity components missing (none detected)"
	case 1:
		return missing[0] + " empty"
	case 2:
		return missing[0] + " and " + missing[1] + " empty"
	default:
		return missing[0] + ", " + missing[1] + " and " + missing[2] + " empty"
	}
}
