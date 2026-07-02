#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 148 smoke — MCP southbound per-identity OAuth bearer + `_meta`
# provenance enrichment (D-278).
#
# This is a unit-tests-class smoke (the binding + injection is an internal
# tool-transport seam consumed per-call — no HTTP/Protocol surface of its
# own beyond the already-covered agent_config verbs). It:
#   1. runs `go test -race` for the MCP driver (oauth binding + per-call
#      bearer + buildIdentityMeta provenance + the D-025 N>=128 mixed-triple
#      no-token-bleed test), the agent-provenance ctx seam, and the config
#      validation table;
#   2. runs the phase-148 integration test under -race (142 broker fixture +
#      go-sdk streamable-HTTP MCP fixture: rotation, provenance, cold path,
#      broker-refusal fail-loud with ZERO unauthenticated calls, the
#      consent_required typed-error park leg);
#   3. static greps: the MCP driver imports ONLY the `internal/tools/auth`
#      interface package — never a concrete `tools/auth/drivers/*` driver
#      (§13); the wire manifest carries the regenerated descriptor fields
#      (D-223 gate reuse).
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 -> SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1: MCP driver oauth + provenance + concurrent no-token-bleed tests.
if go test -race -count=1 -timeout 300s \
    ./internal/tools/drivers/mcp/... >/dev/null 2>&1; then
    ok 'phase 148: MCP driver oauth/provenance/no-token-bleed tests pass under -race'
else
    fail 'phase 148: MCP driver tests failed (run `go test -race ./internal/tools/drivers/mcp/...`)'
fi

# 1b: the agent-provenance ctx seam.
if go test -race -count=1 -timeout 120s \
    ./internal/tools/ >/dev/null 2>&1; then
    ok 'phase 148: agent-provenance ctx seam tests pass under -race'
else
    fail 'phase 148: agent-provenance tests failed (run `go test -race ./internal/tools/`)'
fi

# 1c: the config validation table (unknown provider lists declared names;
# stdio+binding rejected; Authorization conflict; reserved annotation keys).
if go test -race -count=1 -timeout 120s \
    -run 'MCPSouthboundOAuth' ./internal/config/... >/dev/null 2>&1; then
    ok 'phase 148: config oauth_provider/meta_annotations validation table passes under -race'
else
    fail 'phase 148: config validation table failed (run `go test -race -run MCPSouthboundOAuth ./internal/config/...`)'
fi

# 2: the phase-148 integration test under -race.
if go test -race -count=1 -timeout 300s \
    -run 'Phase148' ./test/integration/... >/dev/null 2>&1; then
    ok 'phase 148: southbound-oauth integration test passes under -race'
else
    fail 'phase 148: integration test failed (run `go test -race -run Phase148 ./test/integration/...`)'
fi

# 3: the MCP driver depends ONLY on the auth interface package — no concrete
# tools/auth/drivers/* import (§13; grep over non-test driver source).
DRIVER_CONCRETE_HITS="$(grep -rn 'internal/tools/auth/drivers/' \
    internal/tools/drivers/mcp/ --include='*.go' | grep -v '_test.go' || true)"
if [[ -z "${DRIVER_CONCRETE_HITS}" ]]; then
    ok 'phase 148: MCP driver imports no concrete tools/auth/drivers/* (§13)'
else
    fail "phase 148: MCP driver imports a concrete auth driver: ${DRIVER_CONCRETE_HITS}"
fi

# 3b: the wire manifest carries the regenerated descriptor fields (D-223).
assert_grep_present \
    'oauth_provider' \
    web/console/src/lib/protocol/wire-manifest.gen.json \
    'phase 148: oauth_provider in regenerated wire manifest (D-223)'
assert_grep_present \
    'meta_annotations' \
    web/console/src/lib/protocol/wire-manifest.gen.json \
    'phase 148: meta_annotations in regenerated wire manifest (D-223)'

smoke_summary
