#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 80 — documentation hygiene polish.
#
# Phase 80 adds NO Protocol method, NO REST endpoint, and NO CLI
# subcommand. Its deliverables are documentation hygiene: a `revive`
# lint gate that is actually enforced in CI, worked examples under
# examples/, and recipe how-to docs under docs/recipes/. There is
# therefore no live-server surface — per CLAUDE.md §4.2 the correct
# shape is a documented `static-only` smoke that asserts the static
# artifacts exist and are wired.
#
# The lint gate itself (`make lint-revive`) and the example build
# (`go build ./examples/...`) are exercised by the CI `lint` and
# `examples` jobs respectively.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# The revive doc-hygiene lint gate.
# ----------------------------------------------------------------------------
assert_file ".golangci-revive.yml" "revive-only lint config exists"
assert_grep_present 'disableStutteringCheck' ".golangci-revive.yml" \
    "revive exported rule disables the stutter naming sub-check"
assert_grep_present 'package-comments' ".golangci-revive.yml" \
    "revive package-comments rule is enabled"
assert_grep_present '^lint-revive:' "Makefile" \
    "Makefile carries the lint-revive target"
assert_grep_present 'make lint-revive' ".github/workflows/ci.yml" \
    "CI lint job runs the revive doc-hygiene gate"
assert_grep_present 'examples build' ".github/workflows/ci.yml" \
    "CI carries the examples build/test job"

# ----------------------------------------------------------------------------
# Worked examples.
# ----------------------------------------------------------------------------
assert_file "examples/README.md" "examples index exists"
assert_file "examples/agents/echo/echo.go" "worked example agent exists"
assert_file "examples/agents/echo/echo_test.go" "worked example agent test exists"
assert_file "examples/tools/weather/weather.go" "worked example tool exists"
assert_file "examples/tools/weather/weather_test.go" "worked example tool test exists"

# ----------------------------------------------------------------------------
# Recipe docs.
# ----------------------------------------------------------------------------
assert_file "docs/recipes/README.md" "recipes index exists"
assert_file "docs/recipes/define-a-tool.md" "define-a-tool recipe exists"
assert_file "docs/recipes/configure-a-planner.md" "configure-a-planner recipe exists"
assert_file "docs/recipes/run-harbor-dev.md" "run-harbor-dev recipe exists"
assert_file "docs/recipes/test-an-agent.md" "test-an-agent recipe exists"
assert_file "docs/recipes/scaffold-an-agent.md" "scaffold-an-agent recipe exists"

smoke_summary
