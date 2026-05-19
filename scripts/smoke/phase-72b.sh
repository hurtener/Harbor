#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 72b — IdentityScope admin-impersonation extension.
#
# Surface assertions (executed only when the surface is live; 404/405/501
# auto-SKIP per CLAUDE.md §4.2):
#   1. start with admin + complete impersonation triplet → 200.
#   2. start with non-admin token + impersonation → 401/403 (CodeScopeMismatch).
#   3. start with admin + impersonation missing a triple component → 401/403
#      (CodeIdentityRequired).
#   4. start with admin + NO impersonation → 200 (backward-compat).
#   5. audit.admin_scope_used event observable on the events stream with
#      Reason=impersonation after an accepted impersonation request.
#
# Until the phase ships, `protocol_call` stubs the calls (SKIP); when the
# phase lands, replace `protocol_call` invocations with real `curl` /
# `assert_status` / `assert_json_path` calls per the assertions above.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. Admin token + complete impersonation triplet — happy path.
protocol_call 'start' \
  '{"identity": {"tenant": "t1", "user": "u-target", "session": "s1", "actor": {"tenant": "t1", "user": "u-admin", "session": "s1"}, "requester": {"tenant": "t1", "user": "u-admin", "session": "s1"}, "impersonating": {"tenant": "t1", "user": "u-target", "session": "s1"}}, "query": "impersonation-smoke"}' \
  'phase 72b: start accepts admin+impersonation triplet'

# 2. Non-admin token + impersonation field → expect 401/403 (CodeScopeMismatch).
protocol_call 'start' \
  '{"identity": {"tenant": "t1", "user": "u-target", "session": "s1", "impersonating": {"tenant": "t1", "user": "u-target", "session": "s1"}}, "query": "impersonation-smoke"}' \
  'phase 72b: start rejects impersonation without admin scope'

# 3. Admin + impersonation missing a triple component → expect 401/403
#    (CodeIdentityRequired — identity is mandatory; the impersonated triple is identity too).
protocol_call 'start' \
  '{"identity": {"tenant": "t1", "user": "u-target", "session": "s1", "actor": {"tenant": "t1", "user": "u-admin", "session": "s1"}, "requester": {"tenant": "t1", "user": "u-admin", "session": "s1"}, "impersonating": {"tenant": "t1", "user": "u-target", "session": ""}}, "query": "impersonation-smoke"}' \
  'phase 72b: start rejects impersonation with incomplete triple'

# 4. Admin + no impersonation → expect 200 (backward-compat).
protocol_call 'start' \
  '{"identity": {"tenant": "t1", "user": "u-admin", "session": "s1"}, "query": "impersonation-smoke"}' \
  'phase 72b: start accepts admin without impersonation (backward-compat)'

# 5. Audit event surfaces on the events stream after an accepted impersonation
#    request. Gated by the events.subscribe filter shape from Phase 72a;
#    skip if 72a's surface is not yet live.
if skip_if_404 "$(api_url /protocol/events/subscribe)" \
  'phase 72b: events.subscribe route absent until Phase 72a Protocol layer ships'; then
  protocol_call 'events/subscribe' \
    '{"filter": {"event_types": ["audit.admin_scope_used"]}}' \
    'phase 72b: audit.admin_scope_used observable after impersonation'
fi

# Surface-existence probe — when the protocol layer ships, this flips from
# SKIP to OK and the protocol_call stubs above are replaced with real
# assert_status / assert_json_path calls.
skip_if_404 "$(api_url /protocol/control/start)" \
  'phase 72b: control/start route absent until Protocol layer ships' || true

smoke_summary
