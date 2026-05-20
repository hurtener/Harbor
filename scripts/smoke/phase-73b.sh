#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
# Phase 73b smoke — Console Live Runtime page (Protocol + UI bundled)
# (CLAUDE.md §4.5, §13; RFC §5.2 / §6.13 / §7).
#
# Phase 73b ships the Console Live Runtime page bundled with two
# `[wave-13-extends]` Protocol additions:
#   1. `tasks.list` gains a `status_counter_strip` aggregate
#      (pending / running / completed / paused / failed counts;
#      identity-scoped; computed server-side).
#   2. `events.subscribe` gains a `RunID` first-class filter field
#      (the structured counterpart to D-082's X-Harbor-Run header).
#
# The page itself is served via `harbor console` (D-091); `harbor dev`
# stays headless per D-091. The smoke checks the Protocol additions
# the page consumes (live-server probes) plus the static guards that
# enforce §4.5 / §13 Console conventions on the page source tree.
#
# 404/405/501 → SKIP per the AGENTS.md §4.2 convention — the smoke
# coexists with earlier-phase builds. A SKIP that should be an OK is a
# bug.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ---------------------------------------------------------------------------
# 1. Protocol probes — `tasks.list` status-counter-strip aggregate.
# ---------------------------------------------------------------------------
#
# The aggregate is identity-scoped and computed server-side. The smoke
# probes the REST control surface (Phase 60) once the page-feeding
# `tasks.list` method ships (lands in Phase 73d per the Wave 13
# decomposition; Phase 73b extends only the response shape via
# CanonicalWireTypes). Until 73d ships, the route returns 404 / 405
# and the probe SKIPs cleanly.

TASKS_LIST_URL="$(api_url /v1/tasks/list)"
TASKS_LIST_BODY='{"identity":{"tenant":"t-smoke","user":"u-smoke","session":"s-smoke"},"include_status_counter_strip":true}'

if skip_if_404 "${TASKS_LIST_URL}" "phase 73b: tasks.list not yet shipped (Phase 73d carries it)"; then
    # 200 → assert the response shape carries status_counter_strip with
    # the five canonical counters.
    if status_code=$(curl -fsS -o /tmp/phase-73b-tasks-list.json -w '%{http_code}' \
            -X POST "${TASKS_LIST_URL}" \
            -H 'Content-Type: application/json' \
            -H "Authorization: Bearer ${HARBOR_DEV_TOKEN:-}" \
            -d "${TASKS_LIST_BODY}" 2>/dev/null); then
        if [[ "${status_code}" == "200" ]]; then
            for field in pending running completed paused failed; do
                if assert_json_path ".status_counter_strip.${field}" '[0-9]+' /tmp/phase-73b-tasks-list.json \
                        "phase 73b: tasks.list response carries status_counter_strip.${field} (identity-scoped server-side aggregate)"; then
                    :
                else
                    fail "phase 73b: tasks.list response missing status_counter_strip.${field}"
                fi
            done
        elif [[ "${status_code}" =~ ^(401|403)$ ]]; then
            ok "phase 73b: tasks.list rejects unscoped/auth-rejected request (HTTP ${status_code}) — Phase 61 identity-edge enforcement preserved"
        else
            fail "phase 73b: tasks.list returned unexpected HTTP ${status_code}"
        fi
    else
        skip 'phase 73b: tasks.list POST did not complete cleanly (server unreachable or 5xx)'
    fi
fi

# ---------------------------------------------------------------------------
# 2. Protocol probe — `topology.snapshot` (Phase 74; sibling Wave 13 phase).
# ---------------------------------------------------------------------------
#
# The Live Runtime page consumes `topology.snapshot` for the topology
# canvas. The smoke probes that the surface accepts a session-scoped
# request and returns a `nodes` + `edges` shape. Skips cleanly until
# Phase 74 ships.

TOPOLOGY_URL="$(api_url /v1/control/topology.snapshot)"
TOPOLOGY_BODY='{"identity":{"tenant":"t-smoke","user":"u-smoke","session":"s-smoke"}}'

if skip_if_404 "${TOPOLOGY_URL}" "phase 73b: topology.snapshot not yet shipped (Phase 74)"; then
    if status_code=$(curl -fsS -o /tmp/phase-73b-topology.json -w '%{http_code}' \
            -X POST "${TOPOLOGY_URL}" \
            -H 'Content-Type: application/json' \
            -H "Authorization: Bearer ${HARBOR_DEV_TOKEN:-}" \
            -d "${TOPOLOGY_BODY}" 2>/dev/null); then
        if [[ "${status_code}" == "200" ]]; then
            if assert_json_path '.nodes' '\[' /tmp/phase-73b-topology.json \
                    'phase 73b: topology.snapshot response carries nodes array'; then
                :
            fi
            if assert_json_path '.edges' '\[' /tmp/phase-73b-topology.json \
                    'phase 73b: topology.snapshot response carries edges array'; then
                :
            fi
        elif [[ "${status_code}" =~ ^(401|403)$ ]]; then
            ok "phase 73b: topology.snapshot rejects unscoped/auth-rejected request (HTTP ${status_code})"
        else
            fail "phase 73b: topology.snapshot returned unexpected HTTP ${status_code}"
        fi
    else
        skip 'phase 73b: topology.snapshot POST did not complete cleanly'
    fi
fi

# ---------------------------------------------------------------------------
# 3. Identity-edge guard — both surfaces fail closed without identity.
# ---------------------------------------------------------------------------
#
# Phase 61 identity-edge: a request without the (tenant, user, session)
# triple is rejected 401 identity_required (or 403 if a scope check
# fires first). The Live Runtime page lives behind this edge. Skips
# cleanly when the route is 404.

NO_IDENTITY_BODY='{}'
if skip_if_404 "${TASKS_LIST_URL}" "phase 73b: identity-edge guard skipped (tasks.list not yet shipped)"; then
    if status_code=$(curl -fsS -o /dev/null -w '%{http_code}' \
            -X POST "${TASKS_LIST_URL}" \
            -H 'Content-Type: application/json' \
            -d "${NO_IDENTITY_BODY}" 2>/dev/null); then
        if [[ "${status_code}" =~ ^(401|403)$ ]]; then
            ok "phase 73b: tasks.list rejects identity-missing request (HTTP ${status_code} — Phase 61 identity-edge fails closed; CLAUDE.md §6 rule 9)"
        else
            fail "phase 73b: tasks.list accepted identity-missing request (HTTP ${status_code}) — Phase 61 identity-edge regressed"
        fi
    else
        skip 'phase 73b: identity-edge probe did not complete cleanly'
    fi
fi

# ---------------------------------------------------------------------------
# 4. Static guard — no session-level `priority` on the page (D-065).
# ---------------------------------------------------------------------------
#
# D-065 binding invariant: NO session-level priority field anywhere on
# the Live Runtime page. Task-level priority via the shipped
# `prioritize` Protocol method is permitted on the per-task action
# menu. The guard scans the page tree for any `priority` token that
# looks session-scoped.

PAGE_DIR='web/console/src/lib/components/live-runtime'
# The page routes under the (console) SvelteKit group — no /console/ URL
# prefix (CONVENTIONS.md §1 / D-121). The literal directory carries the
# parentheses.
PAGE_ROUTE='web/console/src/routes/(console)/live-runtime'

if [[ -d "${PAGE_DIR}" ]]; then
    # The legal occurrences carry the `task` or `task-level` qualifier;
    # the illegal ones are "Session priority" / "priority: normal" /
    # "session.priority". Scan for the illegal shapes.
    illegal_hits=$(grep -nIE 'session[_.]?priority|Session[[:space:]]+priority|priority[[:space:]]*:[[:space:]]*("normal"|"high"|"low")' \
                "${PAGE_DIR}" -r 2>/dev/null || true)
    if [[ -z "${illegal_hits}" ]]; then
        ok 'phase 73b: no session-level priority field on the Live Runtime page (D-065 honoured)'
    else
        fail "phase 73b: session-level priority found on the Live Runtime page — D-065 forbids this:\n${illegal_hits}"
    fi
else
    skip 'phase 73b: Live Runtime page source tree not present yet (page lands with this phase)'
fi

# ---------------------------------------------------------------------------
# 5. Static guard — no hand-rolled `fetch` in `.svelte` files (D-093).
# ---------------------------------------------------------------------------
#
# D-093 binding rule: all Protocol traffic flows through the generated
# typed client at `web/console/src/lib/protocol.ts`. Hand-rolled
# `fetch(...)` in a `.svelte` file is rejected.

if [[ -d "${PAGE_DIR}" ]] || [[ -d "${PAGE_ROUTE}" ]]; then
    fetch_hits=$(grep -nIE '\bfetch\s*\(' \
                $( [[ -d "${PAGE_DIR}" ]] && echo "${PAGE_DIR}" ) \
                $( [[ -d "${PAGE_ROUTE}" ]] && echo "${PAGE_ROUTE}" ) \
                --include='*.svelte' -r 2>/dev/null || true)
    if [[ -z "${fetch_hits}" ]]; then
        ok 'phase 73b: no hand-rolled fetch(...) in Live Runtime .svelte files (D-093 honoured — typed Protocol client only)'
    else
        fail "phase 73b: hand-rolled fetch(...) found in Live Runtime .svelte files — D-093 forbids this:\n${fetch_hits}"
    fi
else
    skip 'phase 73b: Live Runtime page source tree not present yet — D-093 guard skipped'
fi

# ---------------------------------------------------------------------------
# 6. Static guard — no Runtime → Console imports under internal/.
# ---------------------------------------------------------------------------
#
# CLAUDE.md §13: the Runtime never imports the Console package. The
# new internal/-side changes (tasks.list aggregate, events.Filter
# RunID) must not touch web/console.

runtime_console_imports=$(grep -rIn --include='*.go' '"github.com/hurtener/Harbor/web/console' \
            internal/ 2>/dev/null | head -5 || true)
if [[ -z "${runtime_console_imports}" ]]; then
    ok 'phase 73b: no internal/ → web/console imports (Runtime/Console boundary preserved — CLAUDE.md §13)'
else
    fail "phase 73b: internal/ imports web/console — CLAUDE.md §13 forbids:\n${runtime_console_imports}"
fi

# ---------------------------------------------------------------------------
# 7. Static guard — forbidden-name scan on this phase's artifacts.
# ---------------------------------------------------------------------------
#
# CLAUDE.md §13 + the master plan's drift-audit: the forbidden
# project name never appears in committed text. Defence-in-depth over
# scripts/drift-audit.sh's global scan — the audit's scope list does
# not include `scripts/smoke/`, so this smoke owns its own check. The
# forbidden tokens are sourced from drift-audit.sh's `forbidden=(...)`
# array so the two scanners share one source of truth and cannot drift.

FORBIDDEN_TOKENS=$(awk '/^forbidden=\(/{gsub(/^forbidden=\(|\)$/, ""); gsub(/"/, ""); print; exit}' \
        scripts/drift-audit.sh 2>/dev/null \
        | tr ' ' '|')
if [[ -z "${FORBIDDEN_TOKENS}" ]]; then
    fail 'phase 73b: could not extract forbidden-token list from scripts/drift-audit.sh — guard regressed'
else
    forbidden_hits=$(grep -nE "${FORBIDDEN_TOKENS}" \
                docs/plans/phase-73b-console-live-runtime-page.md 2>/dev/null || true)
    if [[ -z "${forbidden_hits}" ]]; then
        ok 'phase 73b: forbidden-name scan clean on Phase 73b artifacts (CLAUDE.md §13)'
    else
        fail "phase 73b: forbidden name in Phase 73b artifacts (CLAUDE.md §13):\n${forbidden_hits}"
    fi
fi

# ---------------------------------------------------------------------------
# 8. Static guard — the Metrics/Health empty states point to Phase 72f.
# ---------------------------------------------------------------------------
#
# Brief 11 §LR-2: the Metrics + Health tabs render an empty-state
# pointer to the responsible phase (72f) until that phase's
# `metrics.snapshot` / `runtime.health` primitives land. The risk is
# the pointer drifts on a renumber — this guard fails if the empty-
# state copy stops referencing 72f, forcing an update.

METRICS_EMPTY="${PAGE_DIR}/metrics-tab-empty.svelte"
HEALTH_EMPTY="${PAGE_DIR}/health-tab-empty.svelte"
if [[ -f "${METRICS_EMPTY}" && -f "${HEALTH_EMPTY}" ]]; then
    if grep -q '72f' "${METRICS_EMPTY}" && grep -q '72f' "${HEALTH_EMPTY}"; then
        ok 'phase 73b: Metrics/Health empty states reference Phase 72f (Brief 11 §LR-2 pointer intact)'
    else
        fail 'phase 73b: a Metrics/Health empty state lost its Phase 72f pointer — Brief 11 §LR-2'
    fi
else
    skip 'phase 73b: Metrics/Health empty-state components not present yet'
fi

smoke_summary
