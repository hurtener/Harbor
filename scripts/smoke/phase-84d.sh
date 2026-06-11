#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 84d — Embedding client (`Embedder`) + semantic memory & skill
# retrieval (D-191).
#
# What this asserts (per §4.2 every check SKIPs on a pre-84d build):
#
#   1. Static: the §4.4 seam exists (interface + factory + registry in
#      internal/embeddings), the bifrost driver registers via the
#      D-196 prod aggregator, both consumers carry the opt-in mode +
#      fail-loud guards, and the à-la-carte recipe + docs-site stub
#      ship.
#   2. Fail-loud boot validation (built binary): a config enabling
#      `memory.retrieval: semantic` WITHOUT an `embeddings` block
#      exits 1 naming the missing key; adding the block flips it to
#      exit 0.
#   3. One embed + retrieval round-trip end-to-end: the Phase 84d
#      integration test (real state/memory/skills drivers + the
#      deterministic test embedder) runs under -race.
#
# §4.3 note (recorded in the phase plan): the plan's smoke sketch
# wanted a live-server semantic skill_search probe, but the preflight
# dev boot deliberately carries NO embedding provider (no stub
# embeddings driver exists — CLAUDE.md §13), so the live surface is
# structurally absent here. The round-trip runs through the
# integration test; the live-provider leg is the HARBOR_LIVE_LLM
# conformance probe in internal/embeddings/drivers/bifrost.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Convenience wrapper — the phase-84b shape.
assert_or_skip() {
    local pattern="$1" file="$2" desc="$3"
    if [ ! -f "${file}" ]; then
        skip "${desc}: ${file} not found (Phase 84d not yet implemented)"
        return
    fi
    if grep -qE "${pattern}" "${file}" 2>/dev/null; then
        ok "${desc}"
    else
        skip "${desc}: pattern '${pattern}' absent (Phase 84d not yet implemented)"
    fi
}

# ----------------------------------------------------------------------------
# 1. Static assertions — the seam + its consumers.
# ----------------------------------------------------------------------------

assert_or_skip 'type Embedder interface' \
    "internal/embeddings/embeddings.go" \
    "static: embeddings.Embedder is the §4.4 seam interface"

assert_or_skip 'func Open\(' \
    "internal/embeddings/registry.go" \
    "static: embeddings.Open is the factory (registry dispatch + identity guard)"

assert_or_skip 'func Cosine' \
    "internal/embeddings/embeddings.go" \
    "static: embeddings.Cosine is the one shared ranking primitive"

assert_or_skip 'embeddings/drivers/bifrost' \
    "internal/drivers/prod/prod.go" \
    "static: the bifrost embedder driver registers via the D-196 prod aggregator"

assert_or_skip 'type EmbeddingsConfig struct' \
    "internal/config/config.go" \
    "static: the embeddings config block exists"

assert_or_skip 'SearchTurns\(ctx context.Context' \
    "internal/memory/memory.go" \
    "static: MemoryStore.SearchTurns is the semantic memory retrieval surface"

assert_or_skip 'ErrSemanticDisabled' \
    "internal/memory/memory.go" \
    "static: a disabled-mode SearchTurns fails loudly (ErrSemanticDisabled)"

assert_or_skip 'Deps.Embedder is required for retrieval mode' \
    "internal/memory/registry.go" \
    "static: the memory registry carries the fail-loud Deps.Embedder guard"

assert_or_skip 'Deps.Embedder is required for retrieval mode' \
    "internal/skills/skills.go" \
    "static: the skills registry carries the fail-loud Deps.Embedder guard"

assert_or_skip 'func \(d \*driver\) searchSemantic' \
    "internal/skills/drivers/localdb/search_semantic.go" \
    "static: localdb Search gained the semantic ranking path"

assert_or_skip 'PathSemantic' \
    "internal/skills/skills.go" \
    "static: the semantic search-result path constant exists"

assert_or_skip 'embeddings.Open' \
    "docs/recipes/embed-and-retrieve.md" \
    "static: the à-la-carte recipe ships the headless path"

assert_or_skip 'embed-and-retrieve' \
    "docs/site/recipes/embed-and-retrieve.md" \
    "static: the docs-site recipe stub exists (Phase 103 manifest rule)"

assert_or_skip 'embed-and-retrieve' \
    "docs/site/.vitepress/config.ts" \
    "static: the docs-site nav carries the recipe (Phase 103 manifest rule)"

# ----------------------------------------------------------------------------
# 2. Fail-loud validation round-trip against the built binary.
# ----------------------------------------------------------------------------

BIN="${ROOT}/bin/harbor"
FIXTURE="internal/config/testdata/valid_minimal.yaml"

if ! grep -qE 'type EmbeddingsConfig struct' internal/config/config.go 2>/dev/null; then
    skip "validate round-trip: Phase 84d not yet implemented"
elif [[ ! -x "${BIN}" ]]; then
    skip "validate round-trip: bin/harbor not built (preflight build step skipped)"
elif [[ ! -f "${FIXTURE}" ]]; then
    skip "validate round-trip: ${FIXTURE} fixture absent"
else
    TMPDIR_84D="$(mktemp -d)"
    trap 'rm -rf "${TMPDIR_84D}"' EXIT

    # Semantic mode WITHOUT an embeddings block → exit 1 naming the key.
    cat "${FIXTURE}" > "${TMPDIR_84D}/semantic-no-embeddings.yaml"
    cat >> "${TMPDIR_84D}/semantic-no-embeddings.yaml" <<'YAML'

memory:
  driver: inmem
  retrieval: semantic
YAML
    set +e
    body=$("${BIN}" validate "${TMPDIR_84D}/semantic-no-embeddings.yaml" 2>&1)
    rc=$?
    set -e
    if [[ "${rc}" -ne 1 ]]; then
        fail "validate: semantic mode without embeddings block expected exit 1; got ${rc}"
    elif ! printf '%s' "${body}" | grep -q 'embeddings'; then
        fail "validate: fail-loud body did not name the embeddings block: ${body}"
    elif ! printf '%s' "${body}" | grep -q 'memory.retrieval'; then
        fail "validate: fail-loud body did not name the enabling mode: ${body}"
    else
        ok "validate: memory.retrieval=semantic without an embeddings block fails loudly naming the missing key (no stub fallback)"
    fi

    # Same config WITH the embeddings block → exit 0.
    cat "${FIXTURE}" > "${TMPDIR_84D}/semantic-with-embeddings.yaml"
    cat >> "${TMPDIR_84D}/semantic-with-embeddings.yaml" <<'YAML'

memory:
  driver: inmem
  retrieval: semantic
  retrieval_top_k: 5

embeddings:
  provider: openai
  model: text-embedding-3-small
  api_key: env.OPENAI_API_KEY
YAML
    if "${BIN}" validate "${TMPDIR_84D}/semantic-with-embeddings.yaml" >/dev/null 2>&1; then
        ok "validate: the embeddings block + semantic retrieval mode validate cleanly"
    else
        fail "validate: semantic config with an embeddings block expected exit 0"
    fi
fi

# ----------------------------------------------------------------------------
# 3. Embed + retrieval round-trip (real drivers; deterministic embedder).
# ----------------------------------------------------------------------------

if [ ! -f "test/integration/phase84d_semantic_retrieval_test.go" ]; then
    skip "round-trip: integration test absent (Phase 84d not yet implemented)"
elif go test -race -count=1 -timeout 300s \
    -run 'TestE2E_Phase84d_(SemanticMemory_PersistsAndIsolates|SemanticSkills_RankAndIsolate|FailureMode_SemanticWithoutEmbedder)' \
    ./test/integration/ >/dev/null 2>&1; then
    ok "round-trip: embed → persist → SearchTurns / semantic skill Search pass under -race (memory + skills consumers end-to-end)"
else
    fail "round-trip: Phase 84d integration tests failed (run: go test -race -run TestE2E_Phase84d ./test/integration/)"
fi

smoke_summary
