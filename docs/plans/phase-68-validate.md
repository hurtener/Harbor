# Phase 68 — `harbor validate`

## Summary

Replace the Phase 63 stub of `harbor validate` with the real subcommand. Validates a Harbor config file (default `harbor.yaml`) without booting any subsystem; surfaces each error category with stable, file:line-precise messages suitable for golden-file pinning and for use as a CI pre-flight check.

## RFC anchor

- RFC §8

## Briefs informing this phase

- brief 06

## Brief findings incorporated

- brief 06 §3 (Devx surface): "CLI golden tests: `harbor dev --help`, `harbor scaffold --help`, `harbor validate`, `harbor inspect-events --run <id>` produce stable output matching golden files." Phase 68 ships golden-pinned errors for every category so a future change to a message is a deliberate, reviewed regeneration.
- brief 06 §4 (CLI subcommand breadth at V1): `validate` is settled as a V1 subcommand. Phase 68 lands the body; later phases (skills validation surface, agent-definition validation surface) extend the same entry point.
- brief 06 §3 (devx posture): "All of this is implemented as protocol clients of the same runtime — no private hooks." `harbor validate` does NOT boot the Runtime / Protocol; it only invokes the in-process `internal/config` validator. The subcommand is a pre-boot tool, not a Protocol client. This is consistent with the brief — `validate` is the one explicit exception to the protocol-client posture because its purpose is to detect breakage BEFORE a server comes up.

## Findings I'm departing from (if any)

None. Phase 68 stays inside the brief 06 envelope: config validation today; skills + agent-definition extension when those surfaces gain standalone validation primitives (Phase 67's scaffolded output is the first real consumer of "skills + agent definitions"; a follow-up phase wires per-skill / per-agent-def validation through the same CLI entry).

## Goals

- Replace `cmd/harbor/cmd_validate.go`'s `not_implemented` stub with a working command body.
- Surface every Phase 02 (`internal/config`) validation rule as a CLI error with stable, golden-pinnable messages.
- Carry file:line precision for every error (file:line:col for YAML AST errors; file:line for semantic errors that name a field path but the loader does not currently track per-field tokens).
- Support `--json` mode that emits `{errors: [{category, message, file, line, hint}]}`.
- Provide a non-zero exit code (1) when validation finds problems; exit 2 for unexpected/internal errors (e.g. unreadable input); exit 0 when the input is valid.

## Non-goals

- Booting the Runtime, the Protocol, the LLM client, or any external dependency. `harbor validate` is a pure pre-boot tool.
- Validating arbitrary skill / agent-definition YAML files (the standalone validation primitives for those do not exist yet — `internal/skills/importer` consumes Markdown-with-frontmatter, not bare YAML; the Agent Registry's `agent_def` types are produced via the Registry API, not loaded from files). When those surfaces land, a successor phase wires them in.
- Replacing `internal/config/loader.go::Validate` — Phase 68 wraps the Phase 02 primitive, it does not rewrite the rules.
- Wiring CI to call `harbor validate` on `examples/*.yaml` — done in this PR via `.github/workflows/ci.yml` since the cost is one line and closes the acceptance loop.

## Acceptance criteria

- [ ] `harbor validate` exits 0 on a valid config (e.g. `examples/harbor.yaml`).
- [ ] `harbor validate testdata/validate/missing-llm-provider.yaml` exits 1 with a stable, golden-pinned error message naming `llm.provider` + the file:line.
- [ ] Each error category (config parse, config semantic, file not found) produces a stable message — pinned by a golden test.
- [ ] `--json` mode emits a single-line JSON object `{errors: [...]}` parseable by `jq`.
- [ ] Exit codes: 0 valid; 1 validation errors; 2 internal / I/O errors.
- [ ] The default path with no argument is `harbor.yaml` in the working directory; missing default is a category-3 error (file not found).
- [ ] `scripts/smoke/phase-68.sh` exercises the surface and shows OK > 0 / FAIL = 0.
- [ ] `.github/workflows/ci.yml` invokes `harbor validate examples/harbor.yaml` as a pre-flight check (CI uses validate as a pre-flight check — master-plan acceptance).
- [ ] Phase 67 cross-phase integration: when `harbor scaffold` lands, the scaffolded config passes `harbor validate`. Phase 68's smoke includes a cross-phase step that runs `harbor scaffold` then `harbor validate <scaffolded path>` and asserts exit 0 — SKIPs cleanly when Phase 67 has not yet merged. (§17.6 — fix what the gate finds.)

## Files added or changed

- `cmd/harbor/cmd_validate.go` — replace stub with real body.
- `cmd/harbor/validate_test.go` — unit + golden tests.
- `cmd/harbor/cmd_stub_test.go` — drop the `validate` entry from `stubCases` (validate is no longer a stub).
- `cmd/harbor/testdata/golden/help.txt` — regenerate; the validate `Short` description drops the "(Phase 68)" suffix.
- `cmd/harbor/testdata/validate/valid.yaml` — minimal valid fixture.
- `cmd/harbor/testdata/validate/missing-llm-provider.yaml` — invalid: bifrost driver with empty `llm.provider`.
- `cmd/harbor/testdata/validate/missing-identity-issuer.yaml` — invalid: empty `identity.issuer`.
- `cmd/harbor/testdata/validate/unknown-state-driver.yaml` — invalid: unknown driver in `state.driver`.
- `cmd/harbor/testdata/validate/malformed-yaml.yaml` — invalid: YAML parse error mid-document.
- `cmd/harbor/testdata/validate/golden/*.txt` — golden file per fixture (human-mode body; `.txt` extension because `.out` is gitignored).
- `cmd/harbor/testdata/validate/golden/*.json` — golden file per fixture (--json body).
- `scripts/smoke/phase-68.sh` — real smoke (replaces skeleton).
- `.github/workflows/ci.yml` — add a `harbor validate examples/harbor.yaml` pre-flight step.
- `README.md` — Status row Pending → Shipped + CLI section pointer.
- `docs/plans/README.md` — Phase 68 row Pending → Shipped.
- `docs/glossary.md` — new entries (validation error category, validation file:line precision).
- `docs/decisions.md` — D-088 entry.

## Public API surface

`harbor validate` is a CLI subcommand; it has no Go-library public surface beyond `cmd/harbor` (a `main` package). The structured-error shape is the `CLIError` defined in `cmd/harbor/errors.go` (D-084) plus a per-error categorisation embedded in the `Message` field. The `--json` body adds an `errors[]` array beside the `error` / `code` / `hint` fields; the wire shape is:

```json
{
  "error": "1 validation error in <file>",
  "code": "validation_failed",
  "hint": "see RFC §10 and CLAUDE.md §10",
  "errors": [
    {"category": "config.semantic", "file": "<file>", "line": 27, "message": "llm.provider must not be empty", "hint": "set llm.provider when driver != mock"}
  ]
}
```

The top-level `error` / `code` / `hint` summarise; `errors[]` is the array of individual findings.

## Test plan

- **Unit:** `cmd/harbor/validate_test.go` runs the cobra subcommand against each testdata fixture; asserts exit code, output prefix (human mode), JSON shape (`--json` mode), and golden equivalence for the body. One test per category. Test names: `TestValidate_Valid_ExitsZero`, `TestValidate_MissingLLMProvider_Golden`, `TestValidate_MissingIdentityIssuer_Golden`, `TestValidate_UnknownStateDriver_Golden`, `TestValidate_MalformedYAML_Golden`, `TestValidate_NoSuchFile_Golden`, `TestValidate_JSON_StableShape`.
- **Integration:** The cross-phase integration with Phase 67's scaffold lives in `scripts/smoke/phase-68.sh` (which calls the binary), not in Go-test form, because the scaffold command spawns subprocesses and writes a project tree — exercising it from the smoke script is the right level. A Go-test cross-phase shim is omitted (the smoke is the integration surface).
- **Conformance:** N/A — no driver interface.
- **Concurrency / leak:** N/A — `harbor validate` is a one-shot CLI command; nothing long-lived ships. The §5 concurrent-reuse contract does not apply (no compiled artifact, no run loop).

## Smoke script additions

- Run `./bin/harbor validate examples/harbor.yaml` → assert exit 0.
- Run `./bin/harbor validate cmd/harbor/testdata/validate/missing-llm-provider.yaml` → assert exit 1.
- Assert the stderr body contains `llm.provider` (the affected field).
- Run `./bin/harbor validate --json cmd/harbor/testdata/validate/missing-llm-provider.yaml` → parse with `jq` (when available), assert `.code == "validation_failed"` and `.errors[0].category != ""`.
- Run `./bin/harbor validate /nonexistent.yaml` → assert exit 2 (internal error category).
- Cross-phase Phase 67 step: `./bin/harbor scaffold my-agent --output <tmpdir>` then `./bin/harbor validate <tmpdir>/...` → SKIP cleanly when `scaffold` still emits `not_implemented` (Phase 67 not merged).

## Coverage target

- `cmd/harbor`: ≥ 75% (master-plan target). The package is mostly subcommand bodies + the structured-error glue; the new validate body and its tests push it well past 75%.

## Dependencies

- Phase 63 (CLI skeleton, D-084) — provides cobra root + `CLIError` + `--json` flag.
- Phase 02 (Configuration loader, D-001) — provides `config.Load(ctx, path)` and `config.Validate` (the underlying primitive Phase 68 wraps).

## Risks / open questions

- File:line precision for *semantic* errors (where Phase 02's `validateXxx` functions emit a field path like `config.llm.provider`) — Phase 02 does not currently record the YAML AST position of each field. Phase 68 derives the line by parsing the YAML AST and looking up the dotted field path; when the field is absent (the error reason IS "must not be empty"), the line is reported as `0` and the message points the operator at the section root instead. This is a pragmatic trade-off, not a bug; a future plumb-through phase can thread token positions down through `internal/config`.
- Skills / agent-definition validation is not in scope. The CLI surface accepts file arguments today and uses the file extension + content to detect kind; only YAML configs are supported in Phase 68. A `--kind config|skill|agent-def` flag is forward-compatible.

## Glossary additions

- **Validation error category** — Harbor's stable taxonomy of `harbor validate` failure modes: `config.parse` (YAML parse / strict-decode failure), `config.semantic` (a Phase 02 `validateXxx` rule fired), `io.not_found` (the named file does not exist), `io.read` (read failure other than not-found). Pinned by D-088.
- **Validation file:line precision** — every error emitted by `harbor validate` carries `(file, line)` where `line >= 1` when the YAML AST resolves the field, or `line == 0` when the failure is "field missing" (no token to point at). The categorisation is stable across releases. Pinned by D-088.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A (no identity-bearing code path).
- [ ] **Reusable artifact concurrent-reuse test** — N/A. `harbor validate` is a one-shot CLI command; no long-lived compiled artifact is constructed.
- [ ] **Integration test for cross-subsystem seam** — covered by `scripts/smoke/phase-68.sh` (the binary smoke is the cross-phase integration; the Phase 67 step SKIPs cleanly when scaffold isn't merged, per §17.6).
- [ ] If new vocabulary: glossary updated — yes, two entries above.
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — no departure.
