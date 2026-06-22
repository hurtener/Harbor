#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 92q smoke — Console advisory + wave-end live E2E + §17.5 audit.
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.
#
# This phase is a Console consumer (the auth_required advisory) + a wave-end
# integration test + an env-gated HARBOR_LIVE_* live probe. The smoke ships as a
# single skip until the surface lands; replace with the static panel-presence and
# live agent_config.* feeding-method assertions when 92q implements its surface.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 92q: smoke skeleton — replace with the auth_required advisory presence + live agent_config.* feeding-method assertions when the phase implements its surface"

smoke_summary
