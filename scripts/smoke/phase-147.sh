#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 147 smoke — events conformance suite home + duplicated-scenario
# fold (D-277). docs/plans/phase-147-events-conformance-suite.md
#
# Planned assertions (flip from skeleton when the phase implements its
# surface):
#   1. go test -race -count=1 ./internal/events/... — the suite via both
#      driver consumers + every remaining driver-specific test.
#   2. Both drivers invoke the suite: `conformancetest.Run` present in
#      internal/events/drivers/inmem/conformance_test.go AND
#      internal/events/drivers/durable/conformance_test.go.
#   3. The folded old test names are GONE from internal/events/drivers/
#      (spot list from the plan's fold mapping table, e.g.
#      TestInmem_Fence_DropsAndEmptiesHistory,
#      TestDurable_Fence_DropsLateEventsAndEmptiesHistory,
#      TestInmem_Bounds_ReportsHeadTail, TestDurable_Close_Idempotent).
#   4. The suite package imports NO concrete driver: no
#      `internal/events/drivers` import in
#      internal/events/conformancetest/conformancetest.go.
#   5. The fold did not over-reach: representative driver-specific tests
#      survived (TestDurable_PublishAfterRestart_NoSequenceCollision,
#      TestConcurrentReuse_DurableBus, TestBus_ConcurrentReuse_ReuseContract).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 147: smoke skeleton — replace with the real assertions when the events conformance suite lands"

smoke_summary
