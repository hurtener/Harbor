#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 160 — sdk/server + `harbor scaffold --with-server` + parity gate.
#
# When the phase lands, this asserts the external-serving path works:
#   - `harbor scaffold --with-server` into a temp dir as an EXTERNAL module;
#   - `go build` the generated cmd/<agent> against the sdk/ facade;
#   - boot it with a `harbor token`-minted JWKS production posture;
#   - /healthz returns 200 and the generated custom tool dispatches through
#     the catalog.
# Degradation: SKIP on a build whose `harbor scaffold` lacks `--with-server`
# (unknown-flag error -> SKIP, mirroring the 404/405/501 convention for a CLI
# flag). Until then it SKIPs. Real assertions land with the implementation PR.
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

skip "phase 160: smoke skeleton — sdk/server + scaffold --with-server not yet implemented; replace with scaffold->build->token-minted-boot->/healthz+tool-dispatch assertions when the phase lands"

smoke_summary
