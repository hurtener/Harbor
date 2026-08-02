#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# When Phase 234 lands, this smoke covers pure explicit/default/omitted
# effective-agent selection; signed reach before tenant-local lifecycle lookup;
# `agent_retired` / `agent_retirement_conflict`; history matrix; redacted
# started/progress/completed events; and replay after cleanup or event faults.
# Until then this skeleton intentionally skips so predecessor builds coexist.
skip "phase 234: pending agent-config retirement implementation"
smoke_summary
