#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 147 smoke — events conformance suite home + duplicated-scenario
# fold (D-277). docs/plans/phase-147-events-conformance-suite.md
#
# Assertions:
#   1. go test -race -count=1 ./internal/events/... — the suite via both
#      driver consumers + every remaining driver-specific test.
#   2. Both drivers invoke the suite: `conformancetest.Run` present in
#      internal/events/drivers/inmem/conformance_test.go AND
#      internal/events/drivers/durable/conformance_test.go.
#   3. The folded old test names are GONE from internal/events/drivers/
#      (spot list from the plan's fold mapping table).
#   4. The suite package imports NO concrete driver: no
#      `internal/events/drivers` import in
#      internal/events/conformancetest/conformancetest.go.
#   5. The fold did not over-reach: representative driver-specific tests
#      survived.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# 1. The full events package tree, including the suite via both driver
#    consumers, passes under -race.
# ----------------------------------------------------------------------------
if [ -d "internal/events/conformancetest" ]; then
    if go test -race -count=1 ./internal/events/... >/tmp/phase-147-events.log 2>&1; then
        ok "phase 147: go test -race ./internal/events/... passes (suite + both consumers + driver-specific tests)"
    else
        fail "phase 147: go test -race ./internal/events/... failed"
        printf '--- go test output ---\n'
        cat /tmp/phase-147-events.log
    fi
else
    skip "phase 147: internal/events/conformancetest not yet implemented"
fi

# ----------------------------------------------------------------------------
# 2. Both drivers invoke the shared suite.
# ----------------------------------------------------------------------------
assert_grep_present 'conformancetest\.Run' \
    "internal/events/drivers/inmem/conformance_test.go" \
    "phase 147: inmem consumer invokes conformancetest.Run"
assert_grep_present 'conformancetest\.Run' \
    "internal/events/drivers/durable/conformance_test.go" \
    "phase 147: durable consumer invokes conformancetest.Run"

# ----------------------------------------------------------------------------
# 3. The folded old test names are gone from internal/events/drivers/ —
#    the fold actually removed the duplication, not just added a suite
#    on top of it. Spot list drawn from the plan's fold-mapping table.
# ----------------------------------------------------------------------------
check_folded_name_absent() {
    local name="$1" hits
    hits=$(grep -rl -- "func ${name}(" internal/events/drivers/ 2>/dev/null || true)
    if [ -z "$hits" ]; then
        ok "phase 147: folded test ${name} removed from internal/events/drivers/"
    else
        fail "phase 147: folded test ${name} still present: ${hits}"
    fi
}

check_folded_name_absent "TestInmem_Fence_DropsAndEmptiesHistory"
check_folded_name_absent "TestInmem_Fence_AfterClose"
check_folded_name_absent "TestDurable_Fence_DropsLateEventsAndEmptiesHistory"
check_folded_name_absent "TestDurable_Fence_AfterClose"
check_folded_name_absent "TestInmem_Bounds_ReportsHeadTail"
check_folded_name_absent "TestDurable_Bounds_ReportsHeadAndTail"
check_folded_name_absent "TestPublish_RejectsMissingIdentity"
check_folded_name_absent "TestSubscribe_RejectsEmptyTripleNonAdmin"
check_folded_name_absent "TestDurable_Subscribe_RejectsEmptyTripleNonAdmin"
check_folded_name_absent "TestReplay_RejectsEmptyFilter"
check_folded_name_absent "TestDurable_Replay_RejectsEmptyTripleNonAdmin"
check_folded_name_absent "TestPublish_CrossTenantIsolation"
check_folded_name_absent "TestReplay_CrossTenant_Isolation"
check_folded_name_absent "TestDurable_Replay_CrossSessionIsolation"
check_folded_name_absent "TestReplay_HeadCursor_ReturnsNilNil"
check_folded_name_absent "TestDurable_ReplayCursorAtHead_ReturnsNil"
check_folded_name_absent "TestReplay_DisabledByConfig_ErrReplayUnavailable"
check_folded_name_absent "TestDurable_NoStateStore_RingZero_ReplayUnavailable"
check_folded_name_absent "TestReplay_RingOverrun_ErrCursorTooOld"
check_folded_name_absent "TestDurable_BestEffort_CursorTooOld"
check_folded_name_absent "TestInmem_Bounds_NoMatch_ErrNoHistory"
check_folded_name_absent "TestDurable_Bounds_EmptySession_ErrNoHistory"
check_folded_name_absent "TestInmem_Bounds_EmptyTriple_ErrIdentityScopeRequired"
check_folded_name_absent "TestDurable_Bounds_EmptyTriple_ErrIdentityScopeRequired"
check_folded_name_absent "TestInmem_Window_TailFirst_OldestFirst"
check_folded_name_absent "TestDurable_Window_TailFirst_MostRecentK_OldestFirst"
check_folded_name_absent "TestInmem_Window_BeforeCursor"
check_folded_name_absent "TestDurable_Window_BeforeCursor_ScrollsUp"
check_folded_name_absent "TestDurable_Window_EmptySession_Nil"
check_folded_name_absent "TestDurable_Window_ReachesHead"
check_folded_name_absent "TestInmem_Window_ReplayDisabled_ErrReplayUnavailable"
check_folded_name_absent "TestPublish_AfterClose_ReturnsBusClosed"
check_folded_name_absent "TestReplay_AfterClose_ErrBusClosed"
check_folded_name_absent "TestDurable_ClosedBus_RejectsOps"
check_folded_name_absent "TestClose_Idempotent"
check_folded_name_absent "TestDurable_Close_Idempotent"

# The two fence-only files are removed entirely (folded wholesale).
assert_file_absent() {
    local p="$1" desc="$2"
    if [ -f "$p" ]; then
        fail "${desc}: ${p} still present, want removed"
    else
        ok "${desc}: ${p} removed"
    fi
}
assert_file_absent "internal/events/drivers/inmem/inmem_fence_test.go" "phase 147: fence-only file folded away"
assert_file_absent "internal/events/drivers/durable/durable_fence_test.go" "phase 147: fence-only file folded away"

# ----------------------------------------------------------------------------
# 4. The suite package imports no concrete driver.
# ----------------------------------------------------------------------------
if [ -f "internal/events/conformancetest/conformancetest.go" ]; then
    assert_grep_absent 'internal/events/drivers' \
        "internal/events/conformancetest/conformancetest.go" \
        "phase 147: conformancetest.go imports no concrete driver"
else
    skip "phase 147: internal/events/conformancetest/conformancetest.go not yet implemented"
fi

# ----------------------------------------------------------------------------
# 5. The fold did not over-reach: representative driver-specific tests
#    survived (recovery/restart, OpenWith, D-025 reuse, ring truncation).
# ----------------------------------------------------------------------------
assert_grep_present 'func TestDurable_PublishAfterRestart_NoSequenceCollision' \
    "internal/events/drivers/durable/recovery_test.go" \
    "phase 147: durable recovery test survived the fold"
assert_grep_present 'func TestConcurrentReuse_DurableBus' \
    "internal/events/drivers/durable/concurrent_test.go" \
    "phase 147: durable D-025 reuse test survived the fold"
assert_grep_present 'func TestBus_ConcurrentReuse_ReuseContract' \
    "internal/events/drivers/inmem/inmem_test.go" \
    "phase 147: inmem D-025 reuse test survived the fold"
assert_grep_present 'func TestInmem_Bounds_WrappedRing_ReportsTruncated' \
    "internal/events/drivers/inmem/inmem_test.go" \
    "phase 147: inmem ring-truncation test survived the fold"
assert_grep_present 'func TestOpenWith_SharedStore_SurvivesBusReopen' \
    "internal/events/drivers/durable/openwith_test.go" \
    "phase 147: durable OpenWith test survived the fold"

smoke_summary
