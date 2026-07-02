#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 149 smoke — HTTP-manifest boot loader wiring (D-279).
#
# Unit-tests-class (the phase-64a / phase-142 precedent): the preflight dev
# server boots once with a fixed config carrying `http_manifests: []`, so the
# boot path is exercised by the -race integration test and the validate flip
# by the built binary's `validate` subcommand against temp configs — no
# network endpoint is touched, keeping this in the parallel batch.
#
# Assertions:
#   1. `go test -race` for internal/config (the validate-flip + loader
#      path-resolution tests).
#   2. `go test -race -run Phase149` for the boot-loader integration test
#      (resolve+invoke, OAuth-wrapped pre-check, 2 failure modes, D-025
#      concurrent-reuse).
#   3. `bin/harbor validate` ACCEPTS a temp config declaring
#      `tools.http_manifests` pointing at a real temp manifest (SKIP — not
#      FAIL — if the binary still emits the pre-149 "not loaded at boot yet"
#      rejection, per the §4.2 coexistence convention).
#   4. `bin/harbor validate` REJECTS a temp config whose relative manifest
#      entry escapes the config directory (§7 rule 5), naming
#      `tools.http_manifests` in the output.
#   5. Static greps: internal/runtime/assemble/assemble.go references
#      LoadManifest/RegisterManifest; internal/config/validate.go no longer
#      contains the "not loaded at boot yet" rejection string.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------
# Assertion 1 — internal/config validate-flip + loader path-resolution
# tests under -race.
# ----------------------------------------------------------------------
test_log=$(mktemp)
if go test -race -count=1 -timeout 180s ./internal/config/... >"${test_log}" 2>&1; then
    ok 'phase 149: internal/config tests pass under -race (http_manifests validate-flip + loader path resolution)'
    rm -f "${test_log}"
else
    fail 'phase 149: internal/config tests failed (run `go test -race ./internal/config/...` for detail)'
    echo "    --- go test output (tail 60 lines) ---"
    tail -60 "${test_log}" | sed 's/^/    /'
    echo "    --- end ---"
    rm -f "${test_log}"
fi

# ----------------------------------------------------------------------
# Assertion 2 — Phase 149 integration test under -race: resolve+invoke
# round trip, OAuth-wrapped broker pre-check, 2 boot failure modes,
# D-025 N=128 concurrent-reuse stress.
# ----------------------------------------------------------------------
test_log=$(mktemp)
if go test -race -count=1 -timeout 180s -run 'Phase149' ./test/integration/... >"${test_log}" 2>&1; then
    ok 'phase 149: HTTP-manifest boot-loader integration test passes under -race (resolve+invoke + OAuth pre-check + 2 failure modes + concurrency stress)'
    rm -f "${test_log}"
else
    fail 'phase 149: integration test failed (run `go test -race -run Phase149 ./test/integration/...` for detail)'
    echo "    --- go test output (tail 60 lines) ---"
    tail -60 "${test_log}" | sed 's/^/    /'
    echo "    --- end ---"
    rm -f "${test_log}"
fi

# ----------------------------------------------------------------------
# Assertions 3-4 — `bin/harbor validate` against temp configs.
# ----------------------------------------------------------------------
if [[ -x "${ROOT}/bin/harbor" ]]; then
    tmp_dir="$(mktemp -d -t harbor-smoke149-XXXXXX)"
    trap 'rm -rf "${tmp_dir}"' EXIT

    mkdir -p "${tmp_dir}/tools"
    cat > "${tmp_dir}/tools/weather.yaml" <<'MANIFEST'
auth:
  weather_key:
    kind: api_key
    header: X-API-Key
    secret: ${HARBOR_SMOKE149_WEATHER_KEY}
tools:
  - name: weather.lookup
    method: GET
    url_template: https://api.weather.example/v1/now?city={{ .Args.city | urlquery }}
    description: Look up current weather by city name.
    auth_ref: weather_key
MANIFEST

    # A config declaring a RELATIVE manifest entry that resolves inside
    # the config directory — must be ACCEPTED.
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
  http_manifests:
    - tools/weather.yaml
YAML
    if "${ROOT}/bin/harbor" validate "${tmp_dir}/valid.yaml" >"${tmp_dir}/validate-ok.log" 2>&1; then
        ok 'phase 149: `harbor validate` accepts a populated tools.http_manifests list (structural checks only)'
    elif grep -qi 'not loaded at boot yet\|not wired' "${tmp_dir}/validate-ok.log"; then
        skip 'phase 149: bin/harbor predates the validate flip (still emits the pre-149 rejection) — rebuild to exercise this leg'
    else
        fail 'phase 149: `harbor validate` rejected a valid tools.http_manifests config'
        echo "    --- validate output ---"
        cat "${tmp_dir}/validate-ok.log" | sed 's/^/    /'
        echo "    --- end ---"
    fi

    # A config declaring a RELATIVE manifest entry that escapes the
    # config directory — must be REJECTED, naming tools.http_manifests.
    cat > "${tmp_dir}/escape.yaml" <<'YAML2'
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
  http_manifests:
    - ../outside.yaml
YAML2
    if "${ROOT}/bin/harbor" validate "${tmp_dir}/escape.yaml" >"${tmp_dir}/validate-escape.log" 2>&1; then
        if grep -qi 'not loaded at boot yet\|not wired' "${tmp_dir}/validate-escape.log" 2>/dev/null; then
            skip 'phase 149: bin/harbor predates the validate flip — path-escape leg needs a rebuilt binary'
        else
            fail 'phase 149: `harbor validate` accepted a relative tools.http_manifests entry that escapes the config directory'
        fi
    elif grep -q 'tools.http_manifests' "${tmp_dir}/validate-escape.log"; then
        ok 'phase 149: `harbor validate` rejects a path-escaping tools.http_manifests entry, naming the field'
    else
        fail 'phase 149: `harbor validate` rejected the escaping config but the error lacks the expected field name'
        echo "    --- validate output ---"
        cat "${tmp_dir}/validate-escape.log" | sed 's/^/    /'
        echo "    --- end ---"
    fi
else
    skip 'phase 149: bin/harbor not built — skipping validate CLI legs (run `make build` first)'
fi

# ----------------------------------------------------------------------
# Assertion 5 — static guards: the wiring exists, the dead-knob
# rejection string is gone.
# ----------------------------------------------------------------------
assert_grep_present 'LoadManifest' internal/runtime/assemble/assemble.go \
    'phase 149: assemble.go calls http.LoadManifest'
assert_grep_present 'RegisterManifest' internal/runtime/assemble/assemble.go \
    'phase 149: assemble.go calls http.RegisterManifest'
assert_grep_absent 'not loaded at boot yet' internal/config/validate.go \
    'phase 149: validate.go no longer carries the pre-wiring rejection string'

smoke_summary
