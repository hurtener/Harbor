#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 154 — OAuth provider credential source: env or coordinator-served
# pull (D-285).
#
# What this asserts (each check SKIPs on a pre-154 build):
#
#   1. Static: the §4.4 credential-source seam exists (interface +
#      factory + env/remote drivers), both drivers register via the
#      D-196 prod aggregator, the config schema carries the
#      `credential_source` + `remote` fields, and the two canonical
#      fetch events + sentinel ship.
#   2. unit-tests: the credsource package tests pass under -race
#      (resolve, TTL/expiry, single-flight, failure legs, no-secret-bytes,
#      the shared conformance suite).
#   3. The §17.8 fixture-server E2E (real StateStore + bus + tokenexchange
#      + coordinator fixture): zero-env boot → first-need pull → exchange
#      succeeds; rotation; coordinator-down fail-loud.
#   4. The example config with a `credential_source: remote` block passes
#      `harbor validate`, and the dual-source misconfiguration is rejected.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_or_skip() {
    local pattern="$1" file="$2" desc="$3"
    if [ ! -f "${file}" ]; then
        skip "${desc}: ${file} not found (Phase 154 not yet implemented)"
        return
    fi
    if grep -qE "${pattern}" "${file}" 2>/dev/null; then
        ok "${desc}"
    else
        skip "${desc}: pattern '${pattern}' absent (Phase 154 not yet implemented)"
    fi
}

# ----------------------------------------------------------------------------
# 1. Static — the seam + its registration + config schema.
# ----------------------------------------------------------------------------

assert_or_skip 'type Source interface' \
    "internal/tools/auth/credsource/credsource.go" \
    "static: credsource.Source is the §4.4 seam interface"

assert_or_skip 'func Resolve\(name string' \
    "internal/tools/auth/credsource/registry.go" \
    "static: credsource.Resolve is the factory (registry dispatch)"

assert_or_skip 'ErrCredentialSourceUnavailable' \
    "internal/tools/auth/credsource/events.go" \
    "static: the typed failure sentinel ships"

assert_or_skip 'EventTypeProviderCredentialFetched' \
    "internal/tools/auth/credsource/events.go" \
    "static: the canonical fetch events are registered"

assert_or_skip 'credsource.MustRegister' \
    "internal/tools/auth/credsource/drivers/env/env.go" \
    "static: the env source self-registers"

assert_or_skip 'credsource.MustRegister' \
    "internal/tools/auth/credsource/drivers/remote/remote.go" \
    "static: the remote source self-registers"

assert_or_skip 'credsource/drivers/env' \
    "internal/drivers/prod/prod.go" \
    "static: the env source registers via the D-196 prod aggregator"

assert_or_skip 'credsource/drivers/remote' \
    "internal/drivers/prod/prod.go" \
    "static: the remote source registers via the D-196 prod aggregator"

assert_or_skip 'CredentialSource string' \
    "internal/config/config.go" \
    "static: the credential_source config field exists"

assert_or_skip 'type ToolOAuthRemoteConfig struct' \
    "internal/config/config.go" \
    "static: the remote config block exists"

assert_or_skip 'allowedCredentialSources' \
    "internal/config/validate.go" \
    "static: the credential_source allowlist mirror exists"

# ----------------------------------------------------------------------------
# 2. Unit tests — the credsource package under -race.
# ----------------------------------------------------------------------------

if [ ! -f "internal/tools/auth/credsource/credsource.go" ]; then
    skip "unit-tests: Phase 154 not yet implemented"
elif go test -race -count=1 -timeout 120s ./internal/tools/auth/credsource/... >/dev/null 2>&1; then
    ok "unit-tests: credsource seam + env/remote drivers pass under -race"
else
    fail "unit-tests: credsource tests failed (run: go test -race ./internal/tools/auth/credsource/...)"
fi

# ----------------------------------------------------------------------------
# 3. §17.8 fixture-server E2E (real drivers).
# ----------------------------------------------------------------------------

if [ ! -f "test/integration/phase154_credential_source_test.go" ]; then
    skip "e2e: integration test absent (Phase 154 not yet implemented)"
elif go test -race -count=1 -timeout 300s -run 'TestE2E_Phase154' ./test/integration/ >/dev/null 2>&1; then
    ok "e2e: zero-env pull → exchange, rotation, and coordinator-down fail-loud pass under -race"
else
    fail "e2e: Phase 154 integration tests failed (run: go test -race -run TestE2E_Phase154 ./test/integration/)"
fi

# ----------------------------------------------------------------------------
# 4. `harbor validate` on a config with a remote credential_source block.
# ----------------------------------------------------------------------------

BIN="${ROOT}/bin/harbor"
FIXTURE="internal/config/testdata/valid_minimal.yaml"

if ! grep -qE 'type ToolOAuthRemoteConfig struct' internal/config/config.go 2>/dev/null; then
    skip "validate round-trip: Phase 154 not yet implemented"
elif [[ ! -x "${BIN}" ]]; then
    skip "validate round-trip: bin/harbor not built (preflight build step skipped)"
elif [[ ! -f "${FIXTURE}" ]]; then
    skip "validate round-trip: ${FIXTURE} fixture absent"
else
    TMPDIR_154="$(mktemp -d)"
    trap 'rm -rf "${TMPDIR_154}"' EXIT

    # A tokenexchange provider with credential_source: remote → exit 0.
    cat "${FIXTURE}" > "${TMPDIR_154}/remote-ok.yaml"
    cat >> "${TMPDIR_154}/remote-ok.yaml" <<'YAML'

tools:
  oauth_token_kek_env: HARBOR_OAUTH_TOKEN_KEK
  oauth_providers:
    - name: m365-broker
      driver: tokenexchange
      credential_source: remote
      token_url: https://broker.example.com/oauth2/token
      remote:
        url: https://coordinator.example.com/broker-credential
        auth_token_env: HARBOR_COORDINATOR_TOKEN
        cache_ttl: 5m
YAML
    if "${BIN}" validate "${TMPDIR_154}/remote-ok.yaml" >/dev/null 2>&1; then
        ok "validate: a tokenexchange provider with credential_source=remote validates cleanly"
    else
        fail "validate: the remote credential_source config expected exit 0"
    fi

    # Dual source (remote + client_id_env) → non-zero, one source rule.
    cat "${FIXTURE}" > "${TMPDIR_154}/dual-source.yaml"
    cat >> "${TMPDIR_154}/dual-source.yaml" <<'YAML'

tools:
  oauth_token_kek_env: HARBOR_OAUTH_TOKEN_KEK
  oauth_providers:
    - name: m365-broker
      driver: tokenexchange
      credential_source: remote
      client_id_env: HARBOR_BROKER_CLIENT_ID
      token_url: https://broker.example.com/oauth2/token
      remote:
        url: https://coordinator.example.com/broker-credential
        auth_token_env: HARBOR_COORDINATOR_TOKEN
YAML
    set +e
    "${BIN}" validate "${TMPDIR_154}/dual-source.yaml" >/dev/null 2>&1
    rc=$?
    set -e
    if [[ "${rc}" -eq 0 ]]; then
        fail "validate: dual-source (remote + client_id_env) expected a non-zero exit"
    else
        ok "validate: declaring both remote and client_id_env is rejected (one source per entry)"
    fi
fi

smoke_summary
