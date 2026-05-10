#!/usr/bin/env bash
# Phase 18 smoke skeleton — ArtifactStore SQLite-blob + Postgres-blob.
#
# Phase 18 will ship internal/artifacts/drivers/{sqlite,postgres} —
# the durable artifact triad (RFC §6.10, §9). Both drivers inherit
# internal/artifacts/conformancetest.Run verbatim. Until the
# implementation lands, this smoke is skip-only. The implementation
# PR replaces the skip with the real test invocation:
#
#   if go test -race -count=1 -timeout 120s ./internal/artifacts/drivers/sqlite/... ./internal/artifacts/drivers/postgres/... >/dev/null 2>&1; then
#       ok 'phase 18: artifact-blob drivers tests pass under -race'
#   else
#       fail 'phase 18: artifact-blob driver tests failed'
#   fi

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 18: artifact-blob drivers — implementation pending; smoke skeleton awaits go-test wiring"
skip "phase 18: artifact-blob has no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
