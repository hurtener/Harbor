#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 17 smoke — ArtifactStore iface + InMem + FS drivers.
#
# Phase 17 ships internal/artifacts: the content-addressed blob store
# for heavy outputs (RFC §6.10). The smoke runs the package test suite
# (which includes the conformance run against both InMem + FS drivers
# and the ScopedArtifacts facade) under -race. There is no HTTP /
# Protocol surface yet (lands in Phase 60+).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 90s ./internal/artifacts/... >/dev/null 2>&1; then
    ok 'phase 17: internal/artifacts tests pass under -race (conformance + facade + InMem + FS drivers)'
else
    fail 'phase 17: internal/artifacts tests failed (run `go test -race ./internal/artifacts/...` for detail)'
fi

skip "phase 17: artifacts has no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
