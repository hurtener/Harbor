# Phase 41 — In-runtime skill generator with persistence

## Summary

Phase 41 lands the planner-callable `skill_propose(persist=true)` tool — Harbor's in-runtime skill generator that validates an LLM-drafted skill, stamps generation provenance (`Origin=Generated`, `OriginRef = "gen:{session_id}:{run_id}"`), scopes by operator-supplied `Scope` (default `project`), and upserts through the Phase 37 `SkillStore` with a mandatory audit emit on every persist. The package extends the Phase 38 planner-tools surface with a fourth catalog-registered tool, threads the Phase 03 `audit.Redactor` over every caller-controlled field that lands on the audit payload, and ships a `Promote` action that supports explicit cross-session-to-project promotion so the cross-session-no-leak contract becomes testable end-to-end.

## RFC anchor

- RFC §6.7

## Briefs informing this phase

- brief 04

## Brief findings incorporated

- **brief 04 §4.8 (generator with persistence).** "Validates the draft via the same `SkillDefinition` validator the importer uses, stamps `Origin=Generated`, stamps `OriginRef = "gen:{session_id}:{trace_id}"`, scopes by the operator-provided `Scope` (default to current project), and inserts via the `LocalSkillStore.upsert_pack_skill` equivalent. Conflict policy: refuse to overwrite a `Origin=PackImport` skill with the same `name`. For `Origin=Generated → Origin=Generated`, last-write-wins gated by `content_hash` change." Phase 41 ports this verbatim onto `skills.SkillStore.Upsert` (Phase 37 already enforces the storage-layer half of the conflict policy via `ErrPackOverwriteRefused`); the generator stamps `OriginRef = "gen:{session_id}:{run_id}"` (Harbor's `RunID` is the analog of the predecessor's per-execution `trace_id`).
- **brief 04 §4.8 (audit shape).** "Record `(actor=identity_triple, action="skill.created", skill_id, content_hash, source_excerpt_hash)`." Phase 41 emits `skill.proposed` (a new event type registered alongside Phase 37's `skill.upserted` taxonomy) carrying the identity quadruple, the action enum (`persisted | rejected | idempotent | validated`), the skill `Name + Origin + Hash`, and the `OriginRef`. **Every caller-controlled string** (any incoming `SkillDraft.Description / Title / Trigger` excerpt the audit subscriber might find useful) flows through `audit.Redactor.Redact` BEFORE the event is published; the raw draft body never reaches the bus.
- **brief 04 §6 (tests required, "generator end-to-end").** "Generator end-to-end: draft → validate → persist → re-discover via `skill_search`." Phase 41's integration test wires the catalog (with both Phase 38's planner tools and Phase 41's `skill_propose` registered), the localdb store, and the bus; the test calls `skill_propose(persist=true)` → calls Phase 38's `skill_search` → asserts the new skill appears in the ranked results.
- **brief 04 §6 (isolation conformance, "Generator output scoped to session A is not discoverable from session B unless promoted to project/tenant scope by an explicit operator action").** The cross-session no-leak test exercises this exactly: identity A persists with `Scope=session`, identity B (different session, same tenant/user/project) calls `skill_get`/`skill_search` → 0 rows. Then identity A's runtime calls the generator's explicit `Promote` API targeting identity B's session → identity B's subsequent `skill_search` MUST find the row. The promotion call writes a sibling row scoped to the target session (the localdb storage filter is unconditionally session-keyed at Phase 37; an explicit-target promotion is the V1 mechanism for the predecessor's "explicit operator action").
- **brief 04 §5 ("Generator is intentionally crippled today").** "The system prompt at `skills/tools/skill_propose_tool.py:43-44` prevents the LLM from claiming persistence because the runtime cannot back the claim. Harbor inverts this — runtime ships persistence, prompt is updated, audit is mandatory." Phase 41 closes the gap on Harbor's side: the runtime DOES persist (the tool's `persist` arg is honored when `true`) AND every persist emits an audit event before the tool returns success. There is no silent path; an audit-emit failure aborts the persist and surfaces a wrapped error.

## Findings I'm departing from (if any)

- **brief 04 §4.8's `source_excerpt_hash` audit field.** The predecessor's audit payload records a hash of the LLM-generated draft source so an operator can later correlate the audit trail with the LLM transcript. Phase 41 does NOT emit a `source_excerpt_hash` field at this time. Two reasons: (a) the LLM-transcript correlation surface (Phase 32+ telemetry that captures LLM round-trips) is downstream of the generator and not yet plumbed into the planner-tool boundary; (b) the audit-redactor pipeline (Phase 03) is the canonical place for any payload field that might carry caller bytes, and the V1 audit emit already carries `Name + Hash + Origin + OriginRef` which is sufficient for the "every persist is auditable" contract. The field is reserved for a follow-up phase to populate without changing the wire shape.

## Goals

- Ship the planner-callable `skill_propose` tool through the Phase 26 catalog via `tools.RegisterFunc`, with the same shape contract Phase 38's three tools follow (reflection-derived JSON Schemas; `Transport=InProcess`; `Loading=LoadingAlways`; `SideEffect=Write`). Tool name exactly `skill_propose`.
- Two behavioral modes:
  - `persist=false` — validate the draft (same `Skill.Validate` the importer uses), stamp the canonical content hash, return a `SkillReceipt{Validated:true, Hash:...}`. No DB write. No audit emit (validation-only is observable through the tool catalog, not the audit pipeline).
  - `persist=true` — validate, stamp `Origin=Generated` + `OriginRef = "gen:{session_id}:{run_id}"` + `Scope` (operator-supplied; default `project`) + `ScopeTenantID = id.TenantID`, run the draft through the conflict policy, upsert via `SkillStore.Upsert`, emit `skill.proposed` on success. Return `SkillReceipt{Persisted:true, Hash:..., OriginRef:..., Result:"persisted"|"idempotent"}`.
- Conflict policy is centralized in `internal/skills/generator`:
  - Existing row `Origin=PackImport` → REFUSE overwrite. Return wrapped `ErrSkillConflict{Reason: "pack_import_protected"}`. Emit `skill.proposed` with `Result="rejected"` (the rejection IS observable on the audit pipeline; the underlying `ErrPackOverwriteRefused` from Phase 37 stays the wrapped sentinel).
  - Existing row `Origin=Generated` AND incoming `ContentHash == existing.ContentHash` → idempotent. Return `SkillReceipt{Persisted:true, Result:"idempotent"}`. Emit `skill.proposed` with `Result="idempotent"`.
  - Existing row `Origin=Generated` AND content hash differs → last-write-wins via the underlying `SkillStore.Upsert`. Emit `skill.proposed` with `Result="persisted"`.
  - No existing row → insert. Emit `skill.proposed` with `Result="persisted"`.
- **Audit is mandatory and fail-loud.** Every persist (including conflict-rejected paths) emits `skill.proposed`. The payload's caller-controlled fields (the `SkillDraft.Title` / `Description` / `Trigger` excerpts that surface for operator triage) are run through `audit.Redactor.Redact` BEFORE `bus.Publish`. **If the audit emit fails, the persist is rolled back to the extent possible** — the generator does NOT silently succeed with an unredacted-or-undelivered audit. Documented in D-054.
- Identity is mandatory (D-001): the tool reads the identity Quadruple from `ctx` via `identity.QuadrupleFrom(ctx)`. A missing / incomplete triple returns wrapped `skills.ErrIdentityRequired` AND emits `skill.identity_rejected` (reuses Phase 37's `skills.EmitIdentityRejected`).
- Concurrent-reuse contract (D-025): the catalog descriptor holds only an immutable closure over the store + redactor + bus. N≥128 goroutines invoking `skill_propose` with distinct skill names against ONE shared catalog under `-race` MUST observe no data races, no context bleed, no goroutine leaks. Two concurrent writers under the same `(identity, name)` MUST resolve to exactly one persisted state — the loser sees either `Result="idempotent"` (hash matched) or `ErrSkillConflict` (pack-protected) deterministically.
- Cross-session promotion: ship a `Promote(ctx, store, redactor, bus, src, name, targets, scope)` Go-level API on the generator package surface that writes sibling rows under each target identity. The tool catalog does NOT currently surface `skill_promote` as a planner tool (operators promote through an out-of-band path until Phase 39's directory subsystem lands its own promotion surface); the API is exercised end-to-end via the integration test.
- `cmd/harbor/main.go` blank-imports `internal/skills/generator` so the package's presence in the binary is observable to deployment reviewers (Phase 60+ bootstrap will call `generator.Register(catalog, store, deps)`).

## Non-goals

- Source-excerpt-hash audit field (deferred — see Findings).
- An LLM-side prompt change ("don't claim persistence" → "you may claim persistence; the runtime backs you"). The LLM prompt evolution is the Phase 45+ planner concrete's territory, not the generator's.
- Phase 39's `Directory` subsystem-driven promotion UX. The generator's `Promote` API is the V1 minimum-viable mechanism for cross-session promotion; Phase 39 will layer a more ergonomic surface on top.
- A `skill_promote` planner-callable tool (Go-level API only at Phase 41).
- Portico-distributed generator output. Generated rows live in the local skill store; cross-tenant rolling-forward is a Phase 49+ concern.
- Validators beyond `Skill.Validate`. The generator reuses the Phase 37 validator verbatim — same shape as the Phase 40 importer will reuse. Additional semantic validation (e.g. tool-existence checks against the catalog) is reserved for a later phase.

## Acceptance criteria

- [ ] `internal/skills/generator/generator.go` defines `SkillDraft`, `SkillReceipt`, `ProposeResult` (enum: `persisted | idempotent | rejected | validated`), `ErrSkillConflict{Reason}`, `Deps`, and `Register(catalog, store, deps)`. Returns wrapped errors on validation failure or catalog conflicts.
- [ ] One catalog-registered tool: `skill_propose`. Name exactly that string. `LoadingAlways`, `Transport=InProcess`, `SideEffect=Write` (persist=true is a write; persist=false is read-only but the descriptor's SideEffect is conservative).
- [ ] `persist=false` path: validates the draft via `Skill.Validate`; computes `CanonicalContentHash`; returns a `SkillReceipt{Validated:true, Hash:...}`. No DB write, no audit emit, no event.
- [ ] `persist=true` path: validates → stamps `Origin=Generated` + `OriginRef = "gen:{session_id}:{run_id}"` + `Scope` (default `project`) + `ScopeTenantID = id.TenantID` + canonical hash → checks the conflict policy → calls `store.Upsert` → emits redacted `skill.proposed` → returns the receipt. The order is binding: hash is stamped BEFORE conflict check; audit emit is BEFORE the tool's success return.
- [ ] Conflict policy precedence: `Origin=PackImport` existing wins (incoming refused with `ErrSkillConflict{Reason:"pack_import_protected"}`). `Origin=Generated` existing with matching hash → idempotent receipt. `Origin=Generated` existing with different hash → LWW overwrite. No existing row → insert.
- [ ] Audit is mandatory: every persist (success, idempotent, rejected) emits `skill.proposed` with the canonical SafePayload shape. Caller-controlled string fields (`Title`, `Description`, `Trigger` excerpts) pass through `audit.Redactor.Redact` BEFORE publish; the unredacted draft body is never serialized to the audit pipeline.
- [ ] Audit-emit failure is fail-loud: a `bus.Publish` error from the audit emit aborts the persist and returns a wrapped error (`fmt.Errorf("skills/generator: audit emit failed (persist rolled back): %w", err)`). Documented in D-054; covered by an `audit_emit_failure_test.go` that injects a bus error and asserts (a) the row is not visible via `SkillStore.Get` after the failed call (the Phase 37 store's transaction commits BEFORE the emit; the rollback is achieved by `Delete` from the generator on emit failure), (b) the error is wrapped, (c) no `skill.proposed` event for the rolled-back persist landed on the bus.
- [ ] Identity-mandatory: missing identity returns wrapped `skills.ErrIdentityRequired` AND emits `skill.identity_rejected` (reuses Phase 37's `EmitIdentityRejected`). Tests pin both branches: identity missing entirely from ctx, and identity partial (empty TenantID).
- [ ] Cross-session promotion via `Promote(ctx, store, redactor, bus, src, name, targets, scope)`. Writes a sibling row under each `target` identity. Each target write also emits a `skill.proposed` with `Result="persisted"` so the audit pipeline sees the cross-session expansion explicitly.
- [ ] **D-025 concurrent-reuse test (`concurrent_test.go`)**: N=128 goroutines propose distinct skill names against one shared catalog under `-race`. Asserts: no data races; per-goroutine identity isolation (the bus subscription for goroutine i sees only events with goroutine i's identity); concurrent same-name writers resolve deterministically (exactly one `Persisted`; others see `Idempotent` or `Conflict`); no goroutine leak (baseline-restored after teardown).
- [ ] **Cross-session integration test (`integration_test.go`, `TestIntegration_CrossSessionPromotion_AgainstLocalDB`)**: real `tools.Catalog` + real `localdb.SkillStore` + real `events.EventBus` + real `audit.Redactor`. Identity A proposes a `Scope=session` skill; identity B (same tenant/user, different session) calls `skill_search` → MUST NOT find. Identity A calls `Promote(...src=idA, name, targets=[idB], scope=ScopeProject)`; identity B re-calls `skill_search` → MUST find.
- [ ] `internal/skills/generator/` package coverage ≥ 90%.
- [ ] `cmd/harbor/main.go` blank-imports `internal/skills/generator`.
- [ ] `docs/plans/README.md` Phase 41 row flipped from `Pending` to `Shipped`.
- [ ] `README.md` Status table flipped from `Pending` to `Shipped` (or row added if absent).
- [ ] D-054 entry in `docs/decisions.md` covering: conflict-policy precedence, audit-emit-failure handling (persist-rolled-back-on-emit-failure), default `Scope=project` rationale, no-`skill_promote`-catalog-tool-at-V1.
- [ ] Glossary entries: `skill_propose`, `Origin=Generated`, `OriginRef`, `SkillDraft`, `SkillReceipt`, `ProposeResult`, conflict-policy enum.
- [ ] `make drift-audit` + `make preflight` + `make check-mirror` pass.

## Files added or changed

```text
internal/skills/generator/
├── generator.go                          # Register + Deps + SkillDraft + SkillReceipt + ProposeResult + handlers + Promote
├── events.go                             # SkillProposedPayload SafeSealed
├── audit.go                              # buildAuditPayload + redact-before-emit helpers
├── generator_test.go                     # Unit: persist=false, persist=true, conflict policy axes
├── promote_test.go                       # Promote(): cross-session sibling write happy + failure modes
├── audit_emit_failure_test.go            # Bus-error injection → persist rolled back, no event landed
├── concurrent_test.go                    # D-025 N=128 stress
├── integration_test.go                   # TestIntegration_CrossSessionPromotion_AgainstLocalDB + end-to-end seed→propose→search
└── testhelpers_test.go                   # shared bus/store builders for tests
internal/skills/events.go                 # add EventTypeSkillProposed const + registration
docs/plans/phase-41-skill-generator.md    # this file
docs/plans/README.md                      # flip Phase 41 row Pending → Shipped
README.md                                 # flip Phase 41 row Pending → Shipped
docs/glossary.md                          # skill_propose / Origin=Generated / OriginRef / SkillDraft / SkillReceipt / ProposeResult / conflict-policy entries
docs/decisions.md                         # D-054 entry
scripts/smoke/phase-41.sh                 # Go-level test surface assertions (no Protocol surface at this wave)
cmd/harbor/main.go                        # blank-import internal/skills/generator
```

## Public API surface

```go
// internal/skills/generator/generator.go

package generator

// ProposeResult describes what skill_propose did. Surfaced on the
// receipt + on the skill.proposed audit payload.
type ProposeResult string

const (
    ResultValidated  ProposeResult = "validated"  // persist=false; no DB write
    ResultPersisted  ProposeResult = "persisted"  // new row OR LWW overwrite
    ResultIdempotent ProposeResult = "idempotent" // hash matched existing Generated
    ResultRejected   ProposeResult = "rejected"   // pack-protected conflict
)

// SkillDraft is the planner-supplied input. Mirrors skills.Skill but
// excludes provenance / lifecycle fields the generator stamps itself
// (Origin, OriginRef, ContentHash, CreatedAt, UpdatedAt, LastUsed,
// UseCount). The planner MAY pass Scope; default is project.
type SkillDraft struct {
    Name           string            `json:"name"`
    Title          string            `json:"title,omitempty"`
    Description    string            `json:"description,omitempty"`
    Trigger        string            `json:"trigger"`
    TaskType       string            `json:"task_type,omitempty"`
    Tags           []string          `json:"tags,omitempty"`
    Steps          []string          `json:"steps"`
    Preconditions  []string          `json:"preconditions,omitempty"`
    FailureModes   []string          `json:"failure_modes,omitempty"`
    RequiredTools  []string          `json:"required_tools,omitempty"`
    RequiredNS     []string          `json:"required_ns,omitempty"`
    RequiredTags   []string          `json:"required_tags,omitempty"`
    Scope          skills.Scope      `json:"scope,omitempty"` // empty -> ScopeProject
    ScopeProjectID string            `json:"scope_project_id,omitempty"`
    Extra          map[string]string `json:"extra,omitempty"` // string-only at the wire (closed schema)
}

// ProposeArgs is the tool input shape.
type ProposeArgs struct {
    Skill   SkillDraft `json:"skill"`
    Persist bool       `json:"persist,omitempty"`
}

// SkillReceipt is the tool output shape.
type SkillReceipt struct {
    Validated bool          `json:"validated"`
    Persisted bool          `json:"persisted"`
    Result    ProposeResult `json:"result"`
    Name      string        `json:"name"`
    Hash      string        `json:"hash"`
    Origin    skills.Origin `json:"origin"`
    OriginRef string        `json:"origin_ref"`
    Scope     skills.Scope  `json:"scope"`
}

// ErrSkillConflict — the conflict policy rejected the persist.
// Reason names the rule that fired (e.g. "pack_import_protected").
type ErrSkillConflict struct {
    Name   string
    Reason string
}

func (e *ErrSkillConflict) Error() string { /* ... */ }

// Deps carries runtime dependencies.
type Deps struct {
    Bus      events.EventBus
    Redactor audit.Redactor
}

// Register installs skill_propose into catalog wired against store.
func Register(catalog tools.ToolCatalog, store skills.SkillStore, deps Deps) error

// Propose is the underlying handler exposed for direct Go-level
// invocation by composition code that doesn't go through the catalog.
func Propose(ctx context.Context, store skills.SkillStore, deps Deps, args ProposeArgs) (SkillReceipt, error)

// Promote writes sibling rows under each target identity so the
// caller-stamped Scope=project/tenant promise becomes visible from
// the target sessions. Emits skill.proposed (Result="persisted")
// for every successful target write.
func Promote(ctx context.Context, store skills.SkillStore, deps Deps,
    src identity.Quadruple, name string, targets []identity.Quadruple, scope skills.Scope) error

const ToolNameSkillPropose = "skill_propose"
```

## Test plan

- **Unit:**
  - `Skill.Validate` passthrough — invalid drafts (empty `Name` / `Trigger` / `Steps`) return wrapped `skills.ErrInvalidSkill` BEFORE any DB call. `persist=false` and `persist=true` both fail at validation.
  - `persist=false` happy path: returns `Validated=true`, `Hash` matches `skills.CanonicalContentHash(skill)`, NO DB row, NO audit event landed (bus subscriber asserts).
  - `persist=true` happy path (no existing row): row visible via `SkillStore.Get` after; `skill.proposed` event landed with `Result="persisted"`; payload `Hash` matches receipt's `Hash`; payload's `Title` is redacted-pass-through (the test injects a `Bearer xyz` substring in `Title`, asserts the audit payload's redacted form does not contain the bearer literal).
  - Conflict policy axes: pack-existing → REFUSED with `ErrSkillConflict{Reason:"pack_import_protected"}` + `Result="rejected"` event; generated-existing-same-hash → idempotent receipt + `Result="idempotent"` event; generated-existing-different-hash → LWW overwrite + `Result="persisted"` event.
  - Scope default: omitted Scope on draft → final stamped Scope is `ScopeProject`.
  - `OriginRef` stamping: format exactly `gen:{session_id}:{run_id}`. The `run_id` field on the ctx Quadruple is the source; identity-only ctx (no RunID) yields `gen:{session_id}:` (the test exercises both populated and empty RunID paths).

- **Integration:**
  - `TestIntegration_GeneratorEndToEnd_AgainstLocalDB`: real catalog + real localdb + real bus + real redactor. Propose with `persist=true` under identity A → call Phase 38's `skill_search` for the same name → row appears in ranked results. Brief 04 §6 "generator end-to-end".
  - `TestIntegration_CrossSessionPromotion_AgainstLocalDB`: identity A persists `Scope=session` skill. Identity B (same tenant+user, different session) calls `skill_search` → 0 rows. Identity A calls `Promote(... src=idA, name, targets=[idB], scope=ScopeProject)`. Identity B re-calls `skill_search` → MUST find. Verified ALSO via direct `store.Get(ctx, idB, name)` to disambiguate from filter behaviour.

- **Conformance:**
  - N/A — Phase 41 owns one concrete generator wrapper. Future drivers (Portico generator) reuse Phase 37's conformance suite via `SkillStore.Upsert`; the generator itself is exercised through unit + integration tests.

- **Concurrency / leak:**
  - **D-025 stress (`concurrent_test.go`):** N=128 goroutines invoke `skill_propose(persist=true)` with per-goroutine identity quadruples (distinct session IDs so the storage layer's identity filter forces independent rows) AND distinct skill names. Asserts: no data races; per-goroutine bus subscription sees only its own goroutine's `skill.proposed` events; goroutine baseline restored within 500ms of WaitGroup join.
  - **Concurrent same-name resolution:** spawn 16 goroutines all proposing the SAME `Name` under the same identity. Exactly one observes `Result="persisted"` on the first write; the rest see `Result="idempotent"` (their hash matches the first writer's persisted hash). The test seeds no pack row, so the expected outcome is "exactly one persisted, fifteen idempotent."

- **Failure-mode:**
  - **Audit emit failure (`audit_emit_failure_test.go`):** inject a `bus.Publish` that returns an error on `skill.proposed` emit. `skill_propose(persist=true)` returns a wrapped error naming the audit-emit failure. `store.Get` of the just-proposed name returns `ErrSkillNotFound` (the persist was rolled back).
  - **Redactor error:** inject a `Redactor` that returns an error on `Redact`. `skill_propose` returns a wrapped error; no audit event landed; no DB write committed (the redactor is run BEFORE `store.Upsert`).

## Smoke script additions

`scripts/smoke/phase-41.sh` runs `go test -race -count=1 -timeout 120s ./internal/skills/generator/...` and verifies `ToolNameSkillPropose = "skill_propose"` is referenced by string-grep in `internal/skills/generator/generator.go` (matches Phase 38's pin-the-constant pattern). Re-runs `go test -race -count=1 -timeout 120s ./internal/skills/...` to assert the Phase 37 + Phase 38 surfaces still pass with the generator added. No Protocol surface yet — that lands in Phase 60+.

## Coverage target

- `internal/skills/generator`: 90%

## Dependencies

- Phase 37 (skill store: `SkillStore`, `Skill`, `Origin`, `Scope`, `CanonicalContentHash`, `ErrPackOverwriteRefused`, `ErrIdentityRequired`, `EmitIdentityRejected`).
- Phase 38 (planner-tools shape + the `Register(catalog, store, deps)` pattern + `inproc.RegisterFunc[I, O]` registration).
- Phase 03 (audit redactor: `audit.Redactor`, `audit.MustFrom`).

## Risks / open questions

- **Audit-emit failure → persist rollback semantics.** Phase 37's `SkillStore.Upsert` commits the SQL transaction BEFORE emitting `skill.upserted`. Phase 41 wraps that — the generator's `propose` calls `store.Upsert` (which emits `skill.upserted`), then emits the additional `skill.proposed` audit event. If `skill.proposed` emit fails, the generator must roll back. The simplest rollback is `store.Delete(ctx, q, name)` — but that ALSO emits `skill.deleted`. The test asserts: the row is gone after the rollback AND the rollback's `skill.deleted` event landed (the audit pipeline sees the full lifecycle: `skill.upserted → audit_emit_failure → skill.deleted`). Documented in D-054.
- **Default `Scope=project` vs. `Scope=session` debate.** RFC §6.7 ("Generator scope default — Settled") + brief 04 Q-4 ("scope_mode = project") both pick project. The narrower `Scope=session` would minimize cross-session leak surface but breaks the predecessor's UX (a session-scoped generated skill would not be discoverable from the same user's next session — defeating the "in-runtime skill author writes a reusable skill" use case). D-054 records the trade-off.
- **`Promote` is a Go-level API not a planner-callable tool.** Cross-session promotion is an operator concern, not an in-session planner concern. Surfacing `skill_promote` as a planner tool would expose every running session to the cross-session-write capability — a privilege escalation. The Go-level API restricts the surface to runtime composition code (Phase 60+ bootstrap). Phase 39's Directory subsystem will layer a more ergonomic promotion surface; Phase 41's API is the minimum-viable seam.
- **Source-excerpt hash (deferred).** Brief 04 §4.8 calls for a `source_excerpt_hash` so the audit trail correlates with the LLM transcript. Reserved for a follow-up phase. Recorded in "Findings I'm departing from".

## Glossary additions

- **`skill_propose`** — planner-callable tool (`internal/skills/generator`) that validates an LLM-drafted skill and, when `persist=true`, stamps `Origin=Generated` + `OriginRef = "gen:{session_id}:{run_id}"` + `Scope` (default `project`), checks the conflict policy, upserts via `SkillStore.Upsert`, and emits a mandatory `skill.proposed` audit event. RFC §6.7.
- **`Origin=Generated`** — provenance marker stamped by `skill_propose(persist=true)`. Distinguishes runtime-generated skills from `Origin=PackImport` (Phase 40's importer). Conflict policy: a Generated row cannot overwrite a PackImport row of the same name; Generated→Generated is content-hash-gated LWW. RFC §6.7, brief 04 §4.8.
- **`OriginRef`** — lineage pointer for the skill. For `Origin=PackImport`: `"<pack-name>@<version>"`. For `Origin=Generated`: `"gen:{session_id}:{run_id}"`. Audit subscribers correlate proposed skills back to their session/run of origin. RFC §6.7, brief 04 §4.8.
- **`SkillDraft`** — planner-supplied input shape for `skill_propose`. Mirrors `skills.Skill` but excludes provenance + lifecycle fields the generator stamps itself. The wire schema uses `map[string]string` for `Extra` (closed JSON Schema for reflection-derived catalog registration).
- **`SkillReceipt`** — `skill_propose` output. Carries `Validated`, `Persisted`, `Result` (`validated | persisted | idempotent | rejected`), `Name`, `Hash`, `Origin`, `OriginRef`, `Scope`.
- **`ProposeResult`** — enum on `SkillReceipt` and `SkillProposedPayload`. Four values: `validated` (persist=false), `persisted` (insert OR LWW overwrite), `idempotent` (hash-matched existing Generated), `rejected` (pack-protected conflict).
- **`ErrSkillConflict`** — typed error returned by `skill_propose(persist=true)` when the conflict policy rejects the persist. Carries `Name` + `Reason` (e.g. `"pack_import_protected"`). Compared via `errors.As`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §6.7`, `brief 04`) resolve
- [ ] Coverage on `internal/skills/generator` ≥ 90%
- [ ] If multi-isolation paths changed: cross-session isolation test passes (yes — `TestIntegration_CrossSessionPromotion_AgainstLocalDB`)
- [ ] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** Yes — `internal/skills/generator/concurrent_test.go` ships the N=128 stress.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists, wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** Yes — `internal/skills/generator/integration_test.go` ships two integration scenarios (end-to-end + cross-session promotion); `audit_emit_failure_test.go` covers the load-bearing failure mode.
- [ ] If new vocabulary: glossary updated (yes — seven new terms)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (yes — D-054)
