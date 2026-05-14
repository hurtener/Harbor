#!/usr/bin/env bash
# Phase 53a smoke — Agent Registry.
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped a Protocol surface.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.
#
# Phase 53a ships the Agent Registry as a RUNTIME-INTERNAL subsystem: it owns the
# registration identity of agents (agent_id / incarnation / version_hash), persists
# via the StateStore, and emits agent.* events on the typed bus. It has NO Protocol
# surface at this phase — `agents.*` Protocol methods and the Console Agents page
# land in the Protocol / Console-attaching waves (54+, 72–75) and twin with their
# own smoke coverage there (RFC §7's "no Console page without its feeding Protocol
# surface" rule). Until then this script skips per the 404 → SKIP convention.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# When the registry gains a Protocol surface in a later phase, replace the skip
# below with assertions, e.g.:
#
#   skip_if_404 "$(api_url /v1/agents)" "agents list surface" || {
#     assert_status 200 "$(api_url /v1/agents)" "agents list returns 200"
#     assert_json_truthy '.agents' "$(api_url /v1/agents)" "agents list is present"
#   }
# ----------------------------------------------------------------------------

skip "phase 53a: Agent Registry is runtime-internal — no Protocol surface until the Protocol/Console waves"

smoke_summary
