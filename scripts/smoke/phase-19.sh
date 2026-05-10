#!/usr/bin/env bash
# Phase 19 smoke — ArtifactStore S3-style driver.
#
# Runs the driver's Go tests under -race. When HARBOR_TEST_S3_DSN (or
# the individual HARBOR_TEST_S3_* vars) is unset (most local-dev
# runs), every S3-touching subtest t.Skips cleanly and `go test`
# exits 0; the smoke reports OK because the package compiled and all
# tests resolved (skipped tests count as passing). CI sets
# HARBOR_TEST_S3_DSN against a MinIO service container so the suite
# actually exercises the driver there.
#
# The driver has no HTTP / Protocol surface yet; that lands in Phase
# 60+. The second skip line documents that.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 240s ./internal/artifacts/drivers/s3/... >/dev/null 2>&1; then
    ok 'phase 19: internal/artifacts/drivers/s3 tests pass under -race (skip-clean without HARBOR_TEST_S3_DSN)'
else
    fail 'phase 19: internal/artifacts/drivers/s3 tests failed (run `go test -race ./internal/artifacts/drivers/s3/...` for detail)'
fi

skip "phase 19: artifact-s3 has no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
