#!/usr/bin/env bash
# Phase 22 smoke — MessageBus + RemoteTransport contracts (V1).
#
# Phase 22 ships internal/distributed: the at-least-once MessageBus
# interface + an in-process loopback driver, and the RemoteTransport
# interface designed against the full A2A v1 spec (vendored at
# docs/specifications/a2a.proto). The actual A2A wire driver lands
# in Phase 29 (southbound). This smoke runs the package test suite
# (a2a Go-shape coverage gate + conformance suite + loopback driver
# tests + registry-surface unit tests) under -race. There is no
# HTTP / Protocol surface yet (lands in Phase 60+).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 90s ./internal/distributed/... >/dev/null 2>&1; then
    ok 'phase 22: internal/distributed tests pass under -race (a2a coverage + conformance + loopback + registry)'
else
    fail 'phase 22: internal/distributed tests failed (run `go test -race ./internal/distributed/...` for detail)'
fi

skip "phase 22: distributed contracts have no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
