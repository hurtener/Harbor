#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 165 — structured reasoning-steps rehydration (D-298), Console-only.
#
# The reconstruction is Console-side (reducer over durable events); this smoke
# proves the READ-BACK carries the events the reducer folds — no real LLM
# needed (the mock exercises the mechanics; reasoning traces are empty under
# the mock, which is fine — this asserts STRUCTURE presence, per 161's
# precedent). When the phase lands:
#   - drive a tool-calling scripted run via the `start` method
#     (POST /v1/control/start); poll tasks.get to terminal;
#   - fetch state.history; assert planner.decision events carry a
#     ReasoningTrace payload key AND tool.invoked/tool.completed carry a
#     ToolName key — the exact events the reducer folds into ordered steps;
#   - assert the planner.decision and tool.* events interleave by sequence
#     (a decision precedes its tool's invoke/complete) — the reconstructable
#     ordering signal.
# Done-definition: OK >= 2, FAIL = 0 once the phase ships.
# Until then it SKIPs. Real assertions land with the implementation PR.
#
#   cp scripts/smoke/_template.sh scripts/smoke/phase-NN.sh
#   chmod +x scripts/smoke/phase-NN.sh
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.
#
# Classification (D-104 — the `# PREFLIGHT_REQUIRES:` header above):
#   - static-only — pure file/text greps, golden compares, file-existence
#     assertions. Runs in the parallel batch BEFORE the dev server boots.
#   - live-server — hits the booted dev server over HTTP (`api_url`,
#     `assert_status`, `skip_if_404`, `assert_json_path`) or reads the
#     preflight server log. Runs serially against the booted instance.
#   - unit-tests — runs `go test` for one or more packages. Parallelisable;
#     `go test` schedules its own internal parallelism.
#
# Pick `live-server` whenever the smoke depends on `HARBOR_BIND` /
# `HARBOR_BASE_URL` / `HARBOR_DEV_TOKEN` / `${HARBOR_DATA_DIR}/server.log`
# or invokes the built `bin/harbor` against a network endpoint. When in
# doubt, `live-server` is the safe default — misclassifying a
# server-touching smoke as `static-only` produces nondeterministic flakes.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Phase NN assertions go below. Examples:
#
#   assert_status 200 "$(api_url /healthz)" "healthz returns 200"
#   assert_json_path '.status' 'ok' "$(api_url /readyz)" "readyz reports status=ok"
#   protocol_call 'sessions/create' '{"tenant":"t1","user":"u1"}' "create session"
#
# Until the phase ships, the script can be empty assertions or a single
# `skip "phase NN: not yet implemented"` to keep preflight green.
# ----------------------------------------------------------------------------

skip "phase 165: smoke skeleton — reasoning-steps rehydration not yet implemented; replace with the state.history planner.decision.ReasoningTrace + tool.* + sequence-interleaving assertions when the phase lands"

smoke_summary
