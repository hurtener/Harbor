#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 188 smoke — Background wake notifications + turn-failure honesty.
#
# This phase extends the shipped `notification.*` topic with two new
# conversational-mirror classes (`notification.task_group_resolved`,
# `notification.task_completed`) and adds the TUI's foreground turn-
# failure line + the muted conversational notification block. No new
# Protocol method: the classes ride the existing `events.subscribe`
# stream, so the live assertion probes that the events surface ACCEPTS
# the new event-type constants in its type filter (never a
# 400 / unknown-event-type).
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501/000 → SKIP (so phase-N+1 scripts coexist with phase-N
#     builds and a standalone run with no dev server degrades gracefully).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# The dev bearer is resolved through common.sh's `dev_bearer`, never by a raw
# ${HARBOR_DEV_TOKEN} read: the raw read is EMPTY outside preflight, so every
# live leg below degrades to a SKIP while the script still exits 0 — "a SKIP
# that should be an OK is a bug" (AGENTS.md §4.2 item 5, issue #624).
# dev_bearer prefers the exported value and falls back to the dev server log.
HARBOR_DEV_TOKEN="$(dev_bearer)"

BIN="${ROOT}/bin/harbor"

# 1. Runtime mapper unit tests — the two new background-wake classes
#    (group-resolved rollup counts + cap/truncation, task-completed
#    gated on NotifyOnComplete) plus the extended concurrent-reuse set.
if go test -race -count=1 -timeout 120s -run 'TestMap' \
    ./internal/runtime/notifications/... >/tmp/phase-188-mapper.log 2>&1; then
    ok 'phase 188: notifications mapper tests pass (task_group_resolved + task_completed classes, N=100 reuse)'
else
    fail 'phase 188: notifications mapper tests failed'
    tail -40 /tmp/phase-188-mapper.log | sed 's/^/    /'
fi

# 2. TUI projection reducer tests — the notification blocks, the
#    foreground-failure dedup, and the ErrorCode threading.
if go test -race -count=1 -timeout 120s -run 'Notification|TaskFailed_ThreadsErrorCode|ClassifyEventType' \
    ./internal/tui/projection/... >/tmp/phase-188-projection.log 2>&1; then
    ok 'phase 188: projection reducer tests pass (notification blocks + foreground dedup + ErrorCode)'
else
    fail 'phase 188: projection reducer tests failed'
    tail -40 /tmp/phase-188-projection.log | sed 's/^/    /'
fi

# 3. TUI conversation-surface tests — the muted notification line, the
#    conversational() boundary, and the turn-failure status-strip line.
if go test -race -count=1 -timeout 120s -run 'Notification|TurnFail|StatusStrip_Turn' \
    ./internal/tui/app/... >/tmp/phase-188-app.log 2>&1; then
    ok 'phase 188: TUI conversation-surface + status-strip tests pass'
else
    fail 'phase 188: TUI conversation-surface tests failed'
    tail -40 /tmp/phase-188-app.log | sed 's/^/    /'
fi

# 4. Integration suite — real bus + real Subscriber, the two new wake
#    classes round-trip + the NotifyOnComplete=false negative.
if go test -race -count=1 -timeout 120s -run 'TestE2E_NotificationsTopic' \
    ./test/integration/... >/tmp/phase-188-integration.log 2>&1; then
    ok 'phase 188: notifications integration suite passes (wake classes round-trip + NotifyOnComplete=false negative)'
else
    fail 'phase 188: notifications integration suite failed'
    tail -40 /tmp/phase-188-integration.log | sed 's/^/    /'
fi

# 5. Static honesty-gate guards (Phase 184/72d house style): a
#    regression that silently reverts the honesty gates fails preflight
#    immediately, even before the golden suite runs.
assert_grep_present 'case "notification":' \
    internal/tui/app/live.go \
    'phase 188: conversational() admits the "notification" kind'
assert_grep_present 'Turn failed' \
    internal/tui/app/model.go \
    'phase 188: statusStrip() carries the foreground turn-failure line'
assert_grep_present 'notification.task_completed", "notification.task_group_resolved", "notification.task_group_cancelled", "notification.task_failed"' \
    internal/tui/projection/projection.go \
    'phase 188: projection classifies the notification classes as EventTyped'
assert_grep_present 'notification.task_group_resolved' \
    web/console/src/lib/events/taxonomy.ts \
    'phase 188: Console taxonomy registers the wake classes'

# 6. Live events-surface assertion — the new event TYPE half. The
#    classes ride the existing events.subscribe stream, so the probe
#    asserts the type filter ACCEPTS the new constants (exit 0, no
#    unknown-event-type rejection). Degrades to SKIP with no dev server
#    / no built binary.
if [[ ! -x "${BIN}" ]]; then
    skip 'phase 188: bin/harbor not built — skipping live events-filter probe'
    smoke_summary
    exit 0
fi
if ! skip_if_404 "$(api_url /healthz)" 'phase 188: dev server reachable for events-filter probe'; then
    smoke_summary
    exit 0
fi
if [[ -z "${HARBOR_DATA_DIR:-}" ]] || [[ ! -f "${HARBOR_DATA_DIR}/server.log" ]]; then
    skip 'phase 188: HARBOR_DATA_DIR/server.log absent — cannot mint dev token for events-filter probe'
    smoke_summary
    exit 0
fi
TOKEN="$(dev_bearer)"
if [[ -z "${TOKEN}" ]]; then
    skip 'phase 188: HARBOR_DEV_TOKEN not found in server.log — skipping events-filter probe'
    smoke_summary
    exit 0
fi
export HARBOR_TOKEN="${TOKEN}"

for etype in notification.task_completed notification.task_group_resolved; do
    probe_log="$(mktemp)"
    probe_status=0
    "${BIN}" inspect-events \
        --bind "${HARBOR_BIND:-127.0.0.1:18080}" \
        --tenant dev --user dev --session dev \
        --type "${etype}" \
        --since 0 \
        --follow=false \
        --json \
        >"${probe_log}" 2>&1 || probe_status=$?
    if [[ "${probe_status}" -eq 0 ]] && ! grep -qi 'unknown event type' "${probe_log}"; then
        ok "phase 188: events.subscribe accepts the ${etype} type filter (new canonical event type)"
    else
        fail "phase 188: events.subscribe rejected the ${etype} type filter (status=${probe_status})"
        tail -10 "${probe_log}" | sed 's/^/    /'
    fi
    rm -f "${probe_log}"
done

smoke_summary
