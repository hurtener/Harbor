#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 142 smoke — external tool-credential provisioning: the `tokenexchange`
# OAuth driver (D-271).
#
# This is a unit-tests-class smoke (no HTTP/Protocol surface of its own — the
# driver is an internal seam consumed by the catalog WrapWithOAuth path). It:
#   1. runs `go test -race` for the driver package + the phase-142 integration
#      test;
#   2. greps that `internal/drivers/prod` carries the driver's blank import
#      (D-196 — never cmd/harbor);
#   3. greps that no NON-TEST file in the driver package references
#      `.Put(` — brokered tokens are never persisted.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1: driver tests under -race.
if go test -race -count=1 -timeout 240s \
    ./internal/tools/auth/drivers/tokenexchange/... >/dev/null 2>&1; then
    ok 'phase 142: tokenexchange driver tests pass under -race'
else
    fail 'phase 142: tokenexchange driver tests failed (run `go test -race ./internal/tools/auth/drivers/tokenexchange/...`)'
fi

# 1b: the phase-142 integration test under -race.
if go test -race -count=1 -timeout 240s -run 'Phase142' ./test/integration/... >/dev/null 2>&1; then
    ok 'phase 142: token-exchange integration test passes under -race'
else
    fail 'phase 142: token-exchange integration test failed (run `go test -race -run Phase142 ./test/integration/...`)'
fi

# 2: the D-196 blank import lives in the production aggregator.
assert_grep_present \
    'internal/tools/auth/drivers/tokenexchange' \
    internal/drivers/prod/prod.go \
    'phase 142: tokenexchange blank import in internal/drivers/prod (D-196)'

# 3: no non-test file in the driver package persists a brokered token.
PROD_PUT_HITS="$(grep -rn '\.Put(' internal/tools/auth/drivers/tokenexchange/ \
    --include='*.go' | grep -v '_test.go' || true)"
if [[ -z "${PROD_PUT_HITS}" ]]; then
    ok 'phase 142: no TokenStore.Put in non-test driver code (brokered tokens never persisted)'
else
    fail "phase 142: TokenStore.Put referenced in driver production code: ${PROD_PUT_HITS}"
fi

smoke_summary
