#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 243 smoke — Consumer-scoped two-phase skill-package import (HA-61).
#
# PENDING STATIC SKELETON. Phase 243 is Planned (master-plan row 243 carries
# Status `Pending`), so this smoke records the plan + decision and pins the
# load-bearing plan contracts. It does NOT claim the surface is implemented:
# there is no live-server leg and no "surface works" assertion. When the
# phase ships, the implementor extends this script with the live assertions
# from the plan's "Smoke script additions" section (upload + validate a
# same-scope fixture, assert no skill before commit, commit exact reviewed
# hashes, open a new session, assert the package survives staging cleanup;
# stale PackageHash/reach/lifecycle, cross-session proposal use, traversal
# archives, forbidden origin pairs, and implicit replacement fail typed).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
source scripts/smoke/common.sh
assert_file docs/plans/phase-243-consumer-skill-package-import.md "phase 243 plan exists"
assert_grep_present "D-422" docs/decisions.md "D-422 is recorded (HA-61)"
assert_grep_present "Pending" docs/plans/README.md "phase 243 is Planned/Pending in the master plan"
assert_grep_present "import_validate" docs/plans/phase-243-consumer-skill-package-import.md "validate/commit two-phase import is planned"
assert_grep_present "PackageHash" docs/plans/phase-243-consumer-skill-package-import.md "versioned PackageHash is planned"
assert_grep_present "skillpkg://" docs/plans/phase-243-consumer-skill-package-import.md "durable skillpkg reference is planned"
assert_grep_present "ScopeUser" docs/plans/phase-243-consumer-skill-package-import.md "server-forced ScopeUser/owner is planned"
smoke_summary
