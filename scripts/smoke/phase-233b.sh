#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Planning-stage guards: the surface is intentionally not implemented yet, but
# the binding plan/RFC must remain present and must not regress to the dev-only
# wire descriptor as its production contract.
assert_file "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "phase 233b plan exists"
assert_grep_present 'agent_config\.register_oauth_mcp_capability' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "canonical atomic capability method is specified"
assert_grep_present 'D-401' "RFC-001-Harbor.md" "RFC carries the D-401 production contract"
assert_grep_present 'SaveIf.*no-active marker' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "first-write compensation is conditional no-active neutralization"
assert_grep_absent 'allow_wire_oauth_descriptor.*production path' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "dev-only descriptor is not made a production path"

skip "phase 233b: implementation endpoint/tests pending; static D-401 design guards ran"
smoke_summary
