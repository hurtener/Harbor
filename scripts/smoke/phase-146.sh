#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 146 smoke — per-task structured output (the `output_schema` field
# on the `start` request → validated `answer_payload` on the task
# envelope, D-276).
#
# Planned assertions once the phase ships:
#   1. Live (always-on): POST /v1/control/start with a NON-COMPILING
#      output_schema → non-200 + CodeInvalidRequest-shaped error (the
#      Protocol-edge gate; no LLM dependency).
#   2. Live (degradable, phase-106 pattern): start a schema-constrained
#      task, poll /v1/tasks/get to terminal; on `complete` assert
#      result_inline carries an `answer_payload` conforming to the sent
#      schema; on `failed` under a keyless/mock-LLM preflight env → SKIP
#      with the failure shape logged.
#   3. Static: `output_schema` present in the committed
#      web/console/src/lib/protocol/wire-manifest.gen.json (StartRequest)
#      AND in the generated docs/site/protocol/types.md (D-209 regen);
#      exactly ONE terminal-validation implementation (the shared runctx
#      envelope builder) references ErrOutputInvalid on the run edges.
#   4. Unit-test leg: go test -race over ./internal/tasks/...,
#      ./internal/runtime/runctx/... and -run '^TestE2E_Phase146_'
#      ./test/integration/...

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 146: smoke skeleton — replace with real assertions when the phase implements its surface"

smoke_summary
