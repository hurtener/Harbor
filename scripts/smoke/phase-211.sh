#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 211 — owner-scoped MCP registry mutators (D-355).
#
# Asserts:
#   - static: Registry.SetRawHTMLTrust resolves through the SCOPED resolver
#     (tenantEntry), not the bare-name r.entry — the connection write lands on
#     the caller's own registration.
#   - static: Registry.Deregister carries an owner and compares it before the
#     delete; the projection detach seam and the production detacher thread that
#     owner through.
#   - static: RefreshDiscovery / Probe are CLASSIFIED in godoc as reads, so a
#     later reader cannot mistake their bare-name resolution for an oversight.
#   - live: mcp.servers.set_raw_html_trust is identity-mandatory, and a name the
#     caller's scope does not resolve fails loud with a typed 404 not_found —
#     the SAME answer the tenant-scoped resolver gives for a registration
#     another tenant owns, so the refusal shape is exercised against the live
#     surface.
#   - live: mcp.servers.list still answers 2xx for an ordinary caller — the READS
#     stay bare-name and process-global (D-287 / D-301).
#   - unit-tests: the registry's tenant-scoped write (including the boot-declared
#     and identity-missing cases and the N=128 two-tenant concurrent run), the
#     owner-scoped deregister, the reconcile owner threading, the production
#     detacher, and the protocol -> tools/mcp integration seam — all under -race.
#
# Every assertion FAILS (never SKIPs) when its guard is removed: the static
# guards fail on a missing pattern rather than skipping (§4.2 item 5 — a
# Shipped phase's own guard reporting SKIP is how an earlier gap survived four
# phases), the live checks compare an exact code, and the `go test` legs fail on
# a genuine test failure. The only SKIP paths are the route probe (a build with
# no MCP surface at all) and a missing curl/jq.
#
# Done-definition: OK >= 8, FAIL = 0.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

REGISTRY_GO='internal/tools/drivers/mcp/registry.go'
ATTACH_GO='internal/tools/drivers/mcp/attach.go'
PROJECTION_GO='internal/runtime/agentcfg/projection/projection.go'
DETACHER_GO='internal/runtime/serve/mcp_detacher.go'

# assert_grep <file> <extended-regexp> <desc>
#
# OK when the pattern matches, FAIL when it does not. Deliberately NOT a skip:
# these guard a shipped surface, and the 404/405/501 -> SKIP convention is for
# forward-phase scripts running against older builds, not for a phase's own
# guards. Patterns use POSIX classes ([[:space:]]) — never \t / \d, which BSD
# grep matches and GNU grep does not, so a guard written that way would silently
# never fire on Linux CI.
assert_grep() {
    local file="$1" pattern="$2" desc="$3"
    if [ ! -f "${file}" ]; then
        fail "${desc}: ${file} does not exist"
        return
    fi
    if grep -qE "${pattern}" "${file}"; then
        ok "${desc}"
    else
        fail "${desc}: no match for /${pattern}/ in ${file}"
    fi
}

# run_filtered_tests <desc> <run-regexp> <packages...>
#
# Runs `go test -race -run <regexp>` over the given packages. OK on a real
# pass; SKIP only when the filter matched no tests at all (an older build);
# FAIL on a genuine test failure (never masked).
run_filtered_tests() {
    local desc="$1" runre="$2"
    shift 2
    local out rc
    # NO CGO_ENABLED=0 here: the race detector needs cgo on Linux, where
    # `CGO_ENABLED=0 go test -race` fails to build with "-race requires cgo"
    # (exit 2) rather than running anything. Harbor's CGo ban (CLAUDE.md §5)
    # governs the shipped BINARY, not the race-instrumented test binary.
    out="$(go test -race -count=1 -run "${runre}" "$@" 2>&1)" && rc=0 || rc=$?
    if [ "${rc}" -eq 0 ]; then
        if printf '%s\n' "${out}" | grep -qE 'no tests to run|no test files'; then
            skip "${desc}: filter '${runre}' matched no tests (phase not yet landed)"
        else
            ok "${desc}"
        fi
        return
    fi
    printf '%s\n' "${out}" | tail -25
    fail "${desc}: go test exited ${rc}"
}

# ----------------------------------------------------------------------------
# Static guards — the scoped resolution is wired where it must be.
# ----------------------------------------------------------------------------

# The scoped resolvers exist and the wire-reachable write goes through the
# tenant-scoped one.
assert_grep "${REGISTRY_GO}" \
    'func \(r \*Registry\) tenantEntry\(name, tenant string\)' \
    'phase 211: the registry declares the tenant-scoped write resolver'

assert_grep "${REGISTRY_GO}" \
    'r\.tenantEntry\(name, id\.TenantID\)' \
    'phase 211: SetRawHTMLTrust resolves under the caller ctx tenant'

# ...and NOT through the bare-name resolver any more. The bare-name r.entry(name)
# stays for the READS, so this guard is scoped to the setter's own body.
if [ -f "${REGISTRY_GO}" ] && \
   sed -n '/^func (r \*Registry) SetRawHTMLTrust(/,/^}/p' "${REGISTRY_GO}" | grep -qE 'r\.entry\(name\)'; then
    fail 'phase 211: SetRawHTMLTrust still resolves by bare name (r.entry)'
else
    ok 'phase 211: SetRawHTMLTrust no longer resolves by bare name'
fi

# Deregister is owner-scoped, and the comparison runs before the delete.
assert_grep "${REGISTRY_GO}" \
    'func \(r \*Registry\) Deregister\(ctx context\.Context, name string, owner auth\.Owner\)' \
    'phase 211: Deregister takes the owner'

assert_grep "${REGISTRY_GO}" \
    'e\.owner != owner' \
    'phase 211: Deregister compares the owner before removing the entry'

# The owner is threaded from both production callers.
assert_grep "${ATTACH_GO}" \
    'Registry\.Deregister\(ctx, ms\.Name, deps\.Owner\)' \
    'phase 211: the attach replace threads its own owner into the deregister'

assert_grep "${PROJECTION_GO}" \
    'Detach\(ctx context\.Context, source string, owner auth\.Owner\) error' \
    'phase 211: the reconcile detach seam carries the reconciling owner'

assert_grep "${DETACHER_GO}" \
    'registry\.Deregister\(ctx, source, owner\)' \
    'phase 211: the production detacher threads the owner into the registry'

# BOTH control-plane verbs are CLASSIFIED, not merely left alone. Counting is
# load-bearing: a single-match grep would still pass with one classification
# deleted, which is the shape of guard that lets an unexamined verb drift back.
CLASSIFIED=0
if [ -f "${REGISTRY_GO}" ]; then
    CLASSIFIED=$(grep -cE 'It is a READ, and its bare-name resolution is deliberate' "${REGISTRY_GO}" || true)
fi
if [ "${CLASSIFIED:-0}" -ge 2 ]; then
    ok 'phase 211: RefreshDiscovery AND Probe each carry their read classification in godoc'
else
    fail "phase 211: read classifications found = ${CLASSIFIED:-0}, want >= 2 (RefreshDiscovery + Probe)"
fi

# The reads themselves are untouched (D-287 / D-301) — GetServer still resolves
# bare-name and process-global.
assert_grep "${REGISTRY_GO}" \
    'func \(r \*Registry\) GetServer\(ctx context\.Context, name string\) \(\*ServerView, error\)' \
    'phase 211: the read projection keeps its bare-name signature'

# ZERO-WIRE: no Protocol version bump rode along with this phase.
assert_grep 'internal/protocol/types/version.go' \
    'ProtocolVersion[[:space:]]*=[[:space:]]*"0\.1\.0"' \
    'phase 211: ProtocolVersion is unchanged (zero-wire)'

# ----------------------------------------------------------------------------
# Live assertions — the pre-existing gates and the reads still behave.
# ----------------------------------------------------------------------------
#
# Deliberately NOT skip_all_if_server_down: that exits the whole script, which
# would take the `go test` guard legs below with it on a standalone run. Only
# the live block degrades when no server is up; the guards always run.
TRUST_URL="$(api_url /v1/control/mcp.servers.set_raw_html_trust)"
LIST_URL="$(api_url /v1/control/mcp.servers.list)"
TOKEN="${HARBOR_DEV_TOKEN:-}"

HEALTH_CODE=000
if command -v curl >/dev/null 2>&1; then
    HEALTH_CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$(api_url /healthz)" || true)
fi

if ! command -v curl >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
    skip 'phase 211: curl/jq unavailable — live assertions skipped'
elif [ "${HEALTH_CODE:-000}" = "000" ] || [ -z "${HEALTH_CODE}" ]; then
    skip 'phase 211: dev server unreachable — live assertions skipped (run under make preflight)'
else
    # p211_post <url> <body> [--no-token]
    p211_post() {
        local url="$1" body="$2" auth="${3:-with-token}"
        local hdrs=(-H 'Content-Type: application/json')
        if [ -n "${TOKEN}" ] && [ "${auth}" = "with-token" ]; then
            hdrs+=(-H "Authorization: Bearer ${TOKEN}")
        fi
        P211_BODY=$(curl -s --max-time 5 -o /tmp/phase211.body -w '%{http_code}' \
            "${hdrs[@]}" -X POST -d "${body}" "${url}" 2>/dev/null || true)
        P211_STATUS="${P211_BODY:-000}"
        P211_BODY=$(cat /tmp/phase211.body 2>/dev/null || echo '{}')
        P211_CODE=$(printf '%s' "${P211_BODY}" | jq -r '.code // .error.code // ""' 2>/dev/null || echo "")
    }

    ID='{"tenant":"dev","user":"dev","session":"dev"}'

    # Route probe with an EMPTY body. Probing with a REAL request would be a
    # trap: this verb answers a genuine unregistered name with 404
    # `not_found`, which a bare status check reads as "route not present" and
    # turns a real answer into a SKIP — the exact §4.2 item 5 failure this
    # phase exists to close. An empty body separates the two cleanly: a mounted
    # route answers 400 `invalid_request` ("name is required"), while an
    # unmounted / unknown method answers 404 `unknown_method`.
    p211_post "${TRUST_URL}" '{}'
    LIVE_TRUST=1
    if [ "${P211_CODE}" = "unknown_method" ]; then
        skip "phase 211: mcp.servers.set_raw_html_trust not a canonical method on this build"
        LIVE_TRUST=0
    else
        case "${P211_STATUS}" in
            405|501|000|'')
                skip "phase 211: mcp.servers.set_raw_html_trust route not present (${P211_STATUS})"
                LIVE_TRUST=0
                ;;
        esac
    fi

    if [ "${LIVE_TRUST}" = "1" ]; then
        # Identity is mandatory on the write — no bearer, no write.
        p211_post "${TRUST_URL}" "{\"identity\":${ID},\"name\":\"p211-no-such-server\",\"trusted\":true}" no-token
        case "${P211_STATUS}" in
            2*) fail "phase 211: identity-less connection write returned ${P211_STATUS}, want a typed refusal" ;;
            *)
                if [ -n "${P211_CODE}" ]; then
                    ok "phase 211: the connection write is identity-mandatory (${P211_STATUS} ${P211_CODE})"
                else
                    fail "phase 211: identity-less connection write returned no typed code: ${P211_BODY}"
                fi
                ;;
        esac

        # A name the caller's scope does not resolve answers a typed not_found —
        # the SAME answer the tenant-scoped resolver returns for a registration
        # another tenant owns, so the refusal shape is exercised live.
        p211_post "${TRUST_URL}" "{\"identity\":${ID},\"name\":\"p211-no-such-server\",\"trusted\":true}"
        if [ "${P211_STATUS}" = "404" ] && [ "${P211_CODE}" = "not_found" ]; then
            ok 'phase 211: an unresolvable connection write fails loud with a typed not_found'
        else
            fail "phase 211: unresolvable connection write returned ${P211_STATUS} ${P211_CODE}, want 404 not_found"
        fi
    fi

    p211_post "${LIST_URL}" "{\"identity\":${ID}}"
    case "${P211_STATUS}" in
        404|405|501|000)
            skip "phase 211: mcp.servers.list route not present (${P211_STATUS} ${P211_CODE})"
            ;;
        2*)
            if printf '%s' "${P211_BODY}" | jq -e 'has("servers")' >/dev/null 2>&1; then
                ok 'phase 211: the bare-name read projection is untouched (servers list answers 2xx)'
            else
                fail "phase 211: mcp.servers.list body has no .servers: ${P211_BODY}"
            fi
            ;;
        *)
            fail "phase 211: mcp.servers.list returned ${P211_STATUS}: ${P211_BODY}"
            ;;
    esac
fi

# ----------------------------------------------------------------------------
# Guard tests (each FAILS, never SKIPs, when its guard is removed).
# ----------------------------------------------------------------------------
run_filtered_tests \
    'phase 211: registry tenant-scoped write + owner-scoped deregister (tools/mcp)' \
    'TestRegistry_(SetRawHTMLTrust_(TenantScoped|UnknownAndOtherTenantAnswerAlike|BootDeclaredStaysWritable|IdentityMissing|CompensatingRevertResolvesSymmetrically|ConcurrentTenants)|TenantEntry_EmptyTenantResolvesNothing|Deregister_(OwnerScoped|ZeroOwnerMatchesOnlyBootDeclared|ConcurrentOwners)|ReadsStayBareName)' \
    ./internal/tools/drivers/mcp/

run_filtered_tests \
    'phase 211: the reconcile detach leg threads the reconciling owner (agentcfg/projection)' \
    'TestReconcileConnections_PassesReconcilingOwnerToDetach' \
    ./internal/runtime/agentcfg/projection/

run_filtered_tests \
    'phase 211: the production detacher is owner-scoped against a real registry (runtime/serve)' \
    'TestMCPConnectionDetacher_Detach_(OwnerScoped|NeverRemovesBootDeclared)' \
    ./internal/runtime/serve/

run_filtered_tests \
    'phase 211: cross-tenant write, boot-declared name, revert path + stress (integration)' \
    'TestE2E_P211_' \
    ./test/integration/

smoke_summary
