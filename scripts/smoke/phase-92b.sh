#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 92b smoke — tenant-override completion (session swap field + Console
# admin UI + multi-replica freshness).
#
# Planning skeleton: SKIPs until the surface lands. At implementation,
# assert (static): RunOverrides.Model present; the session-override consume
# seam wired in the run loop; the five governance wire types typed (no
# longer in protocol-ts-untyped-allow.json). Live: a session override wins
# over a set tenant default on the next run.
#
# Conventions (AGENTS.md §4.2): 404/405/501 -> SKIP; OK >= 1 once shipped;
# use scripts/smoke/common.sh helpers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# RunOverrides gains a Model field (the session-level swap).
if grep -rqE 'Model\s+\*string' internal/protocol/types/runs.go 2>/dev/null; then
    ok 'phase 92b: RunOverrides.Model field present (session-level swap)'
else
    skip 'phase 92b: RunOverrides.Model not yet present'
fi

# The session-override consume seam is wired in the run loop (Store.Consume
# called from production, not just tests).
if grep -rqE '\.Consume\(' cmd/harbor/cmd_dev_runloop.go 2>/dev/null; then
    ok 'phase 92b: session-override consume seam wired at run start'
else
    skip 'phase 92b: session-override consume seam not yet wired'
fi

# The governance tenant-override wire types are typed (removed from the
# untyped allow-list).
if grep -q 'GovernanceTenantOverrides' web/console/scripts/protocol-ts-untyped-allow.json 2>/dev/null; then
    skip 'phase 92b: governance tenant-override types still allow-listed (not yet typed)'
else
    ok 'phase 92b: governance tenant-override wire types typed (de-allow-listed)'
fi

smoke_summary
