#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 76 — cross-tenant isolation conformance harness.
#
# Phase 76 ships NO Protocol method, REST endpoint, or MCP/A2A surface —
# it is a `_test.go`-only cross-subsystem isolation gate. There is
# nothing for the live-server smoke to hit, so the server surface is a
# documented SKIP per the 404/405/501 → SKIP convention (AGENTS.md §4.2).
#
# The harness itself is exercised by `make test` and the dedicated
# `isolation` CI job (.github/workflows/ci.yml), not by the preflight
# smoke gate. This script's one real assertion is a static
# file-existence check so an accidental deletion of the harness is
# caught here too.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# The harness file must exist — its absence is a regression.
assert_file "test/integration/isolation_conformance_test.go" \
  "phase 76: isolation conformance harness present"

# No Protocol / REST surface in this phase — documented SKIP.
skip "phase 76: no Protocol endpoint — harness runs via 'make test' + the isolation CI job"

smoke_summary
