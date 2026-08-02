#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_grep_present 'Empty/invalid fields, duplicate tenants, and an' \
    'docs/plans/phase-233a-durable-session-overlay-personal-skills.md' \
    'phase 233a pins fail-loud static cutover validation'
assert_grep_present '__session_personal_cutover__' \
    'RFC-001-Harbor.md' \
    'phase 233a pins the reserved cutover control scope'
smoke_summary
