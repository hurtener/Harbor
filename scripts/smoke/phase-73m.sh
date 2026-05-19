#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 73m — Console Settings page + harbor console subcommand.
#
# Surface assertions (404/405/501 auto-SKIP per AGENTS.md §4.2):
#   1. 4 NEW read methods (runtime.info / runtime.storage / runtime.llm_posture / governance.posture).
#   2. 1 NEW admin method (auth.rotate_token) requires console.admin claim.
#   3. harbor console subcommand boots (CLI smoke, separate from live-server probe).
#   4. Page route /console/settings — depends on (3), so coupled here.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. 4 new read methods.
protocol_call 'runtime/info' '{}' \
  'phase 73m: runtime.info returns build + Protocol version + drivers'
protocol_call 'runtime/storage' '{}' \
  'phase 73m: runtime.storage returns per-subsystem driver projection'
protocol_call 'runtime/llm_posture' '{}' \
  'phase 73m: runtime.llm_posture returns provider + model + MockMode'
protocol_call 'governance/posture' '{}' \
  'phase 73m: governance.posture returns IdentityTiers projection'

# 2. Admin method rejected without claim.
protocol_call 'auth/rotate_token' '{}' \
  'phase 73m: auth.rotate_token rejects requests without console.admin claim'

# 3. harbor console subcommand smoke — CLI side. SKIP until cmd lands.
if [ -x "./bin/harbor" ]; then
  if ./bin/harbor console --help >/dev/null 2>&1; then
    ok 'phase 73m: harbor console --help exits 0'
  else
    skip 'phase 73m: harbor console subcommand absent until 73m lands'
  fi
else
  skip 'phase 73m: ./bin/harbor not built — harbor console smoke deferred'
fi

# 4. Page route (depends on harbor console being up; same coupling).
skip 'phase 73m: /console/settings route lands with this phase'

skip_if_404 "$(api_url /protocol/runtime/info)" \
  'phase 73m: runtime.info route absent until Protocol layer ships' || true

smoke_summary
