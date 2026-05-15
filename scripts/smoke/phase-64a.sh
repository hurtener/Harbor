#!/usr/bin/env bash
# Phase 64a smoke — Tool catalog OAuth + approval wiring (D-090).
#
# Phase 64a ships `internal/tools/catalog` — the operator-config-driven
# wiring builder that auto-wraps registered tool descriptors with the
# matching `approval.ApprovalGate` and / or OAuth-aware invocation
# wrapper. The wiring runs at `harbor dev` boot from `cfg.Tools.Entries`.
#
# This is a code-only wiring phase: no NEW Protocol surface lands
# here (the wire-side `approve` / `reject` Protocol methods already
# shipped in Phase 54+). The smoke therefore exercises:
#
#   1. The package test suite (catalog wrappers + D-025 + policy
#      allowlist mirror).
#   2. The Phase 64a integration test — full APPROVE / REJECT round-
#      trip + OAuth wrapper + identity propagation + concurrency
#      stress + failure modes.
#   3. The `harbor validate` surface accepts a config with `tools.entries`.
#   4. The `harbor validate` surface REJECTS a config with an unknown
#      policy name (the §13 amendment fail-loud guard at validate time).
#   5. Static guards on the catalog package shape.
#
# The 404/405/501 → SKIP convention is unused here — no NEW HTTP
# surfaces ship in this phase.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

CATALOG_PKG="internal/tools/catalog"

# ----------------------------------------------------------------------
# Assertion 1 — package test suite under -race.
# Covers: wrapper composition (approval outermost), policy resolution,
# unknown-tool / unknown-policy / unknown-provider fail-loud, D-025
# N=128 concurrent reuse, identity propagation, allowlist mirror.
# ----------------------------------------------------------------------
test_log=$(mktemp)
if go test -race -count=1 -timeout 180s ./${CATALOG_PKG}/... >"${test_log}" 2>&1; then
    ok 'phase 64a: internal/tools/catalog tests pass under -race (wrappers + D-025 + policy/binding-scope allowlist mirror)'
    rm -f "${test_log}"
else
    fail 'phase 64a: catalog tests failed (run `go test -race ./internal/tools/catalog/...` for detail)'
    echo "    --- go test output (tail 60 lines) ---"
    tail -60 "${test_log}" | sed 's/^/    /'
    echo "    --- end ---"
    rm -f "${test_log}"
fi

# ----------------------------------------------------------------------
# Assertion 2 — Phase 64a integration test under -race.
# Full APPROVE/REJECT cycle + OAuth wrapper + composition order +
# failure mode + concurrency stress + goroutine-leak.
# ----------------------------------------------------------------------
test_log=$(mktemp)
if go test -race -count=1 -timeout 180s -run 'TestE2E_Phase64a' ./test/integration/... >"${test_log}" 2>&1; then
    ok 'phase 64a: catalog-wiring integration test passes under -race (APPROVE + REJECT + OAuth + composition order + concurrency stress + leak)'
    rm -f "${test_log}"
else
    fail 'phase 64a: integration test failed (run `go test -race -run TestE2E_Phase64a ./test/integration/...` for detail)'
    echo "    --- go test output (tail 60 lines) ---"
    tail -60 "${test_log}" | sed 's/^/    /'
    echo "    --- end ---"
    rm -f "${test_log}"
fi

# ----------------------------------------------------------------------
# Assertion 3 — `harbor validate` accepts a config with valid
# `tools.entries`. The Phase 68 validator picks up the new fields
# automatically through `internal/config.Validate`.
# ----------------------------------------------------------------------
if [[ -x "${ROOT}/bin/harbor" ]]; then
    tmp_dir="$(mktemp -d -t harbor-smoke64a-XXXXXX)"
    trap 'rm -rf "${tmp_dir}"' EXIT

    # Write a config with valid tools.entries.
    cat > "${tmp_dir}/valid.yaml" <<'YAML'
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
  entries:
    - name: delete_doc
      approval:
        policy: deny-all
        reason: "deletion requires human review"
    - name: github_read
      oauth:
        provider: github
        binding_scope: user
YAML
    if "${ROOT}/bin/harbor" validate "${tmp_dir}/valid.yaml" >"${tmp_dir}/validate-ok.log" 2>&1; then
        ok 'phase 64a: `harbor validate` accepts a config with tools.entries (approval + oauth)'
    else
        fail 'phase 64a: `harbor validate` rejected a valid tools.entries config'
        echo "    --- validate output ---"
        cat "${tmp_dir}/validate-ok.log" | sed 's/^/    /'
        echo "    --- end ---"
    fi

    # Write a SECOND config with an unknown approval policy. Building
    # it from scratch (rather than appending to the valid one) keeps
    # YAML indentation correct.
    cat > "${tmp_dir}/bad-policy.yaml" <<'YAML2'
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
  entries:
    - name: bad_tool
      approval:
        policy: not-a-real-policy
YAML2
    if "${ROOT}/bin/harbor" validate "${tmp_dir}/bad-policy.yaml" >"${tmp_dir}/validate-bad.log" 2>&1; then
        fail 'phase 64a: `harbor validate` accepted an unknown approval policy (should fail closed)'
    elif grep -qE 'approval.policy|not-a-real-policy' "${tmp_dir}/validate-bad.log"; then
        ok 'phase 64a: `harbor validate` rejects unknown approval policy with named-field error'
    else
        fail 'phase 64a: `harbor validate` rejected the config but the error lacks the expected field hint'
        echo "    --- validate output ---"
        cat "${tmp_dir}/validate-bad.log" | sed 's/^/    /'
        echo "    --- end ---"
    fi
else
    skip 'phase 64a: `harbor validate` smoke (bin/harbor not built)'
fi

# ----------------------------------------------------------------------
# Assertion 4 — static guards on the catalog package shape.
# ----------------------------------------------------------------------
CATALOG_FILE="${CATALOG_PKG}/catalog.go"
if [[ ! -f "${CATALOG_FILE}" ]]; then
    fail "phase 64a: ${CATALOG_FILE} missing"
else
    if grep -q 'type Builder struct' "${CATALOG_FILE}" && \
       grep -q 'func.*WrapWithApproval' "${CATALOG_FILE}" && \
       grep -q 'func.*WrapWithOAuth' "${CATALOG_FILE}"; then
        ok 'phase 64a: Builder + WrapWithApproval + WrapWithOAuth declared (the master-plan acceptance surface)'
    else
        fail 'phase 64a: catalog.go does not declare Builder + WrapWithApproval + WrapWithOAuth'
    fi
fi

# ----------------------------------------------------------------------
# Assertion 5 — static guard: ErrToolNotRegistered + ErrUnknownApprovalPolicy
# + ErrUnknownOAuthProvider sentinels exist (the §13 fail-loud guards).
# ----------------------------------------------------------------------
if grep -q 'ErrToolNotRegistered' "${CATALOG_FILE}" && \
   grep -q 'ErrUnknownApprovalPolicy' "${CATALOG_FILE}" && \
   grep -q 'ErrUnknownOAuthProvider' "${CATALOG_FILE}"; then
    ok 'phase 64a: ErrToolNotRegistered / ErrUnknownApprovalPolicy / ErrUnknownOAuthProvider sentinels declared (§13 fail-loud trip wires)'
else
    fail 'phase 64a: catalog.go does not declare the three §13 fail-loud sentinels'
fi

# ----------------------------------------------------------------------
# Assertion 6 — static guard: composition order pinned (approval
# outermost). D-090 settles this; the comment in catalog.go references
# D-090 explicitly.
# ----------------------------------------------------------------------
if grep -q 'D-090' "${CATALOG_FILE}" && grep -q 'approval ( oauth' "${CATALOG_FILE}"; then
    ok 'phase 64a: composition order pin (approval outermost) documented in package doc (D-090)'
else
    fail 'phase 64a: composition order pin not documented in catalog.go'
fi

smoke_summary
