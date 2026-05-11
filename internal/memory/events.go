package memory

import (
	"github.com/hurtener/Harbor/internal/events"
)

// EventTypeMemoryIdentityRejected is emitted on the events bus when
// a `MemoryStore` method is called with a missing identity triple
// (D-001 fail-closed contract). The store ALSO returns
// `ErrIdentityRequired`; the bus emit makes the rejection
// observable from the Console / audit pipeline.
//
// Registered via `events.RegisterEventType` from this package's
// `init()` so `Publish` accepts the type without
// `ErrUnknownEventType`.
const EventTypeMemoryIdentityRejected events.EventType = "memory.identity_rejected"

func init() {
	events.RegisterEventType(EventTypeMemoryIdentityRejected)
}

// MemoryIdentityRejectedPayload reports a missing-identity
// rejection. SafePayload by construction — both fields are bounded
// enumerable strings (the operation name + a static reason); no
// caller-controlled bytes survive on the payload.
//
// `Operation` is the rejected method name ("AddTurn",
// "GetLLMContext", etc.). `Reason` is a short static string
// indicating which component was missing
// ("tenant_id empty" / "user_id empty" / "session_id empty" /
// "tenant_id and user_id empty", etc.).
//
// The Event's `Identity` field carries whatever the caller supplied
// (zeroed or partial); the bus's `ValidateEvent` would normally
// reject empty-triple events, so the bus publisher substitutes the
// missing components with a `"<missing>"` sentinel so the rejection
// event itself is bus-publishable. Subscribers MAY admin-scope-
// filter to fan-in cross-tenant rejections.
type MemoryIdentityRejectedPayload struct {
	events.SafeSealed
	Operation string
	Reason    string
}

// missingIdentitySentinel is the substitute for any empty component
// on the rejection event so the bus's `ValidateEvent` triple check
// passes. The audit-visible payload's `Reason` field names the
// component that was actually missing, so the sentinel is purely a
// bus-layer publishability device.
const missingIdentitySentinel = "<missing>"
