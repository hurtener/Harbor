#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 29 smoke — A2A southbound driver (full v1 spec).
#
# Phase 29 ships the wire RemoteTransport driver for A2A (the JSON-RPC
# 2.0 over HTTPS + SSE binding) and the tools-side ToolProvider that
# materialises each peer's AgentSkill into the catalog as a Tool
# entry with Transport=TransportA2A. Both packages live behind §4.4
# seams; both register through init() (the wire driver into
# `distributed.RegisterRemoteTransport("a2a", …)`; the tools-side
# Provider has no factory registry — catalogs are built in code).
#
# The smoke runs both package test suites under -race:
#
#   - internal/distributed/drivers/a2a — wire driver + JSON-RPC client
#     + SSE client + AgentCard cache + Registry + security guard +
#     the inherited `conformancetest.RunRemoteTransport` suite (mock
#     A2A server via httptest.Server with the full Agent Card).
#   - internal/tools/drivers/a2a — ToolProvider adapter + integration
#     test that wires the wire driver → mock A2A server → catalog +
#     ToolPolicy shell end-to-end.
#
# There is no HTTP / Protocol surface yet (lands in Phase 60+); the
# smoke skips the surface stub at the end.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 180s ./internal/distributed/drivers/a2a/... >/dev/null 2>&1; then
    ok 'phase 29: internal/distributed/drivers/a2a tests pass under -race (wire driver + conformance + registry + security)'
else
    fail 'phase 29: internal/distributed/drivers/a2a tests failed (run `go test -race ./internal/distributed/drivers/a2a/...` for detail)'
fi

if go test -race -count=1 -timeout 180s ./internal/tools/drivers/a2a/... >/dev/null 2>&1; then
    ok 'phase 29: internal/tools/drivers/a2a tests pass under -race (ToolProvider adapter + catalog integration)'
else
    fail 'phase 29: internal/tools/drivers/a2a tests failed (run `go test -race ./internal/tools/drivers/a2a/...` for detail)'
fi

skip "phase 29: A2A southbound driver has no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
