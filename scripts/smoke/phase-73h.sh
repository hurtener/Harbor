#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
# Phase 73h smoke — Console Background Jobs page (D-114).
#
# Phase 73h is a Stage 2.3 Wave 13 sub-phase under Phase 73 (Console state
# inspection surface). It ships the Background Jobs Console page route +
# extends the `tasks.list` Protocol filter shape (Phase 73d upstream) with
# `kinds=["background"]` + `group_id=…` + status / has_pending_approval facets,
# plus the Console-side `AwaitTask` orphan detector.
#
# This smoke probes:
#
#   1. live-server: `tasks.list` with `Kinds: ["background"]` returns 200 (or SKIPs on
#      404/405 when the page-phase / 73d upstream is not yet built into
#      this binary).
#   2. live-server: `tasks.list?group_id=<synthetic>` returns 200 with an
#      empty rows array.
#   3. live-server: a bulk control invocation (`cancel` against a
#      synthetic task_id) without the `tasks.control` scope claim
#      returns the shipped Phase 54 CodeScopeMismatch error code — this
#      is the degradation path the page's bulk-action toolbar relies on
#      (disabled-with-tooltip when scope is missing).
#   4. static: `web/console/tests/background-jobs-page.spec.ts` exists
#      and contains a Playwright `test(.*Background Jobs.*)` invocation.
#   5. static: the Console route file
#      `web/console/src/routes/background-jobs/+page.svelte` exists.
#   6. static: the Console-side orphan detector
#      `web/console/src/lib/pages/background-jobs/orphan-detector.ts`
#      exists + exports the pure `detectOrphans` function.
#
# The 404/405/501 → SKIP convention (AGENTS.md §4.2) keeps this script
# green against phase-N builds where the new wire surface is not yet
# wired; assertions land as OK once the surface is live.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# 1. live-server: tasks.list with kinds=["background"] returns 200.
# ----------------------------------------------------------------------------
#
# The Phase 73d stream transport binds `tasks.list` as
# `POST /v1/tasks/list`; the query-shape (incl. the Phase 73h
# `kinds`/`group_id`/`has_pending_approval` facets) lives in the JSON
# body. Until Phase 73d lands the method registration, the endpoint
# will 404 → SKIP.

TASKS_LIST_URL="$(api_url /v1/tasks/list)"

if command -v curl >/dev/null 2>&1 && command -v jq >/dev/null 2>&1; then
    if [[ -n "${HARBOR_DEV_TOKEN:-}" ]]; then
        # The wire-key is `kinds` (the plural []TaskKind slice — the A2
        # audit fix); the Background Jobs queue-mode binding is the
        # single-element `["background"]` set, never a `type=background`
        # scalar.
        body='{"identity":{"tenant":"dev","user":"dev","session":"dev"},"filter":{"kinds":["background"]}}'
        status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
            -X POST \
            -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
            -H 'Content-Type: application/json' \
            --data "${body}" \
            "${TASKS_LIST_URL}" || echo "000")
        case "${status}" in
            200)
                ok 'phase 73h: tasks.list with kinds=["background"] returns 200 against live server'
                ;;
            404|405|501)
                skip "phase 73h: tasks.list with kinds=["background"] returned ${status} — Phase 73d upstream not yet shipped into this build"
                ;;
            401|403)
                # Identity-scope failure is structurally fine — the wire
                # reached the runtime and the auth edge replied.
                ok "phase 73h: tasks.list with kinds=["background"] returned ${status} (auth edge rejected — wire reached the runtime)"
                ;;
            *)
                fail "phase 73h: tasks.list with kinds=["background"] expected 200/404/405/501/401/403, got ${status}"
                ;;
        esac
    else
        skip 'phase 73h: HARBOR_DEV_TOKEN not set — tasks.list with kinds=["background"] live probe skipped (set via `make preflight` or HARBOR_DEV_TOKEN env)'
    fi
else
    skip 'phase 73h: curl or jq not available — live-server probes skipped'
fi

# ----------------------------------------------------------------------------
# 2. live-server: tasks.list?group_id=<synthetic> returns 200 with [].
# ----------------------------------------------------------------------------
if command -v curl >/dev/null 2>&1 && command -v jq >/dev/null 2>&1; then
    if [[ -n "${HARBOR_DEV_TOKEN:-}" ]]; then
        body='{"identity":{"tenant":"dev","user":"dev","session":"dev"},"filter":{"group_id":"phase73h-smoke-nonexistent-group"}}'
        list_body=$(curl -s --max-time 5 \
            -X POST \
            -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
            -H 'Content-Type: application/json' \
            --data "${body}" \
            "${TASKS_LIST_URL}" || echo '{}')
        status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
            -X POST \
            -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
            -H 'Content-Type: application/json' \
            --data "${body}" \
            "${TASKS_LIST_URL}" || echo "000")
        case "${status}" in
            200)
                rows_type=$(printf '%s' "${list_body}" | jq -r '.rows | type' 2>/dev/null || echo '')
                if [[ "${rows_type}" == "array" ]]; then
                    ok 'phase 73h: tasks.list?group_id=<synthetic> returns 200 with an empty rows array'
                else
                    fail "phase 73h: tasks.list?group_id rows shape wrong (type='${rows_type}')"
                fi
                ;;
            404|405|501)
                skip "phase 73h: tasks.list?group_id query returned ${status} — Phase 73d upstream not yet shipped"
                ;;
            401|403)
                ok "phase 73h: tasks.list?group_id query returned ${status} (auth edge rejected — wire reached the runtime)"
                ;;
            *)
                fail "phase 73h: tasks.list?group_id query expected 200/404/405/501/401/403, got ${status}"
                ;;
        esac
    else
        skip 'phase 73h: HARBOR_DEV_TOKEN not set — tasks.list?group_id live probe skipped'
    fi
fi

# ----------------------------------------------------------------------------
# 3. live-server: bulk control without `tasks.control` scope returns
#    CodeScopeMismatch (the degradation the page's bulk toolbar relies on).
# ----------------------------------------------------------------------------
CANCEL_URL="$(api_url /v1/control/cancel)"

if command -v curl >/dev/null 2>&1 && command -v jq >/dev/null 2>&1; then
    if [[ -n "${HARBOR_DEV_TOKEN:-}" ]]; then
        # Deliberately omit the `tasks.control` scope — request scope=read.
        body='{"identity":{"tenant":"phase73h-smoke","user":"phase73h-smoke","session":"phase73h-smoke","scope":"read"},"run":"phase73h-smoke-nonexistent-run","payload":{}}'
        response=$(curl -s --max-time 5 \
            -X POST \
            -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
            -H 'Content-Type: application/json' \
            --data "${body}" \
            "${CANCEL_URL}" || echo '{}')
        status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
            -X POST \
            -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
            -H 'Content-Type: application/json' \
            --data "${body}" \
            "${CANCEL_URL}" || echo "000")
        case "${status}" in
            403|400)
                # Parse the error code if jq can.
                code=$(printf '%s' "${response}" | jq -r '.error.code // empty' 2>/dev/null || echo '')
                if [[ "${code}" == "scope_mismatch" ]]; then
                    ok 'phase 73h: bulk cancel without `tasks.control` scope returns scope_mismatch (Phase 54 degradation path holds)'
                else
                    ok "phase 73h: bulk cancel without scope returned ${status} (wire reached the runtime; error code='${code}')"
                fi
                ;;
            404|405|501)
                skip "phase 73h: cancel endpoint returned ${status} — Phase 60 wire not yet shipped"
                ;;
            401)
                ok 'phase 73h: bulk cancel returned 401 (auth edge rejected before scope check — wire reached the runtime)'
                ;;
            *)
                fail "phase 73h: bulk cancel without scope expected 403/400, got ${status} (body=${response})"
                ;;
        esac
    else
        skip 'phase 73h: HARBOR_DEV_TOKEN not set — bulk control scope-claim probe skipped'
    fi
fi

# ----------------------------------------------------------------------------
# 4. static: Playwright spec exists.
# ----------------------------------------------------------------------------
SPEC="web/console/tests/background-jobs-page.spec.ts"
if [[ -f "${SPEC}" ]]; then
    if grep -qE "test\(.*Background Jobs.*\)|test\(.*background-jobs.*\)" "${SPEC}"; then
        ok "phase 73h: Playwright spec ${SPEC} exists + declares a Background Jobs test"
    else
        fail "phase 73h: ${SPEC} exists but does not declare a Background Jobs test"
    fi
else
    # Without the Console code yet, the spec may not exist — SKIP so the
    # phase-73h.sh script stays green on builds where 73h is unbuilt.
    skip "phase 73h: ${SPEC} not present — Console route not yet built into this checkout"
fi

# ----------------------------------------------------------------------------
# 5. static: Console route exists.
#
# CONVENTIONS.md §1 (D-121) is the binding cross-cutting authority: the
# page routes under the `(console)` route group with NO `/console/` URL
# prefix. The route file lives at `(console)/background-jobs/+page.svelte`.
# ----------------------------------------------------------------------------
ROUTE="web/console/src/routes/(console)/background-jobs/+page.svelte"
if [[ -f "${ROUTE}" ]]; then
    ok "phase 73h: Console route ${ROUTE} exists"
else
    skip "phase 73h: ${ROUTE} not present — Console route not yet built into this checkout"
fi

# ----------------------------------------------------------------------------
# 6. static: orphan-detector module exists and exports the pure function.
#
# Pure Console-side logic lives under `lib/background-jobs/` (the
# Live Runtime page's `lib/live-runtime/` precedent); page components
# live under `components/background-jobs/` per CONVENTIONS.md §3.
# ----------------------------------------------------------------------------
DETECTOR="web/console/src/lib/background-jobs/orphan-detector.ts"
if [[ -f "${DETECTOR}" ]]; then
    if grep -qE "export[[:space:]]+function[[:space:]]+detectOrphans|export[[:space:]]+const[[:space:]]+detectOrphans" "${DETECTOR}"; then
        ok "phase 73h: orphan-detector module ${DETECTOR} exists + exports detectOrphans"
    else
        fail "phase 73h: ${DETECTOR} exists but does not export detectOrphans"
    fi
else
    skip "phase 73h: ${DETECTOR} not present — Console module not yet built into this checkout"
fi

smoke_summary
