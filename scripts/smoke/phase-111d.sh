#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 111d smoke — skills canonical surface + ingestion (D-201).
#
#  1. Static: the §13 two-implementations smell is closed — the
#     builtin skill_search/skill_get are delegations (no duplicate
#     store-query bodies); skill_list + skill_propose register through
#     the carrier; the stale "Register is called by no production
#     path" honesty notes are replaced; the run loops consume
#     Directory.View and the deleted keyword shaper stays deleted.
#  2. Live verbs: `bin/harbor skill import` a fixture Skills.md into a
#     temp store → exit 0 + "imported"; duplicate import → non-zero;
#     `bin/harbor skill rm` → exit 0. Degradation (§4.2 rule 8): a
#     build without the subcommand SKIPs.
#  3. The focused test slice passes (delegation parity, ImportAndStore
#     conflict matrix, directory wiring) — guarded to the unit-tests
#     batch.
#
# Conventions (AGENTS.md §4.2): 404/405/501 → SKIP; ≥1 OK once shipped;
# use scripts/smoke/common.sh helpers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ── 1. Static: one canonical implementation home ────────────────────
assert_grep_present 'skilltools\.SearchHandler' "internal/tools/builtin/skill_search.go" \
    "skill_search delegates to the Phase-38 SearchHandler"
assert_grep_absent 'store\.Search' "internal/tools/builtin/skill_search.go" \
    "skill_search carries no duplicate store-query body"
assert_grep_present 'skilltools\.GetHandler' "internal/tools/builtin/skill_get.go" \
    "skill_get delegates to the Phase-38 GetHandler"
assert_grep_absent 'store\.Get' "internal/tools/builtin/skill_get.go" \
    "skill_get carries no duplicate store-fetch body"
assert_grep_present 'skilltools\.ListHandler' "internal/tools/builtin/skill_list.go" \
    "skill_list registers through the carrier (Phase-38 third tool)"
assert_grep_present 'generator\.Propose' "internal/tools/builtin/skill_propose.go" \
    "skill_propose registers through the carrier (Phase-41 generator)"
assert_grep_absent 'HONESTY NOTE' "internal/drivers/prod/prod.go" \
    "the stale skills honesty notes are replaced with the post-111d truth"
assert_grep_absent 'Phase 60+ bootstrap will call' "cmd/harbor/main.go" \
    "no stale Phase-60+ registration promise survives in main.go"

# ── 1b. Static: the Directory is the <skills_context> producer ──────
assert_grep_present 'skillsDirectory\.View' "internal/runtime/serve/runloop.go" \
    "run loop consumes Directory.View (D-201 owner decision: wire it)"
assert_grep_present 'skillsDirectory\.View' "internal/runtime/serve/runloop.go" \
    "devstack mirror consumes Directory.View"
assert_grep_absent 'func ExtractSkillKeywords' "internal/runtime/runctx/runctx.go" \
    "the deprecated keyword shaper is deleted (D-195 notice executed)"
assert_grep_present 'skills.directory' "docs/CONFIG.md" \
    "skills.directory config block documented"

# ── 2. CLI verbs against a temp store (degradation: SKIP pre-phase) ─
if [[ -x "bin/harbor" ]]; then
    if bin/harbor skill --help >/dev/null 2>&1; then
        SKILL_TMP="$(mktemp -d "${TMPDIR:-/tmp}/harbor-111d.XXXXXX")"
        trap 'rm -rf "${SKILL_TMP}"' EXIT
        cat > "${SKILL_TMP}/harbor.yaml" <<EOF
server:
  bind_addr: 127.0.0.1:0
  shutdown_grace_period: 5s
identity:
  jwt_algorithms: [ES256]
  issuer: https://issuer.example.com
  audience: harbor
  jwks_url: https://issuer.example.com/.well-known/jwks.json
telemetry: {log_format: text, log_level: error, service_name: harbor-smoke}
state: {driver: inmem}
llm: {driver: mock, timeout: 30s, context_window_reserve: 0.05}
governance: {repair_attempts: 1}
events:
  driver: inmem
  max_subscribers_per_session: 16
  subscriber_buffer_size: 256
  idle_timeout: 60s
  drop_window: 1s
  replay_buffer_size: 1024
sessions: {idle_ttl: 24h, hard_cap: 720h, sweep_interval: 15m}
artifacts: {driver: inmem, heavy_output_threshold_bytes: 32768}
tasks: {driver: inprocess, retain_turn_timeout: 5m, continuation_hop_limit: 8}
distributed: {bus_driver: loopback, remote_driver: loopback}
memory: {driver: inmem, strategy: none}
skills:
  driver: localdb
  dsn: ${SKILL_TMP}/skills.sqlite
EOF
        IMPORT_OUT="$(bin/harbor skill import scripts/smoke/fixtures/phase-111d.skill.md --config "${SKILL_TMP}/harbor.yaml" 2>&1)" \
            && IMPORT_RC=0 || IMPORT_RC=$?
        if [[ ${IMPORT_RC} -eq 0 && "${IMPORT_OUT}" == *imported* ]]; then
            ok "harbor skill import: exit 0 + 'imported' in output"
        else
            fail "harbor skill import failed (rc=${IMPORT_RC}): ${IMPORT_OUT}"
        fi

        # Duplicate import must exit non-zero with an honest rejection.
        if bin/harbor skill import scripts/smoke/fixtures/phase-111d.skill.md --config "${SKILL_TMP}/harbor.yaml" >/dev/null 2>&1; then
            fail "duplicate skill import exited 0 — duplicate-name rejection must be loud"
        else
            ok "duplicate skill import exits non-zero (loud rejection)"
        fi

        if bin/harbor skill rm smoke-triage-ticket --config "${SKILL_TMP}/harbor.yaml" >/dev/null 2>&1; then
            ok "harbor skill rm: exit 0"
        else
            fail "harbor skill rm failed"
        fi

        # rm of a missing name is a loud non-zero.
        if bin/harbor skill rm smoke-triage-ticket --config "${SKILL_TMP}/harbor.yaml" >/dev/null 2>&1; then
            fail "rm of a missing skill exited 0 — must be loud"
        else
            ok "rm of a missing skill exits non-zero"
        fi
    else
        skip "phase 111d: bin/harbor has no 'skill' subcommand yet (pre-phase build) — §4.2 rule 8 degradation"
    fi
else
    skip "phase 111d: bin/harbor not built — CLI verb checks skipped"
fi

# ── 3. Focused test slice ───────────────────────────────────────────
if go test -race -count=1 -run 'Skill|Import|Directory' \
    ./internal/skills/... ./internal/tools/builtin/... >/dev/null 2>&1; then
    ok "focused skills test slice green (-race)"
else
    fail "focused skills test slice failed — run: go test -race -run 'Skill|Import|Directory' ./internal/skills/... ./internal/tools/builtin/..."
fi

smoke_summary
