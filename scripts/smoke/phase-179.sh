#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 179 — Go Protocol client foundation (D-315).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file "internal/protocol/client/client.go" "Protocol REST client exists"
assert_file "internal/protocol/client/stream.go" "Protocol SSE client exists"
assert_file "sdk/protocolclient/protocolclient.go" "curated public client facade exists"
assert_file "test/integration/protocol_client_test.go" "real-driver client integration exists"

if go test -race ./internal/protocol/client ./sdk/protocolclient ./cmd/harbor; then
    ok "Protocol client, external facade, and inspect goldens pass under race"
else
    fail "Protocol client, external facade, or inspect race tests failed"
fi

if go test -race ./test/integration -run '^TestE2E_ProtocolClient_' -count=1; then
    ok "authenticated real-driver reconnect integration passes under race"
else
    fail "authenticated real-driver reconnect integration failed"
fi

if grep -rEq --include='*.go' 'func (readSSE|ParseSSEFrames|readSSEUntilIdle|fetchSSEUntilIdle)\(' cmd/harbor; then
    fail "command-local SSE parser function remains"
else
    ok "command-local SSE parsers are removed"
fi
assert_grep_present 'protocolclient\.New' "cmd/harbor/inspect_common.go" \
    "inspect events and runs use the promoted client"
assert_grep_present 'protocolclient\.New' "cmd/harbor/cmd_inspect_topology.go" \
    "inspect topology uses the promoted client"

smoke_summary
