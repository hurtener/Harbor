# Phase 83n — harbor-init

## Summary

Phase 83n introduces the operator-facing **`harbor init`** subcommand
and the supporting documentation surface that turns Harbor from a
"library with a binary glued on" into a framework with a real
adoption path. `harbor init` drops a tiered, commented `harbor.yaml`
plus three companion files (`AGENTS.md`, `CLAUDE.md`, `README.md`)
into a fresh directory. Operators edit the yaml — uncomment one of
four LLM provider examples, opt into built-in tools, add MCP servers,
tune memory — and then run `harbor scaffold` (Phase 83o reads the
edited yaml). The companion `docs/CONFIG.md` reference indexes every
knob the runtime exposes, with a CI test that fails the build when a
new config field lands without documentation.

The phase also ships the first two operator-opt-in built-in tools —
`clock.now` and `text.echo` — under a new `internal/tools/builtin/`
package. Both register on demand by name through the new
`tools.built_in` yaml field; nothing in the binary registers them
unless an operator lists them. Built-ins close the §13 "primitive
without consumer" rule for the new yaml field: ship the knob, ship
two real consumers, ship the documentation, all in the same phase.

## RFC anchor

- RFC §8 — CLI surface (`harbor init` is a new subcommand alongside
  `harbor dev` / `scaffold` / `validate` / `console`).
- RFC §6.4 — Tool catalog (the built-in tools live here).

## Briefs informing this phase

- brief 06
- brief 03

## Brief findings incorporated

- brief 06 §1 — DevX is binding: the first-clone experience must
  produce a working agent with zero ceremony. `harbor init` is the
  zero-ceremony entry; the tiered yaml + `AGENTS.md` / `CLAUDE.md` /
  `README.md` triple make the next step legible without forcing the
  operator to read the RFC first.
- brief 06 §3 — Documentation lives next to the code it documents,
  and CI rejects drift. `docs/CONFIG.md` ships in this phase with a
  Go test (`internal/config/doc_drift_test.go`) that walks the
  `Config` struct and asserts every yaml path is documented. New
  config fields without a doc entry fail CI.
- brief 03 §1 — A built-in tool registers the same way an operator's
  custom tool registers — `inproc.RegisterFunc`. There is no special
  "built-in" descriptor shape; the only difference is that `clock.now`
  and `text.echo` are shipped in the binary, opt-in by name.

## Findings I'm departing from (if any)

None.

## Goals

- New subcommand: `harbor init [--name NAME] [--target DIR]`. Drops
  four files into the target directory (default `.`). Refuses to
  overwrite any file that already exists. Uses the target directory's
  basename when `--name` is omitted.
- Tiered `harbor.yaml.tmpl`:
  - **REQUIRED** block: `identity` filled with placeholder values
    that pass validation, and a `llm` block with four commented
    provider examples (OpenRouter / Anthropic / OpenAI / NVIDIA NIM).
    The operator uncomments exactly one and adjusts.
  - **COMMON KNOBS** block: commented templates for `planner`,
    `memory`, `state`, `tools`, `skills`, `governance`.
  - **ADVANCED** block: pointer to `docs/CONFIG.md` for every other
    knob; defaults shown in comments so the operator sees what's
    applied when the key is absent.
- Companion files:
  - `AGENTS.md.tmpl` — binding rules for AI assistants editing this
    agent (mirrors Harbor's own §1-§3 posture: identity is mandatory,
    fail loud, no stubs).
  - `CLAUDE.md.tmpl` — verbatim mirror of `AGENTS.md.tmpl`.
  - `README.md.tmpl` — operator-facing walkthrough: edit yaml,
    `harbor validate`, `harbor scaffold`, `harbor dev`.
- Two opt-in built-in tools at `internal/tools/builtin/`:
  - `clock.now` — returns the current UTC time as RFC 3339 plus epoch
    milliseconds.
  - `text.echo` — returns its `text` input verbatim. Useful for
    smoke-testing the planner → executor → trajectory loop without an
    external dependency.
  - Registration: `builtin.Register(cat, names []string)` registers
    only the named built-ins. Unknown names fail loud with the list
    of known built-ins in the error.
- New yaml field: `tools.built_in []string`. Validated against the
  built-in registry. Wired into `cmd/harbor/cmd_dev.go::bootDevStack`
  (and the D-094 `harbortest/devstack` mirror).
- `docs/CONFIG.md` — exhaustive knob reference, one section per
  Config sub-struct. Each entry: YAML path, type, default,
  validation, example. The first-pass goal is "every yaml path
  configurable on `Config` has a corresponding `### path` heading in
  CONFIG.md."
- `internal/config/doc_drift_test.go` — Go test that walks the
  `Config` struct via reflection (using `walkLeaves`), collects every
  yaml path, reads `docs/CONFIG.md`, and asserts each path appears as
  a heading. Fails the build when a new field lands without
  documentation.

## Non-goals

- **Scaffold reads the operator-edited yaml.** That's Phase 83o.
  Today `harbor scaffold` still renders its own self-contained yaml
  from `cmd/harbor/scaffold/templates/minimal-react/harbor.yaml.tmpl`;
  83o changes scaffold to consume the init-dropped yaml when present.
- **Per-tool Go stubs from yaml.** Also Phase 83o.
- **More built-in tools.** `clock.now` + `text.echo` are the V1.1
  surface. Operators add their own through `inproc.RegisterFunc` in
  the scaffolded project (or, post-83o, through yaml tool
  declarations).
- **Hot-reload for `tools.built_in`.** Restart-required, same posture
  as the rest of `ToolsConfig`.
- **A "harbor init --template foo" surface.** V1.1 ships exactly one
  init template (`default`). Adding template selection is a Phase 83p+
  concern when a second template earns its keep.

## Acceptance criteria

- [ ] `cmd/harbor/cmd_init.go::newInitCmd` registers the subcommand;
      `runInit` calls `harborinit.Init`.
- [ ] `cmd/harbor/init/init.go::Init` materialises four files
      (`harbor.yaml`, `AGENTS.md`, `CLAUDE.md`, `README.md`) into
      `opts.TargetDir`. Refuses to overwrite an existing file
      (returns `ErrFileExists` naming the offending path).
- [ ] `cmd/harbor/init/templates/default/` contains the four `.tmpl`
      files, embedded via `embed.FS`.
- [ ] The init-dropped `harbor.yaml` parses through
      `internal/config.LoadFromBytes` after the operator uncomments
      one LLM provider block and pastes a fake api-key env var.
- [ ] `internal/tools/builtin/builtin.go::Register(cat, names)`
      registers each name through `inproc.RegisterFunc`. Unknown name
      → `ErrUnknownBuiltIn` naming the registry's known set.
- [ ] `internal/tools/builtin/clock.go::Now` returns the current UTC
      time as `{rfc3339, epoch_ms}`.
- [ ] `internal/tools/builtin/text.go::Echo` returns its `{text}`
      input verbatim.
- [ ] `internal/config.ToolsConfig` gains `BuiltIn []string`.
      `validateTools` rejects any entry not in the builtin allowlist
      (mirrored from `internal/tools/builtin.KnownNames` via the
      same package-mirror pattern §4.4 uses for OAuth drivers /
      planner drivers / approval policies).
- [ ] `internal/tools/builtin` ships a one-line registration-mirror
      test (`TestKnownNames_MirrorsConfigAllowlist`) that asserts the
      config allowlist and `builtin.KnownNames` stay in lockstep.
- [ ] `cmd/harbor/cmd_dev.go::bootDevStack` calls
      `builtin.Register(cat, cfg.Tools.BuiltIn)` after the catalog is
      constructed.
- [ ] `harbortest/devstack/devstack.go` mirrors the built-in
      registration call (D-094).
- [ ] `docs/CONFIG.md` exists and includes one `### <yaml.path>`
      heading per config field walked by `walkLeaves(Config{}, ...)`.
- [ ] `internal/config/doc_drift_test.go::TestConfigDoc_AllFieldsDocumented`
      passes against the shipped CONFIG.md; it fails when a fresh
      field is added to the Config struct without a corresponding
      CONFIG.md heading.
- [ ] `scripts/smoke/phase-83n.sh` asserts:
      - `harbor init --help` returns 0 and mentions the subcommand;
      - the init-dropped yaml contains the four LLM example markers;
      - `cmd/harbor/init/templates/default/` ships exactly four
        `.tmpl` files;
      - `internal/tools/builtin/` contains `clock.go`, `text.go`,
        `builtin.go`;
      - `docs/CONFIG.md` exists and has the required top-level
        sections.

## Files added or changed

- `cmd/harbor/cmd_init.go` — new; cobra wiring for `harbor init`.
- `cmd/harbor/init/init.go` — new; the `Init` engine + sentinel
  errors.
- `cmd/harbor/init/templates/default/harbor.yaml.tmpl` — new; tiered
  config template.
- `cmd/harbor/init/templates/default/AGENTS.md.tmpl` — new.
- `cmd/harbor/init/templates/default/CLAUDE.md.tmpl` — new.
- `cmd/harbor/init/templates/default/README.md.tmpl` — new.
- `cmd/harbor/root.go` — register the new subcommand on the root.
- `internal/tools/builtin/builtin.go` — new; the registration
  dispatcher + sentinel errors + `KnownNames`.
- `internal/tools/builtin/clock.go` — new; `clock.now`.
- `internal/tools/builtin/text.go` — new; `text.echo`.
- `internal/tools/builtin/builtin_test.go` — new; registration +
  allowlist-mirror test.
- `internal/config/config.go` — add `BuiltIn []string` to
  `ToolsConfig`.
- `internal/config/validate.go` — validate `tools.built_in` against
  the mirrored allowlist.
- `internal/config/doc_drift_test.go` — new; CONFIG.md drift test.
- `cmd/harbor/cmd_dev.go` — call
  `builtin.Register(cat, cfg.Tools.BuiltIn)` after catalog
  construction.
- `harbortest/devstack/devstack.go` — D-094 mirror.
- `docs/CONFIG.md` — new; full knob reference.
- `docs/plans/README.md` — Phase 83n row + flip to Shipped.
- `docs/decisions.md` — D-153 entry.
- `docs/glossary.md` — `harbor init`, `built-in tool` entries.
- `scripts/smoke/phase-83n.sh` — new.
- `README.md` — flip 83n status + add the init flow to "Getting
  started."

## Public API surface

- `harborinit.Init(opts Options) (Result, error)` —
  `package init` at `cmd/harbor/init/init.go`. Materialises files
  into `opts.TargetDir`. Sentinel errors:
  `ErrInvalidName`, `ErrFileExists`, `ErrInitFailed`.
- `builtin.Register(cat tools.ToolCatalog, names []string) error` —
  registers the named built-ins. `ErrUnknownBuiltIn` on an unknown
  name.
- `builtin.KnownNames() []string` — the registry-mirror that the
  config validator anchors on.

## Test plan

- **Unit:**
  - `harborinit.Init` golden tests: rendered yaml passes
    `internal/config.LoadFromBytes` (with the operator-uncommented
    OpenRouter block), refuses overwrite on existing files, returns
    `ErrInvalidName` on bad names.
  - `builtin.Register` happy-path + unknown-name error.
  - `clock.Now` returns a parseable RFC 3339 string; epoch_ms is
    within ±1s of `time.Now().UnixMilli()`.
  - `text.Echo` round-trips its input.
  - `TestKnownNames_MirrorsConfigAllowlist` asserts the two surfaces
    stay aligned (§4.4 mirror pattern).
- **Integration:**
  - `cmd/harbor/cmd_init_test.go` boots `harbor init --target <tmp>`
    via the cobra command tree; asserts all four files land and the
    yaml validates after a single uncomment.
  - `cmd/harbor/cmd_dev_runloop_test.go` extends an existing dev
    fixture: with `tools.built_in: [text.echo]` in the yaml, the
    catalog has `text.echo` registered and `Resolve("text.echo")`
    succeeds.
- **Conformance:**
  - N/A (no new transport).
- **Concurrency / leak:**
  - N/A — `harborinit.Init` is a one-shot file emitter; built-in
    tools register through the existing `inproc` driver whose
    concurrent-reuse contract D-025 already covers.

## Smoke script additions

`scripts/smoke/phase-83n.sh` asserts:

- `bin/harbor init --help` returns 0 (or skips when `bin/harbor`
  absent — `static-only` smoke band).
- `cmd/harbor/init/templates/default/` ships exactly four `.tmpl`
  files.
- The yaml template contains the four LLM-provider example markers
  (`Example 1: OpenRouter`, etc.).
- The yaml template contains the `built_in` opt-in marker.
- `internal/tools/builtin/{clock,text,builtin}.go` exist.
- `docs/CONFIG.md` exists and contains the per-subsystem top-level
  headings (`## Server`, `## Identity`, `## LLM`, etc.).
- `cmd/harbor/cmd_dev.go` references `builtin.Register`.
- `harbortest/devstack/devstack.go` mirrors the built-in registration
  (D-094).

## Coverage target

- `cmd/harbor/init`: 85% (new package, all paths reachable from
  tests).
- `internal/tools/builtin`: 90% (two trivial tools + a registration
  dispatcher).
- `cmd/harbor`: 80% (existing).

## Dependencies

- Phase 67 (`harbor scaffold` — the §4.4 seam pattern this phase
  mirrors).
- Phase 63 (cobra subcommand wiring + CLIError surface).
- Phase 26 (`tools.ToolCatalog` + `inproc.RegisterFunc`).

## Risks / open questions

- **`docs/CONFIG.md` rot.** The `doc_drift_test` catches missing
  headings, but not stale content (a field whose default changed but
  whose doc still says the old value). V1.1 ships with the test as
  the floor; richer assertions (e.g. "the documented default matches
  `defaults()`'s field value") are a Phase 83p+ concern.
- **AGENTS.md / CLAUDE.md mirror in the scaffolded project.** The
  init drops both verbatim; the operator is free to edit either
  independently. There is no enforcement at the operator-project
  level (Harbor's own mirror gate doesn't apply downstream). Documented
  in the rendered files.
- **Init's identity placeholders pass validation but won't accept
  real JWTs.** That's deliberate — the operator MUST set real
  issuer/audience/jwks values before any non-local deployment. The
  README and AGENTS.md flag this loudly.

## Glossary additions

- **harbor init** — the subcommand that drops the editable yaml +
  AGENTS.md/CLAUDE.md/README into a directory. Predecessor in the
  operator workflow: nothing. Successor: `harbor validate` →
  `harbor scaffold` (83o reads the yaml) → `harbor dev`.
- **built-in tool** — a tool shipped in the Harbor binary that
  registers on demand by name via `tools.built_in: [name]`. V1.1
  ships `clock.now` + `text.echo`. Distinct from operator-authored
  tools (registered via `inproc.RegisterFunc` from the scaffolded
  Go project).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Concurrent-reuse — N/A: `harborinit.Init` is a one-shot;
      built-in tools register through the existing `inproc` driver
      whose D-025 contract is already tested
- [ ] Integration test exists per §17 — `cmd/harbor/cmd_init_test.go`
      covers the new subcommand end-to-end; the dev fixture exercises
      the built-in registration path
- [ ] Glossary updated
- [ ] If a brief finding was departed from: N/A
