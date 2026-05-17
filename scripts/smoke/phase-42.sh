#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 42 smoke — planner interface + Decision sum + RunContext.
#
# Phase 42 is a PURE CODE phase: it ships the Planner interface, the
# Decision sum-type, RunContext, the views, a stub finish.Planner, and
# the conformance harness skeleton + import-graph lint. None of those
# surfaces are reachable through the Protocol — the runtime executor
# that DRIVES a planner end-to-end is a later phase that has its own
# protocol surface and its own smoke check.
#
# The §4.2 convention: a Phase with no protocol surface ships a
# skip-shaped smoke. Coverage of the planner package's correctness is
# the test suite (internal/planner/... — including the §13 import-
# graph lint), not the smoke runner.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 42: planner package is pure types + stub + conformance skeleton; the runtime executor that exposes a protocol surface lands at a later phase. Correctness is gated by go test ./internal/planner/... (including the §13 import-graph lint in internal/planner/conformance/importgraph_test.go)."

smoke_summary
