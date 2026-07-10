#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 160 — sdk/server + `harbor scaffold --with-server` + parity gate.
#
# The no-LLM subset of the external-serving path (a smoke cannot spend an LLM
# turn):
#   - `harbor scaffold --with-server --from-config <probe>` into a temp dir as
#     an EXTERNAL module (replace-directive appended to go.mod so it builds
#     against the local checkout — the phase-112b.sh:216 precedent);
#   - `go build` the generated cmd/<agent> against the sdk/ facade;
#   - boot it behind a `harbor token`-minted JWKS production posture
#     (keygen -> jwks_file -> mint);
#   - /healthz 200; the generated custom tool is PRESENT in tools.list
#     (discovery probe, authenticated); a request without a token -> 401.
#   OK >= 3, FAIL = 0.
# The tool DISPATCH leg (LLM-driven invocation) is the env-gated live leg
# (HARBOR_LIVE_SERVE) run by the coordinator — SKIP here.
# Degradation: SKIP on a build whose `harbor scaffold` lacks `--with-server`.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

BIN="${ROOT}/bin/harbor"
GATE_DEADLINE_SECS="${HARBOR_PHASE160_GATE_DEADLINE_SECS:-240}"

# with_deadline <cmd...> — portable bounded execution (macOS ships no
# GNU timeout by default; perl's alarm is the fallback).
with_deadline() {
    if command -v timeout >/dev/null 2>&1; then
        timeout "${GATE_DEADLINE_SECS}" "$@"
    else
        perl -e 'alarm shift; exec @ARGV' "${GATE_DEADLINE_SECS}" "$@"
    fi
}

if [[ ! -x "${BIN}" ]]; then
    skip "phase 160: ${BIN} not built — run 'make build' (preflight builds it)"
    smoke_summary
    return 0 2>/dev/null || exit 0
fi

# --- 0. Degradation: does this build's scaffold carry --with-server? ---------

# Capture the help text first: piping directly into `grep -q` under
# `pipefail` lets grep close the pipe early, SIGPIPE-killing `harbor`, and the
# pipeline then reports failure even though the flag IS present.
scaffold_help="$("${BIN}" scaffold --help 2>&1 || true)"
if ! printf '%s' "${scaffold_help}" | grep -q -- '--with-server'; then
    skip "phase 160: harbor scaffold has no --with-server flag on this build"
    smoke_summary
    return 0 2>/dev/null || exit 0
fi

TMPDIR="$(mktemp -d)"
SERVER_PID=""
cleanup() {
    if [[ -n "${SERVER_PID}" ]] && kill -0 "${SERVER_PID}" 2>/dev/null; then
        kill "${SERVER_PID}" 2>/dev/null || true
        wait "${SERVER_PID}" 2>/dev/null || true
    fi
    rm -rf "${TMPDIR}"
}
trap cleanup EXIT

KEYS="${TMPDIR}/keys"
ISSUER="https://issuer.smoke.local"
AUDIENCE="phase-160-server"

# --- 1. keygen: the production JWKS local-dev loop ---------------------------

if ! "${BIN}" token keygen --out "${KEYS}" --alg ES256 >/dev/null 2>&1; then
    fail 'phase 160: harbor token keygen failed'
    smoke_summary
    return 0 2>/dev/null || exit 0
fi
if [[ -f "${KEYS}/jwks.json" && -f "${KEYS}/private.pem" ]]; then
    ok 'phase 160: harbor token keygen produced jwks.json + private.pem (the production JWKS local-dev loop)'
else
    fail 'phase 160: keygen did not produce jwks.json + private.pem'
    smoke_summary
    return 0 2>/dev/null || exit 0
fi

# --- 2. Probe config: jwks_file posture + a compiled custom tool -------------

PROBE_YAML="${TMPDIR}/harbor.yaml"
cat > "${PROBE_YAML}" <<YAML
server:
  bind_addr: 127.0.0.1:0
  shutdown_grace_period: 5s
identity:
  jwt_algorithms: [ES256]
  issuer: ${ISSUER}
  audience: ${AUDIENCE}
  jwks_file: ${KEYS}/jwks.json
telemetry:
  log_format: json
  log_level: error
  service_name: phase-160-server
llm:
  provider: openrouter
  model: anthropic/claude-haiku-4-5
  api_key: env.OPENROUTER_API_KEY
  timeout: 60s
  model_profiles:
    anthropic/claude-haiku-4-5:
      context_window_tokens: 200000
tools:
  custom:
    - name: weather.lookup
      description: Look up current weather by city.
      input:
        city: string
      output:
        summary: string
YAML

# --- 3. Scaffold --with-server as an external module -------------------------

PROJ="${TMPDIR}/srv-agent"
if ! (cd "${TMPDIR}" && "${BIN}" scaffold --name srv-agent --output "${PROJ}" --with-server --from-config "${PROBE_YAML}") >/dev/null 2>&1; then
    fail 'phase 160: harbor scaffold --with-server --from-config failed'
    smoke_summary
    return 0 2>/dev/null || exit 0
fi
if [[ -f "${PROJ}/cmd/srv-agent/main.go" ]]; then
    ok 'phase 160: scaffold --with-server emitted cmd/srv-agent/main.go (the serving entry point)'
else
    fail 'phase 160: scaffold --with-server did not emit cmd/<name>/main.go'
    smoke_summary
    return 0 2>/dev/null || exit 0
fi
{
    printf '\n'
    printf 'replace github.com/hurtener/Harbor => %s\n' "${ROOT}"
} >> "${PROJ}/go.mod"

# --- 4. External build of the serving binary ---------------------------------

BUILD_LOG="${TMPDIR}/build.log"
if ! with_deadline sh -c "cd '${PROJ}' && go mod tidy && go build -o '${TMPDIR}/srv-agent-bin' ./cmd/srv-agent" >"${BUILD_LOG}" 2>&1; then
    fail 'phase 160: EXTERNAL BUILD RED — scaffolded --with-server binary does not build (see tail)'
    tail -30 "${BUILD_LOG}" | sed 's/^/    /'
    smoke_summary
    return 0 2>/dev/null || exit 0
fi
ok 'phase 160: EXTERNAL BUILD green — scaffolded --with-server binary builds against the sdk/ facade'

# --- 5. Boot the serving binary behind the minted JWKS posture ---------------
# bifrost accepts any non-empty key at construction (it validates against the
# provider only on a real request), so a dummy key lets the production server
# BOOT without spending an LLM turn — discovery + 401 never call the LLM.

SERVER_LOG="${TMPDIR}/server.log"
OPENROUTER_API_KEY="sk-phase160-smoke-dummy" \
    "${TMPDIR}/srv-agent-bin" --config "${PROBE_YAML}" --bind 127.0.0.1:0 \
    >"${SERVER_LOG}" 2>&1 &
SERVER_PID=$!

# Discover the ephemeral bound address the server prints at listen time.
BOUND=""
for _ in $(seq 1 50); do
    if ! kill -0 "${SERVER_PID}" 2>/dev/null; then
        break
    fi
    BOUND="$(grep -o 'HARBOR_DEV_BOUND=[^ ]*' "${SERVER_LOG}" 2>/dev/null | head -1 | cut -d= -f2 || true)"
    [[ -n "${BOUND}" ]] && break
    sleep 0.2
done

if [[ -z "${BOUND}" ]]; then
    fail 'phase 160: scaffolded server did not bind a listener (no HARBOR_DEV_BOUND in log; see tail)'
    tail -30 "${SERVER_LOG}" | sed 's/^/    /'
    smoke_summary
    return 0 2>/dev/null || exit 0
fi
BASE="http://${BOUND}"

# Wait for /healthz.
HEALTHY=""
for _ in $(seq 1 50); do
    code="$(curl -s -o /dev/null -w '%{http_code}' "${BASE}/healthz" 2>/dev/null || true)"
    if [[ "${code}" == "200" ]]; then
        HEALTHY=1
        break
    fi
    sleep 0.2
done
if [[ -n "${HEALTHY}" ]]; then
    ok "phase 160: scaffolded --with-server binary booted; /healthz 200 at ${BASE} (production JWKS posture)"
else
    fail 'phase 160: scaffolded server /healthz never returned 200 (see tail)'
    tail -30 "${SERVER_LOG}" | sed 's/^/    /'
    smoke_summary
    return 0 2>/dev/null || exit 0
fi

# --- 6. 401 without a token (identity is mandatory) --------------------------

noauth_code="$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
    -d '{}' "${BASE}/v1/tools/list" 2>/dev/null || true)"
if [[ "${noauth_code}" == "401" ]]; then
    ok 'phase 160: tools.list without a token -> 401 (identity mandatory)'
else
    fail "phase 160: tools.list without a token -> ${noauth_code} (want 401)"
fi

# --- 7. Discovery: the compiled tool is present in the catalog ---------------

TOKEN="$("${BIN}" token mint --key "${KEYS}/private.pem" \
    --tenant acme --user alice --session s1 \
    --issuer "${ISSUER}" --audience "${AUDIENCE}" 2>/dev/null | tr -d '\r\n' || true)"
if [[ -z "${TOKEN}" ]]; then
    fail 'phase 160: harbor token mint produced no token'
else
    list_body="$(curl -s -X POST -H 'Content-Type: application/json' \
        -H "Authorization: Bearer ${TOKEN}" -d '{}' "${BASE}/v1/tools/list" 2>/dev/null || true)"
    if printf '%s' "${list_body}" | grep -q 'weather.lookup'; then
        ok 'phase 160: discovery — the compiled tool weather.lookup is present in the served catalog (tools.list, authenticated)'
    else
        fail "phase 160: discovery — weather.lookup absent from tools.list; body=${list_body}"
    fi
fi

# --- 8. Dispatch leg — env-gated live leg (needs a real LLM) -----------------

skip 'phase 160: tool DISPATCH leg is the env-gated HARBOR_LIVE_SERVE live leg (needs a real LLM); run by the wave live-verification step'

smoke_summary
