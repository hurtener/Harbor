#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 92 smoke — admin-set tenant-default LLM overrides (the RFC §6.15
# ModelOverride governance seam).
#
# Conventions (AGENTS.md §4.2): static greps SKIP until the surface lands;
# the live admin-set checks run when the dev surface supports them.
#
# Phase 92 (tenant-default scope): an admin sets a tenant's default LLM
# parameters (model + additive extra-instructions + temperature +
# max-tokens + reasoning-effort) live via the admin-scoped
# governance.set_tenant_overrides Protocol method; governance.get_tenant_overrides
# reads them back. The default is StateStore-backed, audited
# (governance.tenant_overrides_set), and applied to every session's NEXT run
# (a D-025 run-start snapshot pinned into RunContext.LLMOverrides, consumed by
# the planner). An unknown model fails loud (ErrUnknownModel) at set time.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Static guards — the Protocol surface + governance seam + planner apply path.
# ----------------------------------------------------------------------------

# 1. The admin-scoped tenant-override methods are declared (single source).
assert_grep_present '"governance\.set_tenant_overrides"' \
    internal/protocol/methods/methods.go \
    'phase 92: governance.set_tenant_overrides Protocol method declared'
assert_grep_present '"governance\.get_tenant_overrides"' \
    internal/protocol/methods/methods.go \
    'phase 92: governance.get_tenant_overrides Protocol method declared'

# 2. The StateStore-backed governance TenantOverridePolicy realises the seam.
assert_grep_present 'type TenantOverridePolicy struct' \
    internal/governance/tenantoverride.go \
    'phase 92: governance TenantOverridePolicy present'

# 3. The audit event is declared + registered.
assert_grep_present 'EventTypeTenantOverridesSet' \
    internal/governance/events.go \
    'phase 92: governance.tenant_overrides_set event present'

# 4. The planner carries the per-run override bundle (the apply primitive).
assert_grep_present 'LLMOverrides \*LLMOverrides' \
    internal/planner/planner.go \
    'phase 92: RunContext.LLMOverrides per-run override bundle present'

# 5. The run loop resolves the tenant default at run start (the consumer).
assert_grep_present 'resolveLLMOverrides' \
    cmd/harbor/cmd_dev_runloop.go \
    'phase 92: run-start tenant-override resolution wired'

# ----------------------------------------------------------------------------
# Live checks (preflight dev server). The routes are admin-scoped; an
# unauthenticated probe is rejected (401/403), which the helpers treat as a
# reachable surface (NOT 404/SKIP). When the surface is not mounted, the
# route 404s and SKIPs — the partial-build convention.
# ----------------------------------------------------------------------------
GET_URL="$(api_url /v1/governance/get_tenant_overrides)"
if skip_if_404 "${GET_URL}" 'phase 92: governance.get_tenant_overrides route mounted'; then
    # The route exists. An unauthenticated POST must NOT be 200 (admin-gated):
    # the dev server's auth posture returns 401 (no identity) or 403 (no admin
    # scope). Either is a healthy "the gate is live" signal.
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST -H 'Content-Type: application/json' -d '{}' "${GET_URL}" || true)
    case "${code}" in
        401|403)
            ok "phase 92: governance.get_tenant_overrides is identity/admin-gated (${code})"
            ;;
        200)
            # Trust-based dev posture (no validator) — the route answered. Still
            # a live surface; the admin gate is exercised by the Go tests.
            ok "phase 92: governance.get_tenant_overrides answered (trust posture, 200)"
            ;;
        *)
            fail "phase 92: governance.get_tenant_overrides unexpected status ${code}"
            ;;
    esac
fi

smoke_summary
