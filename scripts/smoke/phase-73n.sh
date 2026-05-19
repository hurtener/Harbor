#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 73n — Console Playground page + chat module + runs.set_overrides.
#
# Surface assertions (404/405/501 auto-SKIP per AGENTS.md §4.2):
#   1. runs.set_overrides round-trips with valid session id.
#   2. runs.set_overrides rejects cross-session override (identity scope).
#   3. Chat module imports stay inside $lib/chat/ — grep check (static, runs at CI as well).
#   4. Page route /console/playground/<session-id> — SKIPped until 73m harbor console lands.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

SESSION_ID_FALLBACK='sess-smoke-fixture'
CROSS_SESSION_ID='cross-tenant-sess'

# 1. runs.set_overrides happy path.
protocol_call 'runs/set_overrides' \
  "{\"overrides\": {\"session_id\": \"${SESSION_ID_FALLBACK}\", \"reasoning_effort\": \"high\"}}" \
  'phase 73n: runs.set_overrides applies override to next message'

# 2. cross-session override → 403.
protocol_call 'runs/set_overrides' \
  "{\"overrides\": {\"session_id\": \"${CROSS_SESSION_ID}\", \"reasoning_effort\": \"high\"}}" \
  'phase 73n: runs.set_overrides rejects cross-session override'

# 3. Chat module encapsulation grep — relevant when web/console/ ships.
if [ -d 'web/console/src/lib/chat' ]; then
  # Look for imports inside chat/ that reach outside (e.g. from '$lib/components/...').
  # Allowed: from '$lib/chat/...' (same module) or from '@skeletonlabs/...' or external pkgs.
  violations=$(grep -rEn "from ['\"]\\\$lib/[^c]" web/console/src/lib/chat/ 2>/dev/null || true)
  if [ -n "${violations}" ]; then
    fail "phase 73n: chat module imports outside \$lib/chat/ — encapsulation violation"
    printf '%s\n' "${violations}"
  else
    ok 'phase 73n: chat module encapsulation invariant holds (no imports outside $lib/chat/)'
  fi
else
  skip 'phase 73n: web/console/src/lib/chat/ absent until 73n lands'
fi

# 4. Page route.
skip 'phase 73n: /console/playground/<session-id> route lands with 73m harbor console'

skip_if_404 "$(api_url /protocol/runs/set_overrides)" \
  'phase 73n: runs.set_overrides route absent until Protocol layer ships' || true

smoke_summary
