#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 92p — Spec-faithful MCP OAuth discovery (401 → RFC 9728 → AS).
# Planning-parked: this skeleton keeps preflight green until the phase ships.
# When the surface lands, replace the skip with: assert the discovery entry
# point + the RFC 9728 protected-resource-metadata parse symbols exist, assert
# the spec-derived fixture is present (not a hand blob), and run the
# internal/tools/auth + internal/tools/drivers/mcp discovery unit tests. Live
# discovery against a real server is the env-gated HARBOR_LIVE_* probe.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 92p: smoke skeleton — replace with real assertions when the phase implements its surface"

smoke_summary
