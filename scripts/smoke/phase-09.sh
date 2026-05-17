#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 09 smoke — Envelopes, Headers, Identity quadruple.
#
# Phase 09 is a types-only package (internal/runtime/messages) — no
# HTTP / Protocol surface. Correctness is verified by the Go test
# suite under -race. The smoke runs the package's tests directly.
# Phase 10's smoke will subsume this once the engine surface lands.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 60s ./internal/runtime/messages/... >/dev/null 2>&1; then
    ok 'phase 09: internal/runtime/messages tests pass under -race'
else
    fail 'phase 09: internal/runtime/messages tests failed (run `go test -race ./internal/runtime/messages/...` for detail)'
fi

skip "phase 09: messages has no HTTP/Protocol surface yet (lands in Phase 60)"

smoke_summary
