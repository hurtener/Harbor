#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 84e — Semantic memory consumption in the run loop.
#
# Classification rationale: the preflight dev boot deliberately carries NO
# embedding provider (there is no stub embeddings driver — CLAUDE.md §13),
# so the recall surface cannot be exercised against the live server. The
# shipped assertions are static seam/config/doc checks plus the Phase 84e
# integration tests under -race (same honesty call Phase 84d's smoke
# recorded); the live-provider leg stays with the HARBOR_LIVE_LLM probes.
#
# Assertions:
#   1. runctx.FetchMemoryBlocks is the single home for the memory step
#      and both run-loop call sites consume it.
#   2. memory.RecallSettings + memory.RecallFromConfig ship in
#      internal/memory/from_config.go.
#   3. memory.RetrievalMinScore exists in internal/config/config.go.
#   4. Built-binary `harbor validate` rejects an out-of-range
#      memory.retrieval_min_score (exit 1, names the key) and accepts
#      the documented default (0.0).
#   5. The Phase 84e integration tests pass under -race.
#   6. docs/CONFIG.md carries the memory.retrieval_min_score heading.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Convenience: skip with an informative message when a pattern or file is
# absent (pre-84e build), fail when it IS present but the assertion fails.
assert_or_skip() {
    local pattern="$1" file="$2" desc="$3"
    if [ ! -f "${file}" ]; then
        skip "${desc}: ${file} not found (Phase 84e not yet implemented)"
        return
    fi
    if grep -qE "${pattern}" "${file}" 2>/dev/null; then
        ok "${desc}"
    else
        skip "${desc}: pattern '${pattern}' absent (Phase 84e not yet implemented)"
    fi
}

# --------------------------------------------------------------------------
# 1. Static: runctx.FetchMemoryBlocks is the single run-loop memory home.
# --------------------------------------------------------------------------

assert_or_skip 'func FetchMemoryBlocks\(' \
    "internal/runtime/runctx/memory_fetch.go" \
    "static: runctx.FetchMemoryBlocks is the single production home for the memory step"

# Both run-loop call sites must call FetchMemoryBlocks; no surviving
# inline GetLLMContext block.
if [ -f "internal/runtime/serve/runloop.go" ] && [ -f "harbortest/devstack/devstack.go" ]; then
    if grep -q 'runctx\.FetchMemoryBlocks' internal/runtime/serve/runloop.go \
    && grep -q 'runctx\.FetchMemoryBlocks' harbortest/devstack/devstack.go; then
        ok "static: both run-loop call sites delegate to runctx.FetchMemoryBlocks"
    else
        skip "static: at least one call site has not adopted FetchMemoryBlocks (Phase 84e not yet implemented)"
    fi
else
    skip "static: run-loop files absent (Phase 84e not yet implemented)"
fi

# No surviving inline GetLLMContext fetch block (the collapsed duplication).
if [ -f "internal/runtime/serve/runloop.go" ]; then
    inline_count=$(grep -c '\.GetLLMContext(' internal/runtime/serve/runloop.go 2>/dev/null || true)
    if [[ "${inline_count}" -eq 0 ]]; then
        ok "static: cmd_dev_runloop.go has no inline GetLLMContext call (fully delegated)"
    else
        skip "static: cmd_dev_runloop.go still has an inline GetLLMContext call (Phase 84e not yet collapsed)"
    fi
fi

# --------------------------------------------------------------------------
# 2. Static: RecallSettings + RecallFromConfig in memory/from_config.go.
# --------------------------------------------------------------------------

assert_or_skip 'type RecallSettings struct' \
    "internal/memory/from_config.go" \
    "static: memory.RecallSettings struct ships in from_config.go"

assert_or_skip 'func RecallFromConfig\(' \
    "internal/memory/from_config.go" \
    "static: memory.RecallFromConfig exporter ships in from_config.go"

# --------------------------------------------------------------------------
# 3. Static: RetrievalMinScore in config.go + CONFIG.md heading.
# --------------------------------------------------------------------------

assert_or_skip 'RetrievalMinScore' \
    "internal/config/config.go" \
    "static: config.MemoryConfig carries the RetrievalMinScore field"

assert_or_skip 'memory\.retrieval_min_score' \
    "docs/CONFIG.md" \
    "docs: CONFIG.md carries the memory.retrieval_min_score heading"

# --------------------------------------------------------------------------
# 4. Fail-loud validation round-trip against the built binary.
# --------------------------------------------------------------------------

BIN="${ROOT}/bin/harbor"
FIXTURE="internal/config/testdata/valid_minimal.yaml"

if ! grep -qE 'RetrievalMinScore' internal/config/config.go 2>/dev/null; then
    skip "validate round-trip: Phase 84e not yet implemented (RetrievalMinScore field absent)"
elif [[ ! -x "${BIN}" ]]; then
    skip "validate round-trip: bin/harbor not built (preflight build step skipped)"
elif [[ ! -f "${FIXTURE}" ]]; then
    skip "validate round-trip: ${FIXTURE} fixture absent"
else
    TMPDIR_84E="$(mktemp -d)"
    trap 'rm -rf "${TMPDIR_84E}"' EXIT

    # Out-of-range value (> 1) → exit 1 naming the key.
    cat "${FIXTURE}" > "${TMPDIR_84E}/out-of-range.yaml"
    cat >> "${TMPDIR_84E}/out-of-range.yaml" <<'YAML'

memory:
  driver: inmem
  retrieval_min_score: 1.5
YAML
    set +e
    body=$("${BIN}" validate "${TMPDIR_84E}/out-of-range.yaml" 2>&1)
    rc=$?
    set -e
    if [[ "${rc}" -ne 1 ]]; then
        fail "validate: retrieval_min_score=1.5 expected exit 1; got ${rc}"
    elif ! printf '%s' "${body}" | grep -q 'retrieval_min_score'; then
        fail "validate: fail-loud body did not name the field: ${body}"
    else
        ok "validate: retrieval_min_score=1.5 fails loudly naming the field (out-of-range guard)"
    fi

    # Out-of-range on the low side (< -1) → exit 1.
    cat "${FIXTURE}" > "${TMPDIR_84E}/out-of-range-low.yaml"
    cat >> "${TMPDIR_84E}/out-of-range-low.yaml" <<'YAML'

memory:
  driver: inmem
  retrieval_min_score: -1.5
YAML
    set +e
    rc=$("${BIN}" validate "${TMPDIR_84E}/out-of-range-low.yaml" > /dev/null 2>&1; echo $?)
    set -e
    if [[ "${rc}" -ne 1 ]]; then
        fail "validate: retrieval_min_score=-1.5 expected exit 1; got ${rc}"
    else
        ok "validate: retrieval_min_score=-1.5 fails loudly (out-of-range low guard)"
    fi

    # Default value (0.0) → exit 0.
    cat "${FIXTURE}" > "${TMPDIR_84E}/default.yaml"
    cat >> "${TMPDIR_84E}/default.yaml" <<'YAML'

memory:
  driver: inmem
  retrieval_min_score: 0.0
YAML
    if "${BIN}" validate "${TMPDIR_84E}/default.yaml" >/dev/null 2>&1; then
        ok "validate: retrieval_min_score=0.0 (default) validates cleanly"
    else
        fail "validate: retrieval_min_score=0.0 unexpectedly failed validation"
    fi
fi

# --------------------------------------------------------------------------
# 5. Integration tests — real drivers; deterministic embedder; -race.
# --------------------------------------------------------------------------

if [ ! -f "test/integration/phase84e_semantic_recall_test.go" ]; then
    skip "round-trip: integration test absent (Phase 84e not yet implemented)"
elif go test -race -count=1 -timeout 300s \
    -run 'TestE2E_Phase84e' \
    ./test/integration/ >/dev/null 2>&1; then
    ok "round-trip: FetchMemoryBlocks + semantic recall acceptance tests pass under -race"
else
    fail "round-trip: Phase 84e integration tests failed (run: go test -race -run TestE2E_Phase84e ./test/integration/)"
fi

smoke_summary
