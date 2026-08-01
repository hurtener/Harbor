package memory

import (
	"github.com/hurtener/Harbor/internal/events"
)

// EventTypeMemoryIdentityRejected is emitted on the events bus when
// a `MemoryStore` method is called with a missing identity triple
// (fail-closed contract). The store ALSO returns
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
// duration. RFC §6.6.
//
// The observable health-transition emit is the explicit exception
// to AGENTS.md §13's "no silent degradation" rule — degraded mode
// IS the observable failure path, and the emit makes it observable
// (and therefore not silent).
const EventTypeMemoryHealthChanged events.EventType = "memory.health_changed"

// EventTypeMemoryRecoveryDropped is emitted when the
// `rolling_summary` recovery backlog overflows `RecoveryBacklogMax`
// and the executor drops the oldest queued batch to make room. Per
// the bounded recovery loop.
const EventTypeMemoryRecoveryDropped events.EventType = "memory.recovery_dropped"

// EventTypeMemoryItemPut is emitted on the events bus when an admin adds
// a memory turn through the `memory.put` Protocol method
// It is the audit trail for the mutation: the Console / audit
// pipeline observes who added what (by key) and when. SafePayload by
// construction — it carries the deterministic (hashed) turn key only,
// never the operator-supplied turn text.
const EventTypeMemoryItemPut events.EventType = "memory.item_put"

// EventTypeMemoryItemDeleted is emitted on the events bus when an admin
// evicts a memory turn through the `memory.delete` Protocol method (
// ). The audit trail for the eviction. SafePayload — the
// hashed key only, never any record value bytes.
const EventTypeMemoryItemDeleted events.EventType = "memory.item_deleted"

// EventTypeMemoryCallerBlockAdmitted is emitted once per run that admits
// caller-supplied content into its read-only external-memory tier. A
// Console that cannot tell caller-asserted memory from runtime-retrieved
// memory can audit neither, so the admission is announced over the
// Protocol.
//
// It carries a SIZE and never CONTENT — see
// [CallerBlockAdmittedPayload].
//
// It fires at admission, which happens before planning, so the event
// lands whether or not the run subsequently succeeds.
const EventTypeMemoryCallerBlockAdmitted events.EventType = "memory.caller_block_admitted"

func init() {
	events.RegisterEventType(EventTypeMemoryIdentityRejected)
	events.RegisterEventType(EventTypeMemoryHealthChanged)
	events.RegisterEventType(EventTypeMemoryRecoveryDropped)
	events.RegisterEventType(EventTypeMemoryItemPut)
	events.RegisterEventType(EventTypeMemoryItemDeleted)
	events.RegisterEventType(EventTypeMemoryCallerBlockAdmitted)
}

// CallerBlockAdmittedPayload is the audit payload for
// [EventTypeMemoryCallerBlockAdmitted]. SafePayload BY CONSTRUCTION, and
// the construction is the point: the only caller-derived quantity on it
// is a byte COUNT. `Tier` and `Key` are runtime-owned constants, not
// caller input — the caller names no key.
//
// No fragment of the admitted content appears here, and none may ever be
// added: the payload is caller-controlled bytes that would require
// redaction, and the audit trail records THAT an admission happened and
// how large it was, never what it said (CLAUDE.md §7 rules 6-7).
type CallerBlockAdmittedPayload struct {
	events.SafeSealed `json:"-"`

	// Bytes is the size of the admitted document as it arrived on the
	// wire. It is the only caller-influenced value on the payload.
	Bytes int `json:"bytes"`
	// Tier is the prompt tier the content was admitted into — the
	// runtime-owned wrapper name, always the read-only external-memory
	// tier.
	Tier string `json:"tier"`
	// Key is the fixed runtime-owned map key the content was composed
	// under within that tier.
	Key string `json:"key"`
}

// MemoryMutationPayload is the audit payload for the `memory.item_put` /
// `memory.item_deleted` events. SafePayload by
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
