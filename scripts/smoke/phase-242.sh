#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
source scripts/smoke/common.sh
assert_file docs/plans/phase-242-task-progress.md "phase 242 plan exists"
assert_grep_present "D-416" docs/decisions.md "D-416 is recorded"
assert_grep_present "task progress" docs/plans/phase-242-task-progress.md "task progress is explicit"
assert_grep_present "tranche" docs/plans/phase-242-task-progress.md "tranche progress is planned"
assert_grep_present "cross-session isolation" docs/plans/phase-242-task-progress.md "cross-session isolation is planned"
assert_grep_present "Protocol" docs/plans/phase-242-task-progress.md "Protocol contract is planned"
smoke_summary
