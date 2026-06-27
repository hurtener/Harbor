# Phase 141 — Native tool-name sanitization for provider-safe tool-calling

## Summary

Live verification of the scaffold-with-tools adopter path surfaced a real
runtime bug: Harbor's dotted tool-naming convention (built-ins `clock.now`,
`text.echo`; scaffolded custom tools `inventory.check`) breaks native
tool-calling against OpenAI-compatible providers, which reject any function
name not matching `^[a-zA-Z0-9_-]{1,64}$` with a `400`. The React planner
declared the catalog name verbatim in `req.Tools` and replayed it verbatim
in the assistant `tool_calls` history, so any dotted-tool agent failed at the
first (or follow-up) LLM call on a real provider. This phase sanitizes tool
names to the provider-safe form when projecting to the LLM and resolves the
returned name back to the real catalog name on dispatch — transparently, with
no operator-facing change.

## RFC anchor

RFC §6.2 (the Planner interface / `RunContext` tool view) and §6.4 (the
Runtime owns the executor / tool dispatch). Native tool-calling is the React
planner's projection of the catalog into `req.Tools` and of the provider's
`tool_calls` back into `CallTool` decisions.

## Briefs informing this phase

`docs/research/INDEX.md` — the native-tool-calling brief (brief 15) that
informed the 107c/107d native `tool_calls` cutover.

## Brief findings incorporated

Brief 15's native-tool-calling model already required declaring the reserved
planner controls (`_spawn_task` / `_await_task`) because real providers reject
*undeclared* tool-call names. This phase closes the sibling gap: real
providers also reject *invalid-character* names, which the dotted catalog
convention produces.

## Findings I'm departing from (if any)

None. This corrects an omission, not a documented decision.

## Goals

- Sanitize every tool name sent to the LLM (declarations + replayed history)
  to the provider-safe form `^[a-zA-Z0-9_-]{1,64}$`.
- Resolve a provider-returned (sanitized) tool-call name back to the real
  catalog name so dispatch hits the registered tool.
- A deterministic round-trip test (a dotted name through declaration +
  projection) — the regression guard the scripted-LLM tests missed (§17.8).

## Non-goals

- Changing the catalog naming convention (dotted names stay the canonical
  catalog key; sanitization is a wire-edge transform).
- A new live-provider CI test (CI has no provider key; the round-trip unit
  test is the gate, and the fix was validated live by hand).

## Acceptance criteria

- [ ] `sanitizeToolName` maps dotted/invalid names to `^[a-zA-Z0-9_-]{1,64}$`
      and is identity on already-valid names.
- [ ] `buildToolDeclarations` emits sanitized names; no `.` reaches `req.Tools`.
- [ ] `renderNativeStepPair` / `renderNativeParallelStep` replay the
      assistant `tool_calls` under the sanitized name.
- [ ] `projectResponse` resolves a sanitized returned name back to the real
      catalog name for `CallTool` (single, parallel, and serialized paths).
- [ ] The full `internal/planner/react` suite passes under `-race`.

## Files added or changed

- `internal/planner/react/tool_name_sanitize.go` (new — `sanitizeToolName`,
  `resolveDeclaredToolName`).
- `internal/planner/react/discovered_tools.go` (`toolToDeclaration` +
  sanitized dedup).
- `internal/planner/react/prompt.go` (`renderNativeStepPair` /
  `renderNativeParallelStep` history replay).
- `internal/planner/react/projector.go` (resolve on projection).
- `internal/planner/react/tool_name_sanitize_test.go` (new — round-trip).
- `examples/embed-runonce/main.go` (live-verification fix: declare a
  `ModelProfiles` entry so the worked example runs, not just compiles).

## Public API surface

None. The sanitization is an internal React-planner wire-edge transform; the
catalog name, the `CallTool` decision, and the operator-facing config are
unchanged.

## Test plan

- `internal/planner/react/tool_name_sanitize_test.go`: `sanitizeToolName`
  table; `buildToolDeclarations` emits no dotted name; `projectResponse`
  resolves a sanitized name back to the dotted catalog name; the resolution
  branches (exact / sanitize-match / passthrough).
- The full react suite + the native-tool-calling integration tests (83l,
  136, wave_v18) stay green.

## Smoke script additions

`scripts/smoke/phase-141.sh` (static): pins the sanitizer file + the
round-trip test names exist (the test suite is the behavioural gate).

## Coverage target

`internal/planner/react` ≥ the package's existing target; the new file is
fully exercised by `tool_name_sanitize_test.go`.

## Dependencies

107c / 107d (native `tool_calls` emission + projection), 133 (the
scaffold-with-tools path whose live verification surfaced this).

## Risks / open questions

- Sanitized-name collisions (two catalog names → same sanitized name) are
  handled by dropping the second from the turn's declarations (logged-able,
  pathological); the canonical fix would disambiguate with a suffix, deferred
  until a real collision is observed.

## Glossary additions

`tool-name sanitization` — see `docs/glossary.md`.

## Pre-merge checklist

- [ ] `make vet test build` green; `make lint` clean.
- [ ] react suite `-race` green; native-tool-calling integration tests green.
- [ ] `make drift-audit` clean; D-270 added; README row added.
