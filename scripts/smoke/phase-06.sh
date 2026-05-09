#!/usr/bin/env bash
# Phase 06 smoke — bus replay + ring buffer + cursor.
#
# Phase 06 extends internal/events with the Replayer capability interface
# and an in-memory ring buffer on the inmem driver. There is no HTTP /
# Protocol surface yet (Protocol exposure lands in Phase 60); correctness
# is verified by `go test -race ./internal/events/...` under `make test`.
#
# This script SKIPs the HTTP-surface assertions and lets the test gate
# carry the load — same shape as phase-05.sh and phase-07.sh.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 06: events replay — Go package only; validated by go test ./internal/events/..."

smoke_summary
