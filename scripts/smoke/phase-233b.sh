#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Planning-stage guards: the surface is intentionally not implemented yet, but
# the binding plan/RFC must retain the production-safe boot-authorized contract.
assert_file "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "phase 233b plan exists"
assert_grep_present 'agent_config\.register_oauth_mcp_capability' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "canonical atomic capability method is specified"
assert_grep_present 'D-401' "RFC-001-Harbor.md" "RFC carries the D-401 production contract"
assert_grep_present 'production-safe, boot-authorized' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "production path requires boot authorization"
assert_grep_present 'explicit signed-capability production opt-in' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "broker opt-in is explicit"
assert_grep_present 'JTI with any different fingerprint' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "replay rejects a JTI bound to another pair"
assert_grep_present 'exact retry of that JTI/fingerprint' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "exact immutable retry converges"
assert_grep_present 'Registry\.DeactivateIfActive' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "first-write repair uses registry-owned conditional deactivation"
assert_grep_present 'composite dispatch/' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "pair publication has one visibility point"
assert_grep_present 'redirects refused' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "bearer sink redirect is fail-closed"

skip "phase 233b: implementation endpoint/tests pending; static D-401 design guards ran"
smoke_summary
