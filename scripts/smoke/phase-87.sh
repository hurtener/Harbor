#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 87 smoke — durable TaskService backend.
#
# Conventions (AGENTS.md §4.2):
#   - The surface is a runtime-internal §4.4 driver (no new Protocol/REST
#     surface), so the assertions are STATIC source greps.
#   - Until the phase ships, each guard SKIPs (the surface is absent); when
#     the durable driver lands, the guards flip to OK. No FAIL pre-ship.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.
#
# Phase 87 adds a `durable` TaskRegistry driver (internal/tasks/drivers/durable)
# that persists task/group/patch records through a state.StateStore so
# background tasks survive a Runtime restart — mirroring the Phase 57 durable
# events driver, passing internal/tasks/conformancetest verbatim. The
# in-process driver stays the default; `durable` is opt-in via tasks.driver.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

DRIVER='internal/tasks/drivers/durable'

# ----------------------------------------------------------------------------
# 1. The durable TaskRegistry driver package exists with a constructor.
# ----------------------------------------------------------------------------
if [ -d "${DRIVER}" ] && grep -rqE 'func New\(' "${DRIVER}"/*.go 2>/dev/null; then
    ok 'phase 87: internal/tasks/drivers/durable exists with a New(...) constructor'
else
    skip 'phase 87: durable TaskRegistry driver not yet implemented'
fi

# ----------------------------------------------------------------------------
# 2. It self-registers under "durable" (§4.4 driver registry).
# ----------------------------------------------------------------------------
if grep -rqE 'Register\(\s*"durable"' "${DRIVER}"/*.go 2>/dev/null; then
    ok 'phase 87: durable driver self-registers under "durable"'
else
    skip 'phase 87: durable driver registration not yet present'
fi

# ----------------------------------------------------------------------------
# 3. It persists through a state.StateStore (the reused triad, no new seam).
# ----------------------------------------------------------------------------
if grep -rqE 'state\.StateStore' "${DRIVER}"/*.go 2>/dev/null; then
    ok 'phase 87: durable driver persists through state.StateStore'
else
    skip 'phase 87: StateStore-backed persistence not yet present'
fi

# ----------------------------------------------------------------------------
# 4. It passes the shared conformance suite (the D-031 gate every driver inherits).
# ----------------------------------------------------------------------------
if grep -rqE 'conformancetest\.Run' "${DRIVER}"/*_test.go 2>/dev/null; then
    ok 'phase 87: durable driver runs the tasks conformance suite'
else
    skip 'phase 87: conformance hook not yet present'
fi

# ----------------------------------------------------------------------------
# 5. The production aggregator blank-imports the durable task driver (§4.4).
# ----------------------------------------------------------------------------
if grep -rqE 'tasks/drivers/durable' internal/drivers/prod/*.go 2>/dev/null; then
    ok 'phase 87: internal/drivers/prod aggregates the durable task driver'
else
    skip 'phase 87: durable task driver not yet in the prod aggregator'
fi

# ----------------------------------------------------------------------------
# 6. The config validator accepts tasks.driver: durable.
# ----------------------------------------------------------------------------
if grep -rqE 'allowedTasksDrivers.*"durable"|"durable".*allowedTasksDrivers' internal/config/*.go 2>/dev/null \
    || grep -rqE '"inprocess".*"durable"|"durable".*"inprocess"' internal/config/validate.go 2>/dev/null; then
    ok 'phase 87: config validator accepts tasks.driver: durable'
else
    skip 'phase 87: tasks.driver: durable not yet in the config allowlist'
fi

smoke_summary
