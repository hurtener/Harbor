#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 92b smoke — tenant-override completion (session swap field + Console
# admin UI + multi-replica freshness).
#
# Conventions (AGENTS.md §4.2): 404/405/501 -> SKIP; OK >= 1 once shipped;
# use scripts/smoke/common.sh helpers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. RunOverrides carries a Model field (the session-level swap).
assert_grep_present 'Model \*string' \
    internal/protocol/types/runs.go \
    'phase 92b: RunOverrides.Model field present (session-level swap)'

# 2. The session-override consume seam is wired in the run loop (Store.Consume
#    called from production).
assert_grep_present '\.Consume\(' \
    internal/runtime/serve/runloop.go \
    'phase 92b: session-override consume seam wired at run start'

# 3. The planner carries SystemPromptOverride (the session replace).
assert_grep_present 'SystemPromptOverride \*string' \
    internal/planner/planner.go \
    'phase 92b: planner LLMOverrides.SystemPromptOverride (replace) present'

# 4. Per-read freshness: the loaded-permanent gate is gone (reloadLocked).
assert_grep_present 'reloadLocked' \
    internal/governance/tenantoverride.go \
    'phase 92b: per-read freshness reload present (multi-replica)'

# 5. The governance tenant-override wire types are typed (removed from the
#    untyped allow-list).
if grep -q 'GovernanceTenantOverrides' web/console/scripts/protocol-ts-untyped-allow.json 2>/dev/null; then
    fail 'phase 92b: governance tenant-override types still allow-listed (not typed)'
else
    ok 'phase 92b: governance tenant-override wire types typed (de-allow-listed)'
fi

# 6. The typed Console client + admin control exist.
assert_grep_present 'export interface GovernanceTenantOverrides' \
    web/console/src/lib/protocol/governance.ts \
    'phase 92b: typed Console governance wire interfaces present'
assert_grep_present 'class GovernanceNamespace' \
    web/console/src/lib/protocol/client.ts \
    'phase 92b: GovernanceNamespace typed client present'

smoke_summary
