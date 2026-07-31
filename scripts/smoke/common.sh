#!/usr/bin/env bash
# Common assertion helpers for Harbor phase smoke scripts.
# Source this from each scripts/smoke/phase-NN.sh.

set -euo pipefail

# Counters incremented by helpers; phase scripts read them at the end.
SMOKE_OK=${SMOKE_OK:-0}
SMOKE_SKIP=${SMOKE_SKIP:-0}
SMOKE_FAIL=${SMOKE_FAIL:-0}

# api_url <path>
# Compose a URL against the local dev server.
api_url() {
    local path="$1"
    local base="${HARBOR_BASE_URL:-http://127.0.0.1:18080}"
    printf '%s%s' "${base}" "${path}"
}

# log <level> <msg>
log()  { printf '[%s] %s\n' "$1" "$2"; }
ok()   { SMOKE_OK=$((SMOKE_OK + 1));     log 'OK'   "$1"; }
skip() { SMOKE_SKIP=$((SMOKE_SKIP + 1)); log 'SKIP' "$1"; }
fail() { SMOKE_FAIL=$((SMOKE_FAIL + 1)); log 'FAIL' "$1"; }

# assert_file <path> <description>
assert_file() {
    local p="$1" desc="$2"
    if [ -f "$p" ]; then
        ok "${desc} (${p})"
    else
        fail "${desc} missing (${p})"
    fi
}

# assert_dir_nonempty <path> <description>
assert_dir_nonempty() {
    local p="$1" desc="$2"
    if [ -d "$p" ] && [ -n "$(ls -A "$p" 2>/dev/null)" ]; then
        ok "${desc} (${p})"
    else
        fail "${desc} empty or missing (${p})"
    fi
}

# -----------------------------------------------------------------------------
# Invocation rules every grep helper below shares. Both exist because a guard
# that reports the WRONG answer is worse than no guard — it is the same failure
# class as a SKIP where an OK belongs (AGENTS.md §4.2 item 5).
#
# `-a` (treat the file as text). A source file may legitimately contain a NUL
# byte in a string literal. grep classifies such a file as BINARY, and BSD
# grep's handling of a binary file differs from GNU's and is sensitive to how it
# is invoked, so a guard over that file can silently stop matching content that
# is plainly there.
#
# `-E` (extended regex), on EVERY helper. These helpers are documented as taking
# an extended-regex pattern, and callers write ERE. A helper on BASIC grep
# silently reinterprets that pattern: `|` becomes a literal, so an alternation
# matches nothing and the guard is permanently green; `\(` opens a group, so an
# escaped-paren pattern is UNBALANCED and grep exits 2, which `2>/dev/null`
# swallows into the "not found" branch — an absence check that can never fail.
# Both shapes were live in the tree (the TUI Protocol-client boundary check and
# a migration-runner absence check), reporting OK for years while blind. One
# regex dialect across all helpers is the fix.
#
# `-r` when the target is a DIRECTORY. Plain grep on a directory does not
# recurse and returns "no match", which for an absence check reads as a pass.
# The helpers detect a directory and add `-r` so a caller cannot get that wrong.
# -----------------------------------------------------------------------------

# _grep_recursive_flag <target>
# Echoes `-r` when target is a directory, empty otherwise. A directory target
# without -r silently matches nothing (see the header note).
#
# ALWAYS returns 0. These scripts run under `set -e`, and the callers assign
# this through a command substitution — a non-zero status would abort the whole
# smoke script on the (normal) non-directory case.
_grep_recursive_flag() {
    if [ -d "$1" ]; then
        printf -- '-r'
    fi
    return 0
}

# assert_grep_absent <pattern> <path-or-dir> <description>
# Asserts an EXTENDED-regex pattern is NOT found in path. Used for
# forbidden-words scans and architectural-boundary checks. Recurses when the
# target is a directory.
assert_grep_absent() {
    local pattern="$1" target="$2" desc="$3" rflag
    rflag="$(_grep_recursive_flag "$target")"
    # NOTE: grep exit code 2 (a malformed pattern / unreadable target) must NOT
    # read as "absent". Capture the status explicitly and fail loud on it.
    # `|| rc=$?` keeps the non-zero statuses from tripping `set -e`.
    local rc=0
    grep -a ${rflag} -qEi -- "$pattern" "$target" 2>/dev/null || rc=$?
    case "${rc}" in
        0) fail "${desc}: forbidden pattern '${pattern}' found in ${target}" ;;
        1) ok "${desc} (no '${pattern}' in ${target})" ;;
        *) fail "${desc}: grep failed (exit ${rc}) on pattern '${pattern}' in ${target} — the guard did not run" ;;
    esac
}

# assert_grep_present <pattern> <path> <description>
# Asserts an extended-regex pattern IS found in path. Used by
# static-guard smokes to pin a load-bearing declaration in a source file.
assert_grep_present() {
    local pattern="$1" target="$2" desc="$3"
    local rflag
    rflag="$(_grep_recursive_flag "$target")"
    if grep -a ${rflag} -qE -- "$pattern" "$target" 2>/dev/null; then
        ok "${desc}"
    else
        fail "${desc} — pattern '${pattern}' absent from ${target}"
    fi
}

# assert_grep_count <pattern> <path> <expected> <description>
# Asserts an extended-regex pattern appears exactly <expected> times in
# path. Used by static-guard smokes that pin a stable count (e.g. the
# canonical Protocol-error-code set).
assert_grep_count() {
    local pattern="$1" target="$2" expected="$3" desc="$4" actual
    local rflag
    rflag="$(_grep_recursive_flag "$target")"
    actual=$(grep -a ${rflag} -cE -- "$pattern" "$target" 2>/dev/null) || actual=0
    if [ "${actual}" -eq "${expected}" ]; then
        ok "${desc} (count=${actual})"
    else
        fail "${desc} — found ${actual} matches of '${pattern}' in ${target}, want ${expected}"
    fi
}

# assert_go_tests_pass <log-path> <go-test-args> <description> <test-name>...
# Runs `go test -v -run '^(T1|T2|…)$' <go-test-args>` and asserts every named
# test actually RAN and PASSED.
#
# The exit code alone is NOT a sufficient assertion here, and this is the whole
# reason the helper exists: `go test -run` whose filter matches nothing prints
# "no tests to run" and exits ZERO. A guard that only checks the exit code
# therefore reports OK after the test it names is renamed or deleted — the
# same "the pass value is also the can't-tell value" shape as a SKIP standing
# in for an OK (AGENTS.md §4.2 item 5). So the helper greps the `-v` output
# for a `--- PASS:` line per NAME; a rename turns it into a FAIL.
#
# <go-test-args> is a single word-split string, ordinarily just the package
# list (e.g. "./internal/state/... ./x/..."). It may ALSO carry extra `go test`
# flags ahead of the packages — "-race -count=1 ./internal/x/" — because they
# land after `-run` on the command line, which `go test` accepts. That is how a
# caller keeps the race detector on a leg that had it before adopting this
# helper; do NOT force `-race` here, since most callers' legs are cheap
# non-concurrent suites that preflight runs on every commit.
#
# `CGO_ENABLED=0` is deliberately NOT set: on Linux the race detector needs
# cgo, and `CGO_ENABLED=0 go test -race` fails to BUILD. Harbor's CGo ban
# governs the shipped binary, not a race-instrumented test binary.
assert_go_tests_pass() {
    local log="$1" pkgs="$2" desc="$3"
    shift 3
    local names=("$@")
    if ! command -v go >/dev/null 2>&1; then
        skip "${desc}: go toolchain not available"
        return
    fi
    local joined pattern t rc=0
    joined="$(printf '%s|' "${names[@]}")"
    pattern="^(${joined%|})$"
    # shellcheck disable=SC2086 # pkgs is a deliberate flags+packages word split
    go test -v -run "${pattern}" ${pkgs} >"${log}" 2>&1 || rc=$?
    if [ "${rc}" -ne 0 ]; then
        fail "${desc} — go test exited ${rc}"
        printf -- '--- go test output (tail) ---\n'
        tail -40 "${log}" | sed 's/^/    /'
        return
    fi
    local missing=''
    for t in "${names[@]}"; do
        if ! grep -qE -- "^[[:space:]]*--- PASS: ${t} \(" "${log}"; then
            missing="${missing} ${t}"
        fi
    done
    if [ -n "${missing}" ]; then
        fail "${desc} — go test exited 0 but these named test(s) never ran (renamed, deleted, or filtered out):${missing}"
        printf -- '--- go test output (tail) ---\n'
        tail -40 "${log}" | sed 's/^/    /'
        return
    fi
    ok "${desc} (${#names[@]} named test(s) ran and passed)"
}

# assert_status <expected> <url> <description>
# Used once the dev server runs (Phase 01+).
assert_status() {
    local expected="$1" url="$2" desc="$3"
    if ! command -v curl >/dev/null 2>&1; then
        skip "${desc}: curl not available"
        return
    fi
    local actual
    # `|| true`, not `|| echo "000"` — curl prints its own "000" via -w on
    # connection failure (see skip_if_404). A dead server stays a FAIL here
    # (no 000 in the case), but the message reports the honest "000".
    actual=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$url" || true)
    case "$actual" in
        404|405|501)
            skip "${desc}: ${actual} (surface not yet implemented)"
            return
            ;;
    esac
    if [ "$actual" = "$expected" ]; then
        ok "${desc}: ${expected} (${url})"
    else
        fail "${desc}: expected ${expected}, got ${actual} (${url})"
    fi
}

# skip_if_404 <url> <description>
# Quick check; SKIP if the surface isn't there yet, or if curl missing.
skip_if_404() {
    local url="$1" desc="$2"
    if ! command -v curl >/dev/null 2>&1; then
        skip "${desc}: curl not available"
        return 1
    fi
    local actual
    # On connection failure curl STILL prints its -w '%{http_code}' format
    # ("000") before exiting non-zero, so an `|| echo "000"` fallback would
    # produce "000000" and dodge the SKIP case below — the surfaced-then-fixed
    # Phase 113b finding (a smoke run with no server proceeded as reachable).
    # `|| true` keeps the single honest "000".
    actual=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$url" || true)
    case "$actual" in
        404|405|501|000|'')
            skip "${desc}: ${actual:-000} (surface not yet implemented)"
            return 1
            ;;
    esac
    return 0
}

# skip_all_if_server_down [description]
# Probe /healthz; if the dev server is unreachable (connection refused →
# curl code 000, or curl absent), SKIP the entire script and exit 0 cleanly.
# This lets a STANDALONE battery run (`for f in scripts/smoke/phase-*.sh`)
# degrade gracefully instead of hard-crashing rc=7 on the first raw
# `curl -sS` under `set -e`, and keeps a live-server healthz assertion from
# reporting FAIL on `got 000` when no server is up. Preflight ALWAYS boots the
# server before invoking a smoke, so this is a no-op there (behavior unchanged).
# Scripts with a live-server dependency call this right after sourcing common.sh.
skip_all_if_server_down() {
    local desc="${1:-dev server}"
    if ! command -v curl >/dev/null 2>&1; then
        skip "${desc}: curl not available — skipping standalone run"
        smoke_summary || true
        exit 0
    fi
    local code
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$(api_url /healthz)" || true)
    case "${code}" in
        000|'')
            skip "${desc}: dev server unreachable at $(api_url /healthz) — skipping standalone run"
            smoke_summary || true
            exit 0
            ;;
    esac
}

# assert_json_path <jq_path> <expected_value> <url> <description>
# GET url, parse with jq, assert path equals value.
assert_json_path() {
    local jq_path="$1" expected="$2" url="$3" desc="$4"
    if ! command -v jq >/dev/null 2>&1; then
        skip "${desc}: jq not available"
        return
    fi
    skip_if_404 "$url" "$desc" || return 0
    local body actual
    body=$(curl -s --max-time 5 "$url" || echo '{}')
    actual=$(printf '%s' "$body" | jq -r "$jq_path" 2>/dev/null || echo "")
    if [ "$actual" = "$expected" ]; then
        ok "${desc}: ${jq_path} = ${expected}"
    else
        fail "${desc}: ${jq_path} expected ${expected}, got ${actual}"
    fi
}

# assert_body_contains <substring> <url> <description>
# GET url; SKIP on 404/405/501/no-server; assert the response body
# contains <substring>. Used for non-JSON surfaces (e.g. the Prometheus
# /metrics text exposition format).
assert_body_contains() {
    local needle="$1" url="$2" desc="$3"
    skip_if_404 "$url" "$desc" || return 0
    local body
    body=$(curl -s --max-time 5 "$url" || echo "")
    if printf '%s' "$body" | grep -qF -- "$needle"; then
        ok "${desc}: body contains '${needle}'"
    else
        fail "${desc}: body missing '${needle}' (${url})"
    fi
}

# protocol_call <method> <params_json> <description>
# Stub for Protocol JSON-RPC calls. Will be filled in once the Protocol lands.
protocol_call() {
    local method="$1"
    local desc="${3:-protocol call ${method}}"
    skip "${desc}: protocol layer not implemented yet"
}

# assert_post_status <expected> <url> <json_body> <description>
# POST json_body to url; SKIP on 404/405/501 (surface absent); assert
# the response status equals <expected>. Used by phase smokes for
# POST-only Protocol routes (the GET-based assert_status would always
# see a 405 on a POST-only route).
assert_post_status() {
    local expected="$1" url="$2" body="$3" desc="$4"
    if ! command -v curl >/dev/null 2>&1; then
        skip "${desc}: curl not available"
        return
    fi
    local actual
    # `|| true`, not `|| echo "000"` — curl prints its own "000" via -w on
    # connection failure (see skip_if_404).
    actual=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST -H 'Content-Type: application/json' -d "$body" "$url" \
        || true)
    case "$actual" in
        404|501|000|'')
            skip "${desc}: ${actual:-000} (surface not yet implemented)"
            return
            ;;
    esac
    if [ "$actual" = "$expected" ]; then
        ok "${desc}: ${expected} (POST ${url})"
    else
        fail "${desc}: expected ${expected}, got ${actual} (POST ${url})"
    fi
}

# dev_bearer
# Echo the dev bootstrap bearer, or the empty string when unresolvable.
# Prefers the exported HARBOR_DEV_TOKEN (preflight mirrors it out of the
# server log); falls back to grepping ${HARBOR_DATA_DIR}/server.log, which
# is what a standalone run against a hand-booted `harbor dev` has. The
# `harbor dev` binary prints `HARBOR_DEV_TOKEN=<jwt>` to stderr at boot.
dev_bearer() {
    if [ -n "${HARBOR_DEV_TOKEN:-}" ]; then
        printf '%s' "${HARBOR_DEV_TOKEN}"
        return 0
    fi
    if [ -n "${HARBOR_DATA_DIR:-}" ] && [ -f "${HARBOR_DATA_DIR}/server.log" ]; then
        grep -m1 '^HARBOR_DEV_TOKEN=' "${HARBOR_DATA_DIR}/server.log" 2>/dev/null \
            | sed 's/^HARBOR_DEV_TOKEN=//' || true
        return 0
    fi
    printf ''
}

# assert_post_status_auth <expected> <url> <json_body> <bearer> <description>
# AUTHENTICATED POST (Authorization: Bearer <bearer>) asserting the status
# equals <expected>. Only 000 (server unreachable) SKIPs. Unlike
# assert_post_status, a 404/405/501 is a FAIL — use this for a route that
# is ALREADY shipped, where an absent route is an un-mounted REGRESSION
# rather than a future surface (§4.2 item 5: a SKIP that should be an OK
# is a bug). Reach for assert_post_status instead when the surface may
# legitimately not exist yet on this build.
assert_post_status_auth() {
    local expected="$1" url="$2" body="$3" bearer="$4" desc="$5"
    if ! command -v curl >/dev/null 2>&1; then
        skip "${desc}: curl not available"
        return
    fi
    if [ -z "${bearer}" ]; then
        fail "${desc}: no dev bearer resolved — the request would be unauthenticated (401), which proves nothing"
        return
    fi
    local actual
    actual=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
        -X POST -H "Authorization: Bearer ${bearer}" \
        -H 'Content-Type: application/json' -d "$body" "$url" \
        || true)
    case "$actual" in
        000|'')
            skip "${desc}: server unreachable (000)"
            return
            ;;
        404|405|501)
            fail "${desc}: got ${actual} — this route is already shipped, so an absent route is an UN-MOUNTED regression, not a future surface (POST ${url})"
            return
            ;;
    esac
    if [ "$actual" = "$expected" ]; then
        ok "${desc}: ${expected} (authenticated POST ${url})"
    else
        fail "${desc}: expected ${expected}, got ${actual} (authenticated POST ${url})"
    fi
}

# assert_json_path_resolves <ref_id> <get_ref_url> <auth_header> <id_header_tenant> <id_header_user> <id_header_session> <description>
# POSTs <ref_id> to the artifacts.get_ref resolver and asserts the id ROUTES:
# HTTP 200 (+ presigned_url, an S3-compat Presigner store) OR 501/presign_unsupported
# (the default CGo-free inmem/fs store — both prove the id reached the resolver
# well-formed). The strict 200-resolves assertion is gated behind a HARBOR_LIVE_* S3 env.
assert_json_path_resolves() {
    local ref_id="$1" get_ref_url="$2" auth="$3" tn="$4" us="$5" se="$6" desc="$7"
    if ! command -v curl >/dev/null 2>&1; then
        skip "${desc}: curl not available"
        return
    fi
    if [ -z "${ref_id}" ] || [ "${ref_id}" = "null" ]; then
        skip "${desc}: no ref id to route (no heavy turn seeded)"
        return
    fi
    local body status
    body="{\"identity\":{\"tenant\":\"${tn}\",\"user\":\"${us}\",\"session\":\"${se}\"},\"id\":\"${ref_id}\",\"scope\":{\"tenant\":\"${tn}\",\"user\":\"${us}\",\"session\":\"${se}\"}}"
    status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
        -X POST -H "Authorization: Bearer ${auth}" -H 'Content-Type: application/json' \
        -d "${body}" "${get_ref_url}" || true)
    case "${status}" in
        200|501)
            ok "${desc}: ref id ROUTES to artifacts.get_ref (${status})"
            ;;
        *)
            fail "${desc}: ref id did not route well-formed (status ${status}, want 200 or 501)"
            ;;
    esac
}

# smoke_summary
# Print final counters. Exit 1 if any FAIL; else 0.
smoke_summary() {
    printf '\n=== Phase smoke summary ===\n'
    printf 'OK:   %d\n' "${SMOKE_OK}"
    printf 'SKIP: %d\n' "${SMOKE_SKIP}"
    printf 'FAIL: %d\n' "${SMOKE_FAIL}"
    if [ "${SMOKE_FAIL}" -gt 0 ]; then
        return 1
    fi
    return 0
}
