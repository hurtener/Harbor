# Phase 133 — scaffold-with-tools execution gate

## Summary

The `harbor scaffold` CLI path advertises a tools-declaring agent, but its
generated `agent_test.go` never calls `RegisterTools` — so a tools-declaring
scaffold compiles and its tests pass while no tool is ever invoked (a
false-green; CLAUDE.md §1 / §13). This phase makes the scaffolded test
register the declared tools onto a real catalog AND dispatch at least one tool
through the catalog/executor, asserting an observable dispatch signal, and adds
a `go test ./...` execution leg (plus a gate-bites self-test) to
`scripts/smoke/phase-112b.sh` that fails loud when the register-and-dispatch
path is inert.

## RFC anchor

- RFC §8 — CLI layer (`harbor scaffold`).
- RFC §6.4 — Tool catalog and transports; "Code-level tool dispatch" (the
  runtime owns dispatch through the catalog/executor — the dispatch signal the
  generated test observes).

## Briefs informing this phase

- brief 06
- brief 07

## Brief findings incorporated

- brief 06 (CLI — `harbor dev`, scaffolding, deployment): the scaffold path is
  an adopter on-ramp; a scaffold that compiles but does not actually run the
  declared surface is an adoption trap. This phase makes the generated test
  prove the declared tool surface is live, not merely present.
- brief 07 (code-level tool calling — the elegance principle): tool dispatch
  happens at the runtime/orchestration level through the catalog, not at the
  LLM provider level. The generated test therefore dispatches through
  `ToolCatalog.Resolve` + the descriptor's `Invoke` (the runtime's dispatch
  path), so the observable signal is a real catalog dispatch — not a stubbed
  provider call.

## Findings I'm departing from (if any)

None.

## Goals

- When `harbor.yaml` declares tools (`tools.custom` and/or `tools.built_in`),
  the scaffolded `agent_test.go` calls `RegisterTools` and drives ≥1 tool
  through the catalog/executor, asserting an observable dispatch signal.
- The standing external-module gate in `scripts/smoke/phase-112b.sh` adds a
  `go test ./...` execution leg on the tool-declaring scaffold that fails loud
  when no tool is registered AND invoked.
- A self-test proves the execution gate BITES: a scaffold whose `RegisterTools`
  registers a tool under the wrong name still COMPILES but FAILS `go test`.
- A toolless scaffold is unchanged — no tool block, no new imports.

## Non-goals

- No change to `RegisterTools`'s signature or the agent/tool templates'
  runtime behavior.
- No new scaffold template or `--with-tools` flag — tools are declared via the
  operator's `harbor.yaml` (`tools.custom` / `tools.built_in`), as today.
- No new smoke file for the heavy execution gate (it extends
  `phase-112b.sh`, which already pays the external-module build cost; §4.3).

## Acceptance criteria

- [ ] `agent_test.go.tmpl` gains a `{{if or .BuiltIns .CustomTools}}` block
  that, when tools are declared, calls `RegisterTools(cat)` and dispatches ≥1
  declared tool through the catalog (`cat.Resolve` + `desc.Invoke`), asserting
  an observable dispatch signal — not merely that `RegisterTools` is defined.
- [ ] When no tools are declared, the rendered `agent_test.go` is unchanged
  (no dispatch test, no `sdk/tools` / `customtools` imports).
- [ ] `scripts/smoke/phase-112b.sh` adds a `go test ./...` execution leg on the
  tool-declaring external scaffold (OK on green, FAIL on red).
- [ ] `scripts/smoke/phase-112b.sh` adds a self-test that rewrites the
  registration name so the module still compiles but `go test` FAILS — proving
  the gate bites (§17.8 anti-rubber-stamp).
- [ ] `scripts/smoke/phase-133.sh` statically pins the template's
  register-and-dispatch block and that the execution gate is wired into
  `phase-112b.sh`.
- [ ] The generator passes `.CustomTools` / `.BuiltIns` truthfully (verified
  §17.6: the same `templateVars` feeds `agent.go` and `agent_test.go`; the
  `minimal-react` template is the only template — no duplicated logic to drift).

## Files added or changed

- `cmd/harbor/scaffold/templates/minimal-react/agent_test.go.tmpl` — gated
  register-and-dispatch test.
- `scripts/smoke/phase-112b.sh` — `go test ./...` execution gate (§3b) +
  dispatch-gate self-test (§3c).
- `scripts/smoke/phase-133.sh` — static template-surface pins (new).
- `docs/plans/phase-133-scaffold-tools-execution.md` — this plan.
- `docs/plans/README.md` — index row + status.
- `docs/decisions.md` — D-267.
- `docs/skills/scaffold-a-harbor-agent/SKILL.md` — note the generated test
  exercises register-and-dispatch when tools are declared.

## Public API surface

N/A — no Go API change. The scaffold templates and the generated project's
test are the only surface touched; `RegisterTools`'s signature is unchanged.

## Test plan

- **Unit:** the scaffold package's existing render tests
  (`cmd/harbor/scaffold/...`) continue to pass.
- **Integration:** `scripts/smoke/phase-112b.sh` scaffolds a tool-declaring
  external module, runs `go mod tidy && go build && go test ./...`, and asserts
  the register-and-dispatch test passes (real catalog, real descriptor
  dispatch, identity propagated via `harbortest.RunOnce`); a self-test proves
  the gate fails loud when dispatch is broken without breaking compilation.
- **Conformance:** N/A.
- **Concurrency / leak:** N/A — no new reusable artifact (the catalog,
  descriptor, and `RunOnce` are pre-existing, separately D-025-tested
  surfaces). The generated test calls `AssertNoLeaks` for cross-session
  isolation.

## Smoke script additions

- `scripts/smoke/phase-112b.sh`: §3b — `go test ./...` on the tool-declaring
  scaffold (EXTERNAL EXECUTION GATE); §3c — registration-name-rewrite self-test
  asserting the module compiles but `go test` fails (the D-267 gate bites).
- `scripts/smoke/phase-133.sh` (static): the template gates the dispatch block
  on declared tools; the block calls `RegisterTools` and dispatches through
  `cat.Resolve` + `desc.Invoke`; the execution gate + self-test are wired into
  `phase-112b.sh`.

## Coverage target

N/A — template + smoke change; no new Go package. `cmd/harbor/scaffold` keeps
its existing coverage (render tests unchanged).

## Dependencies

- 112b (the external-consumer compile gate this phase extends).
- 67, 83o (the scaffold engine + per-custom-tool stubs the test dispatches).

## Risks / open questions

- The execution gate adds one external `go test` leg (~2s warm) to
  `phase-112b.sh`, bounded by the existing `GATE_DEADLINE_SECS`.
- The self-test's break (a registration-name rewrite via `sed`) depends on the
  emitted registration-name line; if the template formatting drifts, the
  self-test fails loud ("could not apply the break") rather than silently
  passing — the §17.8 fail-loud posture is preserved.
- Built-ins-only scaffolds: the dispatch test tolerates an args-validation
  error from a stub built-in invoked with an empty payload (reaching the
  executor is the dispatch signal); only `ErrToolNotFound` — registration
  produced nothing — fails the gate. The fully-asserted path is the dominant
  custom-tool case.

## Glossary additions

None — no new vocabulary.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A, no isolation path changed; the generated test calls `AssertNoLeaks`.
- [x] **If this phase builds a reusable artifact: concurrent-reuse test passes** — N/A: this phase builds no new reusable artifact (template + smoke only).
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists** — the `phase-112b.sh` execution gate exercises scaffold → catalog dispatch end-to-end on a real external module.
- [x] If new vocabulary: glossary updated — N/A, none introduced.
- [x] If a brief finding was departed from: justified above + decisions.md entry filed — N/A, no departure.
