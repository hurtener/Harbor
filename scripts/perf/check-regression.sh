#!/usr/bin/env bash
# Phase 79 (D-136) — the perf-regression gate.
#
# Runs the Harbor benchmark suite, compares the fresh numbers against
# the committed baseline (docs/perf/baseline.txt) via `benchstat`, and
# exits non-zero when any benchmark regresses beyond the threshold.
#
# Design (see docs/plans/phase-79-performance-benchmarks.md + D-136):
#   - benchstat is a dev/CI-only tool. It is invoked via `go run` with
#     a PINNED version — it never enters the runtime binary's go.mod
#     production require surface.
#   - Shared CI runners are noisy. The gate absorbs jitter three ways:
#       (a) `-count` runs each benchmark N times so benchstat has a
#           sample to compute variance from;
#       (b) benchstat reports a `~` verdict (no statistically-
#           significant change) in its `vs base` column — a `~` is a
#           PASS regardless of the raw delta;
#       (c) the threshold is generous (default 30%, see below) — only
#           a statistically-significant slowdown past it fails.
#   - The baseline is updated DELIBERATELY by a human in a reviewed PR
#     (`make bench > docs/perf/baseline.txt`); it is never auto-refreshed.
#
# Env knobs (all optional):
#   PERF_THRESHOLD    regression threshold, percent      (default: 30)
#   PERF_COUNT        benchmark repetitions for sampling (default: 6)
#   PERF_BENCHTIME    `go test -benchtime` value         (default: 100ms)
#   BENCHSTAT_VERSION pinned benchstat module version    (default below)
#
# Threshold calibration: the master plan's "> 10% slowdown blocks"
# is the INTENT, but Go microbenchmarks of the concurrent engine /
# bus paths legitimately swing ±20-30% run-to-run on a shared,
# noisy-neighbour CI runner (measured — see
# docs/plans/phase-79-performance-benchmarks.md §Risks + D-136). A
# 10% gate would flake constantly. The default is therefore 30% —
# generous enough to absorb shared-runner jitter (the master plan's
# explicit "design the gate to tolerate noise" requirement) while
# still catching the regression class that matters: a refactor that
# halves throughput or doubles latency. The full benchstat report is
# ALWAYS printed, so a reviewer can still eyeball a sub-threshold
# drift. A stricter gate is opt-in via PERF_THRESHOLD on a quiet
# dedicated machine.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# The base to regress against. CI sets PERF_BASE_FILE to a benchmark
# run generated on the SAME runner from the PR's base commit — the only
# hardware-noise-immune comparison (Go embeds GOMAXPROCS in benchmark
# names, so a committed baseline produced on different-core-count
# hardware cannot be paired by benchstat). With PERF_BASE_FILE unset
# (local `make bench-check`), the committed baseline is used — valid
# only on the machine that generated it.
BASELINE="${PERF_BASE_FILE:-docs/perf/baseline.txt}"
THRESHOLD="${PERF_THRESHOLD:-30}"
COUNT="${PERF_COUNT:-6}"
BENCHTIME="${PERF_BENCHTIME:-100ms}"
# golang.org/x/perf ships no tagged semver releases — the pin is a
# module pseudo-version. Bump deliberately when a benchstat fix is
# needed; `go run` resolves it into the module cache without ever
# touching this repo's go.mod / go.sum.
BENCHSTAT_VERSION="${BENCHSTAT_VERSION:-v0.0.0-20260512194132-3cf34090a3db}"
BENCHSTAT_PKG="golang.org/x/perf/cmd/benchstat@${BENCHSTAT_VERSION}"

if [ ! -f "${BASELINE}" ]; then
  echo "perf-gate: ERROR — baseline ${BASELINE} missing." >&2
  echo "perf-gate: regenerate it with 'make bench > ${BASELINE}' and commit." >&2
  exit 1
fi

REPORT="$(mktemp -t harbor-perf-report.XXXXXX)"
CSV="$(mktemp -t harbor-perf-csv.XXXXXX)"

# The PR-side benchmark run. CI pre-generates it (on the same runner as
# the base) and passes it via PERF_PR_FILE; locally it is produced here.
if [ -n "${PERF_PR_FILE:-}" ]; then
  if [ ! -s "${PERF_PR_FILE}" ]; then
    echo "perf-gate: ERROR — PERF_PR_FILE ${PERF_PR_FILE} missing or empty." >&2
    exit 1
  fi
  NEW="${PERF_PR_FILE}"
  trap 'rm -f "${REPORT}" "${CSV}"' EXIT
  echo "perf-gate: using pre-generated PR benchmark file ${NEW}"
else
  NEW="$(mktemp -t harbor-perf-new.XXXXXX)"
  trap 'rm -f "${NEW}" "${REPORT}" "${CSV}"' EXIT
  echo "perf-gate: running benchmark suite (count=${COUNT}, benchtime=${BENCHTIME})..."
  go test -run='^$' -bench=. -benchmem \
    -count="${COUNT}" -benchtime="${BENCHTIME}" \
    ./test/benchmarks/... | tee "${NEW}"
fi

echo
echo "perf-gate: comparing against ${BASELINE} via benchstat (${BENCHSTAT_VERSION})..."
# Human-readable report for the operator.
go run "${BENCHSTAT_PKG}" "${BASELINE}" "${NEW}" | tee "${REPORT}"
# Machine-readable CSV for the gate decision. benchstat's CSV groups
# rows by metric: a header row `,file,CI,file,CI,vs base,P` precedes
# each metric block; the `vs base` column is `~` (non-significant) or
# a signed percentage like `+12.34%`.
go run "${BENCHSTAT_PKG}" -format csv "${BASELINE}" "${NEW}" > "${CSV}" 2>/dev/null

# Fail loudly if benchstat produced nothing — an empty CSV would make
# the parser below silently pass every benchmark (CLAUDE.md §5 "fail
# loudly"; a gate that green-lights on its own tooling failure is
# worse than no gate).
if [ ! -s "${CSV}" ] || ! grep -q 'vs base' "${CSV}"; then
  echo "perf-gate: ERROR — benchstat produced no comparable CSV output." >&2
  echo "perf-gate: the benchmark run or the baseline file is malformed." >&2
  exit 1
fi

echo
# Parse the CSV. For each metric block we track the unit (from the
# 2nd header column, e.g. `sec/op` or `turns/sec`) and the `vs base`
# column index. A regression is:
#   - latency / alloc metric (sec/op, ns/op, B/op, allocs/op):
#     a POSITIVE delta past the threshold.
#   - throughput metric (anything ending /sec — envelopes/sec,
#     turns/sec, frames/sec): a NEGATIVE delta past the threshold.
# `~` rows are non-significant and always pass. `geomean` rows are
# aggregates and skipped.
FAIL="$(
  awk -F',' -v t="${THRESHOLD}" '
    # Header row: 2nd column names the metric unit; locate vs-base col.
    $2 ~ /\/op$|\/sec$/ {
      unit=$2; vb=0;
      for (i=1;i<=NF;i++) if ($i=="vs base") vb=i;
      next;
    }
    # Skip blank rows, the goos/goarch preamble, and geomean aggregates.
    $1=="" || $1 ~ /^geomean/ { next }
    # Data row: needs a vs-base column and a delta value present.
    {
      if (vb==0 || vb>NF) next;
      d=$vb;
      if (d=="~" || d=="") next;            # non-significant → pass
      sign=substr(d,1,1);
      mag=d; gsub(/[^0-9.]/,"",mag);
      if (mag+0 <= t+0) next;               # within threshold → pass
      if (unit ~ /\/sec$/) {
        # throughput: a drop (negative delta) is the regression.
        if (sign=="-")
          printf "REGRESSION %s %s slowed (throughput %s)\n", $1, d, unit;
      } else {
        # latency/alloc: a rise (positive delta) is the regression.
        if (sign=="+")
          printf "REGRESSION %s %s slower (%s)\n", $1, d, unit;
      }
    }
  ' "${CSV}"
)"

if [ -n "${FAIL}" ]; then
  echo "${FAIL}" >&2
  echo
  echo "perf-gate: FAIL — one or more benchmarks regressed past ${THRESHOLD}%." >&2
  echo "perf-gate: if the change is intentional, regenerate the baseline" >&2
  echo "perf-gate:   make bench > ${BASELINE}" >&2
  echo "perf-gate: and commit it in this PR with reviewer sign-off." >&2
  exit 1
fi

echo "perf-gate: OK — no benchmark regressed past ${THRESHOLD}% (benchstat '~' verdicts are non-significant)."
