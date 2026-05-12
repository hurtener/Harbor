# Phase 38 — Skill planner tools (search / get / list)

## Summary

Phase 38 lands the planner-facing surface for Harbor's skills subsystem: three Tools (`skill_search`, `skill_get`, `skill_list`) registered through the Phase 26 catalog. Each tool wraps the Phase 37 `SkillStore` with capability filtering (`RequiredTools / RequiredNS / RequiredTags ⊆ allowed sets`), injection-time redaction (disallowed tool names scrubbed; optional PII redaction over titles / triggers / steps / preconditions / failure modes), and a tiered budgeter (full → drop optional → cap steps to 3 → `ErrSkillTooLarge`). The package self-registers via `init()`-time blank-imported from `cmd/harbor/main.go` so the catalog seam fires at boot.

## RFC anchor

- RFC §6.7
- RFC §6.4

## Briefs informing this phase

- brief 04

## Brief findings incorporated

- **brief 04 §4.5 (capability filtering).** "`_skill_is_applicable` gates each candidate: the skill's `RequiredTools/Namespaces/Tags` must be subsets of the allowed sets." Phase 38 ports the predecessor's subset gate verbatim onto `CapabilityContext{AllowedTools, AllowedNamespaces, AllowedTags}` — `len(required) == 0` is unconstrained; any required member missing from the corresponding allowed set excludes the skill.
- **brief 04 §4.5 (tool-name redaction).** "Disallowed tool names are scrubbed from skill text before injection; replacement is `"a suitable tool (use tool_search)"` when search is available, else `"a suitable tool"`." Phase 38 implements both replacement variants behind a single `Redactor` and selects based on whether the run's catalog view contains `tool_search` (proxy: an `AllowedTools` membership probe).
- **brief 04 §4.5 (tiered budgeter).** "Start full → drop optional (preconditions, failure_modes) → cap steps to 3. Stop at the first attempt that fits within `max_tokens`." Phase 38's `Budgeter.Fit(skills, maxTokens)` implements three ladder steps in order; if step 3 still exceeds the budget, the function returns `ErrSkillTooLarge` (fail-loud per CLAUDE.md §5).
- **brief 04 §4.5 (PII redaction).** "PII redaction (email/phone/bearer-tokens/URL query strings) runs over titles/triggers/steps/preconditions/failure_modes when `redact_pii=true`." Phase 38 implements a regex-driven PII redactor reused across the three text-bearing fields; the `RedactPII bool` knob lives on the planner-tool `CapabilityContext` so policy is per-call rather than per-process.
- **brief 04 §6 (tests required).** "Capability filter: skill filtered out when `required_tools` not subset of `allowed`; redacted text strips disallowed names; `tool_search`-aware replacement string." Phase 38 ships unit tests for each branch.

## Findings I'm departing from (if any)

None. Phase 38 ports brief 04 §4.5's three settled mechanics (capability filter, tool-name + PII redaction, tiered budgeter) verbatim. The departure is purely surface-shape: brief 04 §3 shows a `SkillProvider` interface with `Search / GetByName / List / Directory / FormatForInjection`; Phase 38 splits the planner-facing tools (`skill_search` / `skill_get` / `skill_list`) from the directory subsystem (Phase 39 owns `Directory`). The capability + redaction + budgeter mechanics live in `internal/skills/tools/` so the directory phase can re-use them without an awkward import.

## Goals

- Ship three planner-callable Tools (`skill_search`, `skill_get`, `skill_list`) registered through the Phase 26 catalog via `tools.RegisterFunc` with reflection-derived schemas.
- Package self-registers its tools at `init()` by calling a `Register(catalog tools.ToolCatalog, store skills.SkillStore, deps Deps) error` helper from a thin bootstrap in `cmd/harbor/main.go` (the catalog + store live in the bootstrap, not at package-init — only the helper is package-level so the seam is observable from tests).
- Capability filter: `CapabilityContext{AllowedTools, AllowedNamespaces, AllowedTags, RedactPII}` carried on the planner-tool args; the filter excludes skills whose `RequiredTools` / `RequiredNS` / `RequiredTags` are NOT subsets of the corresponding allowed set.
- Redactor: scrubs disallowed tool names from skill text (titles / triggers / descriptions / steps / preconditions / failure modes) and replaces with `"a suitable tool (use tool_search)"` when `tool_search` is in the allowed set, `"a suitable tool"` otherwise. When `RedactPII=true`, additionally redacts emails / phone / bearer tokens / URL query strings.
- Tiered budgeter: ladder = full → drop optional (`Preconditions`, `FailureModes`) → cap `Steps` to 3 → `ErrSkillTooLarge`. Token estimate is chars / 4 (the planner-side estimator stays consistent with the LLM safety net at §6.5).
- Identity-mandatory at every tool: the args carry the identity quadruple implicitly via `ctx`, and the tool rejects with wrapped `ErrIdentityRequired` (via `skills.EmitIdentityRejected`) on a missing component.
- Concurrent-reuse contract (D-025): one Register call → three reusable descriptors; N≥128 concurrent invocations against a shared catalog under `-race`.

## Non-goals

- Virtual-directory subsystem (`Directory(cfg)`, `pinned_then_recent` / `pinned_then_top`) — owned by Phase 39.
- Skills.md importer (parser, normaliser, attachment resolver, round-trip) — owned by Phase 40.
- In-runtime generator (`skill_propose(persist=true)`, generator audit) — owned by Phase 41.
- Tokenizer fidelity beyond chars/4 — the planner-side estimator stays consistent with the §6.5 LLM safety net (also chars/4 at V1); a tokenizer-backed estimator is post-V1.
- HTTP / Protocol surface for the three tools — Phase 60+ exposes the tool catalog over the Protocol.
- Per-tool prompt formatting beyond the redacted+budgeted text envelope — the Phase 45 ReAct planner owns prompt assembly.

## Acceptance criteria

- [ ] `internal/skills/tools/tools.go` defines `CapabilityContext`, `Deps`, `Register(catalog, store, deps)` + the three handler functions (`searchHandler`, `getHandler`, `listHandler`).
- [ ] `internal/skills/tools/filter.go` defines `Filter(skills []skills.Skill, cap CapabilityContext) []skills.Skill` excluding skills whose `RequiredTools / RequiredNS / RequiredTags` are NOT subsets of the corresponding allowed set.
- [ ] `internal/skills/tools/redactor.go` defines `Redact(s skills.Skill, cap CapabilityContext) skills.Skill` returning a copy with disallowed tool names replaced and (when `RedactPII=true`) PII redacted across titles / triggers / descriptions / steps / preconditions / failure modes.
- [ ] `internal/skills/tools/budgeter.go` defines `Fit(skills []skills.Skill, maxTokens int) ([]skills.Skill, error)` implementing the tiered ladder. Step 4 returns wrapped `ErrSkillTooLarge`.
- [ ] Three Tools registered via `tools.RegisterFunc` with reflection-derived schemas (input shape: `SearchArgs / GetArgs / ListArgs`; output shape: `SearchResult / GetResult / ListResult`).
- [ ] Tool names exactly: `skill_search`, `skill_get`, `skill_list`. `LoadingMode = LoadingAlways` so the planner sees them in every step.
- [ ] Capability filter excludes mismatched skills (tested with required ⊄ allowed across all three axes).
- [ ] Redactor strips disallowed tool names from every text field; replacement string matches `tool_search`-aware selection.
- [ ] Budgeter fits within `max_tokens` for representative corpora; over-budget after step 3 returns `ErrSkillTooLarge` wrapped via `fmt.Errorf("%w: ...")`.
- [ ] Identity-mandatory: missing identity returns wrapped `skills.ErrIdentityRequired` AND emits `skill.identity_rejected` on the bus (reuses Phase 37's `skills.EmitIdentityRejected`).
- [ ] **D-025 concurrent-reuse test**: N≥128 concurrent invocations against a shared catalog with three distinct identities; no data races, no context bleed, no goroutine leaks; under `-race`.
- [ ] Integration test wires the real `tools.Catalog` + `skills.SkillStore` (`localdb` driver against `:memory:`) + `events.EventBus` and asserts a search → get → list round-trip with identity propagation + ≥1 failure mode (capability rejection).
- [ ] `internal/skills/tools/` package coverage ≥ 85%.
- [ ] `cmd/harbor/main.go` blank-imports `internal/skills/tools` so the package's `init()` registers nothing (intentional — registration is a runtime call against a concrete catalog, not a package-level singleton) but the package is in the binary's import graph.

## Files added or changed

```text
internal/skills/tools/
├── tools.go                          # Register + Deps + CapabilityContext + tool args/results + handlers
├── filter.go                         # capability subset filter
├── redactor.go                       # tool-name + PII redactor
├── budgeter.go                       # tiered budgeter (ErrSkillTooLarge)
├── tools_test.go                     # unit tests for handlers + Register
├── filter_test.go                    # capability filter axes
├── redactor_test.go                  # tool-name + PII redaction matrix
├── budgeter_test.go                  # ladder steps + ErrSkillTooLarge
├── concurrent_test.go                # D-025 N=128 stress
└── integration_test.go               # in-package adapter: catalog + store + bus round-trip
docs/plans/phase-38-skill-planner-tools.md  # this file
docs/plans/README.md                  # flip Phase 38 row to Shipped
README.md                             # add Phase 38 status row
docs/glossary.md                      # CapabilityContext, skill_search, skill_get, skill_list, tiered budgeter
docs/decisions.md                     # D-048 — Phase 38 capability + redaction + budgeter call
scripts/smoke/phase-38.sh             # smoke — Go-level test surface (planner tools have no Protocol surface yet)
scripts/smoke/phase-37.sh             # flip to OK assertion — Phase 38 now exists, Phase 37's smoke can also assert internal/skills/... passes
cmd/harbor/main.go                    # blank import internal/skills/tools (presence in import graph for tree-shake awareness)
```

## Public API surface

```go
// internal/skills/tools/tools.go

package tools

// CapabilityContext carries the planner-supplied capability envelope
// at invocation time. Drivers gate skills by subset relations on the
// three Allowed* sets; PII redaction is an opt-in policy knob.
type CapabilityContext struct {
    AllowedTools      []string
    AllowedNamespaces []string
    AllowedTags       []string
    RedactPII         bool
}

// Deps carries the runtime dependencies Register needs.
//
// `Bus` is mandatory so identity-rejection emits land on the audit
// pipeline (Phase 37's contract).
type Deps struct {
    Bus events.EventBus
}

// Register installs `skill_search`, `skill_get`, `skill_list` into
// `catalog` against `store`. Returns wrapped errors from the catalog
// on duplicate names. Safe to call once per process.
func Register(catalog tools.ToolCatalog, store skills.SkillStore, deps Deps) error

// SearchArgs / SearchResult — `skill_search` input/output shapes.
type SearchArgs struct {
    Query      string             `json:"query"`
    Limit      int                `json:"limit,omitempty"`
    Capability CapabilityContext  `json:"capability"`
}
type SearchResult struct {
    Skills []RankedSkill `json:"skills"`
    Path   string        `json:"path"`
}

// GetArgs / GetResult — `skill_get` input/output shapes.
type GetArgs struct {
    Names      []string           `json:"names"`
    MaxTokens  int                `json:"max_tokens,omitempty"`
    Capability CapabilityContext  `json:"capability"`
}
type GetResult struct {
    Skills      []Skill `json:"skills"`
    Summarized  bool    `json:"summarized"`
    DroppedSteps bool   `json:"dropped_steps"`
}

// ListArgs / ListResult — `skill_list` input/output shapes.
type ListArgs struct {
    Scope      skills.Scope       `json:"scope,omitempty"`
    TaskType   string             `json:"task_type,omitempty"`
    Tags       []string           `json:"tags,omitempty"`
    Limit      int                `json:"limit,omitempty"`
    Offset     int                `json:"offset,omitempty"`
    Capability CapabilityContext  `json:"capability"`
}
type ListResult struct {
    Skills []Skill `json:"skills"`
}

// Sentinel.
var ErrSkillTooLarge = errors.New("skills/tools: skill exceeds max_tokens after ladder")

// Tool names — used by the Phase 26 catalog and the planner prompt.
const (
    ToolNameSkillSearch = "skill_search"
    ToolNameSkillGet    = "skill_get"
    ToolNameSkillList   = "skill_list"
)
```

## Test plan

- **Unit:**
  - `Filter` axes: `RequiredTools ⊄ AllowedTools` excludes; `RequiredNS ⊄ AllowedNamespaces` excludes; `RequiredTags ⊄ AllowedTags` excludes; empty-required is unconstrained.
  - `Redactor` tool-name scrub: skill text contains disallowed tool name → replaced with the `tool_search`-aware variant when `AllowedTools` includes `tool_search`, the bare variant otherwise.
  - `Redactor` PII matrix when `RedactPII=true`: emails (`a@b.com`), bearer tokens (`Bearer <jwt>`), phone numbers (`+1 555-123-4567`), URL query strings (`?token=...`) all redacted to `[REDACTED-PII]` across titles / triggers / descriptions / steps / preconditions / failure modes.
  - `Budgeter.Fit` ladder: step 0 (full) fits → returns input; step 1 (drop optional) fits → returns slice with `Preconditions` + `FailureModes` cleared; step 2 (cap steps) fits → returns slice with `Steps` truncated to 3; step 3 (still over budget) → `ErrSkillTooLarge`.
  - `searchHandler` / `getHandler` / `listHandler`: missing identity → wrapped `ErrIdentityRequired` + `skill.identity_rejected` emit; happy-path roundtrip; capability filter + redactor + budgeter compose.

- **Integration:**
  - In-package adapter (`integration_test.go`): build a real `tools.Catalog`, attach a real `events.EventBus` (inmem driver), open a real `skills.SkillStore` (`localdb` driver against `:memory:`), populate with 5 skills, exercise `skill_search` → `skill_get` → `skill_list` through the catalog's `Resolve(name).Invoke(ctx, args)` path. Assert identity propagation, ≥1 failure mode (capability rejection on a skill whose `RequiredTools` is not subset of `AllowedTools`).

- **Conformance:**
  - N/A — Phase 38 owns one concrete planner-tool implementation. Future drivers (Portico SkillStore at post-V1) reuse Phase 37's conformance suite; Phase 38's planner tools are a thin wrapper that is exercised through the integration test.

- **Concurrency / leak:**
  - **D-025 stress (`concurrent_test.go`):** N=128 goroutines invoke a mix of `skill_search` / `skill_get` / `skill_list` against ONE shared catalog with three distinct identities under `-race`. Assert: no data races (race detector), no context bleed (per-goroutine identity scope assertions), no cross-cancellation, no goroutine leak (`runtime.NumGoroutine()` returns to baseline after teardown).

## Smoke script additions

`scripts/smoke/phase-38.sh` runs `go test -race -count=1 -timeout 120s ./internal/skills/tools/...` and verifies the three tool names (`skill_search`, `skill_get`, `skill_list`) are referenced by string-grep so the registration constants don't silently disappear. The script also re-runs `go test -race -count=1 -timeout 60s ./internal/skills/...` to assert the Phase 37 surface still passes once Phase 38's wrapper sits on top. No Protocol surface yet — that lands in Phase 60+.

Additionally, `scripts/smoke/phase-37.sh` flips from the placeholder `skip` to an OK assertion against `go test -race -count=1 -timeout 60s ./internal/skills/...` (the Phase 37 contract is now smoke-observable transitively via the planner-tool surface).

## Coverage target

- `internal/skills/tools`: 85%.

## Dependencies

- Phase 26 (tool catalog: `tools.ToolCatalog`, `tools.RegisterFunc`, `ToolPolicy`).
- Phase 37 (skill store: `skills.SkillStore`, `skills.Skill`, `skills.RankedSkill`, `skills.EmitIdentityRejected`, `skills.ErrIdentityRequired`).

## Risks / open questions

- **Token estimate at V1 is chars/4** — matches the §6.5 LLM safety net's estimator at V1 so the planner's budget math doesn't drift from the safety net's enforcement. A tokenizer-backed estimator (tiktoken / Anthropic counter) is post-V1; if it lands, the Budgeter swaps the estimator behind a single function rather than re-doing the ladder.
- **Tool-name redaction string matching uses word-boundary regex** so a tool named `email` doesn't false-positive on `"emails"`. The regex is compiled once at Register time and cached on a per-Redactor instance; allocations stay flat across invocations.
- **PII redaction patterns are best-effort.** The four canonical patterns (email / phone / bearer / URL query) catch the bulk of operator-controllable inputs; the redaction is documented as fail-loud-on-pattern-error, never silently passing through a panicking regex. Operator-customisable PII patterns are deferred to a later phase.
- **No package-level catalog registration.** The Phase 26 catalog is constructed at boot, not at package-init. Phase 38's `Register(catalog, store, deps)` is called from the bootstrap path (Phase 60+ ships `harbor dev`'s runtime composition). At Phase 38, the helper is unit-tested via a synthetic catalog + skill store; production composition happens at the bootstrap when it lands.

## Glossary additions

- **`skill_search`** — planner-callable tool returning ranked skill candidates by query. Wraps `skills.SkillStore.Search` with capability filtering + redaction. RFC §6.7.
- **`skill_get`** — planner-callable tool returning the full text of one or more named skills, redacted and budget-fit. Wraps `skills.SkillStore.Get` with capability filtering + redaction + tiered budgeter. RFC §6.7.
- **`skill_list`** — planner-callable tool returning paged skill enumeration filtered by scope / task_type / tags. Wraps `skills.SkillStore.List` with capability filtering + redaction. RFC §6.7.
- **`CapabilityContext`** — planner-supplied envelope of allowed tools / namespaces / tags + `RedactPII` flag. Drives Phase 38's filter + redactor. RFC §6.7.
- **`tiered budgeter`** — three-step ladder fitting skill text within `max_tokens`: full → drop optional → cap steps to 3 → `ErrSkillTooLarge`. brief 04 §4.5.
- **`ErrSkillTooLarge`** — sentinel returned by the budgeter when no ladder step fits within `max_tokens`. CLAUDE.md §5 fail-loud contract.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §6.7`, `RFC §6.4`, `brief 04`) resolve
- [ ] Coverage on `internal/skills/tools` ≥ 85%
- [ ] If multi-isolation paths changed: cross-session isolation test passes (yes — handlers carry identity-mandatory contract and integration test asserts identity propagation)
- [ ] **Concurrent-reuse test**: N=128 against a shared catalog under `-race` — `internal/skills/tools/concurrent_test.go`
- [ ] **Integration test**: in-package adapter wires real `tools.Catalog` + `skills.SkillStore` (`localdb`) + `events.EventBus` end-to-end, identity propagation asserted, ≥1 failure mode covered, `-race` enabled
- [ ] Glossary updated (yes)
- [ ] Brief-finding departures (none) documented above
- [ ] D-048 entry filed in `docs/decisions.md`
