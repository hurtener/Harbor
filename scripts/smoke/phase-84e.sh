#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# D-477 supersedes Phase 84e's native session-memory semantic recall.
# Keep a real refusal/consumer gate, not a passing skip for missing old symbols.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 300s ./internal/config \
    -run '^TestMemoryRetirement_RejectsSemanticIndexConfiguration$'; then
    ok "removed semantic-memory YAML and environment settings fail explicitly"
else
    fail "removed semantic-memory configuration was not rejected"
fi

if go test -race -count=1 -timeout 300s ./test/integration \
    -run '^TestE2E_CallerMemory_'; then
    ok "external caller-memory admission, isolation and composition remain intact"
else
    fail "external caller-memory consumer regression"
fi

smoke_summary
