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

# ----------------------------------------------------------------------
# INERT-SMOKE DETECTION (the structural half of the wave-v1.24 audit).
#
# Until now this harness failed on one condition only: a smoke script
# exiting non-zero. A script that asserted NOTHING — every check a SKIP —
# exited 0 and read as green. That is the structural enabler behind the
# inert guards this wave repaired: phase 213 posting to a route that never
# existed, and phase 216's only live assertion SKIPping two different ways.
# Both had been dead since the day they were written and preflight said
# PASS every time.
#
# `assess_smoke_output` reads a finished smoke's captured output and
# records the script when its summary shows OK == 0 AND FAIL == 0. Whether
# that is fatal depends on the phase's master-plan Status:
#
#   - Status `Shipped`  -> FAIL. §4.2 item 5: "A SKIP that should be an OK
#     is a bug." A shipped phase's surface exists, so its smoke must assert
#     something against it.
#   - anything else     -> reported, not fatal. A Pending phase's skeleton
#     legitimately all-SKIPs against a build that has not shipped its
#     surface yet; that is the §4.2 item 4 convention, not a defect.
#
# The Status read is deliberately fail-LOUD in the direction that matters:
# a row that cannot be parsed is treated as Shipped, so a master-plan
# reformat cannot silently disable this gate. Set
# HARBOR_PREFLIGHT_ALLOW_INERT=1 to downgrade every case to a report — for
# an emergency only, and it must be justified in the PR exactly as
# HARBOR_PREFLIGHT_SKIP is (§4.1).
#
# THE BASELINE. When this gate was first switched on, 24 shipped-phase
# scripts already violated it — measured, not estimated. Failing all 24 on
# day one would have got the gate disabled within a week, so they are listed
# in `scripts/smoke/inert-baseline.txt` as KNOWN DEBT. A script NOT in that
# file that goes all-SKIP is a NEW regression and fails; a script IN it that
# starts asserting something is reported as a stale entry to delete. The
# rationale for each direction is in the file's own header.
# ----------------------------------------------------------------------
INERT_BASELINE_FILE='scripts/smoke/inert-baseline.txt'
INERT_SHIPPED=()          # inert, shipped, NOT baselined -> FAIL
INERT_BASELINED=()        # inert, shipped, baselined     -> reported as debt
INERT_PENDING=()          # inert, not shipped            -> expected
INERT_SEEN_BASELINED=''   # newline-joined; used to find stale baseline entries

# is_baselined <smoke-path>
is_baselined() {
    [ -f "${INERT_BASELINE_FILE}" ] || return 1
    grep -vE '^[[:space:]]*(#|$)' "${INERT_BASELINE_FILE}" 2>/dev/null \
        | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//' \
        | grep -qxF -- "$1"
}

# phase_is_shipped <phase-token>   e.g. 213, 83w, 113a
# Echoes "yes" unless the master-plan row for the phase exists AND its
# final column says something other than Shipped.
phase_is_shipped() {
    local n="$1" row status
    row="$(grep -m1 -E "^\| *${n} +\|" docs/plans/README.md 2>/dev/null || true)"
    if [ -z "${row}" ]; then
        # No row at all: cannot prove it is unshipped, so treat as shipped.
        printf 'yes'
        return 0
    fi
    # Last non-empty pipe-delimited field.
    status="$(printf '%s' "${row}" | sed -E 's/[[:space:]]*\|[[:space:]]*$//; s/.*\|[[:space:]]*//')"
    # Statuses in the master plan are `Shipped`, `Shipped (v1.23)`,
    # `Pending`, `Pending (V1.5.x)`, `Post-V1`, ... — match on the prefix,
    # and default anything unrecognised to Shipped so a new status word
    # cannot quietly disable the gate.
    case "${status}" in
        Pending*|pending*|Post-V1*|Post-v1*|Deferred*|deferred*) printf 'no' ;;
        *)                                                       printf 'yes' ;;
    esac
}

# assess_smoke_output <smoke-path> <captured-output-path>
assess_smoke_output() {
    local smoke="$1" out_path="$2" ok_n fail_n n
    [ -f "${out_path}" ] || return 0
    ok_n="$(grep -m1 -E '^OK:[[:space:]]+[0-9]+' "${out_path}" 2>/dev/null | grep -oE '[0-9]+' | head -1 || true)"
    fail_n="$(grep -m1 -E '^FAIL:[[:space:]]+[0-9]+' "${out_path}" 2>/dev/null | grep -oE '[0-9]+' | head -1 || true)"
    # No summary block at all (the script died before smoke_summary): the
    # non-zero exit path already accounts for that. Nothing to add.
    [ -n "${ok_n}" ] && [ -n "${fail_n}" ] || return 0
    [ "${ok_n}" = "0" ] && [ "${fail_n}" = "0" ] || return 0
    n="$(basename "${smoke}" | sed 's/^phase-//; s/\.sh$//')"
    if [ "$(phase_is_shipped "${n}")" != "yes" ]; then
        INERT_PENDING+=("${smoke}")
    elif is_baselined "${smoke}"; then
        INERT_BASELINED+=("${smoke}")
        INERT_SEEN_BASELINED="${INERT_SEEN_BASELINED}
${smoke}"
    else
        INERT_SHIPPED+=("${smoke}")
    fi
}

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
            # `|| rc=$?`: a bare `wait` on a failed child trips set -e and
            # kills the whole harness BEFORE the aggregator prints the
            # batch's buffered output — the failing smoke's report is lost
            # and its siblings are orphaned mid-run.
            rc=0
            wait "${head_pid}" 2>/dev/null || rc=$?
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
        rc=0
        wait "${p}" 2>/dev/null || rc=$?
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
        assess_smoke_output "${s_name}" "${out_path}"
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
LIVE_OUT_DIR="$(mktemp -d -t harbor-preflight-live-XXXXXX)"
for smoke in ${LIVE_SERVER[@]+"${LIVE_SERVER[@]}"}; do
    echo ""
    echo "preflight: running ${smoke}"
    live_out="${LIVE_OUT_DIR}/$(basename "${smoke}").out"
    # `tee` so the operator still sees the run live AND the inert-smoke
    # detector gets the counters.
    #
    # PIPESTATUS[0] is the smoke's own status (`tee` always succeeds) and it
    # MUST be read inside the group, before any other command runs: every
    # obvious shorthand clobbers it. `if ! pipeline; then :; fi` resets it via
    # the `:`, and `pipeline || true` resets it via the `true` — both silently
    # report every failing smoke as rc=0. Verified, not assumed.
    smoke_rc=0
    { bash "${smoke}" 2>&1 | tee "${live_out}"; smoke_rc="${PIPESTATUS[0]}"; } || true
    if [ "${smoke_rc}" -ne 0 ]; then
        echo "preflight: ${smoke} reported failures"
        TOTAL_FAIL=$((TOTAL_FAIL + 1))
    fi
    assess_smoke_output "${smoke}" "${live_out}"
done
rm -rf "${LIVE_OUT_DIR}"

echo ""
echo "=== preflight summary ==="

if [ "${#INERT_PENDING[@]}" -gt 0 ]; then
    echo ""
    echo "preflight: ${#INERT_PENDING[@]} smoke script(s) asserted nothing (OK=0, FAIL=0) for a phase that has"
    echo "  not shipped yet — expected, per the 404/405/501 -> SKIP convention (§4.2 item 4):"
    for s in "${INERT_PENDING[@]}"; do echo "    ${s}"; done
fi

if [ "${#INERT_BASELINED[@]}" -gt 0 ]; then
    echo ""
    echo "preflight: ${#INERT_BASELINED[@]} smoke script(s) asserted nothing (OK=0, FAIL=0) for a shipped"
    echo "  phase and are listed as KNOWN DEBT in ${INERT_BASELINE_FILE}."
    echo "  Each is a guard that cannot currently fail. Paying one down means making it"
    echo "  assert something and deleting its line from that file."
    for s in "${INERT_BASELINED[@]}"; do echo "    ${s}"; done
fi

# A baseline entry that ran and DID assert something is stale — delete the line.
# A WARNING, not a FAIL: whether some of these assert anything is
# environment-dependent, so stale here is not necessarily stale everywhere.
if [ -f "${INERT_BASELINE_FILE}" ]; then
    stale_entries=''
    while IFS= read -r entry; do
        [ -n "${entry}" ] || continue
        # Only judge entries whose script actually ran in this invocation.
        [ -f "${entry}" ] || continue
        if ! printf '%s\n' "${INERT_SEEN_BASELINED}" | grep -qxF -- "${entry}"; then
            stale_entries="${stale_entries}    ${entry}
"
        fi
    done < <(grep -vE '^[[:space:]]*(#|$)' "${INERT_BASELINE_FILE}" 2>/dev/null \
        | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//')
    if [ -n "${stale_entries}" ]; then
        echo ""
        echo "preflight: STALE inert-baseline entries — these asserted something in this run,"
        echo "  so they are no longer debt. Delete their lines from ${INERT_BASELINE_FILE}:"
        printf '%s' "${stale_entries}"
    fi
fi

if [ "${#INERT_SHIPPED[@]}" -gt 0 ]; then
    echo ""
    echo "preflight: ${#INERT_SHIPPED[@]} smoke script(s) asserted NOTHING (OK=0, FAIL=0) for a SHIPPED phase"
    echo "  and are NOT in ${INERT_BASELINE_FILE}."
    echo "  A shipped phase's surface exists, so its smoke must report at least one OK."
    echo "  All-SKIP on a shipped phase means the guards are inert — pointing at a route"
    echo "  that does not exist, sending an identity the server refuses, or gated behind a"
    echo "  precondition that never holds. See CLAUDE.md §4.2 item 5."
    echo "  Fix the guard. Do NOT add it to the baseline: that file records debt this gate"
    echo "  inherited, not a place to park a guard you just wrote."
    for s in "${INERT_SHIPPED[@]}"; do echo "    ${s}"; done
    if [ "${HARBOR_PREFLIGHT_ALLOW_INERT:-0}" = "1" ]; then
        echo "  HARBOR_PREFLIGHT_ALLOW_INERT=1 — downgraded to a report. Justify this in the PR."
    else
        TOTAL_FAIL=$((TOTAL_FAIL + ${#INERT_SHIPPED[@]}))
    fi
fi

if [ "${TOTAL_FAIL}" -gt 0 ]; then
    echo "preflight: FAIL (${TOTAL_FAIL} smoke script(s) reported failures or asserted nothing)"
    exit 1
fi
echo "preflight: PASS"
exit 0
