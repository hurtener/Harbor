#!/usr/bin/env bash
# Phase 19 smoke skeleton — ArtifactStore S3-style driver.
#
# Phase 19 will ship internal/artifacts/drivers/s3 — the third leg of
# the Harbor artifact persistence triad and the only V1 driver that
# implements the optional Presigner capability (RFC §6.10). Built on
# aws-sdk-go-v2; tests gate on HARBOR_TEST_S3_DSN against a MinIO
# container in CI. Until the implementation lands, this smoke is
# skip-only.
#
# The implementation PR replaces the skip with the real test
# invocation:
#
#   if go test -race -count=1 -timeout 240s ./internal/artifacts/drivers/s3/... >/dev/null 2>&1; then
#       ok 'phase 19: artifact-s3 driver tests pass under -race'
#   else
#       fail 'phase 19: artifact-s3 driver tests failed'
#   fi

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 19: artifact-s3 driver — implementation pending; smoke skeleton awaits go-test wiring"
skip "phase 19: artifact-s3 has no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
