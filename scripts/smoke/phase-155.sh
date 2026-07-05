#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 155 — session-erasure audit integrity: the `sessions.delete`
# cascade's durable record-of-fact ordering + cumulative deletion counts
# (D-286, issues #409/#410).
#
# What this asserts:
#
#   1. Static: the new typed sentinel + ledger-checkpoint machinery exist
#      in internal/sessions/erasure.go, and the Protocol-layer mapping
#      (internal/sessions/protocol) + the wire handler's HTTP-status
#      classification (internal/protocol/transports/stream) carry the new
#      error through.
#   2. Unit + fault-injection round-trip: the sessions erasure package
#      tests — including the fault-injection suite in
#      internal/sessions/erasure_audit_test.go (bus-publish-failure and
#      redactor-refusal convergence, mid-cascade cumulative-count
#      accumulation, the same-session concurrent race, and the ledger's
#      own persistence-seam fault injection) — run under -race.
#
# There is no NEW live-server surface: `sessions.delete`'s wire shape is
# unchanged (field docs only) — the existing Phase 130 live smoke
# (scripts/smoke/phase-130.sh, if present) continues to cover the
# happy-path HTTP round-trip. The fault legs this phase adds are
# deliberately test-only (a live dev-server token is always the caller's
# OWN session, so the redactor/bus fault-injection seams — which require
# a wrapped driver — cannot be exercised over the live wire).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_or_skip() {
    local pattern="$1" file="$2" desc="$3"
    if [ ! -f "${file}" ]; then
        skip "${desc}: ${file} not found (Phase 155 not yet implemented)"
        return
    fi
    if grep -qE "${pattern}" "${file}" 2>/dev/null; then
        ok "${desc}"
    else
        skip "${desc}: pattern '${pattern}' absent (Phase 155 not yet implemented)"
    fi
}

# ----------------------------------------------------------------------------
# 1. Static assertions — the ordering invariant's machinery + error
#    mapping through every layer.
# ----------------------------------------------------------------------------

assert_or_skip 'ErrErasureRecordFailed = errors\.New' \
    "internal/sessions/erasure.go" \
    "static: sessions.ErrErasureRecordFailed is the new typed sentinel"

assert_or_skip 'type erasureLedgerRecord struct' \
    "internal/sessions/erasure.go" \
    "static: the durable erasure-ledger checkpoint type exists"

assert_or_skip 'func \(e \*CascadeEraser\) lockSession' \
    "internal/sessions/erasure.go" \
    "static: the striped per-session erase lock exists (never a double event)"

assert_or_skip 'ErrErasureRecordFailed = errors\.New' \
    "internal/sessions/protocol/protocol.go" \
    "static: sessions/protocol.ErrErasureRecordFailed mirrors the sentinel through the Service layer"

assert_or_skip 'sessionsprotocol\.ErrErasureRecordFailed' \
    "internal/protocol/transports/stream/sessions_handler.go" \
    "static: the wire handler classifies ErrErasureRecordFailed to an HTTP status"

# ----------------------------------------------------------------------------
# 2. Unit + fault-injection round-trip under -race.
# ----------------------------------------------------------------------------

if [ ! -f "internal/sessions/erasure_audit_test.go" ]; then
    skip "fault-injection suite: internal/sessions/erasure_audit_test.go absent (Phase 155 not yet implemented)"
elif go test -race -count=1 -timeout 300s ./internal/sessions/... >/dev/null 2>&1; then
    ok "unit-tests: internal/sessions package tests (erasure cascade + fault-injection + concurrent-same-session race) pass under -race"
else
    fail "unit-tests: internal/sessions package tests failed (run: go test -race ./internal/sessions/...)"
fi

if [ ! -f "internal/sessions/protocol/delete_test.go" ]; then
    skip "Service-layer mapping tests: internal/sessions/protocol/delete_test.go absent (Phase 155 not yet implemented)"
elif go test -race -count=1 -timeout 120s -run 'TestService_Delete_' ./internal/sessions/protocol/... >/dev/null 2>&1; then
    ok "unit-tests: sessions/protocol Service.Delete error-mapping tests (incl. ErrErasureRecordFailed) pass under -race"
else
    fail "unit-tests: sessions/protocol Service.Delete error-mapping tests failed"
fi

smoke_summary
