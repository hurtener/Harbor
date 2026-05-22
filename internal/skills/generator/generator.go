package generator

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	tcat "github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// ToolNameSkillPropose is the planner-visible name of the generator
// tool. Load-bearing string — the planner prompt references it; a
// silent rename would break every downstream model. The smoke script
// pins it with a string-grep (`scripts/smoke/phase-41.sh`).
const ToolNameSkillPropose = "skill_propose"

// ProposeResult describes what `skill_propose` did. Surfaced on the
// `SkillReceipt` AND on the `skill.proposed` audit payload's
// `Result` field. Bounded enumerable string — never carries caller
// bytes.
type ProposeResult string

// ProposeResult values. The four-way enumeration covers every persist
// branch the generator can return.
const (
	// ResultValidated — `persist=false` path. The draft passed
	// validation; no DB write was performed; no audit event was
	// emitted.
	ResultValidated ProposeResult = "validated"
	// ResultPersisted — `persist=true` path. A new row was inserted
	// OR an existing `Origin=Generated` row was overwritten by the
	// content-hash-gated LWW rule.
	ResultPersisted ProposeResult = "persisted"
	// ResultIdempotent — `persist=true` path. An existing
	// `Origin=Generated` row had `ContentHash` equal to the
	// incoming draft's canonical hash; no write was needed. The
	// receipt's `Persisted` field is true (the caller's intent —
	// "make this skill exist" — is satisfied), but `Result` is
	// `idempotent` so audit subscribers can distinguish from
	// genuine writes.
	ResultIdempotent ProposeResult = "idempotent"
	// ResultRejected — `persist=true` path. An existing
	// `Origin=PackImport` row with the same `(identity, scope,
	// name)` key blocked the overwrite (conflict policy from RFC
	// §6.7 + brief 04 §4.8). The generator returned
	// `*ErrSkillConflict` AND emitted `skill.proposed` with
	// `Result="rejected"` so the rejection is observable from the
	// audit pipeline.
	ResultRejected ProposeResult = "rejected"
)

// SkillDraft is the planner-supplied input shape for `skill_propose`.
// Mirrors `skills.Skill` but excludes provenance / lifecycle fields
// the generator stamps itself (`Origin`, `OriginRef`, `ContentHash`,
// `CreatedAt`, `UpdatedAt`, `LastUsed`, `UseCount`). The planner MAY
// pass `Scope`; an empty value falls back to `skills.ScopeProject`
// per RFC §6.7's "Generator scope default — Settled" decision.
//
// The wire schema uses `map[string]string` for `Extra` so the inproc
// driver's reflection-derived JSON Schema stays closed (every value
// is a string). Drafts that need typed extra values can encode them
// as JSON strings.
type SkillDraft struct {
	Extra          map[string]string `json:"extra,omitempty"`
	Scope          skills.Scope      `json:"scope,omitempty"`
	Title          string            `json:"title,omitempty"`
	Description    string            `json:"description,omitempty"`
	Trigger        string            `json:"trigger"`
	TaskType       string            `json:"task_type,omitempty"`
	Name           string            `json:"name"`
	ScopeProjectID string            `json:"scope_project_id,omitempty"`
	Steps          []string          `json:"steps"`
	RequiredTools  []string          `json:"required_tools,omitempty"`
	RequiredNS     []string          `json:"required_ns,omitempty"`
	RequiredTags   []string          `json:"required_tags,omitempty"`
	FailureModes   []string          `json:"failure_modes,omitempty"`
	Preconditions  []string          `json:"preconditions,omitempty"`
	Tags           []string          `json:"tags,omitempty"`
}

// ProposeArgs is the planner-tool input shape. Wraps a `SkillDraft`
// plus the `persist` flag.
type ProposeArgs struct {
	Skill   SkillDraft `json:"skill"`
	Persist bool       `json:"persist,omitempty"`
}

// SkillReceipt is the planner-tool output shape. Carries the result
// of the propose call (`validated` / `persisted` / `idempotent` /
// `rejected`) plus the stamped provenance fields the planner needs
// to correlate with subsequent searches.
type SkillReceipt struct {
	Result    ProposeResult `json:"result"`
	Name      string        `json:"name"`
	Hash      string        `json:"hash"`
	Origin    skills.Origin `json:"origin,omitempty"`
	OriginRef string        `json:"origin_ref,omitempty"`
	Scope     skills.Scope  `json:"scope,omitempty"`
	Validated bool          `json:"validated"`
	Persisted bool          `json:"persisted"`
}

// ErrSkillConflict is returned when the conflict policy refused a
// `persist=true` call. The Reason field names the rule that fired
// (e.g. `"pack_import_protected"`). Compared via `errors.As` —
// callers that want to inspect the rule should type-assert to
// `*ErrSkillConflict`.
type ErrSkillConflict struct {
	Name   string
	Reason string
}

// Error implements the error interface.
func (e *ErrSkillConflict) Error() string {
	return fmt.Sprintf("skills/generator: conflict on %q: %s", e.Name, e.Reason)
}

// ErrSkillConflictSentinel is a comparison sentinel for `errors.Is`.
// `*ErrSkillConflict` instances Is-match this sentinel so callers can
// pattern-match without type-asserting.
var ErrSkillConflictSentinel = errors.New("skills/generator: conflict")

// Is implements the `errors.Is` contract — every `*ErrSkillConflict`
// matches `ErrSkillConflictSentinel`.
func (e *ErrSkillConflict) Is(target error) bool {
	return target == ErrSkillConflictSentinel
}

// Deps carries the runtime dependencies the generator needs. All
// three fields are mandatory:
//
//   - `Bus` so identity-rejection AND audit-mandatory emits land on
//     the audit pipeline.
//   - `Redactor` so caller-controlled excerpts on the audit payload
//     pass through the canonical redaction rules before publish.
type Deps struct {
	Bus      events.EventBus
	Redactor audit.Redactor
}

// Register installs `skill_propose` into `catalog`, wired against
// `store` + `deps`. Returns wrapped errors on validation failure or
// catalog conflicts (duplicate name → indicates a misconfigured boot
// path, not a runtime fault).
//
// Concurrent reuse: the registered descriptor holds an immutable
// closure over (store, deps); per-call state lives on (ctx, args).
// D-025 holds.
func Register(catalog tcat.ToolCatalog, store skills.SkillStore, deps Deps) error {
	if catalog == nil {
		return errors.New("skills/generator: catalog is nil")
	}
	if store == nil {
		return errors.New("skills/generator: store is nil")
	}
	if deps.Bus == nil {
		return errors.New("skills/generator: deps.Bus is required (events.EventBus)")
	}
	if deps.Redactor == nil {
		return errors.New("skills/generator: deps.Redactor is required (audit.Redactor)")
	}

	propose := func(ctx context.Context, args ProposeArgs) (SkillReceipt, error) {
		return Propose(ctx, store, deps, args)
	}
	if err := inproc.RegisterFunc[ProposeArgs, SkillReceipt](
		catalog,
		ToolNameSkillPropose,
		propose,
		tcat.WithDescription("Validate an LLM-drafted skill and, when persist=true, stamp Origin=Generated + OriginRef=gen:{session}:{run} + Scope (default project), enforce the conflict policy (pack-protected; Generated→Generated content-hash-gated LWW), upsert via the SkillStore, and emit a mandatory skill.proposed audit event. Returns a SkillReceipt with the result branch (validated/persisted/idempotent/rejected) + the canonical content hash."),
		tcat.WithSideEffect(tcat.SideEffectWrite),
		tcat.WithLoading(tcat.LoadingAlways),
		tcat.WithTags("skills", "generate"),
		tcat.WithBus(deps.Bus),
		tcat.WithSource(tcat.ToolSourceID("skills/generator")),
	); err != nil {
		return fmt.Errorf("skills/generator: register %s: %w", ToolNameSkillPropose, err)
	}
	return nil
}

// Propose is the handler exposed for direct Go-level invocation by
// runtime composition code that does not go through the catalog. The
// catalog-registered handler is a thin closure around this function.
//
// Behavior:
//
//   - `persist=false`: validate → compute canonical hash → return
//     receipt with `Validated=true`, `Result=ResultValidated`,
//     `Hash` populated. NO DB write. NO audit event.
//   - `persist=true`: validate → stamp `Origin=Generated` +
//     `OriginRef = "gen:{session_id}:{run_id}"` + `Scope` (default
//     `project`) + `ScopeTenantID = id.TenantID` + canonical hash →
//     check conflict policy → call `store.Upsert` → emit redacted
//     `skill.proposed` → return the receipt.
//
// Audit is mandatory: every persist (success, idempotent, rejected)
// emits `skill.proposed` BEFORE the function returns success. Audit-
// emit failure aborts the persist and returns a wrapped error;
// `Propose` calls `store.Delete` on the just-inserted row in the
// success-path emit-failure case so the caller's `Get` returns
// `ErrSkillNotFound`. The rollback's own `skill.deleted` emit landing
// on the bus is NOT load-bearing for correctness — it's observability.
func Propose(ctx context.Context, store skills.SkillStore, deps Deps, args ProposeArgs) (SkillReceipt, error) {
	if store == nil {
		return SkillReceipt{}, errors.New("skills/generator: Propose called with nil store")
	}
	if deps.Bus == nil {
		return SkillReceipt{}, errors.New("skills/generator: Propose called without Bus")
	}
	if deps.Redactor == nil {
		return SkillReceipt{}, errors.New("skills/generator: Propose called without Redactor")
	}

	// Identity is mandatory (D-001). Read the Quadruple from ctx;
	// missing / incomplete triple → wrapped ErrIdentityRequired +
	// skill.identity_rejected emit (Phase 37's helper).
	q, ok := identity.QuadrupleFrom(ctx)
	if !ok {
		// No Quadruple ctx key — fall back to Identity key and
		// build a zero-RunID Quadruple if present.
		if id, ok2 := identity.From(ctx); ok2 {
			q = identity.Quadruple{Identity: id}
		}
	}
	if err := identity.Validate(q.Identity); err != nil {
		return SkillReceipt{}, skills.EmitIdentityRejected(ctx, deps.Bus, q, "generator.skill_propose")
	}

	// Build the skill record from the draft. Provenance + lifecycle
	// fields are stamped AFTER validation.
	skill := buildSkillFromDraft(args.Skill, q)

	// Validate via the same skills.Skill validator the importer
	// uses (brief 04 §4.8: "validates the draft via the same
	// SkillDefinition validator the importer uses"). Validation
	// happens against the post-stamp record so Origin / Scope are
	// non-empty for the validator.
	if err := skill.Validate(); err != nil {
		return SkillReceipt{}, err
	}

	// Canonical content hash. Computed once; reused for the
	// conflict-policy hash compare AND the receipt's `Hash` field.
	skill.ContentHash = skills.CanonicalContentHash(skill)

	if !args.Persist {
		// persist=false: validation-only. No DB write, no audit
		// emit. The receipt's Origin / OriginRef / Scope are
		// intentionally NOT stamped on the returned struct — the
		// caller asked for validation, not provenance.
		return SkillReceipt{
			Validated: true,
			Persisted: false,
			Result:    ResultValidated,
			Name:      skill.Name,
			Hash:      skill.ContentHash,
		}, nil
	}

	// persist=true: walk the conflict policy.
	//
	// We probe the existing row via store.Get. The localdb's Upsert
	// also enforces pack-overwrite refusal at the SQL level (Phase
	// 37); we duplicate the probe here so we can return our typed
	// `*ErrSkillConflict` + emit the rejection audit event before
	// the SQL layer surfaces its sentinel.
	existing, getErr := store.Get(ctx, q, skill.Name)
	switch {
	case errors.Is(getErr, skills.ErrSkillNotFound):
		// No existing row — fall through to insert path.
	case getErr != nil:
		// Identity-rejection from store.Get? skill_propose's
		// identity check above already covered the missing-triple
		// case, so any error here is unexpected — bubble up.
		return SkillReceipt{}, fmt.Errorf("skills/generator: probe existing: %w", getErr)
	case existing.Origin == skills.OriginPack:
		// Pack-protected: refuse. Emit the rejection event BEFORE
		// returning the typed error so the audit pipeline observes
		// every persist attempt.
		conflict := &ErrSkillConflict{Name: skill.Name, Reason: "pack_import_protected"}
		if emitErr := emitProposed(ctx, deps, q, args.Skill, skill, ResultRejected, conflict.Reason, false); emitErr != nil {
			// Audit emit failed on a rejection. The DB was not
			// mutated; surface the wrapped error rather than the
			// conflict — the audit failure is the more severe
			// fault.
			return SkillReceipt{}, fmt.Errorf("skills/generator: audit emit failed on rejection: %w", emitErr)
		}
		return SkillReceipt{
			Validated: true,
			Persisted: false,
			Result:    ResultRejected,
			Name:      skill.Name,
			Hash:      skill.ContentHash,
			Origin:    skill.Origin,
			OriginRef: skill.OriginRef,
			Scope:     skill.Scope,
		}, conflict
	case existing.Origin == skills.OriginGenerated && existing.ContentHash == skill.ContentHash:
		// Idempotent: existing Generated row with matching content
		// hash. No DB write needed; emit the idempotent event so
		// audit subscribers see the call landed.
		if emitErr := emitProposed(ctx, deps, q, args.Skill, skill, ResultIdempotent, "", false); emitErr != nil {
			return SkillReceipt{}, fmt.Errorf("skills/generator: audit emit failed on idempotent: %w", emitErr)
		}
		return SkillReceipt{
			Validated: true,
			Persisted: true,
			Result:    ResultIdempotent,
			Name:      skill.Name,
			Hash:      skill.ContentHash,
			Origin:    skill.Origin,
			OriginRef: skill.OriginRef,
			Scope:     skill.Scope,
		}, nil
	}
	// Either: no existing row OR existing Generated row with
	// different hash. Both paths go through store.Upsert (insert OR
	// LWW overwrite respectively).

	if err := store.Upsert(ctx, q, skill); err != nil {
		// Pack-overwrite refusal from the storage layer should not
		// reach us (we probed above), but handle it defensively so
		// a race between probe and upsert surfaces a typed error.
		if errors.Is(err, skills.ErrPackOverwriteRefused) {
			conflict := &ErrSkillConflict{Name: skill.Name, Reason: "pack_import_protected"}
			if emitErr := emitProposed(ctx, deps, q, args.Skill, skill, ResultRejected, conflict.Reason, false); emitErr != nil {
				return SkillReceipt{}, fmt.Errorf("skills/generator: audit emit failed on race-rejection: %w", emitErr)
			}
			return SkillReceipt{}, conflict
		}
		return SkillReceipt{}, fmt.Errorf("skills/generator: upsert: %w", err)
	}

	// Persist landed. Emit the audit event. On emit failure, roll
	// back the just-inserted row by calling store.Delete so the
	// caller's Get returns ErrSkillNotFound — fail-loudly per
	// CLAUDE.md §13 + the spec's audit-mandatory contract.
	if emitErr := emitProposed(ctx, deps, q, args.Skill, skill, ResultPersisted, "", false); emitErr != nil {
		// Best-effort rollback. Delete's failure is folded into
		// the surfaced error so the caller sees both.
		delErr := store.Delete(ctx, q, skill.Name)
		if delErr != nil {
			return SkillReceipt{}, fmt.Errorf("skills/generator: audit emit failed AND rollback delete failed: emit=%w deleteErr=%w", emitErr, delErr)
		}
		return SkillReceipt{}, fmt.Errorf("skills/generator: audit emit failed (persist rolled back): %w", emitErr)
	}

	return SkillReceipt{
		Validated: true,
		Persisted: true,
		Result:    ResultPersisted,
		Name:      skill.Name,
		Hash:      skill.ContentHash,
		Origin:    skill.Origin,
		OriginRef: skill.OriginRef,
		Scope:     skill.Scope,
	}, nil
}

// Promote writes sibling rows under each `target` identity so a
// row previously persisted under `src` (with `Scope` set to a
// non-session value) becomes visible from the targets. The Phase 37
// localdb storage layer filters by `(tenant, user, session)`
// unconditionally; an explicit-target promotion is the V1
// minimum-viable mechanism for "cross-session promotion REQUIRES
// Scope=project or Scope=tenant" — Phase 39's Directory subsystem
// will layer a more ergonomic surface on top.
//
// Each target write:
//
//   - Re-reads the source row via `store.Get(ctx, src, name)`.
//   - Restamps `Scope` to the supplied value, preserving the
//     original `OriginRef` so audit subscribers can correlate
//     promoted siblings back to the original draft.
//   - Calls `store.Upsert(ctx, target, skill)` and emits a
//     `skill.proposed` event with `Promotion=true` AND
//     `Result="persisted"` for that target.
//
// Returns the first error encountered; subsequent targets are NOT
// attempted (the strict-fail model is simpler than partial-success
// and matches the storage-layer transactional shape).
func Promote(ctx context.Context, store skills.SkillStore, deps Deps, src identity.Quadruple, name string, targets []identity.Quadruple, scope skills.Scope) error {
	if store == nil {
		return errors.New("skills/generator: Promote called with nil store")
	}
	if deps.Bus == nil {
		return errors.New("skills/generator: Promote called without Bus")
	}
	if deps.Redactor == nil {
		return errors.New("skills/generator: Promote called without Redactor")
	}
	if err := identity.Validate(src.Identity); err != nil {
		return skills.EmitIdentityRejected(ctx, deps.Bus, src, "generator.Promote")
	}
	if name == "" {
		return errors.New("skills/generator: Promote name is empty")
	}
	if len(targets) == 0 {
		return errors.New("skills/generator: Promote targets slice is empty")
	}
	if scope == "" {
		scope = skills.ScopeProject
	}
	switch scope {
	case skills.ScopeProject, skills.ScopeTenant, skills.ScopeGlobal:
	case skills.ScopeSession:
		return errors.New("skills/generator: Promote scope=session is contradictory (use ScopeProject or ScopeTenant)")
	default:
		return fmt.Errorf("skills/generator: Promote unknown scope %q", scope)
	}

	// Read the source row once; copy + restamp per target.
	source, err := store.Get(ctx, src, name)
	if err != nil {
		return fmt.Errorf("skills/generator: Promote source read: %w", err)
	}

	for _, target := range targets {
		if err := identity.Validate(target.Identity); err != nil {
			return skills.EmitIdentityRejected(ctx, deps.Bus, target, "generator.Promote.target")
		}
		copyForTarget := source
		copyForTarget.Scope = scope
		copyForTarget.ScopeTenantID = target.TenantID
		// Preserve OriginRef so the audit trail records the source
		// session / run, not the target's. Recompute the content
		// hash defensively — the canonical hasher excludes Scope /
		// ScopeTenantID / OriginRef anyway, but recomputing keeps
		// the storage row's hash field consistent with its content.
		copyForTarget.ContentHash = skills.CanonicalContentHash(copyForTarget)

		if err := store.Upsert(ctx, target, copyForTarget); err != nil {
			return fmt.Errorf("skills/generator: Promote upsert (target=%s/%s/%s): %w",
				target.TenantID, target.UserID, target.SessionID, err)
		}

		// Per-target audit emit. The draft carries the source row's
		// caller-controlled fields so the redactor still passes.
		draftForAudit := SkillDraft{
			Name:    copyForTarget.Name,
			Title:   copyForTarget.Title,
			Trigger: copyForTarget.Trigger,
		}
		if err := emitProposed(ctx, deps, target, draftForAudit, copyForTarget, ResultPersisted, "promoted", true); err != nil {
			// Roll back THIS target. Other targets stay (the
			// strict-fail model is per-target — we surface the
			// first failure and don't continue).
			delErr := store.Delete(ctx, target, copyForTarget.Name)
			if delErr != nil {
				return fmt.Errorf("skills/generator: Promote audit emit failed AND rollback delete failed for target=%s/%s/%s: emit=%w delete=%w",
					target.TenantID, target.UserID, target.SessionID, err, delErr)
			}
			return fmt.Errorf("skills/generator: Promote audit emit failed for target=%s/%s/%s (rolled back): %w",
				target.TenantID, target.UserID, target.SessionID, err)
		}
	}
	return nil
}

// buildSkillFromDraft constructs the `skills.Skill` record from the
// planner-supplied draft + the identity Quadruple. Stamps provenance
// (`Origin=Generated`, `OriginRef = "gen:{session_id}:{run_id}"`) and
// scope (default `ScopeProject`); ScopeTenantID is set from the
// identity tenant; ContentHash is left empty (the caller populates
// it via `skills.CanonicalContentHash` after Validate).
//
// `Extra` on the draft is `map[string]string` (closed schema); the
// skill's `Extra` is `map[string]any`. The convert widens the value
// types; downstream consumers see strings.
func buildSkillFromDraft(d SkillDraft, q identity.Quadruple) skills.Skill {
	scope := d.Scope
	if scope == "" {
		scope = skills.ScopeProject
	}
	extra := make(map[string]any, len(d.Extra))
	for k, v := range d.Extra {
		extra[k] = v
	}
	return skills.Skill{
		Name:           d.Name,
		Title:          d.Title,
		Description:    d.Description,
		Trigger:        d.Trigger,
		TaskType:       d.TaskType,
		Tags:           d.Tags,
		Steps:          d.Steps,
		Preconditions:  d.Preconditions,
		FailureModes:   d.FailureModes,
		RequiredTools:  d.RequiredTools,
		RequiredNS:     d.RequiredNS,
		RequiredTags:   d.RequiredTags,
		Origin:         skills.OriginGenerated,
		OriginRef:      fmt.Sprintf("gen:%s:%s", q.SessionID, q.RunID),
		Scope:          scope,
		ScopeTenantID:  q.TenantID,
		ScopeProjectID: d.ScopeProjectID,
		Extra:          extra,
	}
}
