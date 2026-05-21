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

# assert_grep_absent <pattern> <path> <description>
# Asserts pattern is NOT found in path. Used for forbidden-words scans.
assert_grep_absent() {
    local pattern="$1" target="$2" desc="$3"
    if grep -q -i -- "$pattern" "$target" 2>/dev/null; then
        fail "${desc}: forbidden pattern '${pattern}' found in ${target}"
    else
        ok "${desc} (no '${pattern}' in ${target})"
    fi
}

# assert_grep_present <pattern> <path> <description>
# Asserts an extended-regex pattern IS found in path. Used by
# static-guard smokes to pin a load-bearing declaration in a source file.
assert_grep_present() {
    local pattern="$1" target="$2" desc="$3"
    if grep -qE -- "$pattern" "$target" 2>/dev/null; then
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
    actual=$(grep -cE -- "$pattern" "$target" 2>/dev/null) || actual=0
    if [ "${actual}" -eq "${expected}" ]; then
        ok "${desc} (count=${actual})"
    else
        fail "${desc} — found ${actual} matches of '${pattern}' in ${target}, want ${expected}"
    fi
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
    actual=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$url" || echo "000")
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
    actual=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$url" || echo "000")
    case "$actual" in
        404|405|501|000)
            skip "${desc}: ${actual} (surface not yet implemented)"
            return 1
            ;;
    esac
    return 0
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
    actual=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST -H 'Content-Type: application/json' -d "$body" "$url" \
        || echo "000")
    case "$actual" in
        404|501|000)
            skip "${desc}: ${actual} (surface not yet implemented)"
            return
            ;;
    esac
    if [ "$actual" = "$expected" ]; then
        ok "${desc}: ${expected} (POST ${url})"
    else
        fail "${desc}: expected ${expected}, got ${actual} (POST ${url})"
    fi
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
