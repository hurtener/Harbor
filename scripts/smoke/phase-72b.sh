#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 72b — IdentityScope admin-impersonation extension (RFC §5.5,
# §7; brief 11 §PG-5; master-plan Phase 72b detail block; D-107).
#
# Phase 72b extends `internal/protocol/types.IdentityScope` with three
# new optional pointer fields — `Actor` / `Requester` /
# `Impersonating` — to carry the admin-on-behalf-of-user triplet on
# every Protocol request. The transport-edge gate validates the
# triplet, requires `auth.ScopeAdmin`, and emits a typed
# `audit.admin_scope_used` event on every accepted impersonation.
#
# This smoke runs the Phase 72b unit + integration suites under -race
# and pins the load-bearing static guards (the wire type carries the
# three fields; the audit payload lives in `internal/protocol/auth`;
# the gate is the single choke point on the control transport; no
# new Protocol error code minted; no Console import). The live-HTTP
# assertions run via httptest in the integration suite — the live
# `harbor dev` boot path that mounts the transport mux is exercised
# by the Phase 64 smoke.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. The wire-type extension: IdentityScope carries the three new
# pointer fields with the `actor` / `requester` / `impersonating`
# JSON tags + `omitempty`. The Phase 72b plan + Brief 11 §PG-5 pin
# the verbatim field names.
TYPES_FILE="internal/protocol/types/control.go"
for field in 'Actor \*IdentityScope' 'Requester \*IdentityScope' 'Impersonating \*IdentityScope'; do
    if grep -qE "^[[:space:]]+${field}" "${TYPES_FILE}" 2>/dev/null; then
        ok "phase 72b: ${TYPES_FILE} declares ${field}"
    else
        fail "phase 72b: ${TYPES_FILE} missing the ${field} field"
    fi
done

# Tag presence: `actor,omitempty` / `requester,omitempty` /
# `impersonating,omitempty`. A missing `omitempty` would leak the
# field as `null` on the wire and break Brief 12's two-surface model
# (a third-party Console would see `actor: null` instead of an
# absent field).
for tag in '"actor,omitempty"' '"requester,omitempty"' '"impersonating,omitempty"'; do
    if grep -qF "${tag}" "${TYPES_FILE}" 2>/dev/null; then
        ok "phase 72b: ${TYPES_FILE} carries JSON tag ${tag}"
    else
        fail "phase 72b: ${TYPES_FILE} missing JSON tag ${tag} — wire shape would leak null on the wire"
    fi
done

# 2. The audit payload type lives in internal/protocol/auth alongside
# AuthRejectedPayload, NOT in the events package (the pre-existing
# events.AdminScopeUsedPayload covers the Phase 05 admin-filter emit
# site; the Phase 72b emit needs the richer shape). D-107.
AUTH_EVENTS="internal/protocol/auth/events.go"
if grep -q 'type AdminScopeUsedPayload struct' "${AUTH_EVENTS}" 2>/dev/null; then
    ok "phase 72b: ${AUTH_EVENTS} declares AdminScopeUsedPayload (typed audit payload for impersonation emit)"
else
    fail "phase 72b: ${AUTH_EVENTS} missing AdminScopeUsedPayload type (D-107)"
fi
if grep -q 'AdminImpersonationReason\s*=\s*"impersonation"' "${AUTH_EVENTS}" 2>/dev/null; then
    ok "phase 72b: ${AUTH_EVENTS} declares the AdminImpersonationReason sentinel constant"
else
    fail "phase 72b: ${AUTH_EVENTS} missing AdminImpersonationReason sentinel (the stable wire shape a Console branches on)"
fi
if grep -q 'type IdentityTriple struct' "${AUTH_EVENTS}" 2>/dev/null; then
    ok "phase 72b: ${AUTH_EVENTS} declares the flat IdentityTriple audit shape"
else
    fail "phase 72b: ${AUTH_EVENTS} missing IdentityTriple flat audit shape"
fi

# 3. The impersonation gate is on the control transport. A non-admin
# request with `Impersonating` set MUST be rejected at the transport
# edge BEFORE Dispatch runs (defence in depth at the transport edge,
# mirroring Phase 61 D-079 §4).
CONTROL_FILE="internal/protocol/transports/control/control.go"
if grep -q 'func (h \*Handler) assertImpersonationShape' "${CONTROL_FILE}" 2>/dev/null; then
    ok "phase 72b: ${CONTROL_FILE} declares the assertImpersonationShape gate"
else
    fail "phase 72b: ${CONTROL_FILE} missing assertImpersonationShape — the transport-edge gate is the load-bearing accountability surface"
fi
if grep -q 'func (h \*Handler) emitAdminScopeUsed' "${CONTROL_FILE}" 2>/dev/null; then
    ok "phase 72b: ${CONTROL_FILE} declares emitAdminScopeUsed (the audit emit)"
else
    fail "phase 72b: ${CONTROL_FILE} missing emitAdminScopeUsed — the audit emit is mandatory on accepted impersonation"
fi
if grep -q 'auth.HasScope(r.Context(), auth.ScopeAdmin)' "${CONTROL_FILE}" 2>/dev/null; then
    ok "phase 72b: ${CONTROL_FILE} gates impersonation on auth.ScopeAdmin (the closed scope set from D-079)"
else
    fail "phase 72b: ${CONTROL_FILE} missing auth.ScopeAdmin gate — D-079 closed scope set requires admin"
fi

# 4. No new Protocol error code minted by Phase 72b (CLAUDE.md §8 +
# §13). The impersonation gate reuses CodeScopeMismatch /
# CodeIdentityRequired / CodeRuntimeError — all already canonical in
# internal/protocol/errors. The expected count is 9: the original
# Phase 56 eight (CodeInvalidRequest / CodeIdentityRequired /
# CodeScopeMismatch / CodePayloadInvalid / CodeUnknownMethod /
# CodeNotFound / CodeRuntimeError / CodeAuthRejected) plus
# CodeIdentityScopeRequired added by Phase 72 (D-105) for the
# Console subscription scope-claim surface — both predecessors to
# 72b, neither minted here.
NEW_CODES=$(grep -cE 'Code[A-Z][A-Za-z]+\s*Code\s*=' internal/protocol/errors/errors.go 2>/dev/null || echo 0)
if [ "${NEW_CODES}" -eq 9 ]; then
    ok "phase 72b: internal/protocol/errors carries the canonical 9-code set (8 Phase 56 codes + Phase 72 CodeIdentityScopeRequired) — no new code minted by 72b"
else
    fail "phase 72b: internal/protocol/errors has ${NEW_CODES} Code constants, want 9 — a new code would be a Protocol-surface change requiring an RFC PR"
fi

# 5. No Console import from the impersonation surface (CLAUDE.md
# §13 — the Runtime never imports Console code). Defence in depth
# against accidental coupling during refactor.
for src in "${TYPES_FILE}" "${AUTH_EVENTS}" "${CONTROL_FILE}"; do
    if grep -qn '"github.com/hurtener/Harbor/web/console' "${src}" 2>/dev/null; then
        fail "phase 72b: ${src} imports the Console — the Runtime never imports Console code (CLAUDE.md §13)"
    fi
done
ok "phase 72b: impersonation surface does not import the Console (Runtime/Console boundary preserved — CLAUDE.md §13)"

# 6. Unit tests in the touched packages pass under -race.
for pkg in internal/protocol/types internal/protocol/auth internal/protocol/transports/control; do
    if go test -race -count=1 -timeout 90s "./${pkg}/..." >/dev/null 2>&1; then
        ok "phase 72b: ${pkg} tests pass under -race"
    else
        fail "phase 72b: ${pkg} tests failed (run \`go test -race ./${pkg}/...\` for detail)"
    fi
done

# 7. Phase 72b integration suite — REAL Phase 60 transport mux +
# REAL Phase 61 ES256 validator + REAL audit/drivers/patterns
# redactor + REAL events/drivers/inmem bus. Six end-to-end scenarios
# + N=16 concurrency stress under -race.
if go test -race -count=1 -timeout 120s -run 'TestE2E_Phase72b' ./test/integration/... >/dev/null 2>&1; then
    ok 'phase 72b: integration suite passes under -race (5-shape gate table + Phase 61 defence-in-depth + N=16 concurrency stress)'
else
    fail 'phase 72b: integration suite failed (run `go test -race -run TestE2E_Phase72b ./test/integration/...` for detail)'
fi

# 8. The IdentityScope wire type is single-sourced in
# internal/protocol/types per CLAUDE.md §8 + D-072. The Phase 58
# lint enforces this at the source level; this assertion is a
# defence-in-depth grep that catches a phase plan that drifted a
# wire type into a sibling package.
if grep -rIn --include='*.go' 'type IdentityScope struct' internal/protocol/ 2>/dev/null | grep -v 'internal/protocol/types/' | grep -q .; then
    fail 'phase 72b: IdentityScope declared outside internal/protocol/types — wire types are single-sourced (CLAUDE.md §8, D-072)'
else
    ok 'phase 72b: IdentityScope single-source preserved (only declared in internal/protocol/types)'
fi

smoke_summary
