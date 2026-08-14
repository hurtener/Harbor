#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 248 smoke — Boot-declared resource-free operator skill baseline
# (HA-66, D-427). Shipped (v1.28): pins the shipped contract — the
# config-file-relative strict eager immutable loader before readiness, the
# exact (tenant, boot_agent_id) binding with no invented boot identity, the
# strict merge with the active durable revision into ONE combined operator
# tier (`boot|revision|both` dedupe at same name+hash, differing hash fails,
# exactly 256 unique combined items), the run path from the eager index
# through exact tenant/effective-boot-agent run-start membership into the run
# snapshot (boot set hash, combined hash, per-item provenance frozen), the
# boot-owned mutation/remove guards, the read-only composition preview
# (`boot_pack_set_hash` under authority/reach gating), the single
# prod/devstack loader path, the `EnsureBootAgentLifecycle` separation, and
# headless `RunOnce` failing loud when `boot_agent_packs` is configured. The
# live assertions (a fresh runtime boots to readiness and the resolved boot
# agent's composition preview includes the baseline entries; a non-default
# agent and a foreign tenant do not compose it; a Protocol mutation/removal
# verb refuses a boot-declared name with the canonical typed error; an
# unresolvable default agent fails loud at boot; headless RunOnce fails loud
# when boot_agent_packs is configured) are exercised by the phase's in-package
# suites (internal/runtime/serve/bootpacks_test.go,
# internal/skills/bootpacks/, harbortest/devstack/devstack_bootpack_test.go),
# not duplicated here.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
source scripts/smoke/common.sh
assert_file docs/plans/phase-248-boot-operator-skill-baseline.md "phase 248 plan exists"
assert_grep_present "D-427" docs/decisions.md "D-427 is recorded (HA-66)"
assert_grep_present "Shipped (v1.28)" docs/plans/README.md "phase 248 is Shipped (v1.28) in the master plan"
assert_grep_present "boot_agent_packs" docs/CONFIG.md "the boot baseline config leaf is documented"
assert_grep_present "boot_agent_packs" examples/harbor.yaml "the example config documents the boot baseline"
assert_grep_present "config-file-relative" docs/plans/phase-248-boot-operator-skill-baseline.md "config-file-relative loader is documented"
assert_grep_present "before readiness" docs/plans/phase-248-boot-operator-skill-baseline.md "eager load before readiness is documented"
assert_grep_present "boot-owned" docs/plans/phase-248-boot-operator-skill-baseline.md "boot-owned mutation/remove guards are documented"
assert_grep_present "source=both" docs/plans/phase-248-boot-operator-skill-baseline.md "strict merge dedupes same-name+same-hash as source=both"
assert_grep_present "256" docs/plans/phase-248-boot-operator-skill-baseline.md "exactly 256 unique combined items cap is documented"
assert_grep_present "boot_pack_set_hash" docs/plans/phase-248-boot-operator-skill-baseline.md "preview reports boot|revision|both and boot_pack_set_hash"
assert_grep_present "composition.preview" docs/site/protocol/methods.md "agent_config.composition.preview is a canonical Protocol method"
assert_grep_present "effective-composition" docs/plans/phase-248-boot-operator-skill-baseline.md "one shared strict effective-composition resolver + preview is documented"
assert_grep_present "EnsureBootAgentLifecycle" docs/plans/phase-248-boot-operator-skill-baseline.md "EnsureBootAgentLifecycle separation is stated"
assert_grep_present "RunOnce" docs/plans/phase-248-boot-operator-skill-baseline.md "headless RunOnce unsupported and fail-loud is stated"

# The shipped run path: eager index -> exact tenant/effective-boot-agent
# run-start membership -> one strict combined operator tier -> snapshot with
# boot/combined hashes and per-item provenance frozen. Pin the sentences that
# would die if the loader/preview-only wiring were ever claimed as the whole
# ship.
assert_grep_present "run-start membership" docs/plans/README.md "master plan pins the exact run-start membership binding"
assert_grep_present "combined hash" docs/plans/README.md "master plan pins the combined hash in the run snapshot"
assert_grep_present "provenance" docs/plans/README.md "master plan pins per-item boot|revision|both provenance frozen in the snapshot"
smoke_summary
