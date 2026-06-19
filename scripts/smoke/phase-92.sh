#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 92 smoke — Console-driven mid-session model swap (two scopes).
#
# Conventions (AGENTS.md §4.2): static greps SKIP until the surface lands;
# the live drive-a-swap checks are added when the dev surface supports them.
#
# Phase 92: session-level swap extends runs.set_overrides with a Model field
# (next-turn, owner-scoped); tenant-level admin default is the new admin-scoped
# governance.swap_model Protocol method (RFC §6.15 ModelOverride seam, audited).
# Effective model resolves session > tenant > cfg.LLM.Model at run start (D-025
# snapshot). Unknown model fails loud (ErrUnsupportedModel).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# 1. RunOverrides carries a Model field (the session-level swap, single source).
# ----------------------------------------------------------------------------
if grep -rqE 'Model\s+\*string' internal/protocol/types/runs.go 2>/dev/null; then
    ok 'phase 92: RunOverrides.Model field present (session-level swap)'
else
    skip 'phase 92: RunOverrides.Model not yet present'
fi

# ----------------------------------------------------------------------------
# 2. The governance.swap_model Protocol method is declared (tenant-level).
# ----------------------------------------------------------------------------
if grep -rqE '"governance\.swap_model"' internal/protocol/methods/methods.go 2>/dev/null; then
    ok 'phase 92: governance.swap_model Protocol method declared'
else
    skip 'phase 92: governance.swap_model method not yet declared'
fi

# ----------------------------------------------------------------------------
# 3. A governance ModelOverride policy backs the tenant default.
# ----------------------------------------------------------------------------
if grep -rqiE 'ModelOverride' internal/governance/*.go 2>/dev/null; then
    ok 'phase 92: governance ModelOverride policy present'
else
    skip 'phase 92: governance ModelOverride policy not yet present'
fi

# ----------------------------------------------------------------------------
# 4. A governance.model_swapped audit/event is declared.
# ----------------------------------------------------------------------------
if grep -rqiE 'model_swapped|ModelSwapped' internal/ 2>/dev/null; then
    ok 'phase 92: governance.model_swapped event present'
else
    skip 'phase 92: governance.model_swapped event not yet present'
fi

smoke_summary
