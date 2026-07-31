#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 167 — Owner-scoped reconcile for runtime-added connections + providers (D-301).
#
# This phase adds NO live Protocol surface — it is an internal reconcile-view
# scoping fix + the (tenant, agent) owner tag — so the smoke is static
# trip-wires + the owner-tag / owner-scoped-reconcile unit + integration tests.
#
# What this asserts (each check SKIPs on a pre-167 build):
#
#   1. Static trip-wires:
#      - the reconcile-view owner tag type exists (internal/tools/auth/owner.go);
#      - the registry exposes the owner-scoped reconcile accessor
#        RuntimeAddedSources (the bare-name read/dispatch paths stay unchanged);
#      - the TWO in-code reconcile NOTEs are REWRITTEN (not deleted) to describe
#        the OWNER-SCOPED view (projection.go + mcp_detacher.go) — the corrected
#        trip-wire (inverts the earlier "the NOTEs are GONE" assertion).
#   2. unit-tests: the owner-tag + owner-scoped-reconcile packages under -race
#      (TestRegistry_BootServerVisibleToEverySession,
#       TestReconcile_OwnerScoped_NeverDetachesBootOrOtherOwner, + siblings).
#   2b. namespace-guarantee: the OTHER half of D-301 — a cross-owner same-name
#      attach fails LOUD and PRE-DIAL, and the process-global bare-name catalog
#      refuses the collision independently. Counts the PASS lines so a renamed
#      or deleted test FAILS instead of passing vacuously ("no tests to run" is
#      a `go test` success).
#   3. The §17.1 integration test (real drivers, two owners + a boot server).
#
# Done-definition: OK >= 3, FAIL = 0.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_or_skip() {
    local pattern="$1" file="$2" desc="$3"
    if [ ! -f "${file}" ]; then
        skip "${desc}: ${file} not found (Phase 167 not yet implemented)"
        return
    fi
    if grep -qE "${pattern}" "${file}" 2>/dev/null; then
        ok "${desc}"
    else
        skip "${desc}: pattern '${pattern}' absent (Phase 167 not yet implemented)"
    fi
}

# ----------------------------------------------------------------------------
# 1. Static trip-wires.
# ----------------------------------------------------------------------------

assert_or_skip 'type Owner struct' \
    "internal/tools/auth/owner.go" \
    "static: the (tenant, agent) reconcile-view owner tag type exists"

assert_or_skip 'func \(r \*Registry\) RuntimeAddedSources\(owner auth\.Owner\)' \
    "internal/tools/drivers/mcp/registry.go" \
    "static: the registry exposes the owner-scoped RuntimeAddedSources accessor"

# The rewritten NOTEs — assert they MENTION the owner-scoped reconcile (they are
# rewritten, NOT deleted). This inverts the discredited "the NOTEs are GONE"
# trip-wire from the rejected full-triple-keying draft.
assert_or_skip 'owner-scoped reconcile' \
    "internal/runtime/agentcfg/projection/projection.go" \
    "static: the projection reconcile NOTE describes the owner-scoped view (rewritten, not deleted)"

assert_or_skip 'owner-scoped reconcile' \
    "internal/runtime/serve/mcp_detacher.go" \
    "static: the detacher AttachedSources NOTE describes the owner-scoped view (rewritten, not deleted)"

# ----------------------------------------------------------------------------
# 2. Unit tests — the owner-tag + owner-scoped-reconcile packages under -race.
# ----------------------------------------------------------------------------

if [ ! -f "internal/tools/drivers/mcp/owner_scoped_test.go" ]; then
    skip "unit-tests: Phase 167 not yet implemented"
elif go test -race -count=1 -timeout 240s \
    -run 'TestRegistry_BootServerVisibleToEverySession|TestRegistry_RuntimeAdd|TestReconcile_OwnerScoped|TestReconcileConnections' \
    ./internal/tools/drivers/mcp/... \
    ./internal/runtime/agentcfg/projection/... >/dev/null 2>&1; then
    ok "unit-tests: owner-tag + owner-scoped-reconcile packages pass under -race"
else
    fail "unit-tests: Phase 167 package tests failed (run: go test -race ./internal/tools/drivers/mcp/... ./internal/runtime/agentcfg/projection/...)"
fi

# ----------------------------------------------------------------------------
# 2b. The D-301 NAMESPACE guarantee — a cross-owner same-name attach fails loud
#     and PRE-DIAL; the bare-name catalog refuses the collision independently.
#
# The count check is deliberate. `go test -run <pattern>` with a pattern that
# matches NOTHING prints "no tests to run" and exits 0, so an arm that only
# checked the exit code would report OK forever after a rename — the vacuous
# instrument this wave keeps finding. Requiring the exact PASS count makes a
# renamed, deleted, or skipped test a FAIL.
# ----------------------------------------------------------------------------

GUARD_FILE="internal/tools/drivers/mcp/cross_owner_name_collision_test.go"
GUARD_EXPECTED=3

if [ ! -f "${GUARD_FILE}" ]; then
    skip "namespace-guarantee: ${GUARD_FILE} absent (guard not yet landed)"
else
    guard_out="$(go test -race -count=1 -timeout 240s -v \
        -run 'TestAttach_CrossOwnerSameName_RefusedPreDial|TestAttach_CrossOwnerDistinctNames_BothAttach|TestAttach_CrossOwnerSameName_CatalogIsTheSecondGate' \
        ./internal/tools/drivers/mcp/ 2>&1 || true)"
    guard_pass="$(printf '%s\n' "${guard_out}" | grep -c '^--- PASS: TestAttach_CrossOwner' || true)"
    if [ "${guard_pass}" -eq "${GUARD_EXPECTED}" ]; then
        ok "namespace-guarantee: cross-owner same-name attach fails loud + pre-dial (${guard_pass}/${GUARD_EXPECTED} D-301 guards pass)"
    else
        fail "namespace-guarantee: expected ${GUARD_EXPECTED} passing D-301 namespace guards, got ${guard_pass} (run: go test -race -v -run TestAttach_CrossOwner ./internal/tools/drivers/mcp/)"
    fi
fi

# ----------------------------------------------------------------------------
# 3. §17.1 integration test (real drivers, two owners + a boot server).
# ----------------------------------------------------------------------------

if [ ! -f "test/integration/phase167_owner_scoped_reconcile_test.go" ]; then
    skip "e2e: integration test absent (Phase 167 not yet implemented)"
elif go test -race -count=1 -timeout 300s -run 'TestE2E_Phase167' ./test/integration/ >/dev/null 2>&1; then
    ok "e2e: owner-scoped reconcile never detaches boot / other owner (real drivers)"
else
    fail "e2e: Phase 167 integration tests failed (run: go test -race -run TestE2E_Phase167 ./test/integration/)"
fi

smoke_summary
