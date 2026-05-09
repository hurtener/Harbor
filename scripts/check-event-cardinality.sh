#!/usr/bin/env bash
# Cardinality lint for event-derived metric labels.
#
# In Phase 56 (metrics derivation) this script becomes binding: it
# fails the build if `slog.String("run_id", ...)` or
# `slog.String("trace_id", ...)` patterns appear inside metric-emit
# code paths — labels keyed on those identifiers explode metric
# cardinality and saturate Prometheus / OTel collectors.
#
# Phase 05 ships the slot. There is no metric-emit code yet, so the
# script just confirms that:
#
#   1. `internal/metrics/` does not yet exist (or is empty); AND
#   2. The forbidden patterns do not appear in `internal/events/`.
#
# When Phase 56 lands, harden this script to scan `internal/metrics/`
# and any other metric-emit packages.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"

FAIL=0

# Defensive: forbid run_id / trace_id metric labels INSIDE the events
# package even before Phase 56 — events.Event.Extra is reserved for
# bounded low-cardinality labels per the Phase 05 plan.
if grep -rE 'slog\.String\("(run_id|trace_id)"' internal/events 2>/dev/null; then
    echo "FAIL: high-cardinality metric label found inside internal/events"
    FAIL=$((FAIL + 1))
fi

if [ -d internal/metrics ]; then
    if grep -rE 'slog\.String\("(run_id|trace_id)"' internal/metrics 2>/dev/null; then
        echo "FAIL: high-cardinality metric label found inside internal/metrics"
        FAIL=$((FAIL + 1))
    fi
fi

if [ "${FAIL}" -gt 0 ]; then
    echo "check-event-cardinality: ${FAIL} violation(s)"
    exit 1
fi

echo "check-event-cardinality: OK (Phase 56 will harden this check)"
exit 0
