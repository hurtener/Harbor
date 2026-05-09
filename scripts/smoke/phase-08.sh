#!/usr/bin/env bash
# Phase 08 smoke — SessionRegistry + lifecycle + GC.
#
# Phase 08 lands internal/sessions as a typed wrapper over Phase 07's
# StateStore. There is no HTTP / Protocol surface yet (sessions Protocol
# methods land in Phase 60); correctness is verified by `go test -race
# ./internal/sessions/...` under `make test`.
#
# This script SKIPs the HTTP-surface assertions and lets the test gate
# carry the load — same shape as phase-05.sh and phase-07.sh.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 08: sessions — Go package only; validated by go test ./internal/sessions/..."

smoke_summary
