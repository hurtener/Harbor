#!/usr/bin/env bash
# Phase 05 smoke — events subsystem.
#
# Phase 05 ships the typed event bus interface + the in-memory driver; no
# HTTP / Protocol surface yet (the Protocol exposes the bus in Phase 58+).
# Correctness is verified by `go test ./internal/events/...` under `make
# test`. This smoke runs the exhaustiveness assertion specifically so the
# preflight gate catches a removed / renamed canonical EventType the
# moment it happens.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Run the exhaustiveness test specifically. If the events package is
# absent (pre-Phase-05 builds), `go test` will report "no Go files" or
# similar and we record SKIP. If it builds and the test passes, we
# record OK. If it builds and the test fails, we record FAIL.
if [ -d "internal/events" ]; then
    if go test -run TestEventTypes_Exhaustiveness ./internal/events/... >/tmp/phase-05-go-test.log 2>&1; then
        ok "phase 05: TestEventTypes_Exhaustiveness passes (canonical EventType set intact)"
    else
        fail "phase 05: TestEventTypes_Exhaustiveness failed"
        printf '--- go test output ---\n'
        cat /tmp/phase-05-go-test.log
    fi
else
    skip "phase 05: internal/events absent (driver not yet implemented)"
fi

skip "phase 05: events bus has no HTTP/Protocol surface yet"

smoke_summary
