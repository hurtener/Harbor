#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 137 smoke — in-tree conformance worked example.
# (RFC §5.1–§5.3; docs/plans/phase-137-conformance-example.md; reaffirms D-210)
#
# The conformance suite (internal/protocol/conformance) is deliberately
# NOT externally importable, and RunSuite is *testing.T-bound — so the
# worked example is a `go test`-compiled harness wiring a custom Factory
# + RunSuite, NOT a runnable client binary. This smoke gates that the
# worked example exists, is genuinely a test harness, and COMPILES + RUNS
# the full suite green against its custom Factory.
#
# Assertion classes (all static / unit-test — no booted-server dependency):
#   1. The worked example exists: the package directory carries a doc.go
#      and the _test.go harness.
#   2. The harness wires a custom Factory + RunSuite (not just the
#      one-line NewDefaultFactory the cert page already shows).
#   3. The cert page points at the worked example.
#   4. EXECUTION GATE: `go test ./examples/protocol-clients/conformance-fork/...`
#      passes — the custom Factory assembles a real-driver stack and the
#      full suite runs green over it (a mis-wire fails, never silently).
#
# Conventions (AGENTS.md §4.2): missing surface → SKIP on pre-137 builds;
# once shipped, OK ≥ count(acceptance criteria covered), FAIL = 0.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

EX_DIR='examples/protocol-clients/conformance-fork'
HARNESS="${EX_DIR}/conformance_fork_test.go"

if [ ! -f "${HARNESS}" ]; then
    skip "phase 137: ${HARNESS} not yet present (phase not implemented)"
    smoke_summary
    exit 0
fi

# --- 1. The worked example exists -------------------------------------------

assert_file "${EX_DIR}/doc.go" 'phase 137: worked-example package doc present'
assert_file "${HARNESS}"       'phase 137: worked-example test harness present'

# --- 2. It wires a custom Factory + RunSuite --------------------------------

assert_grep_present 'conformance\.Factory' "${HARNESS}" \
    'phase 137: the harness wires a custom conformance.Factory'
assert_grep_present 'conformance\.RunSuite' "${HARNESS}" \
    'phase 137: the harness hands the custom Factory to RunSuite'
assert_grep_present 'conformance\.Stack' "${HARNESS}" \
    'phase 137: the custom Factory returns a *conformance.Stack'
# The point is a CUSTOM assembly, not the one-line default the cert page
# already shows — the harness builds its own surface + mux + validator.
assert_grep_present 'transports\.NewMux' "${HARNESS}" \
    'phase 137: the custom Factory assembles its own wire mux (a real fork seam)'
assert_grep_present 'auth\.NewValidator' "${HARNESS}" \
    'phase 137: the custom Factory assembles its own JWT validator'

# --- 3. The cert page points at the worked example --------------------------

CERT_PAGE='docs/site/protocol/conformance-certification.md'
assert_grep_present 'conformance-fork' "${CERT_PAGE}" \
    'phase 137: the certification page points at the in-tree worked example'

# --- 4. EXECUTION GATE — the worked example compiles + the suite runs green --

if ! command -v go >/dev/null 2>&1; then
    skip 'phase 137: go toolchain unavailable — execution gate skipped'
    smoke_summary
    exit 0
fi

if go test "./${EX_DIR}/..." -count=1 -timeout 300s >/dev/null 2>&1; then
    ok 'phase 137: EXECUTION GATE green — the custom Factory compiles + RunSuite passes over it'
else
    fail "phase 137: the worked example failed (run: go test ./${EX_DIR}/... for detail)"
fi

smoke_summary
