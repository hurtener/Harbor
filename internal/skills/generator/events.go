// Package generator owns Harbor's in-runtime skill generator with
// persistence (Phase 41, RFC §6.7).
//
// The generator validates a planner-drafted skill, stamps generation
// provenance (`Origin=Generated`, `OriginRef = "gen:{session_id}:{run_id}"`),
// scopes by operator-supplied `Scope` (default `project`), upserts via
// the Phase 37 `SkillStore`, and emits a mandatory `skill.proposed`
// audit event on every persist. Audit-emit failure rolls back the
// persist; there is no silent path (CLAUDE.md §13).
//
// Conflict policy (centralised here; the underlying SkillStore's
// `ErrPackOverwriteRefused` is the storage-layer half):
//
//   - Existing row `Origin=PackImport` → REFUSE; return
//     `*ErrSkillConflict{Reason:"pack_import_protected"}` AND emit a
//     `skill.proposed` event with `Result="rejected"`.
//   - Existing row `Origin=Generated` AND incoming `ContentHash` ==
//     existing → idempotent; return `Result="idempotent"`.
//   - Existing row `Origin=Generated` AND content differs → LWW
//     overwrite; return `Result="persisted"`.
//   - No existing row → insert; return `Result="persisted"`.
//
// Identity is mandatory (D-001): missing identity returns wrapped
// `skills.ErrIdentityRequired` AND emits `skill.identity_rejected` on
// the bus.
//
// Concurrent reuse (D-025): the registered tool descriptor holds an
// immutable closure over (store, redactor, bus). One catalog + one
// store is safe to share across N concurrent goroutines.
package generator

import (
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/skills"
)

// SkillProposedPayload is the audit-mandatory payload emitted on
// every `skill_propose(persist=true)` invocation, plus every
// `Promote` per-target write. Caller-controlled fields are pre-
// redacted by the generator BEFORE the payload is built: the
// `RedactedTitleExcerpt` / `RedactedTriggerExcerpt` carry the
// post-redactor truncated form of the draft's `Title` / `Trigger`
// fields (capped at `auditExcerptCap` characters). Per CLAUDE.md §7
// rule 7 (no untyped tool arguments in audit payloads), the payload
// is typed and bounded. SafePayload so the bus does not re-run the
// pre-redacted excerpts through the redactor.
type SkillProposedPayload struct {
	events.SafeSealed

	// Name is the skill's primary key. Bounded by `skills.Skill`
	// validation (non-empty, identity-scoped uniqueness).
	Name string

	// Origin is always `OriginGenerated` for a `skill_propose`
	// event. Recorded for subscriber correlation.
	Origin skills.Origin

	// Scope is the final stamped scope (default `project`).
	Scope skills.Scope

	// OriginRef is `"gen:{session_id}:{run_id}"`. Identifies the
	// session / run that authored the skill.
	OriginRef string

	// ContentHash is the canonical sha256 of the skill body
	// (`skills.CanonicalContentHash`). Bounded hex string.
	ContentHash string

	// Result is the outcome the generator returned to the caller.
	// One of: "validated" | "persisted" | "idempotent" | "rejected".
	Result string

	// Reason is empty on `persisted` / `idempotent` and carries a
	// short bounded string on `rejected` (e.g. `"pack_import_protected"`)
	// or `validated`. Never carries caller bytes.
	Reason string

	// RedactedTitleExcerpt is the post-redactor truncated form of
	// the draft's `Title`. Bounded at `auditExcerptCap` characters.
	// Empty when the draft's Title was empty.
	RedactedTitleExcerpt string

	// RedactedTriggerExcerpt is the post-redactor truncated form of
	// the draft's `Trigger`. Bounded at `auditExcerptCap` characters.
	RedactedTriggerExcerpt string

	// Promotion is true when the event was emitted by a `Promote`
	// per-target write rather than a direct `skill_propose`. Lets
	// subscribers distinguish cross-session expansion from in-
	// session writes.
	Promotion bool
}
