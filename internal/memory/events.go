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

// EventTypeMemoryHealthChanged is emitted on every `Health` FSM
// transition under `rolling_summary`. Subscribers (Console, audit
// pipeline) render the transition; SREs alert on `degraded`
// duration. Phase 24, RFC §6.6, D-035.
//
// The observable health-transition emit is the explicit exception
// to AGENTS.md §13's "no silent degradation" rule — degraded mode
// IS the observable failure path, and the emit makes it observable
// (and therefore not silent).
const EventTypeMemoryHealthChanged events.EventType = "memory.health_changed"

// EventTypeMemoryRecoveryDropped is emitted when the
// `rolling_summary` recovery backlog overflows `RecoveryBacklogMax`
// and the executor drops the oldest queued batch to make room. Per
// D-035 (bounded recovery loop).
const EventTypeMemoryRecoveryDropped events.EventType = "memory.recovery_dropped"

// EventTypeMemoryItemPut is emitted on the events bus when an admin adds
// a memory turn through the `memory.put` Protocol method (Phase 108n /
// D-186). It is the audit trail for the mutation: the Console / audit
// pipeline observes who added what (by key) and when. SafePayload by
// construction — it carries the deterministic (hashed) turn key only,
// never the operator-supplied turn text.
const EventTypeMemoryItemPut events.EventType = "memory.item_put"

// EventTypeMemoryItemDeleted is emitted on the events bus when an admin
// evicts a memory turn through the `memory.delete` Protocol method (Phase
// 108n / D-186). The audit trail for the eviction. SafePayload — the
// hashed key only, never any record value bytes.
const EventTypeMemoryItemDeleted events.EventType = "memory.item_deleted"

func init() {
	events.RegisterEventType(EventTypeMemoryIdentityRejected)
	events.RegisterEventType(EventTypeMemoryHealthChanged)
	events.RegisterEventType(EventTypeMemoryRecoveryDropped)
	events.RegisterEventType(EventTypeMemoryItemPut)
	events.RegisterEventType(EventTypeMemoryItemDeleted)
}

// MemoryMutationPayload is the audit payload for the `memory.item_put` /
// `memory.item_deleted` events (Phase 108n / D-186). SafePayload by
// construction: `Operation` is a bounded enumerable string ("put" /
// "delete"); `Key` is the deterministic content-addressed turn key (a
// sha256 prefix), never operator-supplied bytes. The record value text is
// NEVER carried here — it is caller-controlled and would require redaction;
// the audit trail records THAT a mutation happened, by key, not its bytes.
type MemoryMutationPayload struct {
	events.SafeSealed
	Operation string
	Key       string
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

// HealthChangedPayload reports a `Health` FSM transition. SafePayload
// by construction — `PriorHealth` + `NewHealth` are bounded
// enumerable strings; `Reason` is a short static string indicating
// the transition cause ("summarizer_failed",
// "retries_exhausted", "recovery_loop_drained", etc.). No
// caller-controlled bytes survive on the payload.
//
// Subscribers MAY admin-scope-filter to fan-in cross-tenant health
// transitions for fleet-level alerting.
type HealthChangedPayload struct {
	events.SafeSealed
	PriorHealth Health
	NewHealth   Health
	Reason      string
}

// RecoveryDroppedPayload reports a recovery-backlog overflow drop.
// SafePayload by construction — `Reason` is a short static string
// ("backlog_overflow"); no caller-controlled bytes.
type RecoveryDroppedPayload struct {
	events.SafeSealed
	Reason string
}
