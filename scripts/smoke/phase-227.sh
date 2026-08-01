#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 227 — one declared-name projection for every model-authored tool path.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source scripts/smoke/common.sh
assert_file docs/plans/phase-227-declared-tool-resolution.md 'phase 227: corrective plan exists'
assert_grep_present '^## D-389 ' docs/decisions.md 'phase 227: declared-tool decision is recorded'

P227_TMP="$(mktemp -d "${TMPDIR:-/tmp}/harbor-phase-227.XXXXXX")"
trap 'rm -rf "${P227_TMP}"' EXIT
P227_GOLOG="${P227_TMP}/go-test.log"

assert_go_tests_pass "${P227_GOLOG}" './internal/planner/react/' \
    'phase 227: planner refuses raw or undeclared names without partial batch state' \
    TestProjectResponse_RejectsRawCatalogKeyBeforeDispatch \
    TestProjectResponse_SerializationRejectsUndeclaredNameAtomically \
    TestReActPlanner_TurnProjectionSurvivesConcurrentCatalogMutation \
    TestResolveDeclaredToolName_DroppedColliderIsNeverDispatched \
    TestResolveDeclaredToolName_ReservedControlNeverResolvesToCatalogTool \
    TestResolveDeclaredToolName_ConcurrentReuse

assert_go_tests_pass "${P227_GOLOG}" './internal/tools/builtin/' \
    'phase 227: discovery, lookup, and action share collision, reservation, and scope policy' \
    TestBuiltins_ModelAuthoredDeclaredNameNeverDispatchesRawCollider \
    TestBuiltins_ReservedNameColliderIsNeverAdvertisedOrCallable \
    TestBuiltins_DeclaredProjectionEnforcesGrantedScopes \
    TestDeclarativeAction_DeclaredProjection_ConcurrentReuse

assert_go_tests_pass "${P227_GOLOG}" './internal/tools/' \
    'phase 227: immutable projection has one reachable winner under concurrent reuse' \
    TestModelToolNameProjection_CollisionWinnerIsOnlyReachableTool \
    TestModelToolNameProjection_ReservedNameCannotResolveCatalogCollider \
    TestModelToolNameProjection_ConcurrentReuse_NoCrossTalk
smoke_summary
