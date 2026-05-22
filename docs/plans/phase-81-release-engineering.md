# Phase 81 — release engineering (versioning, changelog)

## Summary

Phase 81 builds Harbor's release tooling: a build-time product-version
stamp wired into the `harbor` binary, a complete `CHANGELOG.md` covering
every V1 phase, a GitHub Actions release workflow triggered on a `v*`
tag push, and a release dry-run that exercises the whole path without a
tag. It is the last build phase before the `v1.0.0` cut (Phase 82) and
ships the tooling Phase 82 consumes — Phase 81 creates no tag itself.

## RFC anchor

- RFC §12

## Briefs informing this phase

- brief 06

## Brief findings incorporated

- brief 06 §"devx" framing: the `harbor` binary is the single operator
  entry point and `harbor version` is its build-information surface;
  this phase makes `harbor version` report a real release semver
  instead of the `v0.0.0-dev` placeholder. The brief's devx emphasis —
  the CLI is the operator's contact surface — is why the release
  version is stamped into the binary itself rather than carried out of
  band.
- brief 06 emphasises a dependency-light, stdlib-grounded toolchain:
  this phase deliberately uses `go build` + a shell script + a GitHub
  Actions workflow rather than a heavyweight release framework
  (goreleaser), matching CLAUDE.md §13's "no heavy frameworks" rule.

Release / packaging is not a primary subsystem in `docs/research/INDEX.md`
— brief 06 (events, observability, devx) is the closest informing brief
because the CLI and the `harbor version` surface fall under devx. No
brief covers semver tagging or changelog policy directly; this phase's
design is anchored in RFC §12 and the master-plan detail block, which is
the correct authority for a release-engineering phase.

## Findings I'm departing from (if any)

None.

## Goals

- Make `harbor version` report a real product release version, derived
  from the git tag at release-build time, stamped into the binary via
  `-ldflags -X`.
- Keep the product release version strictly distinct from the Harbor
  Protocol version (`internal/protocol/types.ProtocolVersion`).
- Ship a genuine, complete `CHANGELOG.md` covering all V1 phases.
- Ship a GitHub Actions release workflow that, on a `v*` tag push,
  builds the CGo-free static binary and produces a release artifact
  (binary + checksum) with SLSA-style build provenance.
- Ship a release dry-run that exercises the release build end-to-end
  without pushing a tag.

## Non-goals

- Creating any `v*` git tag. Tagging is the operator's job in Phase 82.
- The `v1.0.0` cut itself, release notes for a specific version, and
  the announcement scaffold — all Phase 82.
- Cross-platform release matrices, package-manager distribution
  (Homebrew, apt, container images), or signed installers — post-V1.
- A heavyweight release framework (goreleaser and similar). The
  stdlib + `go build` + a GitHub Actions workflow surface is the
  deliberate, dependency-light choice (CLAUDE.md §13).

## Acceptance criteria

- [ ] `harbor version` reports the product release version; an
      un-stamped build reports the `v0.0.0-dev` sentinel.
- [ ] The product release version is distinct from
      `types.ProtocolVersion` and documented as such.
- [ ] `CHANGELOG.md` exists at the repo root, follows the
      Keep-a-Changelog format, and covers all V1 phases (01–81 plus the
      lettered phases).
- [ ] `.github/workflows/release.yml` triggers on a `v*` tag push and
      builds a release artifact — so `git tag v1.0.0-rc.1` (pushed)
      produces a release artifact.
- [ ] `make release-dryrun` builds the release artifact, verifies the
      checksum, and confirms the stamped binary reports the stamped
      version — without a tag.
- [ ] SLSA-style build provenance is attached to the release artifact
      (the master-plan stretch; landed via GitHub's native attestor).
- [ ] `scripts/smoke/phase-81.sh` passes with OK ≥ acceptance count and
      FAIL = 0.

## Files added or changed

```text
cmd/harbor/root.go               # HarborVersion: const → var (link-time stampable)
cmd/harbor/cmd_version.go        # header doc: product-vs-Protocol version distinction
CHANGELOG.md                     # new — Keep-a-Changelog covering all V1 phases
scripts/release-build.sh         # new — the version-stamping release build (single source)
scripts/release-dryrun.sh        # new — exercise the release build without a tag
scripts/smoke/phase-81.sh        # new — static-only smoke
.github/workflows/release.yml    # new — v*-tag release workflow + workflow_dispatch dry-run
Makefile                         # new release-build / release-dryrun targets
docs/plans/phase-81-release-engineering.md  # this plan
docs/plans/README.md             # Phase 81 row Pending → Shipped
docs/decisions.md                # D-139
docs/glossary.md                 # release-version vocabulary
README.md                        # Status table row + release-process pointer
```

## Public API surface

No new exported Go symbols. `cmd/harbor.HarborVersion` changes from a
`const` to a `var` (same name, same type, same default value) so the
release build can stamp it at link time via
`-ldflags -X 'main.HarborVersion=…'`. The shell-level surface other
phases depend on:

- `scripts/release-build.sh [OUTPUT_DIR]` — builds the version-stamped
  static artifact + a SHA-256 checksum. Honours `HARBOR_RELEASE_VERSION`
  and the standard `GOOS` / `GOARCH` cross-build knobs.
- `scripts/release-dryrun.sh` — runs the release build with a synthetic
  version and asserts the artifact, checksum, and version stamp.
- `make release-build` / `make release-dryrun` — the Makefile entry
  points.

## Test plan

- **Unit:** the existing `cmd/harbor/cmd_version_test.go` already pins
  the `harbor version` human + `--json` renderings and that the output
  contains `HarborVersion`. Because `HarborVersion` is now a `var`
  rather than a `const`, the test continues to pass unchanged (it reads
  the symbol, it does not assume const-ness).
- **Integration:** `scripts/release-dryrun.sh` is the binding
  functional test (the master-plan "release dry-run"). It runs the
  exact `scripts/release-build.sh` path the release workflow runs,
  forcing a synthetic version, and asserts: the artifact + checksum
  exist; the checksum verifies; the stamped binary's `harbor version`
  reports the stamped string (human + JSON); and an un-stamped
  `go build` still reports the `v0.0.0-dev` sentinel (proving the stamp
  is opt-in, never silently applied).
- **Conformance:** N/A — Phase 81 ships no driver-shaped subsystem.
- **Concurrency / leak:** N/A — Phase 81 builds no reusable runtime
  artifact (engine / tool / planner / driver / redactor / client /
  catalog). The release scripts are one-shot build tooling, not a
  shared, concurrently-invoked runtime object; D-025 does not apply.

## Smoke script additions

`scripts/smoke/phase-81.sh` (`static-only`) asserts:

- `CHANGELOG.md` exists, follows Keep-a-Changelog, has an Unreleased
  section, and covers both the first and last V1 phase waves.
- `scripts/release-build.sh` and `scripts/release-dryrun.sh` exist; the
  build script stamps `main.HarborVersion` via `-ldflags -X` and is
  CGo-free.
- The `release-build` and `release-dryrun` Makefile targets exist.
- `.github/workflows/release.yml` exists, triggers on a `v*` tag, has a
  `workflow_dispatch` dry-run path, and attaches build provenance.
- `HarborVersion` is a `var` (link-time stampable), not a `const`.

## Coverage target

`cmd/harbor`: no regression from the current level. Phase 81's
production-code change is a single `const → var` conversion of an
already-tested symbol; it adds no new branches. The release scripts are
shell tooling, exercised by `scripts/release-dryrun.sh` and the smoke,
not by Go coverage.

## Dependencies

All V1 phases (01–80, including the lettered phases). Phase 81's
CHANGELOG covers every shipped phase, and the release artifact is the
fully-assembled binary, so every prior phase is a soft dependency. The
hard build dependency is Phase 63 (the `harbor` CLI skeleton —
`HarborVersion` and the `harbor version` subcommand).

## Risks / open questions

- **Cross-phase CHANGELOG drift.** The CHANGELOG is hand-authored from
  the master plan and the README status table; a future phase that does
  not update it would drift. Mitigation: the CHANGELOG `[Unreleased]`
  section is the living record, and Phase 82 moves it under a dated
  `[1.0.0]` heading — the cut is the forcing function.
- **SLSA provenance and repository permissions.** The release workflow
  requests `id-token: write` / `attestations: write`. If a repository
  setting disallows attestations, that step fails. This is an operator
  configuration concern surfaced loudly (a failed workflow step), not a
  silent degradation — acceptable per CLAUDE.md §5.
- **The release workflow is only fully exercised on a real `v*` tag
  push**, which Phase 82 performs. Phase 81 verifies the build path via
  the `workflow_dispatch` dry-run job and `make release-dryrun`; the
  tag-triggered publish path is structurally identical and shares
  `scripts/release-build.sh`, so the dry-run de-risks it.

## Glossary additions

- **product release version** — the `harbor` binary's own semver,
  reported by `harbor version`, stamped at release-build time from the
  git tag. Distinct from the Harbor Protocol version.
- **release artifact** — the CGo-free static `harbor` binary plus its
  SHA-256 checksum, produced by `scripts/release-build.sh` and
  published by the release workflow on a `v*` tag push.

Both terms are added to `docs/glossary.md` in this PR.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes — N/A, no identity-scoped code changed.
- [x] If this phase builds a reusable artifact: concurrent-reuse test passes — N/A: Phase 81 builds no reusable runtime artifact; it ships release tooling (one-shot build scripts) and a docs file. See Test plan.
- [x] If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists — the release dry-run (`scripts/release-dryrun.sh`) is the end-to-end functional test of the release build path; it exercises the real `harbor` build and the real `harbor version` surface.
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above + decisions.md entry filed — N/A, no departure.
