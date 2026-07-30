#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 218 — the search cluster's user axis becomes a scoped boundary (D-363).
#
# The defect this guards: `internal/search` gated results on the TENANT axis
# alone and passed `req.Filter.UserIDs` to storage unexamined, so a caller with
# no admin-tier claim read another user's rows inside their own tenant. Two
# distinct shapes — an ELIDED user_ids reaching storage as a wildcard (sessions,
# tasks, artifacts) and a NAMED foreign user_ids honoured verbatim (all four).
#
# Asserts:
#   - static: the user-axis helpers + sentinel exist beside their tenant twins.
#   - static: all four searchers AND the aggregate dispatcher call them.
#   - static (the regression trip-wire): no `req.Filter.UserIDs` survives in any
#     searcher — the raw-filter read IS the bug's fingerprint, so a
#     re-introduction fails preflight instead of waiting for a reviewer.
#   - static: tasks' rowScopedCtx re-check covers the USER component, not the
#     tenant alone.
#   - static: the artifacts searcher binds the `heavy` bool its siblings bind.
#   - static: the Protocol edge maps the new sentinel to a wire code.
#   - live: the five search.* methods still answer for an ordinary caller — the
#     fix must not turn a working surface into a refusal (the D-352 failure
#     mode this phase was explicitly warned about).
#   - live: a search.query naming a FOREIGN user_ids is GRANTED (200) under the
#     dev bearer — which carries BOTH admin-tier claims, decoded rather than
#     assumed — so the entitled path is not swallowed by the fold; and the same
#     request UNAUTHENTICATED is refused. The claim-ABSENT refusal is NOT
#     reachable live (`harbor dev` mints one bearer and it is privileged); it is
#     pinned by the unit legs and by the integration test, which compare the
#     wire code exactly. Full reasoning in the live block.
#   - unit-tests: the per-searcher fold/refusal arms, the helper contract, the
#     two-principal N=128 concurrent-reuse arm, and the integration test — all
#     under -race.
#
# Every assertion FAILS (never SKIPs) when its guard is removed: the static
# guards fail on a missing pattern, the absence guard fails on a re-introduced
# pattern, the live check compares an exact status, and the `go test` legs fail
# on a genuine failure. The only SKIP paths are a build with no search surface
# at all, a missing curl/jq, and a `-run` filter matching no tests on a build
# where the phase has not landed.
#
# Mutation-verified (§4.2 item 5 — a shipped phase's own guard reporting SKIP
# is how the 109k gap survived four phases). Record each transition in D-363:
#   - revert a searcher's fold to `req.Filter.UserIDs`  -> OK -> FAIL
#   - delete the ErrCrossUserRequiresAdmin refusal      -> OK -> FAIL
#   - revert rowScopedCtx to the tenant-only compare    -> OK -> FAIL
#   - revert the events Admin flag to crossTenant alone -> OK -> FAIL
#   - discard the artifacts `heavy` bool again          -> OK -> FAIL
#
# Done-definition: OK >= 10, SKIP = 0, FAIL = 0.
#
# ---------------------------------------------------------------------------
# The guards below are UNCONDITIONAL. They were authored ahead of the
# implementation behind a self-activating landed-probe (the presence of the
# phase's sentinel in internal/search/search.go), so that preflight stayed green
# on the branch while the plan waited for its code and no guard had to be
# re-derived from prose afterwards. The phase has landed, so the probe is gone
# and every assertion runs on every invocation.
# ---------------------------------------------------------------------------

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

SEARCH_GO='internal/search/search.go'
AGGREGATE_GO='internal/search/aggregate.go'
ARTIFACTS_GO='internal/search/artifacts/index.go'
EVENTS_GO='internal/search/events/index.go'
SESSIONS_GO='internal/search/sessions/index.go'
TASKS_GO='internal/search/tasks/index.go'
PROTOCOL_GO='internal/protocol/search.go'

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
# Static guards — the user axis exists, and it is wired at every call site.
# ----------------------------------------------------------------------------

# The helpers, beside their tenant twins. Two separate greps rather than one
# alternation: an alternation would pass with either half deleted, and the fold
# (EffectiveUserSet) and the gate (CrossUserRequested) are independently
# load-bearing — the fold without the gate honours a named foreign user, and
# the gate without the fold still wildcards an elided one.
assert_grep "${SEARCH_GO}" \
    'func CrossUserRequested\(callerUser string, req types\.SearchRequest\) bool' \
    'phase 218: the user-axis gate predicate is declared'

assert_grep "${SEARCH_GO}" \
    'func EffectiveUserSet\(callerUser string, req types\.SearchRequest\) \[\]string' \
    'phase 218: the user-axis effective-set (the FOLD) is declared'

assert_grep "${SEARCH_GO}" \
    'ErrCrossUserRequiresAdmin[[:space:]]*=[[:space:]]*errors\.New' \
    'phase 218: the cross-user refusal sentinel is declared'

# Every enforcement site. Iterating the five files rather than one tree-wide
# grep is load-bearing: a tree-wide match would still pass with four of the five
# sites reverted, which is exactly the shape of gap this phase exists to close
# (72c gated one axis where it was looking and never enumerated the rest).
for f in "${AGGREGATE_GO}" "${ARTIFACTS_GO}" "${EVENTS_GO}" "${SESSIONS_GO}" "${TASKS_GO}"; do
    assert_grep "${f}" \
        'CrossUserRequested\(' \
        "phase 218: ${f} gates the user axis"
done

# The fold reaches storage in each of the four searchers. The aggregate
# dispatcher does not compute a set (it rewrites the sub-request and delegates),
# so it is deliberately absent from this loop.
for f in "${ARTIFACTS_GO}" "${EVENTS_GO}" "${SESSIONS_GO}" "${TASKS_GO}"; do
    assert_grep "${f}" \
        'EffectiveUserSet\(' \
        "phase 218: ${f} scopes storage to the effective user set"
done

# THE REGRESSION TRIP-WIRE. `req.Filter.UserIDs` reaching a searcher's body IS
# the defect: the effective-set helper must be the only reader. Scoped to the
# four index.go files rather than the package, because search.go's own helper
# legitimately reads the field.
for f in "${ARTIFACTS_GO}" "${EVENTS_GO}" "${SESSIONS_GO}" "${TASKS_GO}"; do
    assert_grep_absent 'req\.Filter\.UserIDs' "${f}" \
        "phase 218: ${f} no longer reads the raw user filter"
done

# The per-row re-scope behind the tasks fan-in covers the USER component. Before
# this phase it compared `ident.TenantID == verified.TenantID` alone, so a
# same-tenant foreign-user session took the UNELEVATED identity.With path — the
# tasks searcher's own half of the leak, one layer below the request gate.
assert_grep "${TASKS_GO}" \
    'ident\.UserID (!=|==) verified\.UserID' \
    'phase 218: the tasks per-row re-scope compares the user, not the tenant alone'

# The §17.6 sibling fix: the artifacts searcher binds the heavy bool its three
# siblings bind. Guarded as an ABSENCE of the discard rather than a presence of
# the binding, because `heavy` appears in the fixed file either way and only the
# discard form is unambiguous.
#
# The file-existence arm used to fall through to the `else`, so DELETING or
# RENAMING the artifacts searcher reported OK — the guard's pass value and its
# "I could not look" value were the same. Existence is now its own assertion
# (wave-v1.24 checkpoint audit).
if [ ! -f "${ARTIFACTS_GO}" ]; then
    fail "phase 218: ${ARTIFACTS_GO} does not exist — the heavy-bool guard cannot run, and an absent artifacts searcher is not a pass"
elif grep -qE 'out, _, rerr := search\.RedactAndCapPreview' "${ARTIFACTS_GO}"; then
    fail 'phase 218: the artifacts searcher still discards the heavy bool (out, _, rerr)'
else
    ok 'phase 218: the artifacts searcher binds the heavy bool its siblings bind'
fi

# The refusal is observable on the wire — an unmapped sentinel would surface as
# a generic runtime error, which is the silent-degradation shape §13 forbids.
assert_grep "${PROTOCOL_GO}" \
    'search\.ErrCrossUserRequiresAdmin' \
    'phase 218: the Protocol edge maps the cross-user refusal to a wire code'

# ZERO-WIRE: no Protocol version bump rode along with this phase.
assert_grep 'internal/protocol/types/version.go' \
    'ProtocolVersion[[:space:]]*=[[:space:]]*"0\.1\.0"' \
    'phase 218: ProtocolVersion is unchanged (zero-wire)'

# ----------------------------------------------------------------------------
# Live assertions — the surface still works, and the new refusal fires.
# ----------------------------------------------------------------------------
#
# Deliberately NOT skip_all_if_server_down: that exits the whole script, which
# would take the `go test` guard legs below with it on a standalone run. Only
# the live block degrades when no server is up; the guards always run.

# `dev_bearer` prefers the exported HARBOR_DEV_TOKEN and falls back to grepping
# the preflight harness's server.log — the plain env read this used to do saw
# nothing on a standalone run, every request went out unauthenticated, and the
# resulting 401s were counted as passes below.
TOKEN="$(dev_bearer)"

HEALTH_CODE=000
if command -v curl >/dev/null 2>&1; then
    HEALTH_CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$(api_url /healthz)" || true)
fi

if ! command -v curl >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
    skip 'phase 218: curl/jq unavailable — live assertions skipped'
elif [ "${HEALTH_CODE:-000}" = "000" ] || [ -z "${HEALTH_CODE}" ]; then
    skip 'phase 218: dev server unreachable — live assertions skipped (run under make preflight)'
else
    # p218_post <url> <body>
    p218_post() {
        local url="$1" body="$2"
        local hdrs=(-H 'Content-Type: application/json')
        if [ -n "${TOKEN}" ]; then
            hdrs+=(-H "Authorization: Bearer ${TOKEN}")
        fi
        P218_STATUS=$(curl -s --max-time 5 -o /tmp/phase218.body -w '%{http_code}' \
            "${hdrs[@]}" -X POST -d "${body}" "${url}" 2>/dev/null || true)
        P218_STATUS="${P218_STATUS:-000}"
        P218_BODY=$(cat /tmp/phase218.body 2>/dev/null || echo '{}')
        P218_CODE=$(printf '%s' "${P218_BODY}" | jq -r '.code // .error.code // ""' 2>/dev/null || echo "")
    }

    ID='{"tenant":"dev","user":"dev","session":"dev"}'

    # (1) THE NON-REGRESSION. The five methods must still answer for an ordinary
    # own-scope caller. D-352 named this exact failure mode for the sibling fix:
    # a stricter identity precondition "would silently turn both into refusals",
    # so a fix that empties a working surface is not a fix. A SCOPE refusal on
    # an OWN-SCOPE read fails.
    #
    # 401 USED TO SIT IN THE OK ARM ALONGSIDE 200. That made the guard's pass
    # value indistinguishable from a total auth regression: had auth started
    # refusing everything, all five methods would have printed OK. The wave-v1.24
    # checkpoint audit split the two — with a bearer in hand, a 401 is a failure;
    # without one the block below never runs, so there is no "the token may be
    # undiscoverable" case left to tolerate here.
    if [ -z "${TOKEN}" ]; then
        if [ -n "${HARBOR_DATA_DIR:-}" ]; then
            fail 'phase 218: HARBOR_DATA_DIR is set but no dev bearer could be resolved — the live legs would run unauthenticated and prove nothing'
        else
            skip 'phase 218: no dev bearer available (standalone run outside the preflight harness) — live legs skipped rather than run unauthenticated'
        fi
    else
    for m in search.query search.sessions search.tasks search.events search.artifacts; do
        p218_post "$(api_url "/v1/control/${m}")" \
            "{\"identity\":${ID},\"query\":\"\",\"page_size\":5}"
        case "${P218_STATUS}" in
            404|405|501|000)
                skip "phase 218: ${m} absent from this build (${P218_STATUS})"
                ;;
            401)
                fail "phase 218: ${m} answered 401 to a request carrying the dev bearer — an auth regression, not an own-scope answer"
                ;;
            403)
                fail "phase 218: ${m} refuses an OWN-SCOPE read with 403 (code=${P218_CODE}) — the fold over-reached"
                ;;
            200)
                ok "phase 218: ${m} still answers an own-scope read (200)"
                ;;
            *)
                fail "phase 218: ${m} own-scope read returned ${P218_STATUS} (code=${P218_CODE})"
                ;;
        esac
    done

    # (2) THE WIDENED PATH. `harbor dev` mints exactly ONE bearer and it carries
    # BOTH admin-tier claims (`["admin","console:fleet"]` — decoded from the
    # boot token, not assumed), so no live probe here can reach the
    # claim-ABSENT branch: every widening this bearer sends is legitimately
    # GRANTED. The same constraint is recorded on the body-scope reconciler's
    # smoke for the same reason.
    #
    # So this leg asserts the half a live probe CAN reach, and it is the half
    # a static grep cannot: that the granted widening still ANSWERS. A fold
    # that also swallowed the entitled path would be the D-352 failure mode in
    # the other direction — a fix that turns a working fleet read into a
    # refusal — and it would pass every static guard above.
    #
    # The claim-ABSENT refusal is pinned by the unit legs (per searcher, both
    # widenings, both claims, via the PRODUCTION ScopeChecker) and by
    # `TestE2E_Phase218_NamedForeignUserIsRefusedNotEmptied`, which drives all
    # five methods through the Protocol dispatcher and compares the wire code
    # exactly. Those legs run below and FAIL loud.
    p218_post "$(api_url /v1/control/search.query)" \
        "{\"identity\":${ID},\"query\":\"\",\"page_size\":5,\"filter\":{\"user_ids\":[\"phase218-not-the-caller\"]}}"
    case "${P218_STATUS}" in
        404|405|501|000)
            skip "phase 218: search.query absent from this build (${P218_STATUS})"
            ;;
        401)
            fail 'phase 218: search.query answered 401 to a request carrying the dev bearer — an auth regression; this arm used to SKIP on "HARBOR_DEV_TOKEN not discoverable", which the token guard above now handles'
            ;;
        200)
            ok 'phase 218: a foreign user_ids is GRANTED under the dev bearer admin-tier claims (the widened read still answers)'
            ;;
        403)
            fail "phase 218: a foreign user_ids was REFUSED (403, code=${P218_CODE}) on a bearer carrying admin-tier claims — the fold swallowed the entitled path"
            ;;
        *)
            fail "phase 218: foreign user_ids returned ${P218_STATUS} (code=${P218_CODE}), want 200 under the dev bearer's claims"
            ;;
    esac
    fi

    # (3) The surface is not answering the widening to an UNAUTHENTICATED
    # caller. Identity is mandatory, so a bearer-less foreign-user request must
    # never reach the gate at all — a 2xx here would mean the axis is reachable
    # without any verified principal to reconcile against.
    if command -v curl >/dev/null 2>&1; then
        P218_ANON=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
            -H 'Content-Type: application/json' -X POST \
            -d "{\"identity\":${ID},\"query\":\"\",\"page_size\":5,\"filter\":{\"user_ids\":[\"phase218-not-the-caller\"]}}" \
            "$(api_url /v1/control/search.query)" 2>/dev/null || true)
        case "${P218_ANON:-000}" in
            401|403)
                ok "phase 218: an unauthenticated foreign user_ids is refused (${P218_ANON})"
                ;;
            404|405|501|000)
                skip "phase 218: search.query absent from this build (${P218_ANON:-000})"
                ;;
            *)
                fail "phase 218: an unauthenticated foreign user_ids returned ${P218_ANON} — identity is mandatory"
                ;;
        esac
    fi
fi

# ----------------------------------------------------------------------------
# Unit + integration legs — the behaviour behind the static guards.
# ----------------------------------------------------------------------------
#
# The static guards prove the helpers are CALLED; only these prove they are
# called CORRECTLY. A grep cannot tell a fold from a wildcard.

run_filtered_tests \
    'phase 218: the user-axis helper contract (fold, dedup, fan-in trigger, tenant parity)' \
    'TestCrossUserRequested|TestEffectiveUserSet' \
    ./internal/search/

run_filtered_tests \
    'phase 218: per-searcher elision fold + foreign-user refusal + admin reopen' \
    'ElidedUserFoldsToCaller|NamedForeignUserRefused|OwnUserNamedNeedsNoClaim|MultiUserFanInRefused|AdminClaimReopensBothWidenings' \
    ./internal/search/artifacts/ ./internal/search/events/ \
    ./internal/search/sessions/ ./internal/search/tasks/

run_filtered_tests \
    'phase 218: the session axis stays open on a single own-other-session read' \
    'OwnOtherSessionNeedsNoClaim|MultiSessionFanInRefused' \
    ./internal/search/sessions/

run_filtered_tests \
    'phase 218: the tasks per-row re-scope elevates on a same-tenant foreign user' \
    'RowScopedCtx_ForeignUserSameTenantElevates' \
    ./internal/search/tasks/

run_filtered_tests \
    'phase 218: the aggregate dispatcher refuses before fan-out' \
    'TestQuery_CrossUserRefusedAtAggregateEdge' \
    ./internal/search/

run_filtered_tests \
    'phase 218: two-principal concurrent reuse, N=128 under -race (no context bleed)' \
    'ConcurrentReuse.*TwoPrincipal|TwoPrincipal.*ConcurrentReuse' \
    ./internal/search/

run_filtered_tests \
    'phase 218: end-to-end cross-user + cross-tenant isolation with real drivers' \
    'TestE2E_Phase218' \
    ./test/integration/

smoke_summary
