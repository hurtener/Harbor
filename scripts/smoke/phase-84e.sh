#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 84e — Semantic memory consumption in the run loop.
#
# Classification rationale: the preflight dev boot deliberately carries NO
# embedding provider (there is no stub embeddings driver — CLAUDE.md §13),
# so the recall surface cannot be exercised against the live server. The
# shipped assertions are static seam/config/doc checks plus the Phase 84e
# integration tests under -race (the same honesty call Phase 84d's smoke
# recorded); the live-provider leg stays with the HARBOR_LIVE_LLM probes.
#
# Planned assertions (see docs/plans/phase-84e-semantic-memory-runloop.md,
# "Smoke script additions"):
#   - runctx.FetchMemoryBlocks exists; both run-loop call sites consume it
#     (no surviving inline GetLLMContext fetch block in cmd/harbor or
#     harbortest/devstack).
#   - built-binary `harbor validate` rejects an out-of-range
#     memory.retrieval_min_score (exit 1, names the key) and accepts the
#     documented default.
#   - the Phase 84e integration tests pass under -race (acceptance shape +
#     default parity + cross-session isolation).
#   - docs/CONFIG.md carries the memory.retrieval_min_score heading.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 84e: not yet implemented"

smoke_summary
