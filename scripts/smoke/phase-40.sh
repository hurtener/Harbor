#!/usr/bin/env bash
# Phase 40 — Skills.md importer (gap-closer).
#
# Phase 40 ships Harbor's spec-compliant Skills.md importer:
# YAML frontmatter + line-based body parser; section normalization;
# attachment resolution as ArtifactRef (option (b) per RFC §6.7);
# byte-stable round-trip invariant `Export(Import(b)) == b` over the
# golden corpus; identity-mandatory at the persistence boundary;
# D-025 concurrent-reuse test at N=128 under -race.
#
# Phase 40 has no HTTP/Protocol surface — the importer is in-process
# at this wave; Phase 60+ may expose an upload endpoint.  Smoke runs
# the Go-level test surface (parser + exporter + path-safety +
# attachment resolver + golden round-trip + D-025) under -race, AND
# verifies the golden corpus directory is non-empty (the round-trip
# invariant has nothing to assert against without fixtures).
#
# Conventions (CLAUDE.md §4.2):
#   - 404/405/501 → SKIP (no Protocol surface yet).
#   - The Go test surface MUST pass — that is the smoke-observable
#     boundary at this phase.
#   - The golden corpus presence is a structural check, not a network
#     call — runs unconditionally.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 120s ./internal/skills/importer/... >/dev/null 2>&1; then
    ok 'phase 40: internal/skills/importer tests pass under -race (parser + exporter + path-safety + attachments + golden round-trip + D-025 N=128)'
else
    fail 'phase 40: internal/skills/importer tests failed (run `go test -race ./internal/skills/importer/...` for detail)'
fi

assert_dir_nonempty 'internal/skills/importer/testdata/golden' 'phase 40: golden Skills.md corpus present (byte-stable round-trip invariant has fixtures to assert against)'

skip "phase 40: Skills.md importer has no HTTP/Protocol surface yet (Phase 60+ may expose import-by-upload over Protocol)"

smoke_summary
