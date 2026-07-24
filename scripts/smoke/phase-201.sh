#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 201 — Skills Postgres driver (durable/shared storage).
#
# When the surface lands, assert (static):
#   - internal/skills/drivers/postgres/ exists and implements SkillStore.
#   - its blank import is present in internal/drivers/prod/prod.go (next to localdb).
# The behavioral parity gate is the shared internal/skills/conformancetest suite
# run against a real Postgres in CI, not this smoke.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 201: skills postgres driver — assertions land with the implementation"

smoke_summary
