# Phase 63 — Harbor CLI skeleton (`harbor` + cobra)

## Summary

Phase 63 turns `cmd/harbor/main.go` from a driver-registration stub into a
cobra-rooted CLI binary that registers the seven settled subcommands
(`dev`, `scaffold`, `validate`, `version`, `inspect-events`,
`inspect-runs`, `inspect-topology`). Only `harbor version` is fully
implemented — it returns the harbor binary's semver, the `runtime/debug`
build hash, and the Phase 59 `types.ProtocolVersion`. Every other
subcommand stubs out as a structured non-zero exit pointing to its
implementing phase, so the §13 "test stubs as production defaults"
amendment is satisfied at the operator-facing seam. Global flags
`--quiet` and `--json` ship now so the structured-error JSON shape is
locked before Phase 64 wires the LLM.

## RFC anchor

- RFC §8
- RFC §5.3

## Briefs informing this phase

- brief 06

## Brief findings incorporated

- brief 06 §2 (the `CLICommand` shape): the CLI wraps `cobra.Command`
  with Harbor's conventions — **structured errors**, `--quiet`, `--json`
  output mode, hint strings. Phase 63 ships the wrapper and the global
  flags so the conventions are set BEFORE the substantive subcommands
  (Phases 64–70) start filling them in.
- brief 06 §6 (CLI golden tests): `harbor --help` and the substantive
  subcommands ship golden-file tests. Phase 63 lays the `testdata/golden/`
  scaffolding and gates `harbor --help` + `harbor version` + every stub
  subcommand's structured error against golden files so a later
  subcommand-completing phase inherits the golden test pattern.
- brief 06 §7 #8 (Harbor CLI skeleton ~1 phase): the master-plan slot
  Phase 63 fills. Subcommands themselves are intentionally split across
  Phases 64–70 — Phase 63 ships only the cobra skeleton + the
  structured-error vocabulary, exactly as brief 06 sized it.

## Findings I'm departing from (if any)

None.

## Goals

- A single `cmd/harbor` package that produces a working `./bin/harbor`
  binary with seven cobra subcommands registered.
- `harbor version` is fully implemented (prints harbor semver + build
  hash + `types.ProtocolVersion`) and ships a `--json` mode emitting the
  same fields as a stable JSON object.
- Stub subcommands (`dev`, `scaffold`, `validate`, `inspect-events`,
  `inspect-runs`, `inspect-topology`) exit non-zero with a structured
  error pointing to the implementing phase — `--json` emits the same
  error as a stable JSON object.
- Global flags `--quiet` and `--json` are declared on the root command
  and inherited by every subcommand; `--quiet` suppresses informational
  output while preserving error output.
- A `cmd/harbor.CLIError` structured-error type lives in `cmd/harbor`
  (NOT under `internal/protocol/errors`, which owns Protocol wire error
  codes — a different surface) and is the only path errors reach
  `Stderr` / `Stdout` in `--json` mode.
- The preflight gate continues to pass: `bin/harbor dev` exits non-zero
  with a structured error, and preflight detects this and treats it as
  "not yet implemented" rather than a hard failure (the same posture
  preflight already takes for a stub binary that exits 0; the
  distinguishing signal is the structured-error stderr body).
- Cobra is added to `go.mod` as a direct dependency (RFC §10 lists
  `cobra` as the settled CLI library — no RFC PR required).
- `cmd/harbor`'s coverage is ≥ 70% via golden-file tests and cobra
  command-driver tests.

## Non-goals

- Implementing the substantive subcommand bodies — `dev` is Phase 64,
  `scaffold` is Phase 67, `validate` is Phase 68, `inspect-events` and
  `inspect-runs` are Phase 69, `inspect-topology` is Phase 70.
- Booting an HTTP server. Phase 60 shipped the transport `http.Handler`s
  but **not** the listener — `harbor dev` is what binds those onto a
  real `net.Listener`, and that is Phase 64's work. The Phase 63 `dev`
  command stubs out non-zero.
- Identity-resolution or auth wiring inside the CLI. Subcommands that
  open Protocol-client connections (Phase 69's `inspect-*`) wire
  identity in their own phases.
- The `--mock` / `HARBOR_DEV_ALLOW_MOCK=1` escape hatch. The Phase 64
  pre-plan note in `docs/plans/README.md` pins the choice; Phase 63
  leaves room for the flag on `dev` (does not declare it now, does not
  preclude it later).
- Hot-reload, draft saving, scaffold templates, validate rules,
  topology rendering — all are downstream phases per the master plan.

## Acceptance criteria

- [ ] `./bin/harbor` builds via `CGO_ENABLED=0 go build -ldflags='-s -w'
      -o bin/harbor ./cmd/harbor` (the Makefile path) — produces a
      single static binary, no CGo.
- [ ] `harbor --help` exits 0 with stable output matching
      `cmd/harbor/testdata/golden/help.txt` (golden-file test in
      `cmd/harbor`).
- [ ] `harbor version` exits 0 and prints `harbor <semver>` +
      `protocol <ProtocolVersion>` + `build <hash>` in a stable
      human-readable form; with `--json`, prints
      `{"harbor":"<semver>","protocol":"<version>","build_hash":"<hash>"}`.
- [ ] `harbor dev`, `harbor scaffold`, `harbor validate`,
      `harbor inspect-events`, `harbor inspect-runs`,
      `harbor inspect-topology` each exit non-zero with a structured
      error of the shape `{"error":"<msg>","code":"not_implemented",
      "hint":"see phase NN — <slug>"}` when `--json` is set; without
      `--json`, print `Error: harbor <subcommand>: not yet implemented
      (phase NN — <slug>)` on stderr.
- [ ] `--quiet` suppresses informational output (the cobra usage line
      on error) but preserves the structured-error body and the exit
      code.
- [ ] The structured-error shape is defined ONCE in `cmd/harbor/errors.go`
      and consumed via a `cmd/harbor.PrintCLIError` helper — no
      hand-rolled JSON in subcommand files.
- [ ] `scripts/smoke/phase-63.sh` asserts: `bin/harbor --help` matches
      the golden, `bin/harbor version` returns non-empty harbor +
      protocol + build_hash fields (both human and `--json`), each
      stub subcommand exits non-zero with the documented structured
      error (both human and `--json`).
- [ ] `scripts/preflight.sh` continues to pass — a Phase 63 build's
      `bin/harbor dev` exits non-zero with the structured "not
      implemented" error; preflight is amended to recognise this as
      the "stub subcommand" posture and treat it as SKIP for the
      `harbor dev` boot step (the smoke checks themselves run as
      always against the binary's `--help` / `version` / subcommand
      outputs).
- [ ] The `cobra` dependency is added to `go.mod` as a direct
      dependency; CI's `make build` produces a CGo-free static binary.
- [ ] Coverage on `cmd/harbor` is ≥ 70% (master-plan target). Golden
      tests + subcommand-driver tests + JSON-shape tests together
      satisfy the target.
- [ ] `docs/plans/README.md` flips Phase 63 from `Pending` to
      `Shipped`; `README.md`'s Status table adds a Phase 63 row; the
      README's "Harbor CLI" prose is updated to describe what the
      binary does today (registers seven subcommands; only `version`
      fully implemented).

## Files added or changed

```text
cmd/harbor/
├── main.go                       # cobra root + driver blank-imports (unchanged blank-import block)
├── root.go                       # `newRootCmd()` — global flags --quiet, --json; binds subcommands
├── errors.go                     # `CLIError` struct + `PrintCLIError` helper; the structured-error shape
├── cmd_version.go                # `newVersionCmd()` — the only fully-implemented subcommand
├── cmd_dev.go                    # `newDevCmd()` — stub; exits non-zero pointing to phase 64
├── cmd_scaffold.go               # `newScaffoldCmd()` — stub; exits non-zero pointing to phase 67
├── cmd_validate.go               # `newValidateCmd()` — stub; exits non-zero pointing to phase 68
├── cmd_inspect_events.go         # `newInspectEventsCmd()` — stub; exits non-zero pointing to phase 69
├── cmd_inspect_runs.go           # `newInspectRunsCmd()` — stub; exits non-zero pointing to phase 69
├── cmd_inspect_topology.go       # `newInspectTopologyCmd()` — stub; exits non-zero pointing to phase 70
├── root_test.go                  # golden-file test for `harbor --help`
├── cmd_version_test.go           # version output tests (human + --json)
├── cmd_stub_test.go              # stub-subcommand structured-error tests (human + --json)
├── errors_test.go                # `CLIError` round-trip + `PrintCLIError` shape tests
└── testdata/
    └── golden/
        ├── help.txt              # `harbor --help` golden
        ├── version_help.txt      # `harbor version --help` golden
        └── stub_dev.txt          # `harbor dev` stderr (human-mode) golden
go.mod / go.sum                   # add `github.com/spf13/cobra` direct
scripts/smoke/phase-63.sh         # new smoke
scripts/preflight.sh              # amend stub-detection to tolerate non-zero exit with structured err
docs/plans/phase-63-cli-skeleton.md  # this plan
docs/plans/README.md              # flip Phase 63 row Pending -> Shipped
docs/decisions.md                 # append D-084
docs/glossary.md                  # add `CLIError`, `Harbor CLI structured error`, `Golden file (CLI)` terms
README.md                         # Status row Phase 63; "Harbor CLI" prose update
```

## Public API surface

- `cmd/harbor.NewRootCmd() *cobra.Command` — the cobra root, used by
  `main.go` and by tests that exercise the command tree
  (`cmd.SetArgs(...)`, `cmd.Execute()`).
- `cmd/harbor.CLIError` — `{Code, Message, Hint string}`. `Error() string`
  returns the human-readable form `harbor <subcommand>: <message>
  [(<hint>)]`. The JSON tags pin the wire shape:
  `{"error","code","hint"}`.
- `cmd/harbor.PrintCLIError(w io.Writer, jsonMode bool, err CLIError)` —
  the single sink for CLI errors; emits the JSON object when
  `jsonMode`, otherwise the human-readable form.
- `cmd/harbor.HarborVersion = "v0.0.0-dev"` — package-level constant; a
  later phase pins a real release semver. Build hash comes from
  `runtime/debug.ReadBuildInfo()`.

Nothing in `cmd/harbor` is imported by anything else in the repo —
`cmd/harbor` is the binary entry point and the closest thing Harbor
has to a "leaf" package. The CLI's structured-error shape is
intentionally NOT in `internal/protocol/errors/`; that home is for
Protocol *wire* error codes consumed by Protocol clients, and the CLI
is a different surface (operator-facing exit codes + structured stderr
JSON, not Protocol responses).

## Test plan

- **Unit:** `errors_test.go` covers `CLIError` zero-value, `Error()`
  rendering with and without `Hint`, `MarshalJSON` round-trip
  (assert wire field names `error`/`code`/`hint`).
- **Unit (cobra-driver):** `cmd_version_test.go` invokes the version
  command via the cobra command tree (`cmd.SetArgs([]string{"version"})`,
  `cmd.SetOut(&buf)`) and asserts the human-mode output contains the
  three labels (`harbor`, `protocol`, `build`), and that `--json` emits
  a `{harbor,protocol,build_hash}` JSON object with `harbor ==
  HarborVersion` and `protocol == types.ProtocolVersion`.
- **Unit (cobra-driver):** `cmd_stub_test.go` invokes each stub
  subcommand and asserts: (a) exit code is non-zero, (b) without
  `--json`, stderr starts with `Error: harbor <subcommand>: not yet
  implemented`, (c) with `--json`, stderr is a single-line JSON
  object with the documented shape and `code == "not_implemented"`,
  (d) the `hint` field references the implementing phase number.
- **Unit (golden):** `root_test.go` runs `harbor --help` via the cobra
  tree and asserts the output matches `testdata/golden/help.txt`. The
  golden is regenerable via `go test -update` (a `-update` flag the
  test honours), so future subcommand additions update the golden in
  the same PR.
- **Integration:** N/A — the CLI consumes `internal/protocol/types`
  only, which is itself a pure-value package with no I/O. There is no
  cross-subsystem seam Phase 63 opens (the consuming seams — Protocol
  client over the wire, config loader, etc. — land in Phases 64+).
- **Conformance:** N/A — the CLI is not a §4.4 driver-registered
  subsystem.
- **Concurrency / leak:** N/A — the CLI is a one-shot process. Each
  invocation is its own `os.Exit`; there is no long-lived reusable
  artifact landing in Phase 63. Phase 64's `harbor dev` will ship the
  long-lived server and pick up the D-025 concurrent-reuse obligation
  there.

## Smoke script additions

`scripts/smoke/phase-63.sh` asserts:

1. **Build present.** `bin/harbor` exists and is executable (the
   preflight build step ran). FAIL if absent.
2. **`harbor --help` golden match.** Runs `./bin/harbor --help` and
   diffs against `cmd/harbor/testdata/golden/help.txt`. SKIP cleanly if
   the binary is absent (matches the 404/405/501 → SKIP convention for
   not-yet-implemented surfaces).
3. **`harbor version` human shape.** Runs `./bin/harbor version` and
   asserts the output contains `harbor v` (the semver prefix),
   the `protocol` label, and the `build` label. FAIL if any label
   is missing.
4. **`harbor version --json` shape.** Runs `./bin/harbor version --json`
   and asserts the output is valid JSON with non-empty `.harbor`,
   `.protocol`, `.build_hash` (via `jq`; SKIP if `jq` is absent per the
   common.sh convention).
5. **`harbor version --json` protocol matches `types.ProtocolVersion`.**
   Asserts `.protocol == "0.1.0"` (the pinned Phase 59 version — a
   future Protocol-version bump updates this assertion).
6. **Stub subcommand exit codes.** For each of `dev`, `scaffold`,
   `validate`, `inspect-events`, `inspect-runs`, `inspect-topology`:
   - Asserts `./bin/harbor <sub> --json` exits non-zero.
   - Asserts the stderr JSON has `.code == "not_implemented"`.
   - Asserts the `.hint` field mentions a phase number.
7. **No live HTTP.** Skips the `/healthz` / `protocol_call` assertions
   per the 404/405/501 → SKIP convention — Phase 63 does not boot a
   server; Phase 64 does.

## Coverage target

- `cmd/harbor`: 70% (master-plan target). Golden-file tests + cobra
  command-driver tests + JSON-shape tests are the primary surface; the
  uncovered fraction is the `main()` function (which only calls
  `NewRootCmd().Execute()`) and the `os.Exit` paths (untestable via
  `go test`).

## Dependencies

- **Phase 60** — Protocol wire transport shipped. `harbor version`'s
  protocol field reads `internal/protocol/types.ProtocolVersion`, which
  is independent of the wire transport, but the master plan pins
  Phase 60 as the prerequisite to keep the wave ordering coherent
  (Phase 64 will mount the Phase 60 transports onto a `net.Listener`).
- **Phase 59** — `types.ProtocolVersion` constant + `Version` parsed
  form. Phase 63 reads the constant directly; the parsed form is
  surface area Phase 64+ will use when negotiating with a connected
  client.
- **Phase 58** — `internal/protocol/errors` (Protocol wire error codes).
  Phase 63 deliberately does NOT add a CLI error code there — the CLI
  structured error is a different surface — but Phase 63 confirms the
  boundary (the §13 single-source pin remains intact).

## Risks / open questions

- **Cobra import surface.** Cobra pulls `spf13/pflag` and a couple of
  small helpers transitively. All MIT-licensed and CGo-free. The
  static-binary invariant is preserved (`go build -ldflags='-s -w'`
  produces a single static binary).
- **Stub-subcommand exit code under preflight.** The §13 amendment
  requires stub subcommands to exit non-zero; the existing preflight
  treats any non-zero `harbor dev` exit as a hard failure. Phase 63
  amends `scripts/preflight.sh` to recognise the structured-error
  stderr body (`"not_implemented"` code) and treat that case as the
  "stub binary" posture (SKIP the live-HTTP step, continue running
  smoke scripts). This is a forward-compatible change — Phase 64's
  real `harbor dev` will *not* exit with that code, so preflight will
  resume the original boot-and-wait posture.
- **Golden-file churn.** Every new subcommand added in Phases 64–70
  will mutate the `harbor --help` golden output (the subcommand list
  grows). The `-update` flag on the golden test is the standard escape
  hatch; the PR adding a subcommand also regenerates the golden in
  the same commit.
- **`HarborVersion` default.** Pinned at `"v0.0.0-dev"` for Phase 63;
  a later phase (likely Phase 78 — the release/distribution phase per
  the master plan) injects a real semver via `-ldflags`. The CLI
  surface is forward-compatible.

## Glossary additions

- **CLIError** — the structured-error type for the Harbor CLI binary
  (`cmd/harbor/errors.go`). Carries `Code` / `Message` / `Hint`; emits
  a stable JSON shape `{"error","code","hint"}` in `--json` mode and a
  human-readable form `Error: harbor <subcommand>: <message> [(<hint>)]`
  otherwise. Distinct from `protocol/errors.Error` (Protocol wire error
  codes consumed by Protocol clients); the CLI surface is operator-
  facing exit codes + stderr JSON, not Protocol responses. Phase 63 ships
  the type; every CLI exit path uses it. D-084.
- **Golden file (CLI)** — the `cmd/harbor/testdata/golden/*.txt` files
  the cobra golden tests diff against. The `-update` flag on the test
  regenerates the golden; a subcommand addition mutates the help golden
  in the same PR (Phase 63 establishes the pattern; Phases 64–70 inherit
  it). brief 06 §6.
- **Stub subcommand** — a Phase 63 subcommand whose body is a non-zero
  exit with a `CLIError{Code: "not_implemented", Hint: "see phase NN"}`.
  The pattern satisfies the §13 "test stubs as production defaults"
  amendment: the help text + exit code + structured error make
  unambiguous that no work happened, so a script invoking `harbor dev`
  on a Phase 63 build does not get fooled. D-084.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target (`cmd/harbor` ≥ 70%)
- [ ] If multi-isolation paths changed: cross-session isolation test
      passes — **N/A:** Phase 63 ships no identity-touching code; the
      CLI is a one-shot process that does not load identity.
- [ ] **If this phase builds a reusable artifact:** concurrent-reuse
      test passes — **N/A:** Phase 63 ships no long-lived reusable
      artifact. Each `bin/harbor` invocation is a one-shot process; the
      cobra root is constructed per-invocation in `main()`. Phase 64
      will ship `harbor dev`'s long-lived server and pick up the D-025
      obligation there.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes
      a cross-subsystem seam:** an integration test exists — **N/A:**
      Phase 63 consumes `internal/protocol/types.ProtocolVersion`
      (a pure constant), no cross-subsystem seam is opened. Phase 64
      will open the first cross-subsystem seam (CLI → Runtime over
      Protocol transports) and ship its integration test.
- [ ] If new vocabulary: glossary updated — CLIError, Golden file
      (CLI), Stub subcommand added.
- [ ] If a brief finding was departed from: justified above +
      decisions.md entry filed — no departures.
