#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 83k — Console release embed: `make build` rebuilds the
# Console bundle first; the release pipeline rebuilds before
# `go build`; the placeholder copy points operators at the rebuild
# command + `go install` workaround. D-157.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Makefile — `build` depends on `console-build`; `build-fast` exists.
# ----------------------------------------------------------------------------
assert_grep_present '^build: console-build' "Makefile" \
    "make build now depends on console-build (Phase 83k)"
assert_grep_present '^build-fast:' "Makefile" \
    "make build-fast target exists for iterative dev (Phase 83k)"

# ----------------------------------------------------------------------------
# Release script rebuilds Console before `go build`.
# ----------------------------------------------------------------------------
assert_grep_present 'make console-build' "scripts/release-build.sh" \
    "release-build.sh rebuilds the Console bundle (Phase 83k)"

# ----------------------------------------------------------------------------
# Staleness gate script ships + executable.
# ----------------------------------------------------------------------------
assert_file "scripts/check-console-bundle.sh" \
    "Console-bundle staleness gate script ships"
if [ -x scripts/check-console-bundle.sh ]; then
    ok "scripts/check-console-bundle.sh is executable"
else
    fail "scripts/check-console-bundle.sh is not executable"
fi

# ----------------------------------------------------------------------------
# CI workflow wires the staleness gate.
# ----------------------------------------------------------------------------
assert_grep_present 'check-console-bundle.sh' ".github/workflows/ci.yml" \
    "CI's frontend-e2e job runs the staleness gate"

# ----------------------------------------------------------------------------
# Placeholder copy — refreshed for go install + harbor init pointers.
# ----------------------------------------------------------------------------
assert_grep_present 'If you ran <code>go install</code>' \
    "cmd/harbor/cmd_console.go" \
    "placeholder page explains the go install workaround (Phase 83k)"
assert_grep_present 'harbor init' "cmd/harbor/cmd_console.go" \
    "placeholder page points to harbor init for first-time operators"
assert_grep_present 'docs/CONFIG.md' "cmd/harbor/cmd_console.go" \
    "placeholder page links to docs/CONFIG.md"

smoke_summary
