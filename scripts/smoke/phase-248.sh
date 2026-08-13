#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 248 smoke — Boot-declared resource-free operator skill baseline
# (HA-66).
#
# PENDING STATIC SKELETON. Phase 248 is Planned (master-plan row 248 carries
# Status `Pending`), so this smoke records the plan + decision and pins the
# load-bearing plan contracts. It does NOT claim the surface is implemented:
# there is no live-server leg and no "surface works" assertion. When the
# phase ships, the implementor extends this script with the live assertions
# from the plan's "Smoke script additions" section (boot a runtime with a
# declared baseline and assert the resolved boot agent's composition preview
# includes the baseline entries; a non-default agent and a foreign tenant do
# not compose it; a Protocol mutation/removal verb refuses a boot-declared
# name with the canonical typed error; an unresolvable default agent fails
# loud at boot).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
source scripts/smoke/common.sh
assert_file docs/plans/phase-248-boot-operator-skill-baseline.md "phase 248 plan exists"
assert_grep_present "D-427" docs/decisions.md "D-427 is recorded (HA-66)"
assert_grep_present "Pending" docs/plans/README.md "phase 248 is Planned/Pending in the master plan"
assert_grep_present "config-file-relative" docs/plans/phase-248-boot-operator-skill-baseline.md "config-file-relative loader is planned"
assert_grep_present "before readiness" docs/plans/phase-248-boot-operator-skill-baseline.md "eager load before readiness is planned"
assert_grep_present "boot-owned" docs/plans/phase-248-boot-operator-skill-baseline.md "boot-owned mutation/remove guards are planned"
assert_grep_present "effective-composition" docs/plans/phase-248-boot-operator-skill-baseline.md "one shared effective-composition resolver + preview is planned"
assert_grep_present "EnsureBootAgentLifecycle" docs/plans/phase-248-boot-operator-skill-baseline.md "EnsureBootAgentLifecycle separation is stated"
assert_grep_present "RunOnce" docs/plans/phase-248-boot-operator-skill-baseline.md "explicit RunOnce/embed support decision is stated"
smoke_summary
