#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 139 — Public-site honesty sweep. Docs-only: this phase adds NO new
# runtime surface, so there is nothing to hit on the dev server. The gate is
# a set of static honesty greps over the marketing landing surface and the
# hot-reload test godoc — exactly the "VitePress build + greps" gate the wave
# coordination doc (docs/plans/wave-v18-coordination.md §4, phase 139) names.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

LANDING="docs/site/.vitepress/theme/landingSpec.ts"
HOTRELOAD_TEST="cmd/harbor/cmd_dev_hot_reload_test.go"

# Both claims use the same canonical manifest-derived count injected by VitePress.
# A release adding a method must not require a second hand-maintained count.
assert_grep_present 'value: String\(release\.methodCount\), label: "canonical Protocol methods"' "${LANDING}" \
    "landing: canonical-methods stat uses the injected manifest count"
assert_grep_present '\$\{release\.methodCount\} canonical methods' "${LANDING}" \
    "landing: Protocol prose uses the same injected manifest count"
assert_grep_present 'const release = __HARBOR_DOCS_RELEASE__' "${LANDING}" \
    "landing: release data comes from the docs build"
assert_grep_present 'methodCount: wireManifest\.methods\.length' "docs/site/.vitepress/config.ts" \
    "landing: docs build derives method count from the canonical wire manifest"
assert_grep_present '__HARBOR_DOCS_RELEASE__: JSON\.stringify\(releaseMetadata\)' "docs/site/.vitepress/config.ts" \
    "landing: VitePress injects the release metadata into the client build"
assert_grep_absent '[0-9]+ canonical methods|value: "[0-9]+", label: "canonical Protocol methods"' "${LANDING}" \
    "landing: no hand-maintained canonical-method count remains"
assert_grep_absent 'canonical methods at v1.6' "${LANDING}" \
    "landing: no stale release qualifier on the methods claim"

# Cosmetic/unprinted dev-banner artifact removed.
assert_grep_absent '3 drivers registered' "${LANDING}" \
    "landing: unprinted '3 drivers registered' banner removed"

# Hot-reload claim qualified to config/YAML reload (the genuine capability).
assert_grep_present 'config reload on your laptop' "${LANDING}" \
    "landing: hot-reload claim qualified to config reload"

# The stale reference to the non-existent integration test file is gone.
assert_grep_absent 'phase65_hot_reload_test.go' "${HOTRELOAD_TEST}" \
    "hot-reload test: stale phase65_hot_reload_test.go reference removed"

smoke_summary
