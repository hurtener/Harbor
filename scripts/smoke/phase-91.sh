#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 91 smoke — Console-driven LLM provider key rotation
# (admin `governance.rotate_key`).
#
# The new-key value is a SECRET — the smoke NEVER asserts key identity over
# the wire (the integration test does, with a real holder). The live check
# asserts only status codes + that the route is admin-gated.
#
# Conventions (AGENTS.md §4.2): 404/405/501 -> SKIP; OK >= 1 once shipped.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_grep_present '"governance\.rotate_key"' \
    internal/protocol/methods/methods.go \
    'phase 91: governance.rotate_key Protocol method declared'

assert_grep_present 'type LiveKey struct' \
    internal/llm/livekey.go \
    'phase 91: atomic LiveKey holder present'

assert_grep_present 'EventTypeKeyRotated' \
    internal/governance/events.go \
    'phase 91: governance.key_rotated event registered'

assert_grep_present 'a.primaryKey.Get()' \
    internal/llm/drivers/bifrost/account.go \
    'phase 91: bifrost account reads the swappable key holder'

# Live (preflight dev server): the route is admin-gated. An unauthenticated
# POST must NOT be 200 — 401 (no identity) / 403 (no admin) / 501 (not
# wired) are all healthy. A 404 means the surface is not mounted (SKIP).
ROUTE="$(api_url /v1/governance/rotate_key)"
if skip_if_404 "${ROUTE}" 'phase 91: governance.rotate_key route mounted'; then
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST -H 'Content-Type: application/json' -d '{}' "${ROUTE}" || true)
    case "${code}" in
        401|403) ok "phase 91: governance.rotate_key is identity/admin-gated (${code})" ;;
        501)     ok "phase 91: governance.rotate_key route present but unwired (501)" ;;
        200)     fail "phase 91: governance.rotate_key answered 200 unauthenticated — admin gate missing" ;;
        *)       fail "phase 91: governance.rotate_key unexpected status ${code}" ;;
    esac
fi

smoke_summary
