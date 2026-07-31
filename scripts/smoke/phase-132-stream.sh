#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 132-stream — WithStream sink on Stack.RunOnce.
#
# The RUNNING coverage for this phase lives in scripts/smoke/phase-112b.sh
# leg 7: it pins `func WithStream`, the ordered-chunks-before-envelope e2e
# test and the no-cross-run-chunk-bleed assertion, and runs them under -race.
# That delegation is REAL — verified by grep, not assumed — and is not
# duplicated here.
#
# What this script asserts is the DELEGATION ITSELF. A bare `skip` pointing at
# another file is unasserted delegation: strip leg 7 out of phase-112b.sh and
# both scripts stay green while phase 132-stream loses all preflight coverage.
#
# Note this script is not even required to exist — scripts/drift-audit.sh
# derives the paired smoke for docs/plans/phase-132-stream-withstream.md as
# `phase-132.sh`. Deleting it was available and was rejected: a tripwire that
# names the missing leg is worth more than one fewer file.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

DELEGATE='scripts/smoke/phase-112b.sh'

if [ ! -f "${DELEGATE}" ]; then
    fail "phase 132-stream: the delegated smoke ${DELEGATE} does not exist — phase 132-stream has NO preflight coverage"
    smoke_summary
    exit $?
fi

assert_grep_present 'func WithStream' "${DELEGATE}" \
    'phase 132-stream: delegate still pins the WithStream sink declaration'
assert_grep_present 'TestRunOnce_WithStream_ChunksArriveBeforeEnvelope' "${DELEGATE}" \
    'phase 132-stream: delegate still pins the ordered-chunks-before-envelope e2e test'
assert_grep_present 'CROSS-RUN CHUNK BLEED' "${DELEGATE}" \
    'phase 132-stream: delegate still pins the no-cross-run-chunk-bleed assertion'
assert_grep_present 'go test -race -run .TestRunOnce_WithStream' "${DELEGATE}" \
    'phase 132-stream: delegate still executes the WithStream streaming suite under -race'

smoke_summary
