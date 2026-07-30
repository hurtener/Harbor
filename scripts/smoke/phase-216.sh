#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 216 — run-start connection RE-ESTABLISHMENT: attach what can be
# attached, report what cannot.
#
# Every guard below is MUTATION-VERIFIED: deleting or weakening the thing it
# names turns an OK into a FAIL, never into a SKIP. The live block degrades to a
# SKIP on its own so the static guards still run standalone.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

PROJECTION="internal/runtime/agentcfg/projection/projection.go"
REATTACHER="internal/runtime/serve/mcp_reattacher.go"
RUNLOOP="internal/runtime/serve/runloop.go"
SERVE="internal/runtime/serve/serve.go"
DEVSTACK="harbortest/devstack/devstack.go"
EVENTS="internal/agentcfg/events.go"
DOCSGEN="cmd/harbor-gen-protocol-docs/events.go"
MANIFEST="web/console/src/lib/protocol/wire-manifest.gen.json"
SITE_EVENTS="docs/site/protocol/events.md"
TRANSPORT="internal/tools/drivers/mcp/transport_sse.go"

# ----------------------------------------------------------------------------
# 1. The seam exists and ReconcileConnections takes it.
# ----------------------------------------------------------------------------
assert_grep_present 'type ConnectionReattacher interface' "${PROJECTION}" \
    'the driver-agnostic ConnectionReattacher seam is declared'
assert_grep_present 'func ReconcileConnections\(.*reattacher ConnectionReattacher' "${PROJECTION}" \
    'ReconcileConnections takes the reattacher (the leg is not a second function)'

# ----------------------------------------------------------------------------
# 2. The attach pass exists and runs AFTER the detach pass in the same function.
#    Pinned by ORDER, not mere presence.
# ----------------------------------------------------------------------------
assert_grep_present 'if reattacher == nil \{' "${PROJECTION}" \
    'a nil reattacher yields the backward-compatible detach-only path'
if awk '/detacher\.Detach\(ctx, src, owner\)/{d=NR} /reattacher\.Reattach\(ctx, owner, id/{a=NR} END{exit !(d>0 && a>0 && d<a)}' "${PROJECTION}"; then
    ok 'the attach pass runs AFTER the detach pass in ReconcileConnections'
else
    fail 'the attach pass does not follow the detach pass in ReconcileConnections (order is load-bearing)'
fi

# ----------------------------------------------------------------------------
# 3. The run loop WIRES a reattacher — the counterpart of the pre-phase fact
#    that `grep -c Attach internal/runtime/serve/runloop.go` returned 0.
# ----------------------------------------------------------------------------
assert_grep_present 'ConnectionReattacher +projection\.ConnectionReattacher' "${RUNLOOP}" \
    'the run-loop driver accepts a ConnectionReattacher'
assert_grep_present 'd\.connectionReattacher' "${RUNLOOP}" \
    'the run loop passes its reattacher into the run-start reconcile'
assert_grep_present 'mcpReattacher = attacher' "${SERVE}" \
    'the production boot wires the attacher as the run-start reattacher'
assert_grep_present 'devReattacher = att' "${DEVSTACK}" \
    'the devstack twin wires the same reattacher (the kit must not drift from the binary)'

# ----------------------------------------------------------------------------
# 4. The stdio RCE gate is RE-APPLIED at the gate, not merely declared.
# ----------------------------------------------------------------------------
assert_grep_present 'ErrReattachStdioNotAllowed' "${REATTACHER}" \
    'the re-attach re-applies the stdio allowlist against CURRENT boot policy'
assert_grep_present 'a\.stdioAllowlist\[desc\.Command\[0\]\]' "${REATTACHER}" \
    'the stdio gate reads argv[0] against the live allowlist (not a stale copy)'

# ----------------------------------------------------------------------------
# 5. The credential-injection kill-switch.
# ----------------------------------------------------------------------------
assert_grep_present 'ErrReattachInjectionDisabled' "${REATTACHER}" \
    'the re-attach honours the credential-injection kill-switch'
assert_grep_present 'desc\.Injection != nil && !a\.allowWireInjection' "${REATTACHER}" \
    'the kill-switch reads the EFFECTIVE opt-in (a persisted mapping is not rebuilt with it off)'

# ----------------------------------------------------------------------------
# 6. The bounded contexts — load-bearing, not hygiene.
# ----------------------------------------------------------------------------
assert_grep_present 'context\.WithTimeout\(ctx, a\.reattachTimeout\)' "${REATTACHER}" \
    'each re-attach runs under its own bounded context'
assert_grep_present 'reconcileConnectionsSweepBudget' "${RUNLOOP}" \
    'the whole reconcile sweep runs under a bounded total'
assert_grep_present 'unownedBoundingTransport' "${TRANSPORT}" \
    'requests the driver does not own the context of are bounded (the SDK-teardown stall)'

# ----------------------------------------------------------------------------
# 7. CREDENTIAL-NEUTRALITY GUARD. The run-start attach path must contain NO
#    token step — a grep-level proof that the leg cannot have grown one, backed
#    by the negative-assertion unit test.
# ----------------------------------------------------------------------------
assert_grep_absent '\.Token\(|InitiateFlow|CompleteFlow|TokenStore' "${REATTACHER}" \
    'the run-start attach path references no token API'

# ----------------------------------------------------------------------------
# 8. REPORT GUARD. Every class in the closed set has an emit site. COUNTED, so
#    deleting ONE still fails.
# ----------------------------------------------------------------------------
assert_grep_count 'MCPReattachClass[A-Za-z]+ +=' "${EVENTS}" 6 \
    'the closed re-attach failure-class set has exactly 6 members'
assert_grep_count 'agentcfg\.MCPReattachClass[A-Za-z]+' "${REATTACHER}" 6 \
    'every declared failure class is reachable from the classifier (one path each)'

# ----------------------------------------------------------------------------
# 9. Both new canonical event types are registered AND joined in the docs
#    generator. COUNTED, so deleting ONE still fails.
# ----------------------------------------------------------------------------
assert_grep_count 'EventTypeMCPConnectionReattach(ed|Failed) events\.EventType|EventTypeMCPConnectionReattach(ed|Failed),$' "${EVENTS}" 4 \
    'both new event types are declared AND listed in the init() registration'
assert_grep_count 'EventTypeMCPConnectionReattached|EventTypeMCPConnectionReattachFailed' "${DOCSGEN}" 2 \
    'both new event types have a docs-generator join row'

# ----------------------------------------------------------------------------
# 10. The generators were RE-RUN and their output committed (never hand-edited).
# ----------------------------------------------------------------------------
# Anchored on the manifest's own quoted-entry form for the same reason.
assert_grep_count '"mcp\.connection\.reattached"|"mcp\.connection\.reattach_failed"' "${MANIFEST}" 2 \
    'the regenerated Console wire manifest carries both event names'
# Anchored on the generator's own heading form, so a MANGLED name (the shape a
# stale or hand-edited page has) fails instead of matching as a prefix.
assert_grep_count '^## `mcp\.connection\.reattached`$|^## `mcp\.connection\.reattach_failed`$' "${SITE_EVENTS}" 2 \
    'the regenerated Protocol events reference carries both event names'

# ----------------------------------------------------------------------------
# 11. No new Protocol method: the phase is internal Go plus two additive
#     canonical events.
# ----------------------------------------------------------------------------
assert_grep_absent 'reattach|attach_now|force_reconcile' 'internal/protocol/methods/methods.go' \
    'no new Protocol method was added for the re-attach leg'

# ----------------------------------------------------------------------------
# LIVE: mcp.servers.list still answers well-formed. The read path is untouched
# by this phase, so a regression here would mean the attach pass corrupted the
# registry. The probe classifies on the BODY, never the bare status — a verb
# that answers a genuine miss with 404 would otherwise convert a real answer
# into a SKIP.
#
# THIS GUARD — the phase's ONLY live assertion — COULD ONLY EVER SKIP, TWO
# WAYS, until the wave-v1.24 checkpoint audit, which makes the header's
# "weakening it turns an OK into a FAIL, never into a SKIP" claim false for
# exactly this one:
#
#   1. It posted to `/v1/mcp/servers/list`, a route that does not exist.
#      `mcp.servers.list` is dispatched through the generic
#      `POST /v1/control/{method}` pattern (internal/protocol/transports/
#      control/control.go:69, decode at mcp_handler.go:61). 404 -> SKIP.
#   2. Its body triple was `harbor-dev/harbor-dev/harbor-dev` while the dev
#      bearer is minted for `dev/dev/dev` (cmd/harbor/devauth.go DevTenant /
#      DevUser / DevSession). `harbor-dev` is the token's *kid*, not its
#      identity. Body-identity reconciliation refuses the mismatch, and the
#      catch-all `*)` arm laundered that refusal into a SKIP too.
#
# The `*)` arm is now a FAIL, matching phase 214's route-probe convention:
# 214 learned this the hard way, having treated a 500 as "route absent" and
# run three assertions against a dead route while reporting green. Only a
# genuinely unreachable server (000) skips.
# ----------------------------------------------------------------------------
live_servers_list() {
    if ! command -v curl >/dev/null 2>&1; then
        skip 'mcp.servers.list probe: curl not available'
        return 0
    fi
    local token url body status out
    token="$(dev_bearer)"
    if [ -z "${token}" ]; then
        skip 'mcp.servers.list probe: no dev bearer resolvable'
        return 0
    fi
    url="$(api_url /v1/control/mcp.servers.list)"
    out="$(mktemp)"
    body='{"identity":{"tenant":"dev","user":"dev","session":"dev"}}'
    status=$(curl -s -o "${out}" -w '%{http_code}' --max-time 10 \
        -X POST -H "Authorization: Bearer ${token}" -H 'Content-Type: application/json' \
        -d "${body}" "${url}" || true)
    case "${status}" in
        200)
            if grep -q '"servers"' "${out}" 2>/dev/null; then
                ok 'mcp.servers.list answers 200 with a .servers body (the read path is intact)'
            else
                fail 'mcp.servers.list answered 200 without a .servers body'
            fi
            ;;
        000|'')
            skip 'mcp.servers.list probe: server unreachable (000)'
            ;;
        *)
            fail "mcp.servers.list probe: got ${status} (want 200) at POST ${url} — this route is already shipped, so anything but 200 under the dev bearer is a regression, not an absent surface; body: $(head -c 300 "${out}" 2>/dev/null)"
            ;;
    esac
    rm -f "${out}"
}
live_servers_list

# ----------------------------------------------------------------------------
# GUARD TESTS — each FAILS on a genuine failure and SKIPs only when the filter
# matches no tests.
# ----------------------------------------------------------------------------
run_guard_test() {
    local pkg="$1" filter="$2" desc="$3"
    if ! command -v go >/dev/null 2>&1; then
        skip "${desc}: go toolchain not available"
        return 0
    fi
    local out rc=0
    out=$(go test -race -count=1 -timeout 600s -run "${filter}" "${pkg}" 2>&1) || rc=$?
    if [ "${rc}" -ne 0 ]; then
        printf '%s\n' "${out}" >&2
        fail "${desc}"
        return 0
    fi
    if printf '%s' "${out}" | grep -q 'no test files'; then
        skip "${desc}: no test files in ${pkg}"
        return 0
    fi
    ok "${desc}"
}

run_guard_test './internal/runtime/agentcfg/projection/' \
    'TestReconcileConnections_(AttachesDeclaredButAbsent|NilReattacherIsDetachOnly|DetachRunsBeforeAttach|AttachPassIsOwnerScoped|AttachErrorsAreJoinedNotFatal|AttachPassHonoursCancellation|ConcurrentAttach_NoCrossTalk)' \
    'the projection attach pass: diff, ordering, owner scoping, nil-reattacher, joined errors, sweep bound, concurrency'

run_guard_test './internal/runtime/serve/' \
    'TestMCPConnectionAttacher_Reattach_(StdioReGatedAgainstCurrentAllowlist|InjectionKillSwitch|OAuthProviderBindingResolves|IsCredentialNeutral|ShortfallSurfacesOnFirstCallNotOnAttach|HeaderAuthenticatedServerFailsLoud|OwnerMandatory)|TestReattach_(FailureClassesAreDistinctAndAllReported|EventPayloadsAreSafeAndScrubbed)' \
    'the attacher gates, the kill-switch, credential neutrality, and the closed failure-class set'

run_guard_test './internal/runtime/serve/' \
    'TestMCPConnectionAttacher_Reattach_(ConcurrentOwners|AlreadyRegisteredUnderOwnerIsNoOp|BackoffBoundsRetries|TerminalClassReportsOnce|BoundedContext)|TestRunLoopDriver_Reattach_' \
    'idempotency under concurrency, the bounded ctx, the bounded retry window, and the run-loop wiring'

run_guard_test './test/integration/' \
    'TestE2E_Phase216_|TestE2E_WaveV124_' \
    'the integration seam: restart survival, rollback re-declare, failure modes, and the wave-end E2E'

smoke_summary
