#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 132 — embed production runner (Stack.RunOnce) + NewRunContext factory.
#
# The RUNNING coverage for this phase lives in scripts/smoke/phase-112b.sh
# leg 6: it compiles the checked-in examples/embed-runonce/ worked example and
# runs the N>=100 concurrent-reuse -race stress plus the NewRunContext
# projection-parity table. That delegation is REAL — verified by grep, not
# assumed — and it is deliberately not duplicated here: running the same
# -race suite twice per preflight buys nothing.
#
# What this script asserts is the DELEGATION ITSELF. A bare `skip` pointing at
# another file is unasserted delegation: strip leg 6 out of phase-112b.sh and
# both scripts stay green while phase 132 loses all preflight coverage. The
# greps below are the tripwire — remove a leg and THIS script names the
# coverage that went missing.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

DELEGATE='scripts/smoke/phase-112b.sh'

if [ ! -f "${DELEGATE}" ]; then
    fail "phase 132: the delegated smoke ${DELEGATE} does not exist — phase 132 has NO preflight coverage"
    smoke_summary
    exit $?
fi

# Leg 6a — the worked example is still compiled by the delegate.
assert_grep_present 'examples/embed-runonce' "${DELEGATE}" \
    'phase 132: delegate still compiles the examples/embed-runonce worked example'

# Leg 6b — the two mandatory tests are still named by the delegate, and the
# delegate still RUNS them under -race.
assert_grep_present 'TestRunOnce_ConcurrentReuse_NoBleedNoLeak' "${DELEGATE}" \
    'phase 132: delegate still pins the N>=100 concurrent-reuse RunOnce -race test'
assert_grep_present 'TestNewRunContext_MemoryParity' "${DELEGATE}" \
    'phase 132: delegate still pins the NewRunContext projection-parity test'
assert_grep_present 'go test -race -run .TestRunOnce' "${DELEGATE}" \
    'phase 132: delegate still executes the RunOnce/NewRunContext suite under -race'

smoke_summary
