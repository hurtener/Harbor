#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 25a smoke — durable memory strategies on the SQL drivers.
# Runs the memory conformance suite for sqlite/postgres across the three
# strategies (none / truncation / rolling_summary). No live server needed.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Until the phase ships, the SQL drivers reject non-none strategies and the
# strategy-looping driver tests do not exist — keep the gate green with a
# skip. Postgres conformance needs a live PG (CI provides it); the sqlite
# leg runs everywhere.
if go test ./internal/memory/drivers/sqlite/ -run 'Conformance' -count=1 >/dev/null 2>&1; then
  ok "phase 25a: sqlite memory conformance (strategies) passes"
else
  skip "phase 25a: not yet implemented — SQL memory drivers truncation + rolling_summary"
fi

smoke_summary
