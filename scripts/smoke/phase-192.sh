#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 192 smoke — task.group_cancelled conversational mirror.
#
# Adds one additive notification class (notification.task_group_cancelled) that
# mirrors an UNPROMPTED (cascade / fail-fast) cancel of a batch-spawned group
# onto the conversation surface, suppressing a directly-operator-initiated
# cancel (D-329). No new Protocol method (the class rides the existing
# events.subscribe surface), so the assertions are unit-test-shaped; the live
# events probe records SKIP on a bare preflight dev server.
#
# Each go-test block SKIPs (not FAILs) when its target test name is absent, so
# this script coexists with pre-192 builds per the sacred SKIP convention.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# --- Class assertions (unit tests) -----------------------------------------

# Run a go test IFF the named test exists in the package; otherwise SKIP so the
# skeleton coexists with a pre-192 build (surface not yet implemented).
run_go_test_if_present() {
    local label="$1" runexpr="$2" pkg="$3" logf
    logf="$(mktemp)"
    if ! go test -list "${runexpr}" "${pkg}" >"${logf}" 2>&1 \
        || ! grep -qE '^Test' "${logf}"; then
        skip "phase 192: ${label} (test/surface absent — pre-192 build)"
        rm -f "${logf}"
        return
    fi
    if go test -run "${runexpr}" -race "${pkg}" >"${logf}" 2>&1; then
        ok "phase 192: ${label}"
    else
        fail "phase 192: ${label}"
        printf '--- go test output ---\n'
        cat "${logf}"
    fi
    rm -f "${logf}"
}

# AC-1/2/3: the mapper synthesizes for cascade/fail-fast origins, suppresses the
# operator-initiated cancel, and reuses the member-outcome summarisation.
run_go_test_if_present "mapTaskGroupCancelled synthesize/suppress per origin" \
    'TestMap.*GroupCancelled|TestSubscriber.*GroupCancelled' \
    './internal/runtime/notifications/'

# AC-4: the engine emits TaskGroupCancelledPayload with a correct Origin.
run_go_test_if_present "engine group-cancel emits TaskGroupCancelledPayload+Origin" \
    'TestEngine.*GroupCancelled|TestGroups.*Cancelled.*Origin' \
    './internal/tasks/engine/'

# AC-7: the TUI projection renders the new type as the muted notification kind.
run_go_test_if_present "tui projection renders notification.task_group_cancelled" \
    'TestReducer.*GroupCancelled|TestClassifyEventType.*GroupCancelled' \
    './internal/tui/projection/'

# AC-11: the tasks->notifications producer chain round-trips over the real bus,
# and an operator-origin cancel produces NO notification.
run_go_test_if_present "E2E notifications topic: cancel mirror + operator suppression" \
    'TestE2E_NotificationsTopic' \
    './test/integration/'

# --- Live-server surface (graceful skip) -----------------------------------

skip "phase 192: notification.task_group_cancelled rides the existing events.subscribe surface; a live assertion needs an authenticated subscriber + a boot-driven cascade-cancelled batch — covered by the extended TestE2E_NotificationsTopic, not this bare-server smoke"

smoke_summary
