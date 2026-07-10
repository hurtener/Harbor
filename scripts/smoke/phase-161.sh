#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 161 — session rehydration carries per-turn metadata.
#
# When the phase lands, this asserts the content-free turn metadata is
# present in the state.history read-back under the preflight dev posture —
# no real LLM needed (the mock driver reports synthetic usage, and the
# driver-neutral cost emit this phase builds fires for the mock path too):
#   - drive a scripted run via the `start` method (POST /v1/control/start);
#     poll tasks.get to terminal;
#   - fetch state.history for the session; assert an llm.cost.recorded
#     event whose payload carries Usage + Model keys;
#   - assert a planner.decision event with a DecisionKind key and a
#     populated envelope run;
#   - negative: no raw tool args/results keys anywhere in the page (the
#     content-free boundary, CLAUDE.md section 7).
# The tool-calling (MCP fixture) leg lives in the integration test, not
# here. Done-definition: OK >= 3, FAIL = 0 once the phase ships.
# Until then it SKIPs. Real assertions land with the implementation PR.
#
# Phase NN smoke template heritage below (kept for helper reference).
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

skip "phase 161: smoke skeleton — rehydration metadata not yet implemented; replace with the scripted-run -> state.history metadata-key assertions (cost Usage/Model + planner.decision DecisionKind + no-args negative) when the phase lands"

smoke_summary
