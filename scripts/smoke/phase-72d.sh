#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 72d — `notification.*` event topic + rules-engine-lite mapper.
#
# This phase ships a NEW event family on the typed bus (per-class topic
# naming locked per docs/plans/wave-13-decomposition.md §12 + D-109)
# plus a runtime-internal mapper that synthesises `notification.*`
# events from a small subset of the existing event taxonomy
# (`task.failed`, `tool.approval_requested`, `governance.budget_exceeded`,
# `tool.auth_required`, `pause.requested`).
#
# Phase 72d adds NO new HTTP/Protocol routes — the topic is consumed
# via the existing `events.subscribe` surface (which lands in Phase 60 +
# Phase 72 + Phase 72a). The §13 primitive-with-consumer rule is
# satisfied by the Stage-1 binding test consumer at
# `internal/runtime/notifications/subscriber_test.go::TestSubscriber_TaskFailedSynthesisesNotificationTaskFailed`,
# per `docs/plans/wave-13-decomposition.md` §12 item 5. The smoke runs
# that test + the integration mapping suite specifically so the
# preflight gate catches a regression the moment the package or its
# wiring drifts.
#
# Live-server smokes for the Protocol-side per-class subscribe shape
# land alongside Phase 72a / Phase 72 / Phase 60 — when the route
# ships, `events.subscribe` will accept a `notification.*` event-type
# filter and a smoke can flip from SKIP to OK there.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. Package present? Falls back to SKIP on pre-72d builds.
if [ ! -d "internal/runtime/notifications" ]; then
    skip "phase 72d: internal/runtime/notifications absent (package not yet implemented)"
    smoke_summary
    exit 0
fi

# 2. §13 BINDING test consumer — the Stage-1 round-trip per
#    docs/plans/wave-13-decomposition.md §12 item 5 + D-109. A FAIL
#    here means the mapper or subscriber wiring regressed.
if go test -race -run TestSubscriber_TaskFailedSynthesisesNotificationTaskFailed \
    ./internal/runtime/notifications/... >/tmp/phase-72d-binding.log 2>&1; then
    ok "phase 72d: §13 binding test consumer round-trip passes (task.failed → notification.task_failed via bus)"
else
    fail "phase 72d: §13 binding test consumer round-trip failed"
    printf '--- go test output ---\n'
    cat /tmp/phase-72d-binding.log
fi

# 3. Mapper unit tests — every V1 mapping plus the ErrUnmappable and
#    concurrent-reuse (N=100 under -race) assertions.
if go test -race -run TestMap ./internal/runtime/notifications/... >/tmp/phase-72d-mapper.log 2>&1; then
    ok "phase 72d: mapper unit tests pass (5 V1 mappings + unmapped + ErrUnmappable + concurrent reuse N=100)"
else
    fail "phase 72d: mapper unit tests failed"
    printf '--- go test output ---\n'
    cat /tmp/phase-72d-mapper.log
fi

# 4. Subscriber leak test — fail-loudly on a goroutine leak in the
#    long-lived Subscriber.
if go test -race -run TestSubscriber_Run_GoroutineLeak \
    ./internal/runtime/notifications/... >/tmp/phase-72d-leak.log 2>&1; then
    ok "phase 72d: Subscriber.Run goroutine-leak test passes (baseline restored after ctx cancel)"
else
    fail "phase 72d: Subscriber.Run goroutine-leak test failed"
    printf '--- go test output ---\n'
    cat /tmp/phase-72d-leak.log
fi

# 5. Integration suite — real bus + real audit redactor + real
#    Subscriber, all V1 mappings + the missing-identity failure mode +
#    the N=20 concurrency stress (§17.3).
if go test -race -run TestE2E_NotificationsTopic \
    ./test/integration/... >/tmp/phase-72d-integration.log 2>&1; then
    ok "phase 72d: integration suite passes (all V1 mappings round-trip + missing-identity fail-loud + N=20 concurrency stress)"
else
    fail "phase 72d: integration suite failed"
    printf '--- go test output ---\n'
    cat /tmp/phase-72d-integration.log
fi

# 6. Protocol-side surface probes — SKIP cleanly until the Protocol
#    layer (Phase 60 + Phase 72 + Phase 72a) ships. When those phases
#    land, the per-class subscribe-filter probes flip from SKIP to OK
#    because the `events.subscribe` route accepts the notification.*
#    event-type constants this phase registered.
skip "phase 72d: events.subscribe accepts notification.* filters — flips to OK when Protocol layer ships (72 + 72a + 60)"

smoke_summary
