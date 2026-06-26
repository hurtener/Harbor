#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 129 smoke — JWKS max-stale / revocation ceiling (D-261).
#
# The ceiling bounds how long the production JWKS validator honors a
# cached key when refreshes fail: past the ceiling, KeyByID fails closed
# with a distinct ErrJWKSStale / `jwks_stale` wire reason instead of
# serving a possibly-revoked key indefinitely. This phase ships NO wire
# change — no new Protocol method / type / error code / capability — so
# the smoke also asserts the wire manifest and ProtocolVersion are
# untouched.
#
# Each assertion maps 1:1 to an acceptance criterion. The static + go-test
# arms are load-bearing (no new endpoint / Protocol method this phase);
# the live `harbor serve` arm follows the §4.2 degradation convention
# (skip when the subcommand is absent).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------
# Static — the config field exists with the documented yaml tag.
# ----------------------------------------------------------------------
assert_grep_present 'JWKSMaxStale' "internal/config/config.go" \
    "phase 129: IdentityConfig declares JWKSMaxStale"
assert_grep_present 'jwks_max_stale' "internal/config/config.go" \
    "phase 129: jwks_max_stale yaml tag present"

# ----------------------------------------------------------------------
# Static — the primitive: sentinel + option + ceiling (jwks.go).
# ----------------------------------------------------------------------
assert_grep_present 'ErrJWKSStale' "internal/protocol/auth/jwks.go" \
    "phase 129: ErrJWKSStale sentinel declared"
assert_grep_present 'func WithJWKSMaxStale' "internal/protocol/auth/jwks.go" \
    "phase 129: WithJWKSMaxStale option declared"

# ----------------------------------------------------------------------
# Static — the consumer: Validator surfaces it distinctly + distinct
# wire reason.
# ----------------------------------------------------------------------
assert_grep_present 'ErrJWKSStale' "internal/protocol/auth/auth.go" \
    "phase 129: keyfunc / mapParserError handle ErrJWKSStale"
assert_grep_present 'jwks_stale' "internal/protocol/auth/middleware.go" \
    "phase 129: reasonForWire emits the jwks_stale wire reason"

# ----------------------------------------------------------------------
# Static — NO wire change. No new Protocol error Code; the wire manifest
# and ProtocolVersion are untouched (the staleness reason is a value of
# the existing free-form `reason` field, not a manifest type).
# ----------------------------------------------------------------------
assert_grep_absent 'ErrJWKSStale' "internal/protocol/errors/errors.go" \
    "phase 129: no new Protocol error Code added (single-source preserved)"
assert_grep_absent 'jwks_stale\|JWKSMaxStale\|JWKSStale' \
    "web/console/src/lib/protocol/wire-manifest.gen.json" \
    "phase 129: wire manifest carries no new symbol (no wire change)"
if grep -q 'ProtocolVersion = "0.1.0"' "internal/protocol/types/version.go"; then
    ok "phase 129: ProtocolVersion unchanged (0.1.0 — no version bump)"
else
    fail "phase 129: ProtocolVersion moved — this phase must be ProtocolVersion-neutral"
fi

# ----------------------------------------------------------------------
# Build/test gates.
# ----------------------------------------------------------------------
if go test -race ./internal/protocol/auth/... ./internal/config/... >/dev/null 2>&1; then
    ok "phase 129: auth + config unit/clock/concurrency tests pass under -race"
else
    fail "phase 129: go test -race ./internal/protocol/auth/... ./internal/config/... failed"
fi
if go test -race -run TestE2E_JWKSMaxStale ./test/integration/... >/dev/null 2>&1; then
    ok "phase 129: JWKS max-stale E2E passes under -race (stale -> jwks_stale; recovery)"
else
    fail "phase 129: JWKS max-stale E2E failed"
fi

# ----------------------------------------------------------------------
# Live (skips per §4.2): a harbor serve boot with a negative
# jwks_max_stale fails non-zero, naming the field. Degrade cleanly on
# builds without the serve subcommand.
# ----------------------------------------------------------------------
BIN="${ROOT}/bin/harbor"
if [ -x "${BIN}" ] && "${BIN}" serve --help >/dev/null 2>&1; then
    if [ -f "${ROOT}/examples/serve.yaml" ]; then
        badcfg="$(mktemp -t harbor-maxstale-bad-XXXXXX)"
        # Inject an invalid (negative) ceiling into the identity block.
        awk '1; /^[[:space:]]*audience:/ { print "  jwks_max_stale: -5s" }' \
            "${ROOT}/examples/serve.yaml" > "${badcfg}"
        out="$(mktemp -t harbor-maxstale-out-XXXXXX)"
        set +e
        "${BIN}" serve --config "${badcfg}" >"${out}" 2>&1
        rc=$?
        set -e
        if [ "${rc}" -ne 0 ] && grep -q 'jwks_max_stale' "${out}"; then
            ok "phase 129: harbor serve fails loud on a negative jwks_max_stale (rc=${rc})"
        else
            fail "phase 129: invalid jwks_max_stale boot did not fail loud (rc=${rc}); output: $(tr '\n' ' ' < "${out}")"
        fi
        rm -f "${badcfg}" "${out}"
    else
        skip "phase 129: examples/serve.yaml absent (cannot run the fail-loud probe)"
    fi
else
    skip "phase 129: harbor serve subcommand absent on this build"
fi

smoke_summary
