#!/usr/bin/env bash
# Phase 26 smoke — Tool catalog core + InProcess registration + ToolPolicy
# reliability shell.
#
# Phase 26 ships the unified tool surface: Tool / ToolDescriptor /
# ToolCatalog / ToolProvider types, the in-process driver
# (`tools.RegisterFunc` with reflection-derived JSON-Schemas), the
# CatalogFilter keyed on the (tenant, user, session) triple plus
# GrantedScopes, argument validation at the catalog edge via
# `santhosh-tekuri/jsonschema/v6`, and the ToolPolicy reliability
# shell (D-024) wrapping every invocation in timeout +
# exponential-backoff retry + validation regardless of transport.
#
# The smoke runs the package test suite (catalog + policy + driver
# tests + conformance suite + D-025 concurrent-reuse test) under
# -race. There is no HTTP / Protocol surface yet (lands in Phase 60+).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 120s ./internal/tools/... >/dev/null 2>&1; then
    ok 'phase 26: internal/tools tests pass under -race (catalog + policy + inproc driver + conformance + D-025 concurrent-reuse)'
else
    fail 'phase 26: internal/tools tests failed (run `go test -race ./internal/tools/...` for detail)'
fi

skip "phase 26: tool catalog has no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
