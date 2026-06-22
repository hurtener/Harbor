#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 92o smoke — run-start connection reconciliation (closes #375 gap 2).
#
# The reconciler (internal/runtime/agentcfg/projection.ReconcileConnections) is a
# run-start projection helper exercised by Go tests, not a live Protocol endpoint —
# hence `unit-tests`. Until the phase implements its surface this is a single skip so
# preflight stays green; on ship, replace the skip with a `go test` assertion against
# internal/runtime/agentcfg/projection covering the reconciliation unit + concurrency
# tests.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 92o: smoke skeleton — replace with a go test assertion against internal/runtime/agentcfg/projection when ReconcileConnections lands"

smoke_summary
