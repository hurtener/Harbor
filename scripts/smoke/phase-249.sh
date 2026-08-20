#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 249 smoke — optional MCP artifact-egress mapping parameters (HA-67).
# This phase adds no Protocol route. The static guards pin the one parser and
# the one Encode boundary, the MCP adapter coverage, and the operator spelling;
# focused Go tests provide the behavioral gate.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file \
    "docs/plans/phase-249-optional-artifact-egress-params.md" \
    "phase 249 plan exists"

assert_grep_present \
    'type mappedParameter struct' \
    "internal/tools/artifactegress/artifactegress.go" \
    "phase 249 compiles internal required/optional parameter metadata"
assert_grep_present \
    'optional := strings.HasSuffix' \
    "internal/tools/artifactegress/artifactegress.go" \
    "phase 249 parses the trailing optional marker"
assert_grep_present \
    'if param.optional' \
    "internal/tools/artifactegress/artifactegress.go" \
    "phase 249 skips absent optional arguments"
assert_grep_present \
    'out\[i\] = param.name' \
    "internal/tools/artifactegress/artifactegress.go" \
    "phase 249 exposes bare names through ParamsFor"

assert_grep_present \
    'TestCompileMapping_OptionalMarkerIsStrippedForSchemaAndRecords' \
    "internal/tools/artifactegress/artifactegress_test.go" \
    "phase 249 unit test pins bare-name projection"
assert_grep_present \
    'TestEncode_OptionalParameters' \
    "internal/tools/artifactegress/artifactegress_test.go" \
    "phase 249 unit test pins missing/nil/empty/non-string/valid cases"
assert_grep_present \
    'TestEncode_ConcurrentReuse_OptionalMapping' \
    "internal/tools/artifactegress/artifactegress_test.go" \
    "phase 249 unit test pins concurrent mapping reuse"
assert_grep_present \
    'TestEgress_OptionalMappedParameterSkipsWhenAbsent' \
    "internal/tools/drivers/mcp/egress_test.go" \
    "phase 249 MCP adapter test pins omitted and supplied optional slots"

assert_grep_present \
    'reference_image_base64_1\?' \
    "examples/harbor.yaml" \
    "phase 249 example config documents optional artifact slots"
assert_grep_present \
    'ends in `\?`' \
    "docs/skills/add-an-in-process-tool/SKILL.md" \
    "phase 249 operator skill documents the optional marker"

smoke_summary
