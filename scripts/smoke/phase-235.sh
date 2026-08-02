#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

P235_TMP="$(mktemp -d "${TMPDIR:-/tmp}/harbor-phase235.XXXXXX")"
trap 'rm -rf "${P235_TMP}"' EXIT

P235_LIST="${P235_TMP}/test-list.log"
if go test -list '^TestE2E_WaveV126$' ./test/integration >"${P235_LIST}" 2>&1 && grep -qx 'TestE2E_WaveV126' "${P235_LIST}"; then
    ok 'phase 235: named v1.26 checkpoint test is present'
else
    fail 'phase 235: named v1.26 checkpoint test is absent or renamed'
fi

P235_LOG="${P235_TMP}/wave.log"
if go test -v -race -count=1 ./test/integration -run '^TestE2E_WaveV126$' >"${P235_LOG}" 2>&1; then
    for leg in reach cas erasure isolation; do
        if grep -qE "^[[:space:]]*--- PASS: TestE2E_WaveV126/${leg} \(" "${P235_LOG}"; then
            ok "phase 235: ${leg} checkpoint leg ran"
        else
            fail "phase 235: ${leg} checkpoint leg did not run"
        fi
    done
else
    fail 'phase 235: v1.26 checkpoint integration test failed'
    tail -60 "${P235_LOG}" | sed 's/^/    /'
fi
smoke_summary
