package skills

import (
	"github.com/hurtener/Harbor/internal/events"
)

// EventTypeSkillUpserted is emitted on every successful `Upsert`.
// Payload carries the canonical skill identifiers + `Idempotent`
// flag so subscribers can distinguish a true write from a hash-
// matched no-op. SafePayload by construction — every field is a
// bounded enumerable string or a boolean; no caller-controlled
// bytes survive on the payload.
const EventTypeSkillUpserted events.EventType = "skill.upserted"

// EventTypeSkillDeleted is emitted on a successful `Delete`. Payload
// carries the canonical identifiers. SafePayload by construction.
const EventTypeSkillDeleted events.EventType = "skill.deleted"

// EventTypeSkillPackOverwriteRefused is emitted when an `Upsert`
// would have overwritten an `Origin=pack` row with non-pack input.
// The store does NOT mutate the row; the emit makes the refusal
// observable from Console / audit. SafePayload by construction.
const EventTypeSkillPackOverwriteRefused events.EventType = "skill.pack_overwrite_refused"

// EventTypeSkillSearchExecuted is emitted on every `Search` call.
// Payload carries the query (HASHED — the search corpus is allowed
// to contain caller bytes per RFC §6.7, but the audit emit MUST NOT
// surface them), the path that produced the result, and the row
// count. SafePayload by construction.
const EventTypeSkillSearchExecuted events.EventType = "skill.search_executed"

// EventTypeSkillIdentityRejected is emitted when a `SkillStore`
// method is called with a missing identity triple (D-001 fail-
// closed contract). Mirrors `memory.identity_rejected` for the
// skills subsystem.
const EventTypeSkillIdentityRejected events.EventType = "skill.identity_rejected"

// EventTypeSkillProposed is the mandatory audit event emitted by the
// Phase 41 in-runtime skill generator (`skill_propose(persist=true)`)
// on every persist — whether the persist succeeded (`persisted`), was
// idempotent (`idempotent`), or was rejected by the conflict policy
// (`rejected`). Caller-controlled string fields (Title / Trigger
// excerpts) are run through `audit.Redactor.Redact` BEFORE the event
// is built (RFC §6.7, brief 04 §4.8). The payload itself is
// SafePayload so the bus does not re-run it through the redactor;
// the generator is the authoritative redaction point.
const EventTypeSkillProposed events.EventType = "skill.proposed"

func init() {
	events.RegisterEventType(EventTypeSkillUpserted)
	events.RegisterEventType(EventTypeSkillDeleted)
	events.RegisterEventType(EventTypeSkillPackOverwriteRefused)
	events.RegisterEventType(EventTypeSkillSearchExecuted)
	events.RegisterEventType(EventTypeSkillIdentityRejected)
	events.RegisterEventType(EventTypeSkillProposed)
}

// SkillUpsertedPayload reports a successful upsert. SafePayload by
// construction — all fields are bounded enumerable strings / ints /
// booleans; no caller-controlled bytes leak through.
type SkillUpsertedPayload struct {
	events.SafeSealed
	Name        string
	Origin      Origin
	Scope       Scope
	ContentHash string
	Idempotent  bool // true → existing row's hash matched; no write occurred
}

// SkillDeletedPayload reports a successful delete. SafePayload by
// construction.
type SkillDeletedPayload struct {
	events.SafeSealed
	Name string
}

// SkillPackOverwriteRefusedPayload reports a refused upsert against
// an existing `Origin=pack` row. `IncomingOrigin` names the offender;
// the store left the row untouched. SafePayload by construction.
type SkillPackOverwriteRefusedPayload struct {
	events.SafeSealed
	Name           string
	ExistingOrigin Origin
	IncomingOrigin Origin
}

// SkillSearchExecutedPayload reports a `Search` execution.
// `QueryHash` is a sha256 hex prefix of the lowercased query so
// repeated identical searches are correlatable for tracing without
// leaking the raw text. SafePayload by construction.
type SkillSearchExecutedPayload struct {
	events.SafeSealed
	QueryHash string // first 16 hex chars of sha256(lowercased query)
	Path      string // "fts5" | "regex" | "exact"
	Limit     int
	ResultN   int
}

// SkillIdentityRejectedPayload mirrors `memory.MemoryIdentityRejectedPayload`.
// Operation is the rejected method name ("Upsert", "Get", "Search",
// "Delete", "List"); Reason is a short static string indicating
// which component was missing.
type SkillIdentityRejectedPayload struct {
	events.SafeSealed
	Operation string
	Reason    string
}

// missingIdentitySentinel substitutes for empty identity components
// on a rejection event so the bus's `ValidateEvent` triple check
// passes. The audit-visible payload's `Reason` field names the
// component that was actually missing; the sentinel is purely a
// bus-layer publishability device. Mirrors `memory`.
const missingIdentitySentinel = "<missing>"
