#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
source scripts/smoke/common.sh
assert_file docs/plans/phase-239-same-run-step-tranche-resume.md "phase 239 plan exists"
assert_grep_present "D-413" docs/decisions.md "D-413 is recorded"
assert_grep_present "same-run" docs/plans/phase-239-same-run-step-tranche-resume.md "same-run resume is planned"
assert_grep_present "tranche" docs/plans/phase-239-same-run-step-tranche-resume.md "step tranche is planned"
assert_grep_present "idempotent" docs/plans/phase-239-same-run-step-tranche-resume.md "resume idempotence is planned"
assert_grep_present "Protocol" docs/plans/phase-239-same-run-step-tranche-resume.md "Protocol contract is planned"
smoke_summary
