#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 39 — Virtual directory subsystem.
#
# Phase 39 ships `Directory(cfg)` + `pinned_then_recent` /
# `pinned_then_top` selectors on top of the Phase 37 `SkillStore`.
# The directory reuses Phase 38's `tools.Filter` + `tools.Redact`
# primitives; no new redactor / filter code lands here.
#
# Phase 39 has no Protocol surface — Phase 60+ exposes the Console
# projection. Smoke runs the Go-level test surface (unit + property +
# D-025 concurrent-reuse) under -race and pins the two Selection
# constant strings so a silent rename surfaces here, not downstream.
#
# Conventions (CLAUDE.md §4.2):
#   - 404/405/501 → SKIP (no Protocol surface yet).
#   - The Go test surface MUST pass — that's the smoke-observable boundary at this phase.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 120s ./internal/skills/... >/dev/null 2>&1; then
    ok 'phase 39: internal/skills tests pass under -race (Directory + property + D-025 + Phase 37/38 surface)'
else
    fail 'phase 39: internal/skills tests failed (run `go test -race ./internal/skills/...` for detail)'
fi

# Pin the two Selection constant strings — a silent rename here would
# break every operator config + Console projection downstream. Grep on
# the registration constants.
if grep -q 'SelectionPinnedThenRecent *Selection *= *"pinned_then_recent"' internal/skills/directory.go \
    && grep -q 'SelectionPinnedThenTop *Selection *= *"pinned_then_top"' internal/skills/directory.go; then
    ok 'phase 39: selector constants pinned (pinned_then_recent / pinned_then_top)'
else
    fail 'phase 39: selector constants drifted — expected pinned_then_recent / pinned_then_top in internal/skills/directory.go'
fi

# Pin the MaxEntries bounds — the brief 04 §3 contract is default=30,
# range [1, 200]. A silent change to these bounds is a brief departure
# that must land in D-052, not as a silent constant edit.
if grep -q 'DefaultMaxEntries *= *30' internal/skills/directory.go \
    && grep -q 'MinMaxEntries *= *1' internal/skills/directory.go \
    && grep -q 'MaxMaxEntries *= *200' internal/skills/directory.go; then
    ok 'phase 39: MaxEntries bounds pinned (default=30, min=1, max=200)'
else
    fail 'phase 39: MaxEntries bounds drifted — expected default=30, min=1, max=200 in internal/skills/directory.go'
fi

skip "phase 39: directory subsystem has no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
