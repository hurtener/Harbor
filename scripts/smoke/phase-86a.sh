#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 86a smoke — distributed task dispatcher (the MessageBus consumer).
#
# Conventions (AGENTS.md §4.2):
#   - Static source greps until the phase ships; each guard SKIPs while the
#     surface is absent and flips to OK when the dispatcher lands. No FAIL
#     pre-ship. The live drive-a-task check is added when the dev surface
#     supports it.
#   - Use helpers from scripts/smoke/common.sh.
#
# Phase 86a wires distributed.OpenBus into the runtime, publishes task-lifecycle
# envelopes onto the durable MessageBus (Phase 86), and runs a fleet RunLoop
# driver that claims (exactly-one-driver lease) + drives a spawned task — turning
# the durable bus + durable TaskService (86 + 87) into a fleet work queue. It is
# the consumer that makes the durable bus load-bearing.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# 1. The runtime opens a MessageBus (the bus is wired into assembly).
# ----------------------------------------------------------------------------
if grep -rqE 'distributed\.OpenBus' internal/runtime/assemble/*.go 2>/dev/null; then
    ok 'phase 86a: internal/runtime/assemble opens a MessageBus (distributed.OpenBus)'
else
    skip 'phase 86a: MessageBus not yet wired into assembly'
fi

# ----------------------------------------------------------------------------
# 2. A distributed dispatcher (subscribe -> claim -> drive) exists.
# ----------------------------------------------------------------------------
if grep -rlqE 'task\.spawned|EventTypeDistributedBusEnvelope' internal/runtime/dispatcher/*.go 2>/dev/null \
    || grep -rlq 'dispatcher' internal/runtime/dispatcher/*.go 2>/dev/null; then
    ok 'phase 86a: distributed dispatcher package present'
else
    skip 'phase 86a: distributed dispatcher not yet implemented'
fi

# ----------------------------------------------------------------------------
# 3. A task claim / lease primitive exists (exactly-one-driver gate).
# ----------------------------------------------------------------------------
if grep -rqiE 'task\.claim|claimTask|leaseTask|task claim' internal/ 2>/dev/null; then
    ok 'phase 86a: task claim / lease primitive present'
else
    skip 'phase 86a: task claim / lease primitive not yet present'
fi

# ----------------------------------------------------------------------------
# 4. The high-level multi-worker deployment design is documented.
# ----------------------------------------------------------------------------
if [ -f docs/design/distributed-execution.md ]; then
    ok 'phase 86a: multi-worker deployment design doc present'
else
    skip 'phase 86a: docs/design/distributed-execution.md not yet present'
fi

smoke_summary
