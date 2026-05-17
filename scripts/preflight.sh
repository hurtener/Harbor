#!/usr/bin/env bash
# Harbor preflight — build, boot, run all phase smokes, tear down.
# This is the gate enforced by the pre-commit hook and CI.
#
# Until Phase 01 lands, build/boot are no-ops; smoke runs against doc state only.
#
# Overrides:
#
#   HARBOR_PREFLIGHT_SKIP=1     skip everything (emergency only; CI never honors)
#   HARBOR_DEV_PORT=18080       legacy override; pins a specific dev port instead
#                               of the ephemeral-port default. Two sibling
#                               worktrees pinning the same port still collide.
#   MAX_PARALLEL_SMOKES=8       cap on the parallel batch fan-out. Defaults to
#                               the number of CPU cores (sysctl/nproc fallback).
#
# Wall-time + concurrency contract (D-104):
#
#   By default the preflight harness binds the dev server to
#   `127.0.0.1:0` (ephemeral port) so two sibling worktrees can run
#   `make preflight` simultaneously without colliding on `:18080`.
#   The actual bound port is parsed out of the server log's
#   `HARBOR_DEV_BOUND=<host:port>` line (emitted unconditionally by
#   `harbor dev`) and exported to every smoke as `HARBOR_BIND`,
#   `HARBOR_PORT`, and `HARBOR_BASE_URL`. `scripts/smoke/common.sh::api_url`
#   reads `HARBOR_BASE_URL` so existing smokes keep working without edit.
#
#   Smokes carry a `# PREFLIGHT_REQUIRES: <class>` header (one of
#   `live-server`, `static-only`, `unit-tests`). The orchestrator runs
#   the `static-only` and `unit-tests` batches in parallel and runs the
#   `live-server` batch serially against the booted dev instance. A
#   missing or unrecognised header fails preflight loud — silent
#   classification defaults would let a server-mutating smoke leak into
#   the parallel batch and produce nondeterministic flakes (CLAUDE.md
#   §13 fail-loud).

set -euo pipefail

if [ "${HARBOR_PREFLIGHT_SKIP:-0}" = "1" ]; then
    echo "preflight: SKIP (HARBOR_PREFLIGHT_SKIP=1)"
    exit 0
fi

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"

# Ephemeral-port default (D-104). An operator who needs to pin can set
# HARBOR_DEV_PORT explicitly; the env var is honoured for backward
# compatibility AND for the rare case where a specific listener is
# required (e.g. an external integration test attaching to a known
# port). The classify/parallelise path below works either way.
PORT="${HARBOR_DEV_PORT:-0}"
DATA_DIR="$(mktemp -d -t harbor-preflight-XXXXXX)"
PID=""
BOUND_ADDR=""

# Export the data dir so phase smokes (Phase 64+ in particular) can
# read the dev server's log file — the dev cmd prints HARBOR_DEV_TOKEN
# to stderr at boot, and phase-64.sh parses it out to drive an
# authenticated control-surface call.
export HARBOR_DATA_DIR="${DATA_DIR}"

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

# ----------------------------------------------------------------------
# Classify smokes by `# PREFLIGHT_REQUIRES:` header (D-104).
# ----------------------------------------------------------------------
#
# Every `scripts/smoke/phase-*.sh` MUST carry exactly one line of the
# form `# PREFLIGHT_REQUIRES: live-server` (or `static-only` /
# `unit-tests`) in its top comment block. We parse it with a single
# `grep` per file — the grammar is intentionally inflexible so a typo
# fails loud.

classify_smoke() {
    local path="$1"
    local match
    match=$(grep -E '^[[:space:]]*#[[:space:]]*PREFLIGHT_REQUIRES:' "$path" \
        | head -1 \
        | sed -E 's/^[[:space:]]*#[[:space:]]*PREFLIGHT_REQUIRES:[[:space:]]*//' \
        | tr -d '[:space:]')
    case "$match" in
        live-server|static-only|unit-tests)
            printf '%s' "$match"
            ;;
        *)
            printf '__missing__'
            ;;
    esac
}

# Collect smokes by class. Bash arrays + nullglob so an empty
# scripts/smoke/ directory or an empty bucket is a clean no-op.
shopt -s nullglob
ALL_SMOKES=(scripts/smoke/phase-*.sh)
shopt -u nullglob

STATIC_ONLY=()
LIVE_SERVER=()
UNIT_TESTS=()
UNCLASSIFIED=()

for smoke in "${ALL_SMOKES[@]}"; do
    cls=$(classify_smoke "$smoke")
    case "$cls" in
        static-only) STATIC_ONLY+=("$smoke") ;;
        live-server) LIVE_SERVER+=("$smoke") ;;
        unit-tests)  UNIT_TESTS+=("$smoke") ;;
        *)           UNCLASSIFIED+=("$smoke") ;;
    esac
done

if [ "${#UNCLASSIFIED[@]}" -gt 0 ]; then
    echo "preflight: FAIL — the following smoke scripts are missing a"
    echo "  '# PREFLIGHT_REQUIRES: live-server|static-only|unit-tests' header:"
    for s in "${UNCLASSIFIED[@]}"; do
        echo "    $s"
    done
    echo "  Add the header in the top comment block. The grammar is exact:"
    echo "    # PREFLIGHT_REQUIRES: live-server"
    echo "  Classify wrong and the parallel batch produces nondeterministic flakes."
    echo "  See scripts/smoke/_template.sh and CLAUDE.md §4.2."
    exit 1
fi

echo "preflight: classified ${#STATIC_ONLY[@]} static-only / ${#LIVE_SERVER[@]} live-server / ${#UNIT_TESTS[@]} unit-tests smokes"

# ----------------------------------------------------------------------
# Run drift-audit (cheap, file-level checks) up front — it gates both
# the parallel and the live-server passes.
# ----------------------------------------------------------------------
TOTAL_FAIL=0
echo ""
echo "preflight: running scripts/drift-audit.sh"
if ! bash scripts/drift-audit.sh; then
    echo "preflight: drift-audit reported failures"
    TOTAL_FAIL=$((TOTAL_FAIL + 1))
fi

# ----------------------------------------------------------------------
# Parallel batch — static-only smokes need NO dev server, so we run
# them BEFORE the boot. The `unit-tests` batch (pure `go test`) also
# parallelises here; `go test` schedules its own internal parallelism
# but the bash-level fan-out lets multiple unrelated packages compile
# concurrently.
# ----------------------------------------------------------------------

# CPU-count-aware fan-out cap. macOS uses sysctl, Linux uses nproc; the
# echo-4 fallback keeps the harness portable to containers that lack
# both.
DEFAULT_PARALLEL=$(sysctl -n hw.ncpu 2>/dev/null || nproc 2>/dev/null || echo 4)
MAX_PARALLEL_SMOKES="${MAX_PARALLEL_SMOKES:-${DEFAULT_PARALLEL}}"

# run_parallel_batch <label> <smoke...>
# Fans out the named smokes with a job-count cap. Each child writes its
# output to a tempfile so the aggregator can print them in deterministic
# order (sorted by script name) after the batch finishes. A non-zero
# exit from any child bumps TOTAL_FAIL by 1.
run_parallel_batch() {
    local label="$1"; shift
    local smokes=("$@")
    if [ "${#smokes[@]}" -eq 0 ]; then
        return 0
    fi

    echo ""
    echo "preflight: running ${#smokes[@]} ${label} smoke(s) in parallel (cap=${MAX_PARALLEL_SMOKES})"

    local out_dir
    out_dir="$(mktemp -d -t harbor-preflight-${label}-XXXXXX)"

    local active=0
    local -a pids=()
    local -a pid_smokes=()
    local -a pid_outputs=()

    # finish_one <pid> <smoke> <out>
    # Waits on a single PID, captures its rc, appends to the per-batch
    # results so the aggregator can print in sorted order at the end.
    local rc_file="${out_dir}/_rc.tsv"
    : > "${rc_file}"

    local i smoke out_file pid rc
    for smoke in "${smokes[@]}"; do
        out_file="${out_dir}/$(basename "${smoke}").out"
        # Each smoke runs as a fresh bash with the inherited env (so
        # HARBOR_BIND / HARBOR_PORT / HARBOR_BASE_URL flow through for
        # the live-server batch; the static / unit-tests batches don't
        # read them but exporting is harmless).
        bash "${smoke}" > "${out_file}" 2>&1 &
        pids+=($!)
        pid_smokes+=("${smoke}")
        pid_outputs+=("${out_file}")
        active=$((active + 1))

        if [ "${active}" -ge "${MAX_PARALLEL_SMOKES}" ]; then
            # Drain the oldest one. We're using a simple "drain head"
            # strategy (rather than wait -n) for bash 3.2 compatibility
            # on macOS — wait -n is bash 4.3+.
            local head_pid="${pids[0]}"
            local head_smoke="${pid_smokes[0]}"
            local head_out="${pid_outputs[0]}"
            wait "${head_pid}" 2>/dev/null
            rc=$?
            printf '%s\t%d\t%s\n' "${head_smoke}" "${rc}" "${head_out}" >> "${rc_file}"
            pids=("${pids[@]:1}")
            pid_smokes=("${pid_smokes[@]:1}")
            pid_outputs=("${pid_outputs[@]:1}")
            active=$((active - 1))
        fi
    done

    # Drain the rest.
    for i in "${!pids[@]}"; do
        local p="${pids[${i}]}"
        local s="${pid_smokes[${i}]}"
        local o="${pid_outputs[${i}]}"
        wait "${p}" 2>/dev/null
        rc=$?
        printf '%s\t%d\t%s\n' "${s}" "${rc}" "${o}" >> "${rc_file}"
    done

    # Aggregate, sorted by smoke name so output is deterministic
    # regardless of completion order.
    local batch_fail=0
    local s_name rc_code out_path
    while IFS=$'\t' read -r s_name rc_code out_path; do
        echo ""
        echo "preflight: running ${s_name}"
        cat "${out_path}" || true
        if [ "${rc_code}" -ne 0 ]; then
            echo "preflight: ${s_name} reported failures (rc=${rc_code})"
            batch_fail=$((batch_fail + 1))
        fi
    done < <(sort "${rc_file}")

    TOTAL_FAIL=$((TOTAL_FAIL + batch_fail))
    rm -rf "${out_dir}"
    return 0
}

run_parallel_batch 'static-only' ${STATIC_ONLY[@]+"${STATIC_ONLY[@]}"}

# unit-tests batch — `go test` already schedules internal parallelism,
# but the bash-level fan-out lets multiple unrelated packages compile
# concurrently (5 smokes — phase 63/67/68/69/70 — all run
# `go test ./cmd/harbor/...`; concurrent compiles share the build cache
# but don't redundantly recompile under -count=1). Default cap is the
# full CPU count; an operator with a noisy machine can lower it via
# `MAX_PARALLEL_UNIT_TESTS=N`. The previous timing-flake under load (a
# leaked HARBOR_BIND env var causing cmd/harbor tests to bind the
# preflight server's port) was fixed at the source — `bootDevStack` no
# longer reads HARBOR_BIND from env; `runDev` threads it through
# `devBootOptions.bindAddr` explicitly (see cmd/harbor/cmd_dev.go).
MAX_PARALLEL_UNIT_TESTS="${MAX_PARALLEL_UNIT_TESTS:-${DEFAULT_PARALLEL}}"
# Save the static-only cap, swap in the unit-tests cap for the batch.
SAVED_CAP="${MAX_PARALLEL_SMOKES}"
MAX_PARALLEL_SMOKES="${MAX_PARALLEL_UNIT_TESTS}"
run_parallel_batch 'unit-tests'  ${UNIT_TESTS[@]+"${UNIT_TESTS[@]}"}
MAX_PARALLEL_SMOKES="${SAVED_CAP}"

# ----------------------------------------------------------------------
# Boot the dev server ONCE for the live-server batch. Skipped when the
# binary is absent OR if the binary is a stub that exits cleanly
# without opening the port — that condition holds until the dev
# subcommand lands in a later phase.
#
# Phase 64 (D-089) makes `harbor dev` boot a real LLM-backed stack;
# the §13 amendment requires a fail-loud at boot when no LLM provider
# is configured. The preflight harness has no real provider, so we
# always set HARBOR_DEV_ALLOW_MOCK=1 here — the dev cmd prints a
# stderr banner [DEV-ONLY MOCK LLM — DO NOT USE IN PRODUCTION] when
# this fires, which the smoke captures via the server log. Production
# operators NEVER set this env var; the only place it appears in this
# repository is this preflight harness and the per-phase smoke tests.
# ----------------------------------------------------------------------
if [ -x bin/harbor ] && [ "${#LIVE_SERVER[@]}" -gt 0 ]; then
    REQUESTED_BIND="127.0.0.1:${PORT}"
    echo ""
    echo "preflight: starting ./bin/harbor dev (requested bind=${REQUESTED_BIND}; ephemeral port resolved from server.log)"
    # The config path: when examples/dev.yaml exists, pass it. The
    # fail-loud-no-config smoke (phase-64.sh assertion 6) launches
    # a SECOND short-lived dev binary against a tmp dir, so we DO
    # need a config here for the long-lived preflight server.
    HARBOR_DATA_DIR="${DATA_DIR}" HARBOR_BIND="${REQUESTED_BIND}" \
    HARBOR_DEV_ALLOW_MOCK=1 \
        ./bin/harbor dev --config examples/dev.yaml >"${DATA_DIR}/server.log" 2>&1 &
    PID=$!
    booted=0
    stub=0
    for _ in $(seq 1 30); do
        # Discover the actual bound addr from the server log. The dev
        # cmd emits a parseable line `HARBOR_DEV_BOUND=<host:port>`
        # immediately after `net.Listen` returns; we wait for it
        # before probing /healthz. The grep + sed pipeline exits 1
        # when the line hasn't been emitted yet (the server is still
        # constructing the listener); under `set -euo pipefail` that
        # propagates and kills the harness. `|| true` swallows the
        # transient empty-match exit code; the next loop iteration
        # retries.
        if [ -z "${BOUND_ADDR}" ] && [ -f "${DATA_DIR}/server.log" ]; then
            BOUND_ADDR="$(grep -m1 '^HARBOR_DEV_BOUND=' "${DATA_DIR}/server.log" 2>/dev/null | sed 's/^HARBOR_DEV_BOUND=//' || true)"
        fi
        if [ -n "${BOUND_ADDR}" ]; then
            if curl -s -f "http://${BOUND_ADDR}/healthz" >/dev/null 2>&1; then
                booted=1
                break
            fi
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
        echo "preflight: server failed to come up (bound=${BOUND_ADDR:-unresolved})"
        echo "--- server log ---"
        cat "${DATA_DIR}/server.log" || true
        exit 1
    else
        # Export the discovered bind addr to every live-server smoke.
        # HARBOR_BASE_URL is what scripts/smoke/common.sh::api_url
        # reads; HARBOR_BIND + HARBOR_PORT cover the smokes that
        # construct CLI flags (e.g. `harbor inspect-events --bind ...`).
        export HARBOR_BIND="${BOUND_ADDR}"
        export HARBOR_BASE_URL="http://${BOUND_ADDR}"
        # Strip the host: prefix to recover the port for HARBOR_PORT
        # (used by phase-69's --bind construction). LastIndex of ':'
        # handles IPv6-bracketed forms.
        HARBOR_PORT="${BOUND_ADDR##*:}"
        export HARBOR_PORT
        # HARBOR_DEV_PORT is the legacy env name some smokes still
        # read; mirror it so older scripts keep working without edit.
        export HARBOR_DEV_PORT="${HARBOR_PORT}"
        # phase-70 reads HARBOR_DEV_TOKEN explicitly from env (its
        # live-server probe is gated on it). Mirror the dev token out
        # of the server log so the operator doesn't have to.
        if [ -f "${DATA_DIR}/server.log" ]; then
            HARBOR_DEV_TOKEN="$(grep -m1 '^HARBOR_DEV_TOKEN=' "${DATA_DIR}/server.log" 2>/dev/null | sed 's/^HARBOR_DEV_TOKEN=//' || true)"
            if [ -n "${HARBOR_DEV_TOKEN}" ]; then
                export HARBOR_DEV_TOKEN
            fi
        fi
        echo "preflight: dev server up at ${HARBOR_BASE_URL}"
    fi
elif [ ! -x bin/harbor ]; then
    echo ""
    echo "preflight: boot skipped (bin/harbor not built)"
else
    echo ""
    echo "preflight: boot skipped (no live-server smokes to run)"
fi

# ----------------------------------------------------------------------
# Serial live-server batch. These smokes mutate / observe shared dev
# state (a SSE stream, an in-mem bus, the singleton draft store) so
# running them in parallel would surface as nondeterministic flakes.
# ----------------------------------------------------------------------
for smoke in ${LIVE_SERVER[@]+"${LIVE_SERVER[@]}"}; do
    echo ""
    echo "preflight: running ${smoke}"
    if ! bash "${smoke}"; then
        echo "preflight: ${smoke} reported failures"
        TOTAL_FAIL=$((TOTAL_FAIL + 1))
    fi
done

echo ""
echo "=== preflight summary ==="
if [ "${TOTAL_FAIL}" -gt 0 ]; then
    echo "preflight: FAIL (${TOTAL_FAIL} smoke script(s) reported failures)"
    exit 1
fi
echo "preflight: PASS"
exit 0
