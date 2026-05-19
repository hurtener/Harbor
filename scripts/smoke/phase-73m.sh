#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 73m — Console Settings page + harbor console subcommand.
#
# Surface assertions (404/405/501 auto-SKIP per AGENTS.md §4.2):
#   1. 1 NEW admin method (auth.rotate_token) requires console.admin claim.
#   2. harbor console subcommand boots (CLI smoke, separate from live-server probe).
#   3. Page route /console/settings — depends on (2), so coupled here.
#   4. harbor dev --help does NOT advertise a console-serving flag (D-091 binding).
#
# NOTE: runtime.info / runtime.health / runtime.counters / runtime.drivers /
# metrics.snapshot are owned by Phase 72f; governance.posture + llm.posture
# are owned by Phase 72g. Their smokes live in their respective phase files;
# 73m only asserts the page route + auth.rotate_token + harbor console boot.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. Admin method rejected without claim.
protocol_call 'auth/rotate_token' '{}' \
  'phase 73m: auth.rotate_token rejects requests without console.admin claim'

# 2. harbor console subcommand smoke — CLI side. SKIP until cmd lands.
if [ -x "./bin/harbor" ]; then
  if ./bin/harbor console --help >/dev/null 2>&1; then
    ok 'phase 73m: harbor console --help exits 0'
  else
    skip 'phase 73m: harbor console subcommand absent until 73m lands'
  fi

  # 4. D-091 binding rule: harbor dev MUST NOT advertise console-serving.
  if ./bin/harbor dev --help 2>&1 | grep -qiE 'serve.*console|console.*serve|--console'; then
    fail 'phase 73m: harbor dev --help advertises console-serving (D-091 violation)'
  else
    ok 'phase 73m: harbor dev --help does NOT advertise console-serving (D-091 honoured)'
  fi
else
  skip 'phase 73m: ./bin/harbor not built — harbor console smoke deferred'
fi

# 3. Page route (depends on harbor console being up; same coupling).
skip 'phase 73m: /console/settings route lands with this phase'

# Surface-existence probe — flips from SKIP to OK once the auth.rotate_token route ships.
skip_if_404 "$(api_url /protocol/auth/rotate_token)" \
  'phase 73m: auth.rotate_token route absent until Protocol layer ships' || true

smoke_summary
