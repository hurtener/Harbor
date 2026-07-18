#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 189 smoke — Cache-token capture at the LLM edge.
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 189: smoke skeleton — replace with the translator PromptTokensDetails read, Usage field, and consumer-decode assertions when the phase implements its surface"

smoke_summary
