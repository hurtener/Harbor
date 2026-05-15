#!/usr/bin/env bash
# Harbor preflight — build, boot, run all phase smokes, tear down.
# This is the gate enforced by the pre-commit hook and CI.
#
# Until Phase 01 lands, build/boot are no-ops; smoke runs against doc state only.
#
# Override:
#   HARBOR_PREFLIGHT_SKIP=1  → skip everything (emergency only; CI never honors)

set -euo pipefail

if [ "${HARBOR_PREFLIGHT_SKIP:-0}" = "1" ]; then
    echo "preflight: SKIP (HARBOR_PREFLIGHT_SKIP=1)"
    exit 0
fi

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"

PORT="${HARBOR_DEV_PORT:-18080}"
DATA_DIR="$(mktemp -d -t harbor-preflight-XXXXXX)"
PID=""

cleanup() {
    if [ -n "${PID}" ]; then
        kill "${PID}" 2>/dev/null || true
        wait "${PID}" 2>/dev/null || true
    fi
    rm -rf "${DATA_DIR}"
}
trap cleanup EXIT

# 1. Build (skipped if no main package yet).
if [ -f cmd/harbor/main.go ]; then
    echo "preflight: building ./bin/harbor"
    CGO_ENABLED=0 go build -ldflags='-s -w' -o bin/harbor ./cmd/harbor
else
    echo "preflight: build skipped (cmd/harbor/main.go absent)"
fi

# 2. Boot (skipped if binary is absent OR if the binary is a stub
# that exits cleanly without opening the port — that condition holds
# until the dev subcommand lands in a later phase).
if [ -x bin/harbor ]; then
    echo "preflight: starting ./bin/harbor dev on 127.0.0.1:${PORT}"
    HARBOR_DATA_DIR="${DATA_DIR}" HARBOR_BIND="127.0.0.1:${PORT}" \
        ./bin/harbor dev >"${DATA_DIR}/server.log" 2>&1 &
    PID=$!
    booted=0
    stub=0
    for _ in $(seq 1 30); do
        if curl -s -f "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
            booted=1
            break
        fi
        if ! kill -0 "${PID}" 2>/dev/null; then
            # Process exited before binding the port — stub binary.
            # `wait` returns the child's exit code; under `set -e` a
            # non-zero exit would kill the script before we can branch
            # on it, so we capture rc inside a conditional context.
            rc=0
            wait "${PID}" 2>/dev/null || rc=$?
            if [ "${rc}" -eq 0 ]; then
                stub=1
                PID=""
                break
            fi
            # Phase 63+ stub: `harbor dev` exits non-zero with a
            # structured CLIError {code: "not_implemented"} pointing to
            # phase 64 (the §13 amendment). Treat that as the stub
            # posture too — the binary is intentionally refusing to
            # boot because the subcommand is not implemented yet. Look
            # for the structured marker in the captured stderr/stdout
            # log; if found, skip the boot step without failing.
            if grep -q '"code":"not_implemented"\|not yet implemented (see phase 64' "${DATA_DIR}/server.log" 2>/dev/null; then
                stub=1
                PID=""
                break
            fi
            echo "preflight: ./bin/harbor dev exited with code ${rc}"
            echo "--- server log ---"
            cat "${DATA_DIR}/server.log" || true
            exit 1
        fi
        sleep 0.5
    done
    if [ "${stub}" -eq 1 ]; then
        echo "preflight: boot skipped (stub binary; dev subcommand not yet implemented)"
    elif [ "${booted}" -ne 1 ]; then
        echo "preflight: server failed to come up on 127.0.0.1:${PORT}"
        echo "--- server log ---"
        cat "${DATA_DIR}/server.log" || true
        exit 1
    fi
else
    echo "preflight: boot skipped (bin/harbor not built)"
fi

# 3. Run drift-audit (cheap, file-level checks).
echo ""
echo "preflight: running scripts/drift-audit.sh"
TOTAL_FAIL=0
if ! bash scripts/drift-audit.sh; then
    echo "preflight: drift-audit reported failures"
    TOTAL_FAIL=$((TOTAL_FAIL + 1))
fi

# 4. Run all phase smokes.
shopt -s nullglob
for smoke in scripts/smoke/phase-*.sh; do
    echo ""
    echo "preflight: running ${smoke}"
    if ! bash "${smoke}"; then
        echo "preflight: ${smoke} reported failures"
        TOTAL_FAIL=$((TOTAL_FAIL + 1))
    fi
done
shopt -u nullglob

echo ""
echo "=== preflight summary ==="
if [ "${TOTAL_FAIL}" -gt 0 ]; then
    echo "preflight: FAIL (${TOTAL_FAIL} smoke script(s) reported failures)"
    exit 1
fi
echo "preflight: PASS"
exit 0
