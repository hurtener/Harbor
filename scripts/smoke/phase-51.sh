#!/usr/bin/env bash
# Phase 51 smoke — Pause-state serialise contract (fail-loud)
# (RFC §6.3 + §3.4; master-plan Phase 51 detail block; D-069).
#
# Phase 51 closes the fail-loudly serialise contract for the pause
# record's OWN wire envelope. Phase 43 closed it for the trajectory;
# Phase 50 propagated trajectory.ErrUnserializable out of Request for
# the trajectory — but reached the pause record's caller-controlled
# Payload field via a bare json.Marshal (loud, but without the
# actionable field path RFC §3.4 mandates). Phase 51 ships
# SerializeRecord / DeserializeRecord (the fail-loud pair for the
# checkpointRecord envelope), exports the Phase 43 reflective walker as
# trajectory.ValidateEncodable so the contract is SHARED not forked
# (CLAUDE.md §13), and stamps/enforces format_version: 1.
#
# This is a code-only library phase: no Protocol surface lands until
# the task.pause / task.resume Protocol methods (a later phase), so the
# HTTP/Protocol assertions skip with a reason per the
# 404/405/501 → SKIP convention.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

PR_PKG="internal/runtime/pauseresume"

# Run the pauseresume package tests under -race. Covers the Phase 51
# negative-test gate (non-encodable Payload leaves → ErrUnserializable
# with a field path; format_version guard; byte-stable round-trip; the
# black-box §11 test through Coordinator.Request) PLUS the Phase 50
# surface (Coordinator round-trip, restart-survival, D-025
# concurrent-reuse — which now exercises the Phase 51 serialise path on
# every Request).
if go test -race -count=1 -timeout 180s ./${PR_PKG}/... >/dev/null 2>&1; then
    ok 'phase 51: internal/runtime/pauseresume tests pass under -race (pause-record serialise negative gate + format_version guard + byte-stable round-trip + Phase 50 surface)'
else
    fail 'phase 51: pauseresume tests failed (run `go test -race ./internal/runtime/pauseresume/...` for detail)'
fi

# Run the trajectory package tests under -race — Phase 51 exports the
# Phase 43 reflective walker as trajectory.ValidateEncodable and
# re-points Trajectory.Serialize's pre-flight at it. The full Phase 43
# suite must still pass unchanged (the trajectory's observable contract
# is byte-for-byte the same).
if go test -race -count=1 -timeout 180s ./internal/planner/trajectory/... >/dev/null 2>&1; then
    ok 'phase 51: internal/planner/trajectory tests pass under -race (ValidateEncodable export — Phase 43 contract unchanged)'
else
    fail 'phase 51: trajectory tests failed after ValidateEncodable export (run `go test -race ./internal/planner/trajectory/...` for detail)'
fi

# Run the Phase 51 conformance / integration test (real state.StateStore
# across in-mem + SQLite into the real Coordinator: conformance-with-
# phase-43, no-half-persist, format_version load guard).
if go test -race -count=1 -timeout 180s -run 'TestE2E_PauseSerialise' ./test/integration/... >/dev/null 2>&1; then
    ok 'phase 51: pause-serialise integration test passes under -race (conformance-with-phase-43 + no-half-persist + format_version guard)'
else
    fail 'phase 51: pause-serialise integration test failed (run `go test -race -run TestE2E_PauseSerialise ./test/integration/...` for detail)'
fi

# Static guard: the Phase 51 serialise contract surface must declare
# SerializeRecord / DeserializeRecord / FormatVersion.
PR_FILE="${PR_PKG}/pauserecord.go"
if [[ ! -f "${PR_FILE}" ]]; then
    fail "phase 51: ${PR_FILE} missing"
else
    missing_symbols=""
    if ! grep -q 'func SerializeRecord(' "${PR_FILE}"; then
        missing_symbols="${missing_symbols} SerializeRecord"
    fi
    if ! grep -q 'func DeserializeRecord(' "${PR_FILE}"; then
        missing_symbols="${missing_symbols} DeserializeRecord"
    fi
    if ! grep -q 'FormatVersion = ' "${PR_FILE}"; then
        missing_symbols="${missing_symbols} FormatVersion"
    fi
    if [[ -n "${missing_symbols}" ]]; then
        fail "phase 51: pauserecord.go missing symbols:${missing_symbols}"
    else
        ok 'phase 51: pauseresume declares SerializeRecord / DeserializeRecord / FormatVersion (master-plan Phase 51 surface)'
    fi
fi

# §13 anti-parallel guard: Phase 51 must SHARE the Phase 43 walker, not
# fork a second one. The shared entry point is trajectory.ValidateEncodable;
# pauserecord.go must reference it (the observable proof the walker is
# shared, not copy-pasted — D-069).
if grep -q 'trajectory.ValidateEncodable' "${PR_FILE}"; then
    ok 'phase 51: pauserecord.go routes through trajectory.ValidateEncodable (the Phase 43 walker is SHARED, not forked — §13, D-069)'
else
    fail 'phase 51: pauserecord.go does not reference trajectory.ValidateEncodable — a forked fail-loudly walker is the §13 two-parallel-implementations anti-pattern (D-069)'
fi

# Import-graph guard: the pauseresume package must NOT import the
# Console — the Runtime never imports Console code (CLAUDE.md §13).
if grep -rIn --include='*.go' '"github.com/hurtener/Harbor/web/console' "${PR_PKG}/" 2>/dev/null | grep -q .; then
    fail 'phase 51: internal/runtime/pauseresume imports the Console — the Runtime never imports Console code (CLAUDE.md §13)'
else
    ok 'phase 51: internal/runtime/pauseresume does not import the Console (Runtime/Console boundary preserved)'
fi

# §13 / §4.4 guard: durability still rides on the existing
# state.StateStore; Phase 51 must NOT mint a parallel persistence-driver
# tree (D-067).
if [[ -d "${PR_PKG}/drivers" ]]; then
    fail 'phase 51: internal/runtime/pauseresume/drivers/ exists — durability must ride on state.StateStore, not a parallel persistence seam (D-067)'
else
    ok 'phase 51: no parallel persistence-driver tree under internal/runtime/pauseresume (durability rides on state.StateStore — D-067)'
fi

# Phase 51 ships no Protocol/HTTP surface — the task.pause /
# task.resume Protocol methods land in a later phase. Skip per the
# 404/405/501 → SKIP convention.
skip "phase 51: the pause-state serialise contract is a code-only runtime library deepening; the task.pause / task.resume Protocol methods land in a later phase"

smoke_summary
