#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 149 smoke — HTTP-manifest boot loader wiring (D-279).
#
# Unit-tests-class (the phase-64a / phase-142 precedent): the preflight dev
# server boots once with a fixed config carrying `http_manifests: []`, so the
# boot path is exercised by the -race integration test and the validate flip
# by the built binary's `validate` subcommand against temp configs — no
# network endpoint is touched, keeping this in the parallel batch.
#
# When the phase ships, this script asserts:
#   1. `go test -race` for internal/config (the validate-flip tests) and the
#      phase-149 integration test (`-run Phase149 ./test/integration/...`).
#   2. `bin/harbor validate` ACCEPTS a temp config declaring
#      `tools.http_manifests` pointing at a real temp manifest (SKIP — not
#      FAIL — if the binary still emits the pre-149 "not loaded at boot yet"
#      rejection, per the §4.2 coexistence convention).
#   3. `bin/harbor validate` REJECTS a temp config whose relative manifest
#      entry escapes the config directory (§7 rule 5), naming
#      `tools.http_manifests` in the output.
#   4. Static greps: internal/runtime/assemble/assemble.go references
#      LoadManifest/RegisterManifest; internal/config/validate.go no longer
#      contains the "not loaded at boot yet" rejection string.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 149: smoke skeleton — replace with real assertions when the HTTP-manifest boot loader lands"

smoke_summary
