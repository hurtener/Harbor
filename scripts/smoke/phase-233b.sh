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
assert_grep_present 'trust_anchor_name' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "JTI operation key includes the trust anchor"
assert_grep_present 'claimed -> revision_committed -> published' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "durable JTI phase machine is specified"
assert_grep_present 'outside the general `ProviderSet`' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "signed provider is pair-owned"
assert_grep_present 'catalog source swap is the sole data-plane dispatch' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "catalog is the only dispatch linearization"
assert_grep_present 'pending-activation/compensation fence' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "first-write security fence is durable"
assert_grep_present 'IDNA2008' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "canonical URL algorithm is pinned"
assert_grep_present 'redirects' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "bearer redirects are fail-closed"

skip "phase 233b: implementation endpoint/tests pending; static D-401 design guards ran"
smoke_summary
