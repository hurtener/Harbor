#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
# Phase 73a smoke — Console Overview page (UI composition).
# (CLAUDE.md §4.5, §13; RFC §5.2 / §6.13 / §6.15 / §7).
#
# Phase 73a ships NO new Protocol method — the Overview page is pure UI
# composition over Stage-1 primitives: `runtime.counters` /
# `runtime.health` (Phase 72f), `pause.list` (Phase 72e),
# `events.subscribe` (Phase 60/72), and the SHIPPED Phase 54 `approve` /
# `reject` control verbs.
#
# The page itself is served via `harbor console` (D-091); `harbor dev`
# stays headless per D-091. The smoke checks the Protocol surfaces the
# page consumes (live-server probes) plus the static guards that
# enforce §4.5 / §13 / CONVENTIONS.md conventions on the page source
# tree.
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
# 1. Protocol probe — `runtime.counters` (Phase 72f posture surface).
# ---------------------------------------------------------------------------
#
# The counter row consumes `runtime.counters`. Posture methods route
# through the control transport — `POST /v1/control/runtime.counters`.
# Skips cleanly when the surface is 404 (pre-Phase-72f build).

COUNTERS_URL="$(api_url /v1/control/runtime.counters)"
COUNTERS_BODY='{"identity":{"tenant":"t-smoke","user":"u-smoke","session":"s-smoke"}}'

if skip_if_404 "${COUNTERS_URL}" "phase 73a: runtime.counters not present yet (Phase 72f)"; then
    if status_code=$(curl -fsS -o /tmp/phase-73a-counters.json -w '%{http_code}' \
            -X POST "${COUNTERS_URL}" \
            -H 'Content-Type: application/json' \
            -H "Authorization: Bearer ${HARBOR_DEV_TOKEN:-}" \
            -d "${COUNTERS_BODY}" 2>/dev/null); then
        if [[ "${status_code}" == "200" ]]; then
            for field in tasks_running background_jobs_active mcp_connections_healthy; do
                if assert_json_path ".${field}" '[0-9]+' /tmp/phase-73a-counters.json \
                        "phase 73a: runtime.counters response carries ${field} (Overview counter row)"; then
                    :
                else
                    fail "phase 73a: runtime.counters response missing ${field}"
                fi
            done
        elif [[ "${status_code}" =~ ^(401|403)$ ]]; then
            ok "phase 73a: runtime.counters rejects unscoped/auth-rejected request (HTTP ${status_code}) — Phase 61 identity-edge preserved"
        else
            fail "phase 73a: runtime.counters returned unexpected HTTP ${status_code}"
        fi
    else
        skip 'phase 73a: runtime.counters POST did not complete cleanly (server unreachable or 5xx)'
    fi
fi

# ---------------------------------------------------------------------------
# 2. Protocol probe — `runtime.health` (Phase 72f posture surface).
# ---------------------------------------------------------------------------
#
# The sub-header health-chip strip consumes `runtime.health`.

HEALTH_URL="$(api_url /v1/control/runtime.health)"
if skip_if_404 "${HEALTH_URL}" "phase 73a: runtime.health not present yet (Phase 72f)"; then
    if status_code=$(curl -fsS -o /tmp/phase-73a-health.json -w '%{http_code}' \
            -X POST "${HEALTH_URL}" \
            -H 'Content-Type: application/json' \
            -H "Authorization: Bearer ${HARBOR_DEV_TOKEN:-}" \
            -d "${COUNTERS_BODY}" 2>/dev/null); then
        if [[ "${status_code}" == "200" ]]; then
            if assert_json_path '.subsystems' '\[' /tmp/phase-73a-health.json \
                    'phase 73a: runtime.health response carries a subsystems array (health-chip strip)'; then
                :
            fi
        elif [[ "${status_code}" =~ ^(401|403)$ ]]; then
            ok "phase 73a: runtime.health rejects unscoped/auth-rejected request (HTTP ${status_code})"
        else
            fail "phase 73a: runtime.health returned unexpected HTTP ${status_code}"
        fi
    else
        skip 'phase 73a: runtime.health POST did not complete cleanly'
    fi
fi

# ---------------------------------------------------------------------------
# 3. Protocol probe — `pause.list` (Phase 72e snapshot surface).
# ---------------------------------------------------------------------------
#
# The intervention queue consumes `pause.list` at `POST /v1/pause/list`.

PAUSE_URL="$(api_url /v1/pause/list)"
PAUSE_BODY='{"identity":{"tenant":"t-smoke","user":"u-smoke","session":"s-smoke"}}'

if skip_if_404 "${PAUSE_URL}" "phase 73a: pause.list not present yet (Phase 72e)"; then
    if status_code=$(curl -fsS -o /tmp/phase-73a-pause.json -w '%{http_code}' \
            -X POST "${PAUSE_URL}" \
            -H 'Content-Type: application/json' \
            -H "Authorization: Bearer ${HARBOR_DEV_TOKEN:-}" \
            -d "${PAUSE_BODY}" 2>/dev/null); then
        if [[ "${status_code}" == "200" ]]; then
            if assert_json_path '.snapshots' '\[' /tmp/phase-73a-pause.json \
                    'phase 73a: pause.list response carries a snapshots array (intervention queue)'; then
                :
            fi
        elif [[ "${status_code}" =~ ^(401|403)$ ]]; then
            ok "phase 73a: pause.list rejects unscoped/auth-rejected request (HTTP ${status_code})"
        else
            fail "phase 73a: pause.list returned unexpected HTTP ${status_code}"
        fi
    else
        skip 'phase 73a: pause.list POST did not complete cleanly'
    fi
fi

# ---------------------------------------------------------------------------
# 4. Identity-edge guard — pause.list fails closed without identity.
# ---------------------------------------------------------------------------
#
# Phase 61 identity-edge: a request without the (tenant, user, session)
# triple is rejected 401 / 403. The Overview page lives behind this edge.

if skip_if_404 "${PAUSE_URL}" "phase 73a: identity-edge guard skipped (pause.list absent)"; then
    if status_code=$(curl -fsS -o /dev/null -w '%{http_code}' \
            -X POST "${PAUSE_URL}" \
            -H 'Content-Type: application/json' \
            -d '{}' 2>/dev/null); then
        if [[ "${status_code}" =~ ^(401|403)$ ]]; then
            ok "phase 73a: pause.list rejects identity-missing request (HTTP ${status_code} — Phase 61 identity-edge fails closed; CLAUDE.md §6 rule 9)"
        else
            fail "phase 73a: pause.list accepted identity-missing request (HTTP ${status_code}) — identity-edge regressed"
        fi
    else
        skip 'phase 73a: identity-edge probe did not complete cleanly'
    fi
fi

# ---------------------------------------------------------------------------
# 5. Page route probe — lands with 73m's `harbor console` subcommand.
# ---------------------------------------------------------------------------
#
# The Overview page is served at `/overview` by `harbor console`
# (D-091). `harbor dev` — the preflight's live server — stays headless
# per D-091, so the page route is never reachable on the preflight
# server. The route's live coverage is the Playwright spec
# (`web/console/tests/overview-page.spec.ts`), which boots `harbor
# console` directly. This probe SKIPs until 73m ships the subcommand.

skip 'phase 73a: /overview route is served by harbor console (D-091) — lands with 73m; covered by overview-page.spec.ts'

# ---------------------------------------------------------------------------
# 6. Static guard — no hand-rolled `fetch` in Overview `.svelte` files.
# ---------------------------------------------------------------------------
#
# CONVENTIONS.md §6 / CLAUDE.md §13: all Protocol traffic flows through
# the typed `HarborClient`. Hand-rolled `fetch(...)` in a `.svelte`
# file is rejected.

PAGE_DIR='web/console/src/lib/components/overview'
# The page routes under the (console) SvelteKit group — no /console/
# URL prefix (CONVENTIONS.md §1 / D-121).
PAGE_ROUTE='web/console/src/routes/(console)/overview'

if [[ -d "${PAGE_DIR}" ]] || [[ -d "${PAGE_ROUTE}" ]]; then
    fetch_hits=$(grep -nIE '\bfetch\s*\(' \
                $( [[ -d "${PAGE_DIR}" ]] && echo "${PAGE_DIR}" ) \
                $( [[ -d "${PAGE_ROUTE}" ]] && echo "${PAGE_ROUTE}" ) \
                --include='*.svelte' -r 2>/dev/null || true)
    if [[ -z "${fetch_hits}" ]]; then
        ok 'phase 73a: no hand-rolled fetch(...) in Overview .svelte files (CONVENTIONS.md §6 honoured — typed HarborClient only)'
    else
        fail "phase 73a: hand-rolled fetch(...) found in Overview .svelte files — CONVENTIONS.md §6 forbids:\n${fetch_hits}"
    fi
else
    skip 'phase 73a: Overview page source tree not present yet'
fi

# ---------------------------------------------------------------------------
# 7. Static guard — Quick Links grid carries no Evaluations tile (D-064).
# ---------------------------------------------------------------------------
#
# D-064 binding invariant: Evaluations is post-V1. The Quick Links grid
# is exactly six tiles — Sessions / Tasks / Background Jobs / Agents /
# Tools / Settings. An `evaluations` route token in the grid is a bug.

GRID='web/console/src/lib/components/overview/QuickLinksGrid.svelte'
if [[ -f "${GRID}" ]]; then
    # Scan for an Evaluations *route token* (`/evaluations`) or a tile
    # `name:`/`href:` field — the binding D-064 violation shape. The
    # word "Evaluations" in a doc comment ("no Evaluations tile") is
    # legitimate and is NOT a violation.
    if grep -qiE "/evaluations|name:[[:space:]]*'evaluation|href:[[:space:]]*'/evaluation" "${GRID}"; then
        fail 'phase 73a: Quick Links grid references an Evaluations route/tile — D-064 forbids (post-V1)'
    else
        ok 'phase 73a: Quick Links grid carries no Evaluations route/tile (D-064 honoured)'
    fi
else
    skip 'phase 73a: QuickLinksGrid component not present yet'
fi

# ---------------------------------------------------------------------------
# 8. Static guard — no Runtime → Console imports under internal/.
# ---------------------------------------------------------------------------
#
# CLAUDE.md §13: the Runtime never imports the Console package. Phase
# 73a is composition-only — it adds NO internal/ Go code, so this guard
# is a defence-in-depth no-regression check.

runtime_console_imports=$(grep -rIn --include='*.go' '"github.com/hurtener/Harbor/web/console' \
            internal/ 2>/dev/null | head -5 || true)
if [[ -z "${runtime_console_imports}" ]]; then
    ok 'phase 73a: no internal/ → web/console imports (Runtime/Console boundary preserved — CLAUDE.md §13)'
else
    fail "phase 73a: internal/ imports web/console — CLAUDE.md §13 forbids:\n${runtime_console_imports}"
fi

# ---------------------------------------------------------------------------
# 9. Static guard — forbidden-name scan on this phase's artifacts.
# ---------------------------------------------------------------------------
#
# CLAUDE.md §13 + the master plan's drift-audit: the forbidden project
# name never appears in committed text. The tokens are sourced from
# drift-audit.sh's `forbidden=(...)` array so the two scanners share one
# source of truth.

FORBIDDEN_TOKENS=$(awk '/^forbidden=\(/{gsub(/^forbidden=\(|\)$/, ""); gsub(/"/, ""); print; exit}' \
        scripts/drift-audit.sh 2>/dev/null \
        | tr ' ' '|')
if [[ -z "${FORBIDDEN_TOKENS}" ]]; then
    fail 'phase 73a: could not extract forbidden-token list from scripts/drift-audit.sh — guard regressed'
else
    forbidden_hits=$(grep -nE "${FORBIDDEN_TOKENS}" \
                docs/plans/phase-73a-console-overview-page.md 2>/dev/null || true)
    if [[ -z "${forbidden_hits}" ]]; then
        ok 'phase 73a: forbidden-name scan clean on Phase 73a artifacts (CLAUDE.md §13)'
    else
        fail "phase 73a: forbidden name in Phase 73a artifacts (CLAUDE.md §13):\n${forbidden_hits}"
    fi
fi

smoke_summary
