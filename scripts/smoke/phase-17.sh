#!/usr/bin/env bash
# Phase 17 smoke skeleton — ArtifactStore iface + InMem + FS drivers.
#
# Phase 17 will ship internal/artifacts: the content-addressed blob
# store for heavy outputs (RFC §6.10). Until that package lands, this
# smoke is skip-only (per scripts/smoke/_template.sh's pattern). The
# implementation PR replaces the skip with the real test invocation:
#
#   if go test -race -count=1 -timeout 90s ./internal/artifacts/... >/dev/null 2>&1; then
#       ok 'phase 17: internal/artifacts tests pass under -race (conformance + facade + drivers)'
#   else
#       fail 'phase 17: internal/artifacts tests failed (run `go test -race ./internal/artifacts/...` for detail)'
#   fi

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 17: ArtifactStore + drivers — implementation pending; smoke skeleton awaits go-test wiring"
skip "phase 17: artifacts has no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
