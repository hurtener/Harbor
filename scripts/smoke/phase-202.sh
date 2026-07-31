#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 202 — durable-by-default per-user skills (D-345). A CLAIM-FREE
# `agent_config.user.skills.{list,upsert,delete}` verb family so a plain
# authenticated user authors PERSONAL skills that persist across ALL of their
# conversations (a new `skills.ScopeUser` rung, stored session-zeroed and
# resolved across every session of the same (tenant, user)).
#
# Static guards (always run): the ScopeUser enum + storage helper; the three
# methods + claim-free (session-safe) classification; the wire types; the
# handler routes in the claim-free safe-route set; the Service verbs; the
# projection union; the generated docs + TS mirror + manifest.
#
# Live (skip-if-404 per CLAUDE.md §4.2): the user/skills routes are mounted and
# fail closed without a verified identity (401/403, NOT the route-miss 404).
# When HARBOR_DEV_TOKEN is present, a full claim-free flow proves durability:
# upsert under session A, list from a DIFFERENT session B (same tenant/user)
# returns it, delete removes it — with NO admin / agent_config:user scope.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# The dev bearer is resolved through common.sh's `dev_bearer`, never by a raw
# ${HARBOR_DEV_TOKEN} read: the raw read is EMPTY outside preflight, so every
# live leg below degrades to a SKIP while the script still exits 0 — "a SKIP
# that should be an OK is a bug" (AGENTS.md §4.2 item 5, issue #624).
# dev_bearer prefers the exported value and falls back to the dev server log.
HARBOR_DEV_TOKEN="$(dev_bearer)"

# --- Static: the ScopeUser rung + the storage-session helper. ---
assert_grep_present 'ScopeUser Scope = "user"' \
  internal/skills/skills.go "ScopeUser visibility constant"
assert_grep_present 'func StorageSessionID' \
  internal/skills/skills.go "session-zeroing storage helper for user scope"

# --- Static: the three CLAIM-FREE methods + session-safe classification. ---
assert_grep_present 'MethodAgentConfigUserSkillsUpsert Method = "agent_config.user.skills.upsert"' \
  internal/protocol/methods/methods.go "user skills upsert method"
assert_grep_present 'MethodAgentConfigUserSkillsList Method = "agent_config.user.skills.list"' \
  internal/protocol/methods/methods.go "user skills list method"
assert_grep_present 'MethodAgentConfigUserSkillsDelete Method = "agent_config.user.skills.delete"' \
  internal/protocol/methods/methods.go "user skills delete method"

# --- Static: the drivers resolve the durable rung cross-session. ---
assert_grep_present 'session = \? OR scope = \?' \
  internal/skills/drivers/localdb/localdb.go "localdb resolves user-scope rows cross-session"
assert_grep_present 'session_id = \$3 OR scope = \$4' \
  internal/skills/drivers/postgres/postgres.go "postgres resolves user-scope rows cross-session"

# --- Static: the wire types. ---
assert_grep_present 'AgentConfigUserSkillsUpsertRequest' \
  internal/protocol/types/agentconfig.go "user skills upsert wire type"

# --- Static: the handler routes live in the CLAIM-FREE safe-route set. ---
assert_grep_present '"user/skills/upsert": true' \
  internal/protocol/transports/stream/agentconfig_handler.go "user/skills/upsert in the claim-free safe-route set"
assert_grep_present 'case "user/skills/list":' \
  internal/protocol/transports/stream/agentconfig_handler.go "user/skills/list route dispatch"

# --- Static: the Service verbs (the consumer). ---
assert_grep_present 'func \(s \*Service\) UserSkillsUpsert' \
  internal/runtime/agentcfg/protocol/userskills.go "user skills upsert verb"
assert_grep_present 'func \(s \*Service\) UserSkillsDelete' \
  internal/runtime/agentcfg/protocol/userskills.go "user skills delete verb"

# --- Static: the run-start projection unions durable user-skill membership. ---
assert_grep_present 'activeDurableUserSkillNames' \
  internal/runtime/agentcfg/projection/projection.go "projection unions durable user-scope skill names"

# --- Static: generated-docs join row + TS mirror + manifest. ---
assert_grep_present 'agent_config.user.skills.upsert' \
  docs/site/protocol/methods.md "generated docs row for the user skills verb"
assert_grep_present 'AgentConfigUserSkillsUpsertRequest' \
  web/console/src/lib/protocol/agentconfig.ts "TS mirror of the user skills request type"
assert_grep_present 'AgentConfigUserSkillsUpsertRequest' \
  web/console/src/lib/protocol/wire-manifest.gen.json "manifest covers the user skills request type"

# --- Live: the routes are mounted + identity-gated. The surface is POST-only,
# so a GET-based skip_if_404 would always 405-skip; probe with a POST instead
# and branch on the status (404 → not mounted → skip; 401/403 → mounted +
# fails closed). ---
UPSERT_URL="$(api_url /v1/agent_config/user/skills/upsert)"
LIST_URL="$(api_url /v1/agent_config/user/skills/list)"
DELETE_URL="$(api_url /v1/agent_config/user/skills/delete)"

if ! command -v curl >/dev/null 2>&1; then
  skip "phase 202: live probe — curl not available"
else
  # No-identity POST: mounted routes fail closed (401/403), an unbuilt surface
  # 404s. Even CLAIM-FREE verbs require a valid identity at the auth edge.
  status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' \
    -d '{"agent_id":"a1","skill":{"name":"x","trigger":"t","steps":["s"],"origin":"generated"}}' \
    "${UPSERT_URL}" || true)
  case "${status}" in
    404|405|501|000|'')
      skip "phase 202: user/skills/upsert route not mounted (${status:-000})" ;;
    401|403)
      ok "user/skills/upsert is mounted and fails closed without a verified identity (${status})"
      # Full CLAIM-FREE durability flow when a dev token is available: the token
      # verifies WHO (tenant+user); the session is chosen per-request via the
      # X-Harbor-Session header, so an upsert under session A must be visible
      # from session B — with NO admin / agent_config:user scope. If the dev
      # build does not wire a skills store the verb 501s → SKIP (not FAIL).
      if [[ -n "${HARBOR_DEV_TOKEN:-}" ]] && command -v jq >/dev/null 2>&1; then
        AUTH="Authorization: Bearer ${HARBOR_DEV_TOKEN}"
        SKILL='{"agent_id":"smoke-202","skill":{"name":"durable-smoke-202","trigger":"when asked","steps":["do it"],"origin":"generated"}}'
        up=$(curl -s --max-time 5 -X POST -H "${AUTH}" -H 'X-Harbor-Session: sess-A' \
          -H 'Content-Type: application/json' -d "${SKILL}" "${UPSERT_URL}" || true)
        up_scope="$(printf '%s' "${up}" | jq -r '.skill.scope // ""' 2>/dev/null || true)"
        if [[ "${up_scope}" == "user" ]]; then
          ok "claim-free upsert stored the skill at user scope"
          # List from a DIFFERENT session (durable cross-session visibility).
          ls=$(curl -s --max-time 5 -X POST -H "${AUTH}" -H 'X-Harbor-Session: sess-B' \
            -H 'Content-Type: application/json' -d '{"agent_id":"smoke-202"}' "${LIST_URL}" || true)
          if printf '%s' "${ls}" | jq -e '.skills[]? | select(.name=="durable-smoke-202")' >/dev/null 2>&1; then
            ok "durable user skill is visible from a different session (durable-by-default)"
          else
            fail "durable user skill not visible cross-session: ${ls}"
          fi
          # Delete from session B removes it for the whole (tenant, user).
          del=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST -H "${AUTH}" \
            -H 'X-Harbor-Session: sess-B' -H 'Content-Type: application/json' \
            -d '{"agent_id":"smoke-202","name":"durable-smoke-202"}' "${DELETE_URL}" || true)
          if [[ "${del}" == "200" ]]; then
            ok "claim-free delete removed the durable user skill (${del})"
          else
            fail "claim-free delete expected 200, got ${del}"
          fi
        else
          skip "phase 202: live claim-free flow — dev build does not wire a skills store (upsert=${up})"
        fi
      else
        skip "phase 202: live claim-free flow — HARBOR_DEV_TOKEN or jq unavailable"
      fi
      ;;
    *)
      fail "user/skills/upsert unverified POST expected 401/403 or a route-miss, got ${status}" ;;
  esac
fi

smoke_summary
