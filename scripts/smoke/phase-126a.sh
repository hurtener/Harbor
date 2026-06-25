#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 126a — USER-scope agent-config tier + user-keyed store + the one
# durable user-scope WRITE surface (D-256). A new closed-set authority scope
# (`agent_config:user`), a user-keyed durable revision store (the agentcfg
# Registry's ConfigScopeUser arm, keyed under the caller's REAL (tenant, user)
# with a DISTINCT agentcfg.user.* record kind + an __agentcfg__ sentinel
# rejection), and the in-phase consumer: the versioned `agent_config.user.*`
# verb family (get/set_revision/list_revisions/diff/rollback) gated on the new
# scope.
#
# Static guards (always run): the scope constant + closed-set membership; the
# five user methods + tier classifier; the ConfigScope discriminator + the
# namespaced keying + the sentinel rejection; the bounded payload's projection
# fields; the Service verbs; the handler route set + tier gate; the generated
# docs join rows; the TS mirror + manifest coverage.
#
# Live (skip-if-404 per CLAUDE.md §4.2): the user route is mounted. A no-bearer
# POST to /v1/agent_config/user/set_revision is rejected (identity/scope) — NOT
# the route-miss 404 — proving the surface is wired. Skips cleanly on a build
# that does not mount the user-scope store.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# --- Static: the new closed-set scope. ---
assert_grep_present 'ScopeAgentConfigUser Scope = "agent_config:user"' \
  internal/protocol/auth/scopes.go "user-scope authority scope constant"
assert_grep_present 'ScopeAgentConfigUser:' \
  internal/protocol/auth/scopes.go "user scope is in the closed canonicalScopes set"

# --- Static: the five user methods + the tier classifier. ---
assert_grep_present 'MethodAgentConfigUserSetRevision Method = "agent_config.user.set_revision"' \
  internal/protocol/methods/methods.go "user set_revision method"
assert_grep_present 'MethodAgentConfigUserRollback Method = "agent_config.user.rollback"' \
  internal/protocol/methods/methods.go "user rollback method"
assert_grep_present 'func IsAgentConfigUserMethod' \
  internal/protocol/methods/methods.go "user tier method classifier"
assert_grep_present 'canonicalAgentConfigUserMethods' \
  internal/protocol/methods/methods.go "user tier method set"

# --- Static: the durable-config scope discriminator + the namespaced keying. ---
assert_grep_present 'ConfigScopeUser' \
  internal/agentcfg/agentcfg.go "ConfigScope user-scope discriminator"
assert_grep_present 'agentcfg.user.' \
  internal/agentcfg/drivers/statestore/statestore.go "distinct user record-kind prefix"
assert_grep_present 'ErrReservedUser' \
  internal/agentcfg/agentcfg.go "reserved-sentinel rejection error"

# --- Static: the bounded payload carries the band's projection-fed fields. ---
assert_grep_present 'user_prompt' \
  internal/protocol/types/agentconfig.go "AgentConfigUserPayload carries the prompt-projection field"
assert_grep_present 'disabled_servers' \
  internal/protocol/types/agentconfig.go "AgentConfigUserPayload carries the tool-exposure disable field"

# --- Static: the Service verbs (the consumer). ---
assert_grep_present 'func \(s \*Service\) UserSetRevision' \
  internal/runtime/agentcfg/protocol/user.go "user set_revision verb"
assert_grep_present 'func \(s \*Service\) UserRollback' \
  internal/runtime/agentcfg/protocol/user.go "user rollback verb"

# --- Static: the handler route set + the user tier gate. ---
assert_grep_present 'agentConfigUserRoutes' \
  internal/protocol/transports/stream/agentconfig_handler.go "user route tier set"
assert_grep_present '"user/set_revision"' \
  internal/protocol/transports/stream/agentconfig_handler.go "user set_revision route"

# --- Static: generated-docs join rows + TS mirror + manifest. ---
assert_grep_present 'MethodAgentConfigUserSetRevision' \
  cmd/harbor-gen-protocol-docs/methods.go "generated-docs join row for the user verb"
assert_grep_present 'agent_config.user.set_revision' \
  docs/site/protocol/methods.md "generated docs row for the user verb"
assert_grep_present 'AgentConfigUserPayload' \
  web/console/src/lib/protocol/agentconfig.ts "TS mirror of the bounded user payload"
assert_grep_present 'AgentConfigUserSetRevisionRequest' \
  web/console/src/lib/protocol/wire-manifest.gen.json "manifest covers the user request type"

# --- Live (skip-if-404). The user route is mounted + identity/scope-gated. A
# no-bearer POST is rejected (401/403), never the route-miss 404. Skips cleanly
# when the user-scope store is not wired. ---
USER_SET_URL="$(api_url /v1/agent_config/user/set_revision)"
if skip_if_404 "${USER_SET_URL}" "user/set_revision route mounted"; then
  status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{"agent_id":"a1","payload":{"user_prompt":"x"}}' \
    "${USER_SET_URL}" || true)
  case "${status}" in
    401|403)
      ok "user/set_revision is mounted and fails closed without a verified scope (${status})"
      ;;
    *)
      fail "user/set_revision unverified POST expected 401/403, got ${status}"
      ;;
  esac
fi

smoke_summary
