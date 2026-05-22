#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 81 — release engineering (versioning, changelog).
#
# Phase 81 adds NO Protocol method and NO REST endpoint. It does add a
# release surface around the EXISTING `harbor version` subcommand
# (Phase 63): a build-time product-version stamp, a CHANGELOG, the
# release build + dry-run scripts, and the release GitHub Actions
# workflow. Per CLAUDE.md §4.2 the correct shape is a `static-only`
# smoke that asserts those static artifacts exist and are wired.
#
# `harbor version` itself is exercised by cmd/harbor's unit tests and
# the §4.2 rule-8 degradation path: a build that predates the version
# stamp still answers `harbor version` (it just reports v0.0.0-dev),
# so this smoke needs no live-server leg. The release dry-run
# (`make release-dryrun`) is the binding functional test — run
# directly and by the release workflow's workflow_dispatch path.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# CHANGELOG.
# ----------------------------------------------------------------------------
assert_file "CHANGELOG.md" "CHANGELOG.md exists at the repo root"
assert_grep_present 'Keep a Changelog' "CHANGELOG.md" \
    "CHANGELOG follows the Keep-a-Changelog format"
# The v1.0.0 cut rolled the CHANGELOG to a dated [1.0.0] section.
assert_grep_present '\[1\.0\.0\]' "CHANGELOG.md" \
    "CHANGELOG carries a [1.0.0] release section"
# The CHANGELOG must cover the V1 surface — assert the first and last
# subsystem groups are both present. Public release surfaces describe
# the product in feature terms, not internal phase numbering (D-142),
# so these assert subsystem content, not "phases NN–MM".
assert_grep_present 'Identity & isolation triple' "CHANGELOG.md" \
    "CHANGELOG covers the identity / foundation subsystems"
assert_grep_present 'conformance harness' "CHANGELOG.md" \
    "CHANGELOG covers the conformance-harness / release-hardening work"

# ----------------------------------------------------------------------------
# Release tooling — the version-stamping single source + the dry-run.
# ----------------------------------------------------------------------------
assert_file "scripts/release-build.sh" "release build script exists"
assert_file "scripts/release-dryrun.sh" "release dry-run script exists"
assert_grep_present "main.HarborVersion=" "scripts/release-build.sh" \
    "release build stamps main.HarborVersion via -ldflags -X"
assert_grep_present 'CGO_ENABLED=0' "scripts/release-build.sh" \
    "release build is CGo-free (static binary)"
assert_grep_present '^release-build:' "Makefile" \
    "Makefile carries the release-build target"
assert_grep_present '^release-dryrun:' "Makefile" \
    "Makefile carries the release-dryrun target"

# ----------------------------------------------------------------------------
# The release workflow.
# ----------------------------------------------------------------------------
assert_file ".github/workflows/release.yml" "release workflow exists"
assert_grep_present "'v\\*'" ".github/workflows/release.yml" \
    "release workflow triggers on a v* tag push"
assert_grep_present 'workflow_dispatch' ".github/workflows/release.yml" \
    "release workflow has a workflow_dispatch dry-run path"
assert_grep_present 'attest-build-provenance' ".github/workflows/release.yml" \
    "release workflow attaches SLSA-style build provenance"

# ----------------------------------------------------------------------------
# The version stamp wiring — HarborVersion is a var (link-time
# stampable), not a const, and `harbor version` reads it.
# ----------------------------------------------------------------------------
assert_grep_present 'var HarborVersion' "cmd/harbor/root.go" \
    "HarborVersion is a var so -ldflags -X can stamp it"
assert_grep_absent 'const HarborVersion' "cmd/harbor/root.go" \
    "HarborVersion is not a const (a const cannot be -X stamped)"

smoke_summary
