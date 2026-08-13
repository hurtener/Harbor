#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
source scripts/smoke/common.sh
assert_file docs/plans/phase-240-virtual-child-profiles.md "phase 240 plan exists"
assert_grep_present "D-414" docs/decisions.md "D-414 is recorded"
assert_grep_present "read-only" docs/plans/phase-240-virtual-child-profiles.md "child profile is read-only"
assert_grep_present "server-derived" docs/plans/phase-240-virtual-child-profiles.md "profile authority is server-derived"
assert_grep_present "parent" docs/plans/phase-240-virtual-child-profiles.md "parent profile is explicit"
assert_grep_present "Protocol" docs/plans/phase-240-virtual-child-profiles.md "Protocol contract is planned"
smoke_summary
