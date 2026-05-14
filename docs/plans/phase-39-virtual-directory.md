# Phase 39 — Virtual directory subsystem

## Summary

Phase 39 lands Harbor's virtual-directory shape: a bounded, identity-scoped, capability-filtered, redacted snapshot of the skill catalogue exposed via `Directory.View(ctx, cap)`. The Directory blends pinned skills (declared via `DirectoryConfig.Pinned`) with `pinned_then_recent` (sort by `UpdatedAt DESC`) or `pinned_then_top` (sort by `UseCount DESC`) selection up to `MaxEntries`. The phase consumes the Phase 37 `SkillStore` surface; the capability-filter and tool-name-scrub logic is shared with Phase 38's planner tools via the `internal/skills/capfilter` leaf package (see the departure note below — a "direct import" of `tools.Filter`/`tools.Redact` is blocked by an import cycle, so the shared logic lives one level down in `capfilter`). The directory is the consumer of the catalog the planner already trusts.

## RFC anchor

- RFC §6.7

## Briefs informing this phase

- brief 04

## Brief findings incorporated

- **brief 04 §4.6 (virtual-directory shape).** "The virtual directory is a small, identity-scoped, capability-filtered snapshot of the catalog for cheap browsing. The provider blends pinned skills (config-declared) with either most-recently-used or most-frequently-used up to `max_entries`. Output is `[]DirectoryEntry{name, title?, trigger?, task_type?}`, redacted before injection." Phase 39 ports the shape verbatim: `SkillView` carries the four planner-visible projection fields; `pinned_then_recent` sorts the non-pinned remainder by `UpdatedAt DESC`; `pinned_then_top` sorts by `UseCount DESC`. Pinned skills always appear in the View when they pass the capability filter under the identity.
- **brief 04 §3 (config shape).** "`VirtualDir { Pinned []string; MaxEntries int (default 30, ge=1 le=200); IncludeFields []string; Selection string // pinned_then_recent | pinned_then_top }`." Phase 39 keeps the same envelope: `DirectoryConfig.Pinned` is a name list (identity-scoped lookups); `MaxEntries` defaults to 30 with the brief's range gates; `Selection` is one of the two string constants. `IncludeFields` is deferred — the four `SkillView` fields are emitted unconditionally at V1 (consumer-side projection is cheap; per-call field inclusion ceremony is post-V1).
- **brief 04 §4.5 + §6 (capability + redaction at injection time).** "Disallowed tool names are scrubbed from skill text before injection; PII redaction runs over titles/triggers/steps/preconditions/failure_modes when `redact_pii=true`." Phase 39 shares the subset/scrub primitives with Phase 38 via the `internal/skills/capfilter` leaf package — one implementation, two consumers, not a parallel implementation (the import-cycle reason a "direct import" of `tools.Filter`/`tools.Redact` is impossible is in the departure note below).
- **brief 04 §6 (test surface).** "Virtual directory: pinned-then-recent vs pinned-then-top selection; respects `max_entries`; identity-scoped." Phase 39 ships unit tests for each axis plus property tests on the three invariants (pinned-always-included, length ≤ MaxEntries, identity scoping).
- **brief 04 §4.2 (fail-closed identity).** "If the identity triple is incomplete, the operation behaves as if memory is disabled and emits an audit event, never returns data scoped to a default." Phase 39 reads identity from `ctx` (matching Phase 38's `identityFromCtx`) and returns wrapped `ErrIdentityRequired` AND emits `skill.identity_rejected` via `skills.EmitIdentityRejected`. The same identity-mandatory contract as the planner-facing tools.

## Findings I'm departing from (if any)

- **`IncludeFields` is deferred.** Brief 04 §3 lists `IncludeFields []string` on `VirtualDir`; Phase 39 always emits the four projection fields (`Name`, `Title`, `Trigger`, `TaskType`). Rationale: the projection is consumer-side; the cost of carrying the four strings per entry is negligible (≤ 200 rows × four strings); a per-call field knob would introduce a hidden-state branch (some callers see Title, some don't) that breaks the SkillView's wire-stability for downstream consumers. If a future caller surfaces a real need to drop a field (e.g. to keep a Console projection under a render budget), the knob lands then with one test per included combination. Recorded in D-052.
- **Pinning is encoded via `Skill.Extra["pinned"] = true`, not a dedicated column.** Phase 37's `SkillStore` does NOT carry a `Pin` / `Unpin` method, and adding one would touch every driver mid-wave. The directory observes pinning via two channels: (a) the `DirectoryConfig.Pinned` name list (the operator-declared, config-time anchor) and (b) `Skill.Extra["pinned"]` (the runtime-stamped anchor, populated by a future operator tool / Console action). Both are checked at View time; either one marks a skill pinned. The current LocalDB driver round-trips `Extra` through JSON unchanged (`marshalExtra` / `unmarshalExtra` in `internal/skills/drivers/localdb/localdb.go`), so no schema change is required. Recorded in D-052.
- **Capability-filter / scrub primitives live in `internal/skills/capfilter`, NOT reused "by direct import" from `internal/skills/tools`.** This plan's original text claimed Phase 39 reuses `tools.Filter` / `tools.Redact` directly. That is not possible: `internal/skills/tools` imports `internal/skills`, so `internal/skills` (where `directory.go` lives) cannot import `internal/skills/tools` — an import cycle. Phase 39 as shipped duplicated the subset/scrub logic inline in `directory.go`, which is the CLAUDE.md §13 "two parallel implementations of one feature" anti-pattern. The Wave 8 §17.5 checkpoint audit closed this for real (per §17.6 — fix it where the bug lives): the subset gate, disallowed-name computation, replacement selection, and word-boundary scrub were extracted into a new leaf package `internal/skills/capfilter` (stdlib-only, so both `internal/skills` and `internal/skills/tools` import it without a cycle). The capability-filter logic now lives in exactly one place. `tools.Filter` / `tools.Redact` keep their signatures (they operate on `skills.Skill`); the directory does its own per-`Skill` field plumbing. Recorded in D-052.

## Goals

- Ship `internal/skills/directory.go` with the `Directory` struct, `DirectoryConfig`, `SkillView`, `Selection` string constants (`pinned_then_recent` / `pinned_then_top`), `MaxEntries` default + range gates, and the `Directory.View(ctx, cap)` API.
- Identity-mandatory: every `View` call validates the identity triple from `ctx`. Missing triple returns wrapped `skills.ErrIdentityRequired` AND emits `skill.identity_rejected` on the bus.
- Capability filter + tool-name scrub: the directory shares the subset/scrub logic with Phase 38 via the `internal/skills/capfilter` leaf package. The filter/scrub *logic* lives in exactly one place; the directory does its own per-`Skill` field plumbing (capfilter operates on primitive inputs, never on `skills.Skill`, to stay a cycle-free leaf).
- Pinned skills always appear in the View when they pass the capability filter under the identity AND fit within `MaxEntries`. (Pinned skills are EXEMPT from the capability check only when explicitly documented — V1 stance is that even pinned skills must pass the filter so a misconfigured allowed-set cannot leak high-capability skills.)
- Selection semantics: `pinned_then_recent` lists pinned skills first (in their `DirectoryConfig.Pinned` declaration order, then by `UpdatedAt DESC` on the remainder), `pinned_then_top` lists pinned first then sorts the remainder by `UseCount DESC`. Ties broken by `Name ASC` for deterministic ordering.
- Concurrent-reuse contract (D-025): N≥128 goroutines invoking `View` against ONE shared `*Directory` under `-race`. Per-goroutine identity, per-goroutine expected pinning set, no identity bleed, no goroutine leaks.
- Property tests using `testing/quick` on three invariants: pinned-always-included when count ≤ MaxEntries; View length ≤ MaxEntries; identity scoping (a skill scoped to tenant A is NEVER in the View of identity B).

## Non-goals

- Skill-store schema changes — pinning rides on `Skill.Extra["pinned"]` + `DirectoryConfig.Pinned`. No `Pin` / `Unpin` methods on `SkillStore` at this phase.
- Operator-facing `IncludeFields` knob — deferred per the departure above.
- Protocol surface for `DirectoryEntries` — Phase 60+ exposes the Console projection over the Protocol.
- Skills.md importer (Phase 40) and in-runtime generator (Phase 41) — sibling phases sit alongside.
- A new PII redactor — PII scrubbing stays in `tools.Redact` (Phase 38); the directory projects only `Name`/`Title`/`Trigger`/`TaskType`, and tool-name scrubbing of `Title`/`Trigger` goes through the shared `capfilter` primitives. (The `capfilter` leaf package itself IS new — it was extracted by the Wave 8 audit to kill the inline duplication; see the departure note. It carries no PII logic.)
- Pin/Unpin runtime tooling (a `skill_pin` planner tool) — that lands when the operator surface is wired (Console phases); the storage round-trip is in place at Phase 39 via `Skill.Extra`.

## Acceptance criteria

- [ ] `internal/skills/directory.go` defines `Directory`, `DirectoryConfig`, `SkillView`, `SelectionPinnedThenRecent` / `SelectionPinnedThenTop`, `DefaultMaxEntries=30`, `MinMaxEntries=1`, `MaxMaxEntries=200`, and the `ErrInvalidConfig` sentinel.
- [ ] `NewDirectory(store, deps, cfg)` validates `cfg`: empty `Selection` defaults to `SelectionPinnedThenRecent`; unknown `Selection` → wrapped `ErrInvalidConfig`; `MaxEntries == 0` defaults to `DefaultMaxEntries`; `MaxEntries < 1 || > 200` → wrapped `ErrInvalidConfig`.
- [ ] `Directory.View(ctx, cap)` validates identity from `ctx` (Quadruple or Identity); missing triple returns wrapped `skills.ErrIdentityRequired` AND emits `skill.identity_rejected` via `skills.EmitIdentityRejected`.
- [ ] `View` fetches all skills under the identity via `SkillStore.List` (Limit=0 → driver default; the directory then bounds locally), applies the `capfilter`-backed capability gate (`filterByCapability`), partitions into pinned/unpinned, sorts each partition per `Selection`, concatenates `pinned ++ unpinned`, truncates to `MaxEntries`, projects to `SkillView` (the projection scrubs disallowed tool names from `Title`/`Trigger` via the shared `capfilter` primitives).
- [ ] **Pinned skills always present** when count(pinned-after-filter) ≤ MaxEntries: the View MUST include every pinned skill that passed the capability filter. When count(pinned-after-filter) > MaxEntries, pinned skills truncate to the first `MaxEntries` (in declaration order, then `UpdatedAt DESC` for the `Extra["pinned"]=true` tail); no unpinned skill appears.
- [ ] **Selection ordering**: `pinned_then_recent` orders the unpinned remainder by `UpdatedAt DESC, Name ASC`; `pinned_then_top` orders the unpinned remainder by `UseCount DESC, Name ASC`. Pinned partition orders config-declared pins first (in declaration order), then `Extra["pinned"]=true` pins by the same per-selection sort.
- [ ] **Identity scoping**: a skill scoped to identity A is NEVER returned in `View` for identity B. The store's `List` already enforces this; Phase 39 inherits the guarantee and asserts it via property tests.
- [ ] **D-025 concurrent-reuse test**: N≥128 goroutines invoke `View` against ONE shared `*Directory`. Each goroutine carries a unique identity + a unique expected pin set; asserts no data races (-race), no identity bleed, no goroutine leaks (`runtime.NumGoroutine` returns to baseline).
- [ ] **Property tests** using `testing/quick`:
  - `Property_PinnedAlwaysIncluded_WhenFitsBudget`: for an arbitrary skill corpus, the View MUST include every pinned skill that passed the capability filter, provided count(pinned-after-filter) ≤ MaxEntries.
  - `Property_ViewLengthBounded`: for any corpus + config, `len(view) ≤ MaxEntries` and `len(view) ≤ len(store-after-filter)`.
  - `Property_IdentityScoping`: for any pair of distinct identities A / B, the View under identity B is disjoint from any skill that exists only under identity A.
- [ ] `internal/skills` coverage ≥ 80% (the existing Phase 37 surface is already ≥ 85%; the new directory file contributes ≥ 80% on its own).
- [ ] Tool name strings (`pinned_then_recent` / `pinned_then_top`) pinned by smoke-script grep.

## Files added or changed

```text
internal/skills/
├── directory.go                              # Directory + DirectoryConfig + SkillView + Selection constants + NewDirectory + View
├── directory_test.go                         # Unit tests (defaults / range gates / selection / pinning / identity)
├── directory_property_test.go                # Property tests (testing/quick) on the three invariants
└── directory_concurrent_test.go              # D-025 N=128 stress
docs/plans/phase-39-virtual-directory.md      # this file
docs/plans/README.md                          # flip Phase 39 row to Shipped
README.md                                     # flip Phase 39 row if present
docs/glossary.md                              # Directory, SkillView, MaxEntries, pinned_then_recent, pinned_then_top
docs/decisions.md                             # D-052 entry: directory shape + pinning model + capability gate
scripts/smoke/phase-39.sh                     # Go-level test surface + selector-string pin
```

## Public API surface

```go
// internal/skills/directory.go

package skills

import (
    "context"
    "errors"

    "github.com/hurtener/Harbor/internal/events"
    skilltools "github.com/hurtener/Harbor/internal/skills/tools"
)

// Selection picks the unpinned-partition ordering. Pinned skills
// always come first; Selection orders only the unpinned remainder
// (and the secondary ordering inside the Extra-pinned subset).
type Selection string

const (
    // SelectionPinnedThenRecent — pinned first, then most-recently-
    // updated skills (UpdatedAt DESC, Name ASC).
    SelectionPinnedThenRecent Selection = "pinned_then_recent"
    // SelectionPinnedThenTop — pinned first, then most-used skills
    // (UseCount DESC, Name ASC).
    SelectionPinnedThenTop Selection = "pinned_then_top"
)

// MaxEntries bounds.
const (
    DefaultMaxEntries = 30
    MinMaxEntries     = 1
    MaxMaxEntries     = 200
)

// ExtraPinnedKey is the Skill.Extra map key the directory treats as a
// runtime-stamped pin marker. Value MUST be the bool `true`; any other
// shape is ignored.
const ExtraPinnedKey = "pinned"

// SkillView is the planner-visible projection of a Skill returned by
// Directory.View. Four fields; the rest of the Skill is dropped at
// the boundary so the View is cheap to inject and Console-renderable
// without leaking storage-layer detail.
type SkillView struct {
    Name     string `json:"name"`
    Title    string `json:"title,omitempty"`
    Trigger  string `json:"trigger,omitempty"`
    TaskType string `json:"task_type,omitempty"`
    // Pinned is true when the skill was anchored by either
    // DirectoryConfig.Pinned (config-time) or Skill.Extra["pinned"]
    // (runtime-time). Useful for Console rendering.
    Pinned bool `json:"pinned"`
}

// DirectoryConfig configures one Directory instance.
type DirectoryConfig struct {
    // Pinned is the operator-declared list of skill names to anchor
    // at the top of every View. Order is preserved across calls.
    // Names that don't exist under the calling identity are dropped
    // silently (matching the brief's "config + storage may disagree;
    // storage wins" stance).
    Pinned []string
    // MaxEntries caps the View length. 0 → DefaultMaxEntries (30).
    // Outside [1, 200] → NewDirectory returns wrapped ErrInvalidConfig.
    MaxEntries int
    // Selection picks the unpinned-partition ordering. Empty →
    // SelectionPinnedThenRecent.
    Selection Selection
}

// Directory exposes the virtual-directory snapshot. Built once at
// boot (or per operator config reload); safe to share across N
// concurrent goroutines (D-025).
type Directory struct {
    // unexported fields: store, bus, cfg, pinSet
}

// NewDirectory validates cfg and returns a usable Directory.
// Empty Selection → SelectionPinnedThenRecent; MaxEntries==0 →
// DefaultMaxEntries; MaxEntries outside [1,200] → wrapped
// ErrInvalidConfig; unknown Selection → wrapped ErrInvalidConfig.
//
// store and deps.Bus are mandatory; nil returns wrapped error.
func NewDirectory(store SkillStore, deps Deps, cfg DirectoryConfig) (*Directory, error)

// View returns the identity-scoped, capability-filtered, redacted,
// bounded snapshot. Identity flows from ctx (Quadruple or Identity).
// Missing identity → wrapped ErrIdentityRequired + skill.identity_rejected
// emit.
func (d *Directory) View(ctx context.Context, cap skilltools.CapabilityContext) ([]SkillView, error)

// ErrInvalidConfig — NewDirectory rejected the DirectoryConfig
// (unknown Selection, out-of-range MaxEntries, nil store/bus).
// Fail-loud per CLAUDE.md §5 + §13.
var ErrInvalidConfig = errors.New("skills: invalid directory config")
```

## Test plan

- **Unit (`directory_test.go`):**
  - `NewDirectory` defaults: empty Selection → `SelectionPinnedThenRecent`; `MaxEntries=0` → 30.
  - `NewDirectory` range gates: `MaxEntries=0` → 30; `MaxEntries=-1`, `MaxEntries=201` → wrapped `ErrInvalidConfig`; `MaxEntries=1` and `MaxEntries=200` accepted.
  - `NewDirectory` unknown Selection: `Selection="unknown"` → wrapped `ErrInvalidConfig` naming both valid values.
  - `NewDirectory` nil store / nil bus → wrapped `ErrInvalidConfig`.
  - `View` missing identity (bare ctx) → wrapped `skills.ErrIdentityRequired`; spy bus captures one `skill.identity_rejected` emit.
  - `View` `pinned_then_recent`: seed 5 skills with distinct `UpdatedAt`; assert unpinned remainder ordered by `UpdatedAt DESC, Name ASC`.
  - `View` `pinned_then_top`: seed 5 skills with distinct `UseCount`; assert unpinned remainder ordered by `UseCount DESC, Name ASC`.
  - `View` pinned-by-config: `DirectoryConfig.Pinned = []string{"alpha", "bravo"}` → both appear first in declaration order, regardless of their `UpdatedAt` / `UseCount`.
  - `View` pinned-by-Extra: a skill with `Extra["pinned"]=true` appears in the pinned partition (after config-declared pins, sorted by the selection's secondary key).
  - `View` MaxEntries truncation: seed 50 skills, `MaxEntries=10` → `len(view) == 10`.
  - `View` pinned-overflow: 30 pinned-by-config + 10 unpinned, `MaxEntries=20` → 20 pinned (declaration-order first 20); zero unpinned.
  - `View` capability filter: a skill with `RequiredTools=["fs_write"]` + `cap.AllowedTools=["http_fetch"]` is excluded from the View (even when pinned).
  - `View` redaction: a skill containing a disallowed tool name is rewritten in the planner-visible projection (title / trigger).
  - `View` empty store → empty View (no error).
  - `View` capability filter excludes all → empty View (no error).

- **Property (`directory_property_test.go`, `testing/quick`):**
  - `Property_PinnedAlwaysIncluded_WhenFitsBudget` — for an arbitrary skill corpus where count(pinned-after-filter) ≤ MaxEntries, every pinned skill that passes the capability filter MUST appear in the View. Counterexample-shrinking lives in `quick.Check`.
  - `Property_ViewLengthBounded` — for any corpus + config, `len(view) ≤ MaxEntries` and `len(view) ≤ len(store-after-filter)`.
  - `Property_IdentityScoping` — for any pair of distinct identities A / B with disjoint skill sets, the View under identity B is disjoint from the View under identity A.

- **Concurrency (`directory_concurrent_test.go`):**
  - **D-025 stress:** N=128 goroutines invoke `View` against ONE shared `*Directory`. Each goroutine carries a unique identity (`tenant=t-i, user=u-i, session=s-i`) and seeds the spy store with its own unique pin set; asserts the returned View contains exactly the goroutine's pin set + the goroutine's unpinned tail. Asserts no data races (-race), no identity bleed (per-goroutine identity stamp on every returned row), no goroutine leak (`runtime.NumGoroutine` returns to baseline within 500ms of WaitGroup join).
  - **Per-goroutine cancellation isolation:** a fraction of goroutines pre-cancel their ctx; assert sibling goroutines' Views are unaffected.

- **Integration:**
  - Folded into the same test file as unit (the directory's "real driver" boundary is the existing `SkillStore` — already exercised end-to-end by Phase 37's localdb tests + Phase 38's integration test). No new integration test file; `directory_test.go` covers the seam via the spy store + capability filter wiring. The Phase 38 integration test against the localdb driver indirectly exercises the same `Filter` + `Redact` primitives the directory reuses.

- **Conformance:** N/A — the directory is a single in-process consumer of `SkillStore`; no driver pluralism at this phase.

## Smoke script additions

`scripts/smoke/phase-39.sh` runs `go test -race -count=1 -timeout 120s ./internal/skills/...` to assert the directory tests pass alongside the Phase 37 surface. Additionally pins the two selector constant strings (`pinned_then_recent`, `pinned_then_top`) via grep on `internal/skills/directory.go` so a silent rename surfaces here. No Protocol surface yet — Phase 60+ exposes the Console projection.

## Coverage target

- `internal/skills` (the directory file specifically): ≥ 80%.

## Dependencies

- Phase 37 (`SkillStore`: `Skill`, `ListFilter`, `Get`, `List`, `EmitIdentityRejected`, `ErrIdentityRequired`).
- Phase 38 (`tools.CapabilityContext` shape; the subset/scrub logic Phase 38 also uses now lives in the shared `internal/skills/capfilter` leaf package — see the departure note).

## Risks / open questions

- **Pinning persistence model.** Pinning rides on `Skill.Extra["pinned"]` + `DirectoryConfig.Pinned` rather than a dedicated `SkillStore.Pin / Unpin` method. The brief 04 §3 sketch mentions a `Pinned []string` config field but not a runtime pin method; Phase 39 follows the brief verbatim. When an operator pin tool / Console action surfaces in a later phase, the runtime side is one line: stamp `Skill.Extra["pinned"] = true` via `SkillStore.Upsert`. The directory reads from both channels at `View` time; no schema migration required.
- **Pinned skills are NOT capability-filter-exempt.** A pinned skill whose `RequiredTools / RequiredNS / RequiredTags` is not a subset of the planner-supplied allowed-set is still excluded from the View. The alternative ("pinned always wins regardless of capability") would silently leak high-capability skills the first time an operator pinned something incompatible with a run's capability envelope. The capability gate is integrity-critical (CLAUDE.md §6 rule 9 + Phase 38 D-048 default-deny stance); pinning is an ordering preference, not a security exemption. Recorded in D-052.
- **Selection ties broken by `Name ASC`.** For deterministic ordering across concurrent reads (and across `testing/quick` property tests), every secondary sort breaks ties by `Name ASC`. Without this, `UpdatedAt` ties on identical timestamps would produce non-deterministic ordering and the property tests would flake under `-race`.
- **MaxEntries == 0 means "default" not "unbounded".** The brief 04 §3 sketch says "default 30, ge=1 le=200." Phase 39 ports the range gates: `MaxEntries == 0` is treated as "default 30" (operator left the field empty); `MaxEntries == -1` or `MaxEntries == 201` is an explicit out-of-range error. There is no opt-out — an operator cannot request "unlimited" because the Director's purpose is bounded injection.
- **`testing/quick` arbitrary-skill generators** — Skill is a complex struct (20+ fields, slices, maps). The property tests use a hand-rolled `quick.Generator` for `Skill` that produces JSON-tree-compatible shapes (deterministic seeds; bounded slice lengths; printable string fields) so the property tests don't blow up the heap. Documented in the test file.

## Glossary additions

- **Directory** — Phase 39's virtual-directory shape. `Directory.View(ctx, cap)` returns a bounded, identity-scoped, capability-filtered, redacted snapshot of the skills catalogue. Built via `NewDirectory(store, deps, cfg)`; safe to share across N concurrent goroutines (D-025). RFC §6.7, brief 04 §4.6.
- **`pinned_then_recent`** — Selection constant: pinned skills first (declaration order from `DirectoryConfig.Pinned`, then `Extra["pinned"]=true` ordered by `UpdatedAt DESC, Name ASC`); unpinned remainder ordered by `UpdatedAt DESC, Name ASC`. The default.
- **`pinned_then_top`** — Selection constant: pinned skills first (same partition rule); unpinned remainder ordered by `UseCount DESC, Name ASC`. For Console "most-used" views.
- **MaxEntries** — `DirectoryConfig.MaxEntries`. Default 30; range [1, 200]. Outside-range → wrapped `ErrInvalidConfig`. `MaxEntries == 0` is treated as "default" (operator left the field unset); there is no unbounded mode.
- **SkillView** — `Directory.View` row projection: `Name`, `Title`, `Trigger`, `TaskType`, `Pinned`. Four planner-visible fields; rest of `Skill` dropped at the boundary. RFC §6.7.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §6.7`, `brief 04`) resolve
- [ ] Coverage on `internal/skills` (directory file) ≥ 80%
- [ ] If multi-isolation paths changed: cross-session isolation test passes (yes — property test `Property_IdentityScoping` + D-025 stress with per-goroutine identities)
- [ ] **Concurrent-reuse test**: N=128 against a shared Directory under `-race` — `internal/skills/directory_concurrent_test.go`
- [ ] **Integration test**: Phase 38's integration test exercises the shared filter/redact primitives the directory reuses; the directory's own seam (store → filter → redact → project) is covered by `directory_test.go` against a spy store that mirrors the production identity-validation contract
- [ ] Glossary updated (yes)
- [ ] Brief-finding departures (`IncludeFields` deferred; `Extra["pinned"]` model) documented above and in D-052
- [ ] D-052 entry filed in `docs/decisions.md`
