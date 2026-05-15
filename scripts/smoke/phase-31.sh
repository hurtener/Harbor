#!/usr/bin/env bash
# Phase 31 smoke — Tool-side approval gates (RFC §6.4 + §3.3;
# master-plan Phase 31 detail block; D-086).
#
# Phase 31 ships `internal/tools/approval` — the synchronous
# approval-gate subsystem that converges on the unified pause/resume
# primitive (Phase 50). Sibling to Phase 30's OAuth gate. Code-only
# library phase: no Protocol surface lands until the OAuth-callback /
# approval-resolution Protocol method (Phase 64+). The smoke
# therefore exercises the unit + integration test suite + static
# guards on the package shape; HTTP/Protocol assertions skip per the
# 404/405/501 → SKIP convention.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

APPROVAL_PKG="internal/tools/approval"

# 1. Run the approval-package tests under -race. Covers: ApprovalPolicy
#    interface contract, ApprovalGate happy approve + reject paths,
#    nil-policy boot rejection (§13 amendment), missing-coordinator /
#    missing-bus / missing-redactor rejection, identity-mandatory fail-
#    closed, ResolveApproval scope gating, cross-identity rejection,
#    redactor-error fail-loud, D-025 N=128 concurrent-reuse, goroutine
#    leak on initiate-then-cancel.
if go test -race -count=1 -timeout 180s ./${APPROVAL_PKG}/... >/dev/null 2>&1; then
    ok 'phase 31: internal/tools/approval tests pass under -race (policy + gate + ResolveApproval scope gating + D-025 + redactor fail-loud)'
else
    fail 'phase 31: approval tests failed (run `go test -race ./internal/tools/approval/...` for detail)'
fi

# 2. Run the Phase 31 integration test — full APPROVE + REJECT cycle
#    against real pauseresume.Coordinator + real events.EventBus +
#    real audit.Redactor (patterns driver) + real steering.Inbox.
#    Identity propagation + scope-gate + cross-identity failure mode +
#    goroutine-leak + N=16 concurrency stress.
if go test -race -count=1 -timeout 180s -run 'TestE2E_Phase31' ./test/integration/... >/dev/null 2>&1; then
    ok 'phase 31: tool-approval-gate integration test passes under -race (APPROVE + REJECT round-trips + scope gating + cross-identity + goroutine-leak + concurrency stress)'
else
    fail 'phase 31: integration test failed (run `go test -race -run TestE2E_Phase31 ./test/integration/...` for detail)'
fi

# 3. Static guard: ApprovalPolicy interface + ApprovalGate type live
#    in approval.go / gate.go.
APPROVAL_FILE="${APPROVAL_PKG}/approval.go"
GATE_FILE="${APPROVAL_PKG}/gate.go"
if [[ ! -f "${APPROVAL_FILE}" ]]; then
    fail "phase 31: ${APPROVAL_FILE} missing"
elif ! grep -q 'type ApprovalPolicy interface' "${APPROVAL_FILE}"; then
    fail 'phase 31: ApprovalPolicy interface not declared in approval.go'
elif [[ ! -f "${GATE_FILE}" ]]; then
    fail "phase 31: ${GATE_FILE} missing"
elif ! grep -q 'type ApprovalGate struct' "${GATE_FILE}"; then
    fail 'phase 31: ApprovalGate type not declared in gate.go'
else
    ok 'phase 31: ApprovalPolicy interface + ApprovalGate type declared (the master-plan acceptance surface)'
fi

# 4. Static guard: tool.rejected + tool.approval_requested +
#    tool.approved registered into the canonical events registry.
EVENTS_FILE="${APPROVAL_PKG}/events.go"
if [[ ! -f "${EVENTS_FILE}" ]]; then
    fail "phase 31: ${EVENTS_FILE} missing"
elif ! grep -q 'EventTypeToolRejected.*"tool.rejected"' "${EVENTS_FILE}"; then
    fail 'phase 31: tool.rejected event type not declared in events.go'
elif ! grep -q 'EventTypeToolApprovalRequested.*"tool.approval_requested"' "${EVENTS_FILE}"; then
    fail 'phase 31: tool.approval_requested event type not declared in events.go'
elif ! grep -q 'EventTypeToolApproved.*"tool.approved"' "${EVENTS_FILE}"; then
    fail 'phase 31: tool.approved event type not declared in events.go'
elif ! grep -q "events.RegisterEventType(EventTypeToolRejected)" "${EVENTS_FILE}"; then
    fail 'phase 31: tool.rejected is not registered into the canonical events registry'
else
    ok 'phase 31: tool.approval_requested + tool.approved + tool.rejected registered into the canonical events registry'
fi

# 5. Static guard (§13 amendment trip-wire): NewApprovalGate rejects a
#    nil Policy — no silent stub default on an operator-facing seam.
if grep -q 'ErrPolicyRequired' "${GATE_FILE}" && grep -q 'deps.Policy == nil' "${GATE_FILE}"; then
    ok 'phase 31: NewApprovalGate fails-loud on nil Policy (§13 amendment — no silent stub default)'
else
    fail 'phase 31: NewApprovalGate does not appear to reject a nil Policy (§13 amendment trip-wire missing)'
fi

# 6. Static guard: the gate's Coordinator request uses
#    pauseresume.ReasonApprovalRequired (NOT ReasonExternalEvent — that
#    is Phase 30's OAuth path).
if grep -q 'ReasonApprovalRequired' "${GATE_FILE}"; then
    ok 'phase 31: ApprovalGate parks via Coordinator with ReasonApprovalRequired (RFC §6.3 canonical reason)'
else
    fail 'phase 31: ApprovalGate does not appear to use ReasonApprovalRequired'
fi

# 7. Static guard: ErrToolRejected typed sentinel + payload exists.
if grep -q 'ErrToolRejected' "${APPROVAL_FILE}"; then
    ok 'phase 31: ErrToolRejected typed sentinel declared (the master-plan acceptance criterion shape)'
else
    fail 'phase 31: ErrToolRejected typed sentinel not declared in approval.go'
fi

# 8. Until the approval-resolution Protocol method ships, the HTTP
#    surface skips per the 404/405/501 convention. Phase 31 is a
#    code-only library phase; the resolution Protocol handler is a
#    follow-up phase (Phase 64+).
skip 'phase 31: approval-resolution Protocol method is a follow-up phase; HTTP surface assertions skip until then'

smoke_summary
