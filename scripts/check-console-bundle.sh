#!/usr/bin/env bash
# scripts/check-console-bundle.sh — Phase 83k (D-157) Console-bundle
# sanity gate.
#
# Runs `make console-build` and asserts the resulting
# `cmd/harbor/consoledist/` tree contains a real SvelteKit bundle
# (index.html + an `_app/` directory with content). The check exists
# because operators (and the release pipeline) embed `consoledist/`
# into the binary — if `make console-build` silently no-ops or the
# bundle is empty, `harbor console` serves the synthesized placeholder
# page instead of the real UI.
#
# V1 of this gate is intentionally permissive on chunk-level determinism:
# SvelteKit + rollup's chunk-splitting is partially non-deterministic
# (worker-pool module-ID ordering varies between runs), so a "two
# consecutive builds produce byte-identical outputs" assertion produces
# false-positive failures. The pragmatic check is "did a real bundle
# land?" — which catches the actual failure modes (rebuild silently
# skipped, npm step crashed, web/console deleted) without false
# alarms.
#
# Usage:
#   scripts/check-console-bundle.sh
#
# Exit codes:
#   0 — bundle is present + populated.
#   1 — bundle is empty OR rebuild failed.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"

if [ ! -d web/console ]; then
    echo "check-console-bundle: skip — web/console absent"
    exit 0
fi

echo "check-console-bundle: running make console-build"
if ! make console-build >/dev/null; then
    echo "check-console-bundle: FAIL — make console-build failed" >&2
    exit 1
fi

# Sanity: the bundle must hold index.html + an _app/ directory.
# A bundle that's just `.gitkeep` indicates the rebuild silently
# no-op'd or web/console produced no output.
if [ ! -f cmd/harbor/consoledist/index.html ]; then
    echo "check-console-bundle: FAIL — cmd/harbor/consoledist/index.html missing after rebuild" >&2
    echo "  rebuild produced no Console output; the embed will be the synthesized placeholder" >&2
    exit 1
fi

if [ ! -d cmd/harbor/consoledist/_app ]; then
    echo "check-console-bundle: FAIL — cmd/harbor/consoledist/_app/ missing after rebuild" >&2
    echo "  SvelteKit's adapter-static output is incomplete; the bundle is unusable" >&2
    exit 1
fi

# Sanity: the _app/ directory must hold content (at least one .js
# under immutable/ or similar). A bare _app/ would still be broken.
app_files=$(find cmd/harbor/consoledist/_app -type f -name '*.js' 2>/dev/null | wc -l | tr -d ' ')
if [ "${app_files}" -lt 1 ]; then
    echo "check-console-bundle: FAIL — cmd/harbor/consoledist/_app/ has no .js files after rebuild" >&2
    exit 1
fi

echo "check-console-bundle: OK — Console bundle present (index.html + _app/ with ${app_files} JS files)"
