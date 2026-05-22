# Phase 80 — documentation-hygiene polish

## Summary

Phase 80 closes the documentation-hygiene loop for the V1 cut: every
package carries a doc comment, every exported identifier carries
godoc, the `revive` lint rules that enforce that are actually run in
CI (they were silently skipped), and `examples/` plus `docs/recipes/`
give a reader runnable, real-API-grounded entry points.

## RFC anchor

- RFC §2 — Goals and non-goals. The master plan cites RFC §2 for this
  phase: Harbor's developer-experience goal (a Go-native SDK an
  operator can pick up and run) depends on a documented public surface
  and worked examples. Phase 80 is the hygiene pass that discharges
  that intent.

## Briefs informing this phase

- brief 06

## Brief findings incorporated

- brief 06 §"Phasing (events + observability + devx)" (line 176):
  "Each phase ships with smoke checks (per `feedback_harbor_doc_hygiene.md`)
  and updates the contributor docs." Phase 80 is the dedicated
  contributor-docs hygiene pass that finding anticipates — it ships a
  `static-only` smoke and adds the `docs/recipes/` contributor surface.
- brief 06 §"What `harbor dev` exposes" (line 108): `harbor dev` boots
  the runtime headless on `127.0.0.1:<port>` and all of it is
  "implemented as protocol clients of the same runtime — no private
  hooks." The `run-harbor-dev.md` recipe is grounded in exactly this
  shape, and the recipe set deliberately documents the public CLI /
  `harbortest` / tool-catalog surfaces only.
- brief 06 §"CLI golden tests" (line 149): the CLI subcommands produce
  stable, golden-pinnable output. The recipes cite the real, current
  flag sets (`harbor dev --config/--port/--no-hot-reload`, `harbor
  scaffold --name/--template/--output`) so a recipe never drifts from
  a golden-pinned surface.

## Findings I'm departing from (if any)

None.

## Goals

- Every Go package under `internal/`, `cmd/`, and `harbortest/`
  carries a package-level doc comment.
- Every exported identifier carries a godoc comment.
- The `revive` `exported` and `package-comments` rules are enabled
  AND clean AND actually run in CI.
- `examples/` carries at least one runnable, end-to-end example agent
  and one example tool that build and test green.
- `docs/recipes/` carries practical how-to guides grounded in real,
  current APIs.

## Non-goals

- Clearing the broader `make lint` backlog (~1000 pre-existing issues
  across ~20 linters that accumulated while the CI lint job silently
  skipped). That is a separate release-hardening effort; several
  fixes (`errcheck`) change error-handling behaviour.
- Renaming exported types to satisfy `revive`'s stutter sub-check
  (`state.StateStore`, `react.ReActPlanner`, …). ~20 cross-package
  renames are out of scope for a documentation phase.
- Any runtime behaviour change. Production-code edits are
  documentation comments and whitespace only.

## Acceptance criteria

- [x] `golangci-lint`'s `revive` `exported` rule is clean (every
      exported identifier has godoc).
- [x] `golangci-lint`'s `revive` `package-comments` rule is clean
      (every package comment correctly attached).
- [x] The lint gate is actually run in CI (the `lint` job installs
      `golangci-lint` and runs `make lint-revive`).
- [x] `examples/` builds end-to-end: `go build ./examples/...` and
      `go test -race ./examples/...` pass; a CI `examples` job runs
      both.
- [x] `docs/recipes/` exists with practical how-to guides grounded in
      real APIs.
- [x] `make vet test build drift-audit check-mirror` pass.

## Files added or changed

```text
.golangci.yml                          revive exported gains disableStutteringCheck; A2A var-naming exclude
.golangci-revive.yml                   NEW — dedicated revive-only lint config (the Phase 80 gate)
Makefile                               NEW lint-revive target
.github/workflows/ci.yml               lint job installs golangci-lint + runs lint-revive; NEW examples job
examples/README.md                     NEW — examples index
examples/agents/echo/echo.go           NEW — worked harbortest.Agent example
examples/agents/echo/echo_test.go      NEW — worked example agent test
examples/tools/weather/weather.go      NEW — worked inproc.RegisterFunc tool example
examples/tools/weather/weather_test.go NEW — worked example tool test
docs/recipes/README.md                 NEW — recipes index
docs/recipes/scaffold-an-agent.md      NEW
docs/recipes/define-a-tool.md          NEW
docs/recipes/configure-a-planner.md    NEW
docs/recipes/run-harbor-dev.md         NEW
docs/recipes/test-an-agent.md          NEW
scripts/smoke/phase-80.sh              NEW — static-only smoke
docs/decisions.md                      NEW — D-138
docs/plans/README.md                   Phase 80 row + detail block flipped to Shipped
README.md                              Status table + Recipes pointer
internal/protocol/transports/stream/*  package-comment blank-line fixes (7 files)
internal/governance/registry.go        package-comment blank-line fix
internal/telemetry/{metrics,tracing}.go secondary file comments de-associated from package
internal/events/filter.go              WireConversion type comment corrected
internal/runtime/flow/flow.go          error-var block comment added
internal/tools/{tools.go,drivers/mcp/content.go}  const-block comments added
internal/llm/llm.go                    const-block comments added; stale //nolint:revive removed
internal/skills/tools/concurrent_test.go  skills_ test field renamed to storedSkills
```

## Public API surface

None. Phase 80 adds no exported types, functions, or methods to the
runtime. The example packages (`examples/agents/echo`,
`examples/tools/weather`) are illustrative, not a depended-on surface.

## Test plan

- **Unit:** `examples/agents/echo/echo_test.go` and
  `examples/tools/weather/weather_test.go` are the worked-example
  tests — they exercise `harbortest.RunOnce` and the
  register→resolve→invoke tool round-trip. Run under `-race` by the
  CI `examples` job.
- **Integration:** N/A — Phase 80 ships no cross-subsystem seam. The
  example tests touch real production surfaces (`harbortest`, the
  tool catalog) end-to-end, which is the integration the examples
  exist to keep honest.
- **Conformance:** N/A — no new driver/interface.
- **Concurrency / leak:** N/A — Phase 80 builds no reusable runtime
  artifact. The example agent/tool are concurrency-safe by
  construction (no mutable state) and the underlying `RunOnce` /
  catalog carry their own D-025 tests from Phases 71 / 26.

## Smoke script additions

`scripts/smoke/phase-80.sh` is `static-only`. It asserts: the
`.golangci-revive.yml` config exists and enables the `revive`
`exported` (with `disableStutteringCheck`) + `package-comments` rules;
the `Makefile` carries the `lint-revive` target; CI runs the gate and
the `examples` job; the worked example files exist; the recipe docs
exist. 17 OK, 0 FAIL.

## Coverage target

- `examples/agents/echo`: 100% (trivial package, fully exercised).
- `examples/tools/weather`: 100% (trivial package, fully exercised).
- No production package's coverage changes — production edits are
  documentation comments only.

## Dependencies

All V1 phases (the phase documents the whole-repo surface). No
ordering dependency beyond "the surfaces being documented exist".

## Risks / open questions

- The broader `make lint` backlog (~1000 issues) is deliberately not
  cleared here. Risk: it stays unenforced. Mitigation: the CI `lint`
  job now genuinely runs a linter (the `revive` gate), so the
  infrastructure to enforce more rules incrementally is in place; a
  follow-up release-hardening pass can widen the enforced set.
- golangci-lint version drift: the gate pins `v1.64.8`. A future Go
  toolchain bump may require a version bump; the pin is in both the
  CI workflow and is compatible with the `.golangci-revive.yml`
  `run.go: "1.26"` floor.

## Glossary additions

None — Phase 80 introduces no new Harbor vocabulary.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §2`, `brief 06`) resolve
- [x] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A, no multi-isolation code path changed.
- [ ] If this phase builds a reusable artifact: concurrent-reuse test passes — N/A, Phase 80 builds no reusable runtime artifact (docs + examples only).
- [ ] If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: integration test exists — N/A, no cross-subsystem seam; the example tests exercise `harbortest` / the tool catalog end-to-end.
- [x] If new vocabulary: glossary updated — N/A, no new vocabulary.
- [x] If a brief finding was departed from: justified above + decisions.md entry filed — none departed from.
