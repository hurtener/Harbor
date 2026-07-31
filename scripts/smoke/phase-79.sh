#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 79 — performance benchmarks.
#
# Phase 79 adds no Protocol method, no REST endpoint and no CLI subcommand, so
# this script long carried a single documented SKIP arguing that the SKIP was
# legitimate. It was not: the phase shipped three artefacts, all three are
# load-bearing, and all three are assertable without a dev server. A guard
# whose only reachable outcome is SKIP asserts nothing (AGENTS.md §4.2 item 5).
#
# What is asserted here:
#   1. test/benchmarks/ compiles AND its Benchmark* functions are DISCOVERABLE
#      — one iteration each (`-benchtime=1x`), so this is a compile+discover
#      gate, not a timing measurement. Timing lives in `make bench` / the CI
#      job; a benchmark suite that stopped compiling is what silently rots.
#   2. scripts/perf/check-regression.sh is present AND executable — the CI job
#      invokes it with `bash`, but a non-executable gate script is the classic
#      way a perf gate turns into a no-op after a checkout/permissions change.
#   3. The `perf-regression` job exists in .github/workflows/ci.yml AND that
#      job invokes check-regression.sh. A benchmark budget whose CI job was
#      deleted is exactly the "perf baseline CI never reads" failure the
#      wave-v1.24 audit named.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Per-phase log path — the unit-tests batch runs concurrently.
LOG="${TMPDIR:-/tmp}/harbor-smoke-phase-79-bench.log"

# --- 1. benchmarks compile and are discoverable -------------------------------
#
# `go test -bench` exits 0 when it discovers NOTHING, so the exit code alone
# cannot tell "the suite ran" from "the suite is empty" — the log must show at
# least one `Benchmark…` result line.
if [ -d "test/benchmarks" ]; then
    if ! command -v go >/dev/null 2>&1; then
        skip 'phase 79: go toolchain not available'
    else
        bench_rc=0
        go test -run '^$' -bench=. -benchtime=1x ./test/benchmarks/... >"${LOG}" 2>&1 || bench_rc=$?
        if [ "${bench_rc}" -ne 0 ]; then
            fail "phase 79: test/benchmarks/ does not compile / run (go test exited ${bench_rc})"
            printf -- '--- go test output (tail) ---\n'
            tail -40 "${LOG}" | sed 's/^/    /'
        elif ! grep -qE '^Benchmark[A-Za-z0-9_]+' "${LOG}"; then
            fail 'phase 79: test/benchmarks/ compiled but reported NO Benchmark* results — the suite is empty or every benchmark was filtered out'
            printf -- '--- go test output (tail) ---\n'
            tail -40 "${LOG}" | sed 's/^/    /'
        else
            bench_n="$(grep -cE '^Benchmark[A-Za-z0-9_]+' "${LOG}")"
            ok "phase 79: test/benchmarks/ compiles and ${bench_n} Benchmark* result(s) are discoverable (-benchtime=1x)"
        fi
    fi
else
    skip 'phase 79: test/benchmarks/ absent (benchmark suite not yet implemented)'
fi

# --- 2. the regression gate script is present AND executable ------------------
PERF_GATE='scripts/perf/check-regression.sh'
if [ ! -f "${PERF_GATE}" ]; then
    fail "phase 79: ${PERF_GATE} is missing — the perf-regression gate has no script to run"
elif [ ! -x "${PERF_GATE}" ]; then
    fail "phase 79: ${PERF_GATE} exists but is NOT executable"
else
    ok "phase 79: ${PERF_GATE} is present and executable"
fi

# --- 3. the CI job exists AND invokes the gate --------------------------------
CI_WORKFLOW='.github/workflows/ci.yml'
if [ ! -f "${CI_WORKFLOW}" ]; then
    fail "phase 79: ${CI_WORKFLOW} is missing — the perf-regression job cannot exist"
else
    ci_job_ok=0
    grep -qE '^[[:space:]]{2}perf-regression:' "${CI_WORKFLOW}" && ci_job_ok=1
    ci_invokes=0
    grep -qF -- 'scripts/perf/check-regression.sh' "${CI_WORKFLOW}" && ci_invokes=1
    if [ "${ci_job_ok}" -eq 1 ] && [ "${ci_invokes}" -eq 1 ]; then
        ok "phase 79: the perf-regression CI job exists in ${CI_WORKFLOW} and invokes ${PERF_GATE}"
    elif [ "${ci_job_ok}" -ne 1 ]; then
        fail "phase 79: no 'perf-regression:' job in ${CI_WORKFLOW} — the benchmark budget is not read by CI"
    else
        fail "phase 79: the perf-regression job exists but no step invokes ${PERF_GATE} — the gate is inert"
    fi
fi

smoke_summary
