#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
source scripts/smoke/common.sh
assert_file docs/plans/phase-241-artifact-output-forwarding.md "phase 241 plan exists"
assert_grep_present "D-415" docs/decisions.md "D-415 is recorded"
assert_grep_present "artifact" docs/plans/phase-241-artifact-output-forwarding.md "artifact forwarding is planned"
assert_grep_present "by reference" docs/plans/phase-241-artifact-output-forwarding.md "reference forwarding is planned"
assert_grep_present "provenance" docs/plans/phase-241-artifact-output-forwarding.md "provenance is planned"
assert_grep_present "Protocol" docs/plans/phase-241-artifact-output-forwarding.md "Protocol contract is planned"
smoke_summary
