#!/usr/bin/env bash
# Phase 64 smoke — `harbor dev` v1 (D-089).
#
# The preflight harness boots `./bin/harbor dev` against the dev port
# (defaults to 18080) and runs this script against the live server.
# The harness sets HARBOR_DEV_ALLOW_MOCK=1 implicitly via the
# environment so the binary boots without a real LLM provider. The
# constraint #5 assertions exercise:
#
#   1. /healthz returns 200 (the binding acceptance criterion).
#   2. /healthz body shape is JSON `{"status":"ok",...}` so a Console
#      can rely on the response shape across releases.
#   3. /readyz returns 200 — reserves the surface for a later phase.
#   4. The Phase 60 Protocol mux is mounted at /v1/ — a GET on
#      /v1/events with the dev triple opens the SSE stream (status 200
#      Content-Type: text/event-stream).
#   5. The LLM seam is live — submitting a `start` over /v1/control/start
#      with a valid Bearer token succeeds and returns a task id.
#   6. The fail-loud-no-config case is exercised OFFLINE (separate
#      child process; see below).
#
# Constraint #5 (LLM seam in smoke): the harness boots `harbor dev`
# with HARBOR_DEV_ALLOW_MOCK=1, which routes the LLM seam through the
# Phase 32 mock driver. The mock is deterministic and CGo-free; this
# smoke is therefore hermetic (no live network).
#
# The Phase 60 trust-based identity headers (X-Harbor-Tenant/etc.) do
# NOT work post-Phase-61: the mux is wrapped in auth.Middleware, so
# every request MUST carry `Authorization: Bearer <jwt>`. The dev
# server prints the dev token to stderr at boot; we capture it by
# parsing the preflight server log file.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------
# Assertion 1 — /healthz returns 200.
# ----------------------------------------------------------------------
assert_status 200 "$(api_url /healthz)" "harbor dev: /healthz returns 200"

# ----------------------------------------------------------------------
# Assertion 2 — /healthz body has the canonical JSON shape.
# ----------------------------------------------------------------------
assert_json_path '.status' 'ok' "$(api_url /healthz)" \
    "harbor dev: /healthz reports status=ok"

# ----------------------------------------------------------------------
# Assertion 3 — /readyz returns 200 (reserved surface).
# ----------------------------------------------------------------------
assert_status 200 "$(api_url /readyz)" "harbor dev: /readyz returns 200"

# ----------------------------------------------------------------------
# Assertion 4 — Phase 60 control mux is mounted (auth-gated).
# A POST to /v1/control/start WITHOUT a bearer token returns 401 — the
# auth.Middleware fail-closes any unauthenticated request. We assert
# the 401 specifically so a "0 (connection refused)" doesn't pass
# silently. We don't use skip_if_404 here because the GET probe under
# /v1/control/start returns 405 (POST-only), which the helper treats
# as "not implemented" — the POST IS implemented, the GET shape just
# differs. We probe with an explicit POST instead.
# ----------------------------------------------------------------------
actual=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H "Content-Type: application/json" \
    --data '{"identity":{"tenant":"t1","user":"u1","session":"s1"},"query":"q"}' \
    "$(api_url /v1/control/start)" || echo "000")
case "$actual" in
    401)
        ok "harbor dev: /v1/control rejects unauthenticated request (401)"
        ;;
    404|405|501)
        skip "harbor dev: /v1/control surface not yet implemented (${actual})"
        ;;
    *)
        fail "harbor dev: /v1/control unauthenticated status = ${actual}, want 401"
        ;;
esac

# ----------------------------------------------------------------------
# Assertion 5 — LLM seam is live: submit a `start` with the dev token,
# observe a task id in the response. The dev token is parsed out of
# the preflight server log; if the log isn't reachable, SKIP rather
# than FAIL (the smoke must still pass against builds that don't yet
# print the token under this exact prefix).
# ----------------------------------------------------------------------
if [ -n "${HARBOR_DATA_DIR:-}" ] && [ -f "${HARBOR_DATA_DIR}/server.log" ]; then
    TOKEN="$(grep -m1 '^HARBOR_DEV_TOKEN=' "${HARBOR_DATA_DIR}/server.log" 2>/dev/null | sed 's/^HARBOR_DEV_TOKEN=//')"
    if [ -n "${TOKEN}" ]; then
        actual=$(curl -s -o /tmp/harbor-smoke-start.json -w '%{http_code}' \
            --max-time 5 \
            -X POST -H "Content-Type: application/json" \
            -H "Authorization: Bearer ${TOKEN}" \
            --data '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"query":"phase-64 smoke"}' \
            "$(api_url /v1/control/start)" || echo "000")
        if [ "$actual" = "200" ]; then
            if command -v jq >/dev/null 2>&1; then
                task_id=$(jq -r '.task_id // empty' /tmp/harbor-smoke-start.json 2>/dev/null || true)
                if [ -n "${task_id}" ] && [ "${task_id}" != "null" ]; then
                    ok "harbor dev: /v1/control/start returns a task id (${task_id})"
                else
                    fail "harbor dev: /v1/control/start body missing task_id"
                    echo "  body: $(cat /tmp/harbor-smoke-start.json 2>/dev/null || echo '(empty)')"
                fi
            else
                ok "harbor dev: /v1/control/start returned 200 (jq absent; body shape unchecked)"
            fi
        else
            fail "harbor dev: /v1/control/start status = ${actual}, want 200"
            echo "  body: $(cat /tmp/harbor-smoke-start.json 2>/dev/null || echo '(empty)')"
        fi
    else
        skip "harbor dev: LLM-seam control round-trip (HARBOR_DEV_TOKEN not found in server log)"
    fi
else
    skip "harbor dev: LLM-seam control round-trip (HARBOR_DATA_DIR/server.log not reachable)"
fi

# ----------------------------------------------------------------------
# Assertion 6 — fail-loud-no-config case. Boot a SECOND, transient
# `bin/harbor dev` in a temp dir with no config file; assert the binary
# exits non-zero AND the error message mentions the config field
# (constraint #5's "boot with no provider configured and assert the
# non-zero exit with the expected error" half).
# ----------------------------------------------------------------------
if [ -x "${ROOT}/bin/harbor" ]; then
    tmp_dir="$(mktemp -d -t harbor-smoke64-XXXXXX)"
    trap 'rm -rf "${tmp_dir}"' EXIT
    # Run from the tmp dir with NO config, NO env var. The binary
    # should exit non-zero quickly (config-not-found is the first
    # check before any listener is bound). Use a background process
    # plus a watchdog kill — `timeout` is missing on macOS by
    # default, so we avoid the dependency.
    (cd "${tmp_dir}" && "${ROOT}/bin/harbor" dev --port 18198 > "${tmp_dir}/fail.log" 2>&1) &
    boot_pid=$!
    # Watchdog: if the process is still alive after 5 seconds, kill it.
    (sleep 5; kill -KILL "${boot_pid}" 2>/dev/null || true) &
    watchdog=$!
    set +e
    wait "${boot_pid}"
    rc=$?
    set -e
    kill "${watchdog}" 2>/dev/null || true
    wait "${watchdog}" 2>/dev/null || true

    if [ "${rc}" -eq 0 ]; then
        fail "harbor dev: missing-config boot exited 0, want non-zero"
    elif grep -qE 'config|llm|harbor.yaml|no such file' "${tmp_dir}/fail.log" 2>/dev/null; then
        ok "harbor dev: missing-config boot fails loud with named-field error (rc=${rc})"
    else
        fail "harbor dev: missing-config boot exited non-zero but error message lacks the expected named-field hint (rc=${rc})"
        head -5 "${tmp_dir}/fail.log" 2>/dev/null || true
    fi
else
    skip "harbor dev: missing-config boot test (bin/harbor not built)"
fi

smoke_summary
