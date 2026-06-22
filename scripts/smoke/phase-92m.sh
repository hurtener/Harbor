#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 92m smoke — add_mcp_connection OAuth config + InitiateFlow parking.
#
# This phase reshapes the `agent_config.add_mcp_connection` Protocol verb (an
# optional OAuth block on the request; authorize_url on the auth_required
# response) and replaces the bare park with InitiateFlow. Classified
# `live-server` because the real assertions hit the booted dev server's
# agent_config surface.
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so this script coexists with pre-92m builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.
#
# When the phase ships, replace the skip below with:
#   - protocol_call 'agent_config.add_mcp_connection' '{... oauth block ...}'
#     and assert the auth_required response carries authorize_url + pause_token
#     (skip_if_404 so a pre-phase build SKIPs).
#   - assert the non-admin caller is rejected (fail-closed admin gate).
#   - static greps: the OAuth-block wire fields, the example-config OAuth entry,
#     the typed-client + generated-docs rows.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 92m: add_mcp_connection OAuth config + InitiateFlow parking — smoke skeleton, replace with real assertions when the phase implements its surface"

smoke_summary
