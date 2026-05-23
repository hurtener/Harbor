# Phase 83o — scaffold-from-yaml

## Summary

Phase 83o completes the operator workflow Phase 83n introduced. The
`harbor scaffold` subcommand now consumes the operator-edited
`harbor.yaml` (dropped by `harbor init`) and materialises a
self-contained Go project: the yaml is copied into the output dir,
the listed built-in tools are registered by the generated agent
code, and each entry under a new `tools.custom` yaml field becomes
a typed Go stub file (`tools/<name>.go` + matching `_test.go`) with
input / output structs derived from the operator's schema
declarations. A new `--patch` flag lets the operator re-run scaffold
after editing the yaml: only NEW files are written, hand-edited
files are left untouched. Together with 83n this closes the
`init → edit → validate → scaffold → dev` loop with the same shape
the predecessor framework was loved for.

## RFC anchor

- RFC §8 — CLI surface (`harbor scaffold` deepens).
- RFC §6.4 — Tool catalog (the generated `tools/<name>.go` stubs
  register via `inproc.RegisterFunc`).

## Briefs informing this phase

- brief 06
- brief 03

## Brief findings incorporated

- brief 06 §1 — DevX is binding. The four-step workflow (`init` →
  edit → `validate` → `scaffold`) only pays off if scaffold reads
  what init dropped. A scaffold that ignores the operator's yaml
  collapses the loop into "rewrite the yaml by hand twice."
- brief 06 §3 — Documentation lives next to the code it documents.
  Each generated `tools/<name>.go` carries a header that names the
  yaml entry it was generated from + a `TODO: implement` marker.
  `docs/CONFIG.md` gains the `tools.custom` knob.
- brief 03 §3 — A custom tool registers the same way an operator's
  hand-written tool would (`inproc.RegisterFunc[Input, Output]`).
  The schema deriver does the work; the scaffold's job is only to
  give the operator a typed shell to fill in.

## Findings I'm departing from (if any)

None.

## Goals

- New yaml field: `tools.custom []CustomToolConfig`. Each entry
  declares `name` / `description` / `input` (map of `field: type`) /
  `output` (map of `field: type`). Allowed types (V1.1): `string`,
  `integer`, `number`, `boolean`, `[]string`.
- Validator: name non-empty + unique + no collision with
  `tools.built_in`; each input/output type in the allowlist.
- `harbor scaffold` gains two flags:
  - `--from-config <path>` — read the named yaml as the upstream
    config. When unset, scaffold auto-detects `./harbor.yaml`. When
    neither flag nor file resolves, scaffold falls back to today's
    template-only behavior (so the existing scaffold-without-init
    path still works).
  - `--patch` — output dir CAN exist; scaffold writes only files
    that don't already exist. Existing files (operator code) are
    skipped with a logged message. Default (without `--patch`) is
    the existing refuse-overwrite behavior.
- When scaffold resolves an upstream yaml:
  - The yaml is copied verbatim into `OutputDir/harbor.yaml`
    (comments preserved).
  - For each `tools.custom[]` entry, scaffold renders one
    `tools/<name>.go` (typed Input/Output structs + stub `Handle`)
    and one `tools/<name>_test.go` (round-trip happy path).
  - `agent.go` gains a `RegisterTools(cat tools.ToolCatalog) error`
    function that wires the built-ins + every custom tool's Handle
    against the catalog at runtime.
  - `README.md` documents the operator's next step
    (`tools/<name>.go` Handle is yours to implement).

## Non-goals

- **Nested / array-of-object input shapes.** V1.1 keeps the schema
  surface flat. Operators with complex shapes write Go by hand
  (the deriver in `internal/tools/drivers/inproc` already supports
  them — the scope cut is only in the yaml-driven shorthand).
- **Reading `tools.custom` at runtime.** The runtime does NOT
  auto-discover scaffolded tools. The operator imports the
  generated `tools/` package and calls `tools.RegisterAll(cat)` from
  the agent's bootstrap path. The wiring is intentionally explicit
  — Harbor never magically registers code it didn't see at
  compile-time.
- **Mutation of operator-edited files even in `--patch` mode.**
  Any file that exists on disk is skipped, not merged. Operators
  who want to merge use `git`.
- **A second scaffold template.** The `minimal-react` template stays
  the only template; 83o reads `--from-config` against it. A
  second template lands when a real second use case earns its keep.

## Acceptance criteria

- [ ] `internal/config.ToolsConfig.Custom []CustomToolConfig`
      lands; `CustomToolConfig` has `Name`, `Description`, `Input`
      (map), `Output` (map).
- [ ] `internal/config/validate.go::validateTools` enforces: each
      `custom[].name` non-empty + unique, no collision with
      `built_in`, each input/output type in the allowlist.
- [ ] `cmd/harbor/scaffold/scaffold.go::Options` gains
      `FromConfigPath string` and `Patch bool`.
- [ ] `cmd/harbor/cmd_scaffold.go` wires `--from-config` + `--patch`
      flags; new CLIError codes for the patch-related failure paths.
- [ ] When upstream yaml resolves: `harbor.yaml` is copied into the
      output dir (verbatim) and `tools.custom[]` entries materialise
      as typed Go stubs in `tools/<name>.go`.
- [ ] `--patch` flag relaxes the `ErrOutputDirExists` check; existing
      files are skipped with a `Result.Skipped []string` slice.
- [ ] The generated `agent.go` includes a `RegisterTools` function
      that registers each built-in and each custom tool's Handle on a
      catalog passed in by the operator.
- [ ] `docs/CONFIG.md` documents `tools.custom`; the doc-drift test
      passes.
- [ ] `scripts/smoke/phase-83o.sh` asserts the static surface.

## Files added or changed

- `internal/config/config.go` — `ToolsConfig.Custom`,
  `CustomToolConfig`.
- `internal/config/validate.go` — `validateCustomTools` + type
  allowlist + built-in collision check.
- `cmd/harbor/scaffold/scaffold.go` — `Options.FromConfigPath`,
  `Options.Patch`; `Result.Skipped`; rework `Scaffold` to read
  upstream yaml + branch on patch mode.
- `cmd/harbor/scaffold/render.go` — refactor `renderTemplate` to
  accept the upstream config + skipped-files surface; new per-tool
  render helper.
- `cmd/harbor/scaffold/templates/minimal-react/agent.go.tmpl` —
  add `RegisterTools` function.
- `cmd/harbor/scaffold/templates/minimal-react/tool.go.tmpl` — NEW;
  per-custom-tool stub.
- `cmd/harbor/scaffold/templates/minimal-react/tool_test.go.tmpl` —
  NEW; per-custom-tool happy-path test.
- `cmd/harbor/cmd_scaffold.go` — flag wiring; patch-mode CLI
  surface; updated help golden.
- `cmd/harbor/testdata/golden/help.txt` — regenerate.
- `docs/CONFIG.md` — `tools.custom` section.
- `docs/plans/README.md` — Phase 83o row + flip to Shipped.
- `docs/decisions.md` — D-154.
- `docs/glossary.md` — `custom tool (yaml-declared)` entry.
- `docs/plans/phase-83o-scaffold-from-yaml.md` — this plan.
- `scripts/smoke/phase-83o.sh` — static-surface assertions.

## Public API surface

- `harborinit.Options` is unchanged.
- `scaffold.Options` gains:
  - `FromConfigPath string` — explicit path to the upstream yaml;
    empty → auto-detect `./harbor.yaml`.
  - `Patch bool` — relax overwrite refusal; only NEW files written.
- `scaffold.Result` gains:
  - `Skipped []string` — relative paths skipped under `--patch`.
- New error: `scaffold.ErrUpstreamConfigInvalid` (wraps an
  `internal/config.ErrConfigInvalid` so callers map to a CLIError).
- New CLI codes: `CodeUpstreamConfigInvalid`,
  `CodeScaffoldPatchRequired` (the message when an operator forgot
  `--patch` against a populated output dir).

## Test plan

- **Unit:**
  - `scaffold.Scaffold` with `FromConfigPath` set to a yaml carrying
    one custom tool: produces the expected `tools/foo.go` +
    `tools/foo_test.go`.
  - Patch mode: re-run against the same output dir with a new
    `tools.custom[]` entry; only the new tool file lands; existing
    `tools/foo.go` is in `Result.Skipped`.
  - Validator: `tools.custom` with a duplicate name fails closed.
  - Validator: `tools.custom` whose name collides with `built_in`
    fails closed.
  - Validator: unknown type in input/output fails closed.
- **Integration:**
  - `cmd_scaffold_test.go::TestScaffoldCmd_FromConfig_HappyPath`
    boots the cobra surface end-to-end against a tmp yaml.
  - `cmd_scaffold_test.go::TestScaffoldCmd_Patch_SkipsExisting`
    asserts the operator-edit-survives invariant.
- **Conformance:** N/A (no new transport).
- **Concurrency / leak:** N/A — `Scaffold` is a one-shot file
  emitter; the generated `tools/<name>.go` stubs register through
  the existing `inproc` driver whose D-025 contract is already
  tested.

## Smoke script additions

`scripts/smoke/phase-83o.sh` asserts:

- New `--from-config` + `--patch` flags appear in the scaffold help.
- `tools.go.tmpl` + `tools_test.go.tmpl` templates ship.
- `agent.go.tmpl` carries the `RegisterTools` function shape.
- `internal/config.CustomToolConfig` declared.
- `validateCustomTools` rejects an unknown type.
- `docs/CONFIG.md` documents `tools.custom`.

## Coverage target

- `cmd/harbor/scaffold`: 85% (existing).
- `cmd/harbor`: 80% (existing).
- `internal/config`: 90% (existing).

## Dependencies

- Phase 67 (`harbor scaffold` core).
- Phase 83n (`harbor init` + `tools.built_in` mirror surface).
- Phase 26 (`tools.ToolCatalog` + `inproc.RegisterFunc`).

## Risks / open questions

- **Naming collisions on case-insensitive filesystems.** Custom
  tools `MyTool` and `mytool` would render to the same file on
  macOS / Windows. The validator's case-sensitive uniqueness check
  catches the intra-yaml dup; the rendered path uses the verbatim
  name. Operators on case-insensitive FSes who pick clashing names
  see the second write win — documented in the rendered README.
- **`tools.custom` rot.** The yaml field is the source of truth for
  the generated stubs; an operator who renames a tool in yaml and
  re-scaffolds gets a NEW file alongside the old (the old name's
  Go file is still on disk). Patch mode doesn't delete. Operators
  who want clean renames `git rm` the old file.
- **The generated `RegisterTools` function compiles only if the
  operator's binary imports `tools/`.** That's documented in the
  generated README; a smoke check (`go vet ./...` in the
  scaffolded project) is implicit via the `examples build + test`
  CI job's pattern.

## Glossary additions

- **custom tool (yaml-declared)** — a tool whose Go shell is
  generated by `harbor scaffold` from a `tools.custom[]` entry in
  `harbor.yaml`. Distinct from a hand-written `inproc.RegisterFunc`
  call (no yaml) and from a built-in (`tools.built_in[]` —
  registers a tool whose Go is shipped in the Harbor binary).
- **`harbor scaffold --patch`** — re-runs scaffold against a
  populated output dir; existing files are skipped, only newly-
  declared tools materialise. The hand-edited code survival
  invariant.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Concurrent-reuse — N/A: `Scaffold` is a one-shot emitter;
      generated tools register via the existing inproc driver whose
      D-025 contract is already tested
- [ ] Integration test exists per §17 — `cmd_scaffold_test.go`
      adds two new tests for the from-config + patch surface
- [ ] Glossary updated
- [ ] If a brief finding was departed from: N/A
