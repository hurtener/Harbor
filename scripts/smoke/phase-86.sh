#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 86 smoke — durable distributed bus driver.
#
# Conventions (AGENTS.md §4.2):
#   - The surface is a runtime-internal §4.4 driver (no new Protocol/REST
#     surface), so the assertions are STATIC source greps.
#   - Until the phase ships, each guard SKIPs (the surface is absent); when
#     the durable bus driver lands, the guards flip to OK. No FAIL pre-ship.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.
#
# Phase 86 adds a `durable` MessageBus driver (internal/distributed/drivers/durable)
# that persists every BusEnvelope through a state.StateStore and projects it onto
# the local events.EventBus, with a background poller for cross-instance fan-out —
# mirroring the Phase 57 durable events driver and the Phase 87 durable tasks
# driver, passing internal/distributed/conformancetest.RunBus verbatim. The
# loopback driver stays the default; `durable` is opt-in via distributed.bus_driver.
# StateStore-backed (Postgres-as-queue on a shared Postgres store); NATS / Redis
# Streams are deferred to later phases.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

DRIVER='internal/distributed/drivers/durable'

# ----------------------------------------------------------------------------
# 1. The durable MessageBus driver package exists with a constructor.
# ----------------------------------------------------------------------------
if [ -d "${DRIVER}" ] && grep -rqE 'func New\(' "${DRIVER}"/*.go 2>/dev/null; then
    ok 'phase 86: internal/distributed/drivers/durable exists with a New(...) constructor'
else
    skip 'phase 86: durable MessageBus driver not yet implemented'
fi

# ----------------------------------------------------------------------------
# 2. It self-registers under "durable" (§4.4 driver registry).
# ----------------------------------------------------------------------------
if grep -rqE 'RegisterBus\(\s*"durable"' "${DRIVER}"/*.go 2>/dev/null; then
    ok 'phase 86: durable bus driver self-registers under "durable"'
else
    skip 'phase 86: durable bus driver registration not yet present'
fi

# ----------------------------------------------------------------------------
# 3. It persists through a state.StateStore (the reused triad, no new seam).
# ----------------------------------------------------------------------------
if grep -rqE 'state\.StateStore' "${DRIVER}"/*.go 2>/dev/null; then
    ok 'phase 86: durable bus driver persists through state.StateStore'
else
    skip 'phase 86: StateStore-backed persistence not yet present'
fi

# ----------------------------------------------------------------------------
# 4. It passes the shared bus conformance suite (the D-031 gate every driver inherits).
# ----------------------------------------------------------------------------
if grep -rqE 'conformancetest\.RunBus' "${DRIVER}"/*_test.go 2>/dev/null; then
    ok 'phase 86: durable bus driver runs the distributed conformance suite'
else
    skip 'phase 86: conformance hook not yet present'
fi

# ----------------------------------------------------------------------------
# 5. The production aggregator blank-imports the durable bus driver (§4.4).
# ----------------------------------------------------------------------------
if grep -rqE 'distributed/drivers/durable' internal/drivers/prod/*.go 2>/dev/null; then
    ok 'phase 86: internal/drivers/prod aggregates the durable bus driver'
else
    skip 'phase 86: durable bus driver not yet in the prod aggregator'
fi

# ----------------------------------------------------------------------------
# 6. The config validator accepts distributed.bus_driver: durable.
# ----------------------------------------------------------------------------
if grep -rqE 'allowedDistributedBusDrivers.*"durable"|"durable".*allowedDistributedBusDrivers' internal/config/*.go 2>/dev/null \
    || grep -rqE '"loopback".*"durable"|"durable".*"loopback"' internal/config/validate.go 2>/dev/null; then
    ok 'phase 86: config validator accepts distributed.bus_driver: durable'
else
    skip 'phase 86: distributed.bus_driver: durable not yet in the config allowlist'
fi

smoke_summary
