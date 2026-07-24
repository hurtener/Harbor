#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 200 — Per-user credential injection for receiver-style MCP servers (HA-34 / D-341).
#
# The injection mapping is CONFIG-ONLY (no Protocol surface — a non-goal this
# phase), so the assertions are:
#   1. Static redaction guard: the audit rule set covers every injected form
#      (Basic scheme + arbitrary vendor header keys + `_meta` credential leaves)
#      so an injected credential can never reach an audit payload uncredacted.
#   2. Static schema guard: the non-secret injection-mapping surface + the
#      fail-closed guards (one auth mode per connection; redaction-covered target
#      key; reserved `_meta` segment) exist in the config validator.
#   3. `bin/harbor validate` REJECTS a connection declaring BOTH an injection
#      mapping and a bearer oauth_provider (one auth mode per connection).
# A live dev-receiver fixture is not part of preflight, so the live probe SKIPs.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# 1. Static redaction guard — every injected form is held to *** by the redactor.
# ----------------------------------------------------------------------------
RULES="internal/audit/rules.go"
for RULE in basic_in_value injection_credential; do
    if grep -q "\"${RULE}\"" "${RULES}" 2>/dev/null; then
        ok "static: audit rule ${RULE} covers a receiver-style injection form"
    else
        fail "static: audit rule ${RULE} missing — an injected credential could leak uncredacted (§7/§13)"
    fi
done
if grep -q "IsReceiverInjectionCredentialKey" internal/config/validate.go 2>/dev/null &&
    grep -q "config.IsReceiverInjectionCredentialKey" "${RULES}" 2>/dev/null; then
    ok "static: the redaction-coverage predicate is single-sourced (validation ⟺ redactor)"
else
    fail "static: redaction-coverage predicate not shared between validation and the redactor"
fi

# ----------------------------------------------------------------------------
# 2. Static schema guard — the non-secret mapping + the fail-closed guards exist.
# ----------------------------------------------------------------------------
if grep -q "MCPCredentialInjectionConfig" internal/config/config.go 2>/dev/null; then
    ok "static: the non-secret injection mapping is on the MCP connection descriptor"
else
    fail "static: MCPCredentialInjectionConfig absent from the config schema"
fi
if grep -q "one auth mode per connection" internal/config/validate.go 2>/dev/null; then
    ok "static: the one-auth-mode guard is enforced in config validation"
else
    fail "static: the one-auth-mode guard is missing from config validation"
fi

# ----------------------------------------------------------------------------
# 3. `bin/harbor validate` rejects injection + bearer on one connection.
# ----------------------------------------------------------------------------
if [[ -x "${ROOT}/bin/harbor" ]]; then
    tmp_dir="$(mktemp -d -t harbor-smoke200-XXXXXX)"
    trap 'rm -rf "${tmp_dir}"' EXIT
    cat > "${tmp_dir}/conflict.yaml" <<'YAML'
server:
  bind_addr: 127.0.0.1:0
  shutdown_grace_period: 5s
identity:
  jwt_algorithms: [RS256, ES256]
  issuer: https://issuer.example.com
  audience: harbor
  jwks_url: https://issuer.example.com/.well-known/jwks.json
telemetry:
  log_format: text
  log_level: info
  service_name: harbor-test
state:
  driver: inmem
llm:
  driver: bifrost
  provider: openrouter
  model: anthropic/claude-sonnet-4
  api_key: env.HARBOR_TEST_FAKE
  timeout: 60s
  context_window_reserve: 0.05
  model_profiles:
    anthropic/claude-sonnet-4:
      context_window_tokens: 200000
      token_estimator: chars_div_4
      json_schema_mode: native
governance:
  repair_attempts: 3
events:
  driver: inmem
  max_subscribers_per_session: 16
  subscriber_buffer_size: 256
  idle_timeout: 60s
  drop_window: 1s
  replay_buffer_size: 10000
sessions:
  idle_ttl: 24h
  hard_cap: 720h
  sweep_interval: 15m
artifacts:
  driver: inmem
  heavy_output_threshold_bytes: 32768
tasks:
  driver: inprocess
  retain_turn_timeout: 5m
  continuation_hop_limit: 8
distributed:
  bus_driver: loopback
  remote_driver: loopback
memory:
  driver: inmem
  strategy: none
tools:
  oauth_token_kek_env: HARBOR_TOOL_OAUTH_KEK
  oauth_providers:
    - name: recv-broker
      driver: tokenexchange
      client_id_env: RECV_CLIENT_ID
      client_secret_env: RECV_CLIENT_SECRET
      token_url: https://broker.example.test/token
      allowed_downstream_hosts: [recv.example.test]
  mcp_servers:
    - name: receiver
      transport_mode: streamable_http
      url: https://recv.example.test
      oauth_provider: recv-broker
      injection:
        provider: recv-broker
        form: basic
YAML
    if "${ROOT}/bin/harbor" validate "${tmp_dir}/conflict.yaml" >"${tmp_dir}/validate.log" 2>&1; then
        fail 'phase 200: `harbor validate` ACCEPTED injection + oauth_provider on one connection (one-auth-mode breach)'
        echo "    --- validate output ---"
        sed 's/^/    /' "${tmp_dir}/validate.log"
        echo "    --- end ---"
    elif grep -qi "one auth mode\|injection" "${tmp_dir}/validate.log"; then
        ok 'phase 200: `harbor validate` rejects injection + oauth_provider (one auth mode per connection)'
    else
        fail 'phase 200: `harbor validate` rejected the config but not for the injection conflict'
        echo "    --- validate output ---"
        sed 's/^/    /' "${tmp_dir}/validate.log"
        echo "    --- end ---"
    fi
else
    skip "phase 200: bin/harbor not built — validate assertion skipped"
fi

# ----------------------------------------------------------------------------
# 4. Live dev-receiver fixture probe — not part of preflight; SKIP cleanly.
# ----------------------------------------------------------------------------
skip "phase 200: live receiver-style fixture probe — no dev receiver server in preflight (covered by the -race integration test)"

smoke_summary
