# Phase 67 — `harbor scaffold`

## Summary

Phase 67 replaces the Phase 63 `harbor scaffold` stub with a real cobra
subcommand that materialises a new Harbor agent project skeleton from an
embedded template (default `minimal-react`). The generated project ships
a production-shaped `harbor.yaml`, a Go module that imports the public
`harbortest` package, a worked example agent + test, and a README — the
config validates against `internal/config.Load + Validate` directly so
the scaffold's "buildable project" contract holds today without
depending on Phase 68's `harbor validate` subcommand (sibling-shipping
in the same wave).

## RFC anchor

- RFC §8

## Briefs informing this phase

- brief 06

## Brief findings incorporated

- **brief 06 §3 + §6 (CLI golden tests).** Brief 06 lists
  `harbor scaffold --help` and the scaffolded output among the surfaces
  that ship CLI golden-file tests; Phase 67 inherits the
  `cmd/harbor/testdata/golden/` scaffolding Phase 63 laid and adds a
  per-file golden directory (`testdata/golden/minimal-react/`) the
  scaffold tests diff every rendered file against.
- **brief 06 §7 #11 ("Draft-save dynamic agent scaffolding").** Brief 06
  sizes this work as "1–2 phases" and pairs the scaffold subcommand
  with the `harbor dev` draft-save endpoint (Phase 66). Phase 67 closes
  the one-shot scaffold half — draft-save lands at Phase 66 once
  Phase 64 ships `harbor dev`. The brief's "draft, iterate, save when
  ready" loop terminates with `harbor scaffold`-emitted layout; this
  phase establishes that layout.
- **brief 06 §2 (the `CLICommand` shape — structured errors, `--quiet`,
  `--json`, hint strings).** Phase 63 (D-084) pinned the
  `CLIError{Code, Message, Hint}` surface; Phase 67's scaffold body
  reuses it for every failure path (existing-output-dir, invalid name,
  unknown template) — no new error codes, no hand-rolled JSON.

## Findings I'm departing from (if any)

- **None on briefs.** A §4.3 deviation against the master-plan
  acceptance criterion ("`harbor validate` returns 0") is documented in
  D-087: at scaffold-time, `harbor validate` is a sibling-shipping
  Phase 68 stub, so the scaffolded-config-validates check is
  instrumented against `internal/config.Load + Validate` directly. The
  cross-phase CLI integration lands in Phase 68's PR per CLAUDE.md
  §17.6.

## Goals

- `harbor scaffold --name <name> [--template <template>] [--output <dir>]`
  materialises a new project directory containing a production-shaped
  Harbor agent skeleton.
- Default template is `minimal-react`; the supported template set is
  embedded in the binary (via Go's `embed` package) so the
  CGo-free / single-static-binary invariant holds.
- The scaffolded `harbor.yaml` parses + validates via
  `internal/config.Load + Validate` with zero further edits — proven by
  a `*_test.go` assertion. The config demonstrates the production
  shape (bifrost LLM driver with an `env.NAME` API key reference,
  sqlite state, real audit redactor) per the Phase 64 pre-plan note's
  "production-shaped config in examples" constraint AND the §13
  amendment.
- Stable golden-file tests pin every rendered file's exact bytes for a
  fixed project name + template, so unintended template drift surfaces
  immediately.
- Every failure mode (invalid name, output dir already exists, unknown
  template) routes through the Phase 63 `CLIError` shape — no
  hand-rolled JSON, no silent partial writes.
- `--json` returns `{"name":"...","output_dir":"...","files":[...]}`
  on success for scripting consumers.

## Non-goals

- The `harbor dev` draft-save flow (Phase 66 — depends on Phase 64 and
  on a running `harbor dev` server which Phase 67 cannot exercise).
- Multiple templates. Phase 67 ships exactly one (`minimal-react`); the
  template-registry surface is wired generically so future phases add
  templates without re-touching the command body.
- Live cross-phase integration with `harbor validate`. The
  acceptance-criterion path uses `internal/config.Load + Validate`
  directly; the CLI integration lands in Phase 68's PR per §17.6.
- A `--mock` LLM escape hatch in the scaffolded config. The Phase 64
  pre-plan note pins the escape-hatch design to the `harbor dev`
  command, not to the scaffold template (the scaffold's job is to
  emit production-shaped config).

## Acceptance criteria

- [ ] `harbor scaffold --name my-agent --output <tmpdir>/my-agent`
      exits 0 and creates a directory containing `go.mod`,
      `harbor.yaml`, `README.md`, `agent.go`, `agent_test.go`.
- [ ] The rendered `harbor.yaml` validates cleanly through
      `internal/config.Load + Validate` with zero further edits — pinned
      by `TestScaffold_RenderedConfig_PassesConfigValidate` in
      `cmd/harbor/scaffold/scaffold_test.go`.
- [ ] Golden-file diff: every rendered file matches
      `cmd/harbor/testdata/golden/minimal-react/<filename>` byte-for-
      byte for a fixed project name (`acme-agent`). A `-update` flag
      on the test regenerates the goldens.
- [ ] Negative paths each emit `CLIError{Code: "..."}`:
      - existing-output-directory → `CodeOutputDirExists`
      - invalid project name (contains `/`, `..`, leading `-`, empty)
        → `CodeInvalidProjectName`
      - unknown template → `CodeUnknownTemplate`
- [ ] `--json` mode emits a single-line JSON object on success:
      `{"name":"<name>","output_dir":"<abs path>","files":[...]}`.
- [ ] `scripts/smoke/phase-67.sh` exercises the built `bin/harbor`
      scaffold against a temp dir, asserts the expected files exist,
      and asserts the rendered config validates via a helper that
      calls `internal/config.Load + Validate`.
- [ ] Coverage on `cmd/harbor/` and the new `cmd/harbor/scaffold/`
      package is ≥ 70% (master-plan target).
- [ ] `docs/plans/README.md` flips Phase 67 row `Pending` → `Shipped`;
      `README.md`'s Status table adds the Phase 67 row + a one-line
      pointer in the testing section.
- [ ] `docs/decisions.md` gains D-087 documenting the template-set,
      the `internal/config.Load + Validate` direct-call deviation, and
      the production-shaped-config posture.

## Files added or changed

```text
cmd/harbor/
├── cmd_scaffold.go                  # real implementation (was stub)
├── cmd_scaffold_test.go             # NEW — cobra-driver tests (golden + negative + JSON shape)
├── scaffold/                        # NEW package — engine + templates
│   ├── doc.go
│   ├── scaffold.go                  # exported entry point + template registry
│   ├── render.go                    # text/template renderer + write
│   ├── scaffold_test.go             # engine-level unit tests + config-validate
│   └── templates/
│       └── minimal-react/
│           ├── go.mod.tmpl
│           ├── harbor.yaml.tmpl
│           ├── README.md.tmpl
│           ├── agent.go.tmpl
│           └── agent_test.go.tmpl
└── testdata/
    ├── golden/
    │   └── minimal-react/           # NEW — golden of every rendered file for "acme-agent"
    │       ├── go.mod
    │       ├── harbor.yaml
    │       ├── README.md
    │       ├── agent.go
    │       └── agent_test.go
    └── golden/help.txt              # regenerated (scaffold's Short loses "(Phase 67)")
scripts/smoke/phase-67.sh            # NEW
docs/plans/phase-67-scaffold.md      # this plan
docs/plans/README.md                 # Phase 67 row Pending → Shipped + deviation note
docs/decisions.md                    # append D-087
docs/glossary.md                     # add Template, minimal-react, Scaffold output
README.md                            # Status row Phase 67 + testing-section pointer
```

## Public API surface

- `cmd/harbor/scaffold.Scaffold(opts Options) (Result, error)` — the
  exported engine entrypoint; `Options{Name, Template, OutputDir}`,
  `Result{Name, OutputDir, Files []string}`. Validates inputs, picks
  the embedded template, renders every file under `OutputDir`, returns
  the absolute paths written.
- `cmd/harbor/scaffold.Templates() []string` — the list of registered
  templates (`["minimal-react"]` in Phase 67). The cobra `--template`
  flag uses this as its allowed-value list.
- `cmd/harbor/scaffold.DefaultTemplate = "minimal-react"`.
- Sentinel errors: `ErrInvalidName`, `ErrOutputDirExists`,
  `ErrUnknownTemplate`. The `cmd/harbor/cmd_scaffold.go` body maps
  these onto `CLIError{Code: ...}` constants.

Nothing in `cmd/harbor/scaffold` is consumed outside `cmd/harbor` — it
is a binary-internal package. The package is exported (top-level under
`cmd/harbor/`) only because `cmd/harbor/cmd_scaffold.go` imports it;
the package's godoc explicitly marks it as "binary-internal".

## Test plan

- **Unit (`cmd/harbor/scaffold/scaffold_test.go`):**
  `TestScaffold_HappyPath_WritesAllFiles`,
  `TestScaffold_InvalidName_FailsLoud`,
  `TestScaffold_OutputDirExists_FailsLoud`,
  `TestScaffold_UnknownTemplate_FailsLoud`,
  `TestScaffold_Templates_ListsMinimalReact`,
  `TestScaffold_RenderedConfig_PassesConfigValidate` (calls
  `internal/config.Load + Validate` on the rendered `harbor.yaml`).
- **Unit (`cmd/harbor/cmd_scaffold_test.go`):**
  golden-file diff (per-file against
  `testdata/golden/minimal-react/`), the `--json` output shape, the
  CLIError mapping for every negative path, `--quiet` does not
  suppress error output (parity with Phase 63's stub-quiet check).
- **Integration:** `TestScaffold_RenderedConfig_PassesConfigValidate`
  IS the integration test — it wires the rendered output against the
  real `internal/config` package (real driver on the seam per
  §17.3 #1) and asserts identity-agnostic shape constraints (the
  scaffold writes a config file, identity propagation is N/A — the
  config is not loaded against any identity context).
- **Conformance:** N/A — scaffold is a one-shot CLI, not a §4.4 driver.
- **Concurrency / leak:** N/A — `scaffold.Scaffold` is a pure function
  that returns synchronously and spawns no goroutines. No long-lived
  state, no D-025 obligation. (Phase 64 will ship the long-lived
  `harbor dev` server and pick up the engine-level D-025 dance there.)

## Smoke script additions

`scripts/smoke/phase-67.sh` asserts:

1. **Build present.** `bin/harbor` is executable; SKIP if absent.
2. **Run the cmd/harbor + scaffold tests under `-race`.** `go test
   -race ./cmd/harbor/... ./cmd/harbor/scaffold/...` exits zero.
3. **End-to-end scaffold.** Runs `./bin/harbor scaffold --name
   smoke-agent --output <tmp>/smoke-agent --json` against the built
   binary, asserts the JSON shape (`.name`, `.output_dir`, `.files`).
4. **Files exist.** Asserts `go.mod`, `harbor.yaml`, `README.md`,
   `agent.go`, `agent_test.go` are present in the scaffolded tree.
5. **Config validates.** Re-runs the scaffold-engine config-validate
   unit test (`go test -run TestScaffold_RenderedConfig`); FAIL on
   non-zero exit. This is the in-PR stand-in for `harbor validate`
   (Phase 68 sibling-shipping; CLI integration lands in Phase 68's PR
   per §17.6).
6. **Negative path.** Runs scaffold twice against the same output dir;
   the second invocation must exit non-zero with
   `.code == "output_dir_exists"`.

## Coverage target

- `cmd/harbor`: ≥ 70% (master-plan target). The scaffold body adds new
  paths; the Phase 63 `errors.go` / `root.go` / other-subcommands path
  remain unchanged.
- `cmd/harbor/scaffold`: ≥ 70% (mirroring the parent package's
  master-plan target for the cmd/harbor subsystem). Golden-file tests,
  negative-path tests, and the config-validate test together saturate
  the small surface.

## Dependencies

- **Phase 63** (CLI skeleton) — provides the cobra command tree,
  `CLIError`, `PrintCLIError`, `--json` / `--quiet` global flags, the
  `testdata/golden/` scaffolding, and the stub registration site this
  phase replaces.
- **Phase 02** (Config loader) — the rendered `harbor.yaml` is
  validated against `internal/config.Load + Validate` as the in-PR
  stand-in for `harbor validate` (sibling-shipping in Phase 68; the
  CLI integration lands in Phase 68's PR per §17.6).

## Risks / open questions

- **Sibling-shipping Phase 68 (`harbor validate`).** The master-plan
  acceptance criterion names `harbor validate` as the validation
  surface; at scaffold-time that subcommand is a sibling-shipping stub.
  The §4.3 deviation in D-087 explains: scaffold's tests use
  `internal/config.Load + Validate` directly; the CLI integration
  smoke step lands in Phase 68's PR per §17.6. The §13
  primitive-with-consumer rule is satisfied: the scaffold's
  consumer-of-the-config-validator is the in-tree `internal/config`
  package (a real, shipped subsystem), not a planned future CLI
  surface.
- **Template embedding + the static-binary invariant.** Templates are
  bundled via `//go:embed`; the produced `bin/harbor` stays CGo-free
  and single-static. No new module-level dependencies.
- **Cross-phase smoke maintenance (§17.6).** Phase 63's smoke
  iterates a fixed `stubs=(dev scaffold validate inspect-events
  inspect-runs inspect-topology)` table and asserts each emits
  `CodeNotImplemented`. Phase 67's real `scaffold` returns
  `CodeInvalidProjectName` on the empty-name invocation the smoke
  uses — so this PR drops `scaffold` from `scripts/smoke/phase-63.sh`'s
  `stubs` array. The §17.6 convention is settled: cross-phase smoke
  maintenance rides with the PR that moves the subcommand out of
  stub status. Future PRs that ship `validate`/`inspect-*` will
  trim the stubs array further; the same comment in `phase-63.sh`
  documents the pattern.
- **Markdownlint regression on `docs/decisions.md`.** Per the
  recurring failure modes in §17.7, every `## D-NNN` heading and `---`
  separator needs blank lines surrounding it. The append is done
  carefully + `markdownlint-cli2` is run before commit.

## Glossary additions

- **`harbor scaffold`** — the CLI subcommand (Phase 67, D-087) that
  materialises a new Harbor agent project skeleton from an embedded
  template. Default template `minimal-react`. Output passes
  `internal/config.Load + Validate` with zero further edits (the
  in-PR stand-in for `harbor validate`, which sibling-ships in
  Phase 68 — the cross-phase CLI integration lands in Phase 68's PR
  per CLAUDE.md §17.6).
- **Template (scaffold)** — the named, embedded set of `.tmpl` files
  under `cmd/harbor/scaffold/templates/<name>/` that `harbor scaffold`
  renders into a new project directory. Phase 67 ships exactly one
  template (`minimal-react`); future phases (or operator-shipped
  out-of-tree templates) extend the surface without re-touching the
  command body.
- **`minimal-react`** — Phase 67's V1 template. Produces a Harbor
  agent project demonstrating the production-shaped config (bifrost
  LLM driver with an `env.NAME` API key reference, sqlite state, real
  audit redactor — per the Phase 64 pre-plan note + the §13
  amendment), a worked Agent + harbortest-driven test, a README, and
  a Go module.
- **Scaffold output** — the directory tree `harbor scaffold` writes:
  `go.mod`, `harbor.yaml`, `README.md`, `agent.go`, `agent_test.go`
  for the `minimal-react` template.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target (`cmd/harbor` ≥ 70%,
      `cmd/harbor/scaffold` ≥ 70%)
- [ ] If multi-isolation paths changed: cross-session isolation test
      passes — **N/A:** Phase 67 ships no identity-touching code; the
      scaffold is a one-shot CLI that writes files to disk.
- [ ] **If this phase builds a reusable artifact:** concurrent-reuse
      test passes — **N/A:** `scaffold.Scaffold` is a pure function
      that returns synchronously and spawns no goroutines. No
      long-lived state. Phase 64 will ship `harbor dev`'s long-lived
      server and pick up the D-025 obligation there.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes
      a cross-subsystem seam:** an integration test exists — the
      rendered-config-validates test
      (`TestScaffold_RenderedConfig_PassesConfigValidate`) IS the
      integration test, wiring real `internal/config.Load + Validate`
      against the rendered output (real driver on the seam per
      §17.3 #1).
- [ ] If new vocabulary: glossary updated — `harbor scaffold`,
      Template (scaffold), `minimal-react`, Scaffold output added.
- [ ] If a brief finding was departed from: justified above +
      decisions.md entry filed — no brief departures; the §4.3
      master-plan deviation (`harbor validate` direct-call) is
      documented in D-087.
