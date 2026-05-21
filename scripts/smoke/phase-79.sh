#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 79 — performance benchmarks.
#
# Phase 79 adds NO Protocol method, NO REST endpoint, and NO CLI
# subcommand. Its deliverable is a `go test -bench` suite under
# test/benchmarks/ plus a perf-regression gate (scripts/perf/
# check-regression.sh) wired into CI as the `perf-regression` job.
#
# There is therefore no live-server surface for this smoke script to
# assert. Per AGENTS.md §4.2 the correct shape for a benchmark-only
# phase is a documented SKIP — and this genuinely IS a SKIP, not a
# "SKIP that should be an OK": the benchmark suite is exercised by
# `make bench` / `make bench-check` and the `perf-regression` CI job,
# not by the preflight HTTP gate.
#
# The benchmark suite's own correctness is gated by `go test` (the
# standard-library testing harness compiles + runs `Benchmark*`
# functions); the regression gate is exercised by CI.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 79: benchmark-only phase — no Protocol/REST/CLI surface; see test/benchmarks/ + the perf-regression CI job"

smoke_summary
