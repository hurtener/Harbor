# Phase 83b — react-tool-schema-injection

## Summary

Upgrade the `<available_tools>` section established by Phase 83a so each tool renders with its full `args_schema`, `side_effects`, and curated examples — closing the dominant source of `args` validation failures (LLM guessing arg shapes because only `name + description` are exposed). Extend the `Tool` struct (`internal/tools`) with an `Examples []ToolExample` field, tag-ranked `minimal → common → edge-case` per brief 13 §2.4. Tool authors opt in incrementally: tools without examples render unchanged.

## RFC anchor

- RFC §6.2
- RFC §6.4

## Briefs informing this phase

- brief 13
- brief 03
- brief 07

## Brief findings incorporated

- brief 13 §2.4: "Examples are the most token-efficient way to constrain `args` shape — a single example is worth several lines of schema prose." — examples are the load-bearing piece, not the verbose schema dump.
- brief 13 §2.4: "The ranking lets V1 tools ship a one-line minimal example and add common / edge-case examples over time" — phase 83b ships with all current Harbor tools getting at most one minimal example; common / edge-case land per-tool as needed.
- brief 13 §3: "LLM guesses args shapes; args validation failures cascade across steps" — closing this is the explicit goal.
- brief 07 §3 (cross-fork synthesis): "Code-level tool calling — runtime owns dispatch; LLM is decision-maker not runner" — the prompt must give the LLM enough information to *decide* correctly. Schemas + examples are the information.

## Findings I'm departing from (if any)

- None. The `args_schema` field already exists on Harbor's `Tool` struct (Phase 26); this phase exposes it in the prompt. The only new field is `Examples []ToolExample`.

## Goals

- Each tool in `<available_tools>` renders with: `name`, `description`, `args_schema` (compact form), `side_effects`, and up to N (configurable, default 3) examples.
- Tool authors can register tag-ranked examples on `Tool`; the rendered prompt picks the top N by tag priority (`minimal` > `common` > `edge-case`).
- Tools without examples render with name + description + schema + side-effects only (backward compatible — no churn for existing tool registrations).
- The schema rendering is **compact JSON** (one line, sorted keys) to maximise KV-cache hit rate, mirroring brief 13 §5 "compact JSON discipline."

## Non-goals

- Multi-language tool descriptions / localised schemas. Out of scope.
- Schema validation hardening on the *catalog* side — Phase 26 already validates schemas at registration; this phase is read-side only.
- Re-rendering examples per turn based on observed failures — that's Phase 83c's repair-guidance territory.

## Acceptance criteria

- [ ] `internal/tools.Tool` gains `Examples []ToolExample` and `ToolExample{Args map[string]any, Description string, Tags []string}` exported types.
- [ ] `internal/planner/react/prompt.go` gets a `renderTool(t tools.Tool, cfg ToolRenderConfig) string` helper that emits the per-tool block.
- [ ] Tag-priority ranking: `minimal` (rank 0) > `common` (rank 1) > `edge-case` (rank 2) > untagged (rank 3). Stable sort by `(rank, originalIndex)`.
- [ ] Default `max_examples_per_tool = 3`; configurable via `PlannerConfig.MaxToolExamplesPerTool` (defaulting to 3 when zero).
- [ ] Existing tools that ship without examples gain a schema + side-effects block in the rendered output (this *is* a deliberate format change from Phase 83a) but **do not** require any code change at the registration site — the new fields are opt-in on `Tool`, and the renderer omits the `examples:` line entirely when `Examples` is empty. The golden fixture for a no-examples tool documents the exact rendered shape.
- [ ] `args_schema` rendering uses `encoding/json` with `MarshalIndent(..., "", "")` (sorted keys via a stable key-sort helper; Go's `encoding/json` already emits keys deterministically for `map[string]any` since 1.21+). The output is single-line compact JSON.
- [ ] Golden fixture extended: `internal/planner/react/testdata/golden_tools_prompt.txt` shows the rendered block for a fixture catalog of two tools (one with examples, one without).
- [ ] Concurrent-reuse: `renderTool` is pure — no shared state. 100+ concurrent calls under `-race` test passes (extends Phase 83a's d025 suite).

## Files added or changed

- `internal/tools/tool.go` (or wherever the `Tool` struct lives) — add `Examples []ToolExample` field; define `ToolExample` type.
- `internal/tools/example_validation.go` (new) — registration-time validation that an example's `Args` keys are a subset of the tool's `args_schema` properties. Fail loudly on mismatch (D-025-style fail-loudly per AGENTS.md §5).
- `internal/planner/react/prompt.go` — `renderTool` helper; integrate into `buildSystemContent`.
- `internal/planner/react/testdata/golden_tools_prompt.txt` (new).
- `internal/planner/react/prompt_test.go` — extend.
- `internal/config/config.go` — `MaxToolExamplesPerTool int` on `PlannerConfig`.
- `examples/harbor.yaml` — comment the new key.
- `scripts/smoke/phase-83b.sh` — static-only assertions on the golden.
- `docs/glossary.md` — fill in the **Tool example** entry (placeholder added in Phase 83a).
- `docs/plans/README.md` — Status column flip on merge.

## Public API surface

```go
// internal/tools/tool.go (delta)

type ToolExample struct {
    Args        map[string]any
    Description string
    Tags        []string // ranked: "minimal" > "common" > "edge-case"
}

type Tool struct {
    Name        string
    Description string
    ArgsSchema  any
    SideEffects string
    Examples    []ToolExample // NEW
    // ...existing fields
}

// internal/planner/react/prompt.go (new helper, not exported by package — internal to the builder)
func renderTool(t tools.Tool, cfg toolRenderConfig) string
```

No protocol surface changes.

## Test plan

- **Unit:**
  - Tag-ranking test: a catalog of one tool with `[edge-case, common, minimal]` examples renders them in `minimal, common, edge-case` order.
  - Limit test: with `MaxToolExamplesPerTool = 1`, only the top-ranked example is rendered.
  - Empty-examples test: tools without examples render with no `examples:` line at all (not `examples: []`).
  - Schema rendering test: a tool with a non-trivial `args_schema` produces compact (no whitespace) JSON on one line.
  - Registration-time validation: a tool registered with an example whose `Args` includes a key not in `args_schema` fails registration loudly with a typed error.
  - Golden fixture test: full `<available_tools>` block for a 2-tool fixture catalog matches `golden_tools_prompt.txt`.
- **Integration:** N/A — pure prompt-content change; no new cross-subsystem seam.
- **Conformance:** existing planner conformance pack re-runs unchanged.
- **Concurrency / leak:** d025 concurrent-reuse extended to cover catalog rendering with examples.

## Smoke script additions

- `scripts/smoke/phase-83b.sh` (classification: `static-only`):
  - Assert `internal/planner/react/testdata/golden_tools_prompt.txt` exists.
  - Grep the golden for `args_schema:`, `side_effects:`, `examples:` substrings (each must appear ≥1 time).
  - Assert the golden does not contain `{{` markers (template fully rendered).

## Coverage target

- `internal/planner/react`: 85% (unchanged).
- `internal/tools`: 85% (unchanged — `Examples` field + validator gain a few tests).

## Dependencies

- 83a (the structured-section builder this phase extends).
- 26 (Tool catalog core — the `Tool` struct we extend).

## Risks / open questions

- **Token budget.** A full schema dump per tool plus 3 examples per tool can blow up token usage in catalogs with 30+ tools. Mitigations: the `MaxToolExamplesPerTool` knob lets ops dial down; tools whose schemas are huge can register a compact `RenderSchema string` field (deferred — flag for a later phase if measurement says we need it). Phase 83b ships with a documented "watch tokens at catalog scale" warning in the operator-facing doc.
- **Example-author burden.** Every tool author now has the option (not the obligation) of adding examples. Risk: the surface becomes performative ("add an example because the prompt has space for one"). Mitigation: validate at registration — examples whose `Args` don't match the schema fail loudly. A passing example is a working example.
- **Compact JSON stability across Go versions.** `encoding/json` map-key ordering is deterministic in Go 1.21+. Harbor pins Go 1.26+ per AGENTS.md §5, so this is fine, but the golden fixture test catches any future regression.

## Glossary additions

- **Tool example** — A curated `(args, description, tags)` triple registered on a `Tool`. Renders into the tool catalog block of the system prompt. Tags rank the example: `minimal` (highest priority) > `common` > `edge-case` > untagged. The planner-side prompt builder renders up to `MaxToolExamplesPerTool` examples per tool, picked top-ranked first.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage ≥ target
- [ ] Cross-isolation test — N/A.
- [ ] **Concurrent-reuse test passes** — `renderTool` purity tested under `-race`; combined builder + catalog rendering tested with N≥100 concurrent invocations.
- [ ] Integration test — N/A (no new cross-subsystem seam; depends on Phase 83a + 26, both already covered).
- [ ] Glossary updated (Tool example entry filled in).
- [ ] No brief departures.
