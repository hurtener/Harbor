#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 150 smoke — run-completion hook: transcript egress through the
# tool catalog (D-280). docs/plans/phase-150-run-completion-hook.md
#
# Assertions once the phase ships:
#   1. go test -race for internal/runtime/steering (completion-hook fire
#      on every terminal outcome, transcript assembly ordering golden,
#      run.hook_dispatched / run.hook_failed events, D-274 counters
#      untouched by a hook dispatch).
#   2. go test -race for internal/agentcfg + the run-start projection
#      (hooks section revision round-trip, diff arm, section-merge
#      preservation, agentcfg-over-yaml precedence).
#   3. go test -race for internal/config (runtime.hooks validation).
#   4. go test -race for the phase 150 E2E (test/integration/ — hook
#      receives the full ordered transcript incl. a steering-injected
#      mid-run user message; identity at the receiving tool; hook error
#      leaves the run outcome unchanged; cancelled run still fires with
#      outcome=cancelled), with a `go test -list` no-match-fails guard.
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

skip "phase 150: smoke skeleton — replace with real assertions when the run-completion hook lands"

smoke_summary
