#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 72e — pause.list snapshot Protocol method.
#
# Surface assertions (404/405/501 auto-SKIP per AGENTS.md §4.2):
#   1. pause.list with no filter returns a paginated snapshot (default page=1, page_size=50).
#   2. cross-tenant filter without auth.ScopeAdmin → 403 (CodeScopeMismatch).
#   3. missing identity context → 401 (CodeIdentityRequired).
#   4. page_size out of range (e.g. 5000) → 400 (CodeInvalidRequest; never silently clamped).
#
# Until the phase ships, `protocol_call` stubs each invocation (SKIP); when
# the Protocol layer lands, replace `protocol_call` invocations with real
# assert_status / assert_json_path calls per the assertions above.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. pause.list happy path — default pagination.
protocol_call 'pause/list' '{}' \
  'phase 72e: pause.list returns paginated snapshot rows'

# 2. cross-tenant filter without admin claim → expect 403 (CodeScopeMismatch).
protocol_call 'pause/list' \
  '{"filter": {"tenant_ids": ["foreign-tenant"]}}' \
  'phase 72e: pause.list rejects cross-tenant filter without auth.ScopeAdmin claim'

# 3. missing identity carrier → expect 401 (CodeIdentityRequired).
protocol_call 'pause/list' '{}' \
  'phase 72e: pause.list rejects request with no identity in context'

# 4. page_size out of range → expect 400 (CodeInvalidRequest; never silently clamped).
protocol_call 'pause/list' \
  '{"page_size": 5000}' \
  'phase 72e: pause.list rejects page_size out of [1,200] range'

# Surface-existence probe — flips from SKIP to OK when the Protocol layer ships.
skip_if_404 "$(api_url /protocol/pause/list)" \
  'phase 72e: pause.list route absent until Protocol layer ships' || true

smoke_summary
