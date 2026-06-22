#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 92l smoke — MCP transport agent-bound OAuth + typed ErrAuthRequired.
#
# When the surface lands this runs `go test` for internal/tools/drivers/mcp
# (transport OAuth injection + typed-error propagation + static-header
# precedence) and greps cmd/harbor + harbortest/devstack to confirm the
# looksLikeAuthRequired string heuristic is replaced by errors.As against the
# typed *auth.ErrAuthRequired. Parked (planning-only) for now.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 92l: smoke skeleton — replace with real assertions when the MCP transport OAuth surface lands"

smoke_summary
