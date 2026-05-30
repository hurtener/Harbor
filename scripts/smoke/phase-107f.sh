#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 107f smoke — session artifact manifest + provenance canonicalisation.
# Exercises the planner-side <session_artifacts> render + provenance resolver
# and the Protocol-side source projection. No live server needed.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# run_filtered_tests <desc> <run-regexp> <packages...>
#
# Runs `go test -run <regexp>` over the given packages. OK on a real
# pass; SKIP when the filter matched no tests (the phase not yet landed,
# so the preflight gate stays green); FAIL on a genuine test failure
# (never masked).
run_filtered_tests() {
    local desc="$1" runre="$2"
    shift 2
    local out rc
    out="$(CGO_ENABLED=0 go test -count=1 -run "${runre}" "$@" 2>&1)" && rc=0 || rc=$?
    if [ "${rc}" -eq 0 ]; then
        if printf '%s\n' "${out}" | grep -qE 'no tests to run|no test files'; then
            skip "${desc}: filter '${runre}' matched no tests (phase not yet landed)"
        else
            ok "${desc}"
        fi
        return
    fi
    printf '%s\n' "${out}" | tail -20
    fail "${desc}: go test exited ${rc}"
}

# 1. The planner-side <session_artifacts> render + ResolveProvenance
#    resolver + BuildArtifactManifest ordering (AC-2/3/4/6/5).
run_filtered_tests \
    "phase 107f: manifest render + provenance resolver (planner)" \
    'SessionArtifact|Manifest|Provenance' \
    ./internal/planner/ ./internal/planner/react/

# 2. The Protocol-side source-discriminator projection — tool/flow
#    artifacts no longer project a blank source (AC-5).
run_filtered_tests \
    "phase 107f: artifact source provenance projection (protocol)" \
    'ArtifactSource|ProjectRow|Provenance' \
    ./internal/protocol/

# 3. The run-loop manifest build + identity scoping + fail-soft
#    (AC-1/2/7).
run_filtered_tests \
    "phase 107f: run-loop manifest build + identity scoping (cmd/harbor)" \
    'ResolveSessionArtifacts|SessionArtifact' \
    ./cmd/harbor/

smoke_summary
