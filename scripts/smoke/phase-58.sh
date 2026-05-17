#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 58 smoke — Protocol types/methods/errors single source
# (RFC §5.1/§5.2/§5.3; CLAUDE.md §8, §13; master-plan Phase 58 detail
# block; D-075).
#
# Phase 58 formalises the single-source discipline Phase 54 (D-072) laid
# the foundation for: internal/protocol/methods, internal/protocol/
# errors, and internal/protocol/types are the ONLY definition sites for
# Protocol method names, error codes, and wire types. It ships a
# go/parser AST-walking checker (internal/protocol/singlesource) plus a
# build-gating `go test` that fails the moment a hardcoded Protocol
# method string / a Protocol error-code constant / a redeclared Protocol
# wire type appears anywhere else under internal/protocol/.
#
# Phase 58 ships NO HTTP / Protocol-wire surface — the wire transport is
# Phase 60. The wire assertions skip per the 404/405/501 -> SKIP
# convention; the checker is exercised in-process via `go test`.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

PROTO_PKG="internal/protocol"
SS_PKG="${PROTO_PKG}/singlesource"

# The Phase 58 build-gating lint: run the single-source checker test
# under -race. Covers the binding clean-tree lint
# (TestSingleSource_ProtocolTreeIsClean — the live internal/protocol
# tree carries zero single-source violations), the per-kind detection
# tests (a hardcoded method string / an error-code constant / a
# redeclared wire type are each caught against a synthetic tree), the
# no-false-positive tests (the canonical packages + comments + struct
# tags + substrings are NOT flagged), the test-file linting, and the
# lockstep tests (the checker's CanonicalMethods / CanonicalWireTypes
# sets are pinned to internal/protocol/{methods,types,errors}).
if go test -race -count=1 -timeout 120s "./${SS_PKG}/..." >/dev/null 2>&1; then
    ok 'phase 58: internal/protocol/singlesource tests pass under -race (clean-tree lint + per-kind detection + no-false-positive + test-file linting + canonical-set lockstep)'
else
    fail 'phase 58: single-source checker tests failed (run `go test -race ./internal/protocol/singlesource/...` for detail)'
fi

# Static guard: the single-source checker itself is present.
CHECKER_FILE="${SS_PKG}/singlesource.go"
if [[ -f "${CHECKER_FILE}" ]]; then
    ok 'phase 58: the Protocol single-source checker is present (internal/protocol/singlesource/singlesource.go)'
else
    fail "phase 58: ${CHECKER_FILE} missing — Phase 58 must ship the single-source checker"
fi

# Static guard (CLAUDE.md §8): a cheap grep backstop for the binding
# lint — no canonical Protocol method wire string appears as a literal
# anywhere under internal/protocol/ outside methods/ (the single source)
# and singlesource/ (the checker, which necessarily names the methods in
# its CanonicalMethods set). This catches a regression even if the Go
# test binary is somehow skipped. The Go test is the precise gate; this
# is the backstop.
method_literal_hits="$(
    grep -rIn --include='*.go' \
        -e '"start"' -e '"cancel"' -e '"pause"' -e '"resume"' \
        -e '"redirect"' -e '"inject_context"' -e '"approve"' \
        -e '"reject"' -e '"prioritize"' -e '"user_message"' \
        "${PROTO_PKG}/" 2>/dev/null \
        | grep -v "${PROTO_PKG}/methods/" \
        | grep -v "${PROTO_PKG}/singlesource/" || true
)"
if [[ -n "${method_literal_hits}" ]]; then
    fail "phase 58: hardcoded Protocol method string(s) under internal/protocol/ outside methods/ — single-source violation (CLAUDE.md §8; D-075):
${method_literal_hits}"
else
    ok 'phase 58: no hardcoded Protocol method string under internal/protocol/ outside methods/ (single source enforced — CLAUDE.md §8)'
fi

# Phase 58 ships no Protocol/HTTP wire surface — the SSE+REST transport
# binding lands in Phase 60. Skip the wire assertions per the
# 404/405/501 -> SKIP convention.
skip "phase 58: Phase 58 is a build-time static single-source checker; it ships no HTTP/Protocol-wire surface — the wire transport lands in Phase 60 (RFC §5.4, D-072)"

smoke_summary
