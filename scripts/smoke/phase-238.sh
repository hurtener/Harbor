#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
source scripts/smoke/common.sh
assert_file docs/plans/phase-238-app-only-callback-catalog.md "phase 238 plan exists"
assert_grep_present "D-412" docs/decisions.md "D-412 is recorded"
assert_grep_present "visibility" docs/plans/phase-238-app-only-callback-catalog.md "App visibility is planned"
assert_grep_present "host-derived" docs/plans/phase-238-app-only-callback-catalog.md "host-derived identity is planned"
assert_grep_present "supported metadata variants" docs/plans/phase-238-app-only-callback-catalog.md "compatibility fixtures are planned"
assert_grep_present "stdio" docs/plans/phase-238-app-only-callback-catalog.md "stdio coverage is planned"
assert_grep_present "Protocol" docs/plans/phase-238-app-only-callback-catalog.md "Protocol contract is planned"
smoke_summary
