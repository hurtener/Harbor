#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 163 — windowed-reads honesty pair (flows since/until + retention
# horizons on runtime.health).
#
# When the phase lands, this asserts (two health-side assertions so the
# done-definition is meetable even when the dev config declares no flows):
#   - runtime.health carries the additive `retention` block;
#   - after a scripted run (the `start` method) the block's `events` entry
#     carries a non-empty, RFC-3339-parsable oldest_retained_at;
#   - flows.runs.list accepts since/until without invalid_request
#     (skip_if_404 when the dev config declares no flows — the bounds
#     semantics are then covered by the integration test).
# Done-definition: OK >= 2, FAIL = 0 (achievable from the two health
# assertions alone) once the phase ships.
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

skip "phase 163: smoke skeleton — flows since/until + retention horizons not yet implemented; replace with the runtime.health retention-block + flows-bounds-acceptance assertions when the phase lands"

smoke_summary
