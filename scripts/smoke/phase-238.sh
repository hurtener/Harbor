#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
source scripts/smoke/common.sh

# --- Original shipped app-only callback catalog surface (v1.27) ---------------
assert_file docs/plans/phase-238-app-only-callback-catalog.md "phase 238 plan exists"
assert_grep_present "D-412" docs/decisions.md "D-412 is recorded"
assert_grep_present "visibility" docs/plans/phase-238-app-only-callback-catalog.md "App visibility is planned"
assert_grep_present "host-derived" docs/plans/phase-238-app-only-callback-catalog.md "host-derived identity is planned"
assert_grep_present "supported metadata variants" docs/plans/phase-238-app-only-callback-catalog.md "compatibility fixtures are planned"
assert_grep_present "stdio" docs/plans/phase-238-app-only-callback-catalog.md "stdio coverage is planned"
assert_grep_present "Protocol" docs/plans/phase-238-app-only-callback-catalog.md "Protocol contract is planned"

# --- Fresh render-admission governance amendment (v1.28) ----------------------
# The amendment is governance-only: HA-56 stays phase 238 / D-412 with no new
# phase, decision, or HA. Pin the surfaces that record the corrected contract.

# Plan: durable reopen through a real Console consumer + negative matrix.
assert_grep_present "Durable reopen through a real Console consumer" \
    docs/plans/phase-238-app-only-callback-catalog.md \
    "amendment: durable reopen through a real Console consumer is a binding acceptance"
assert_grep_present "wrong/missing identity" \
    docs/plans/phase-238-app-only-callback-catalog.md \
    "amendment: negative identity case is pinned"
assert_grep_present "missing current .ui://. resource" \
    docs/plans/phase-238-app-only-callback-catalog.md \
    "amendment: negative resource-availability case is pinned"
assert_grep_present "stale registry" \
    docs/plans/phase-238-app-only-callback-catalog.md \
    "amendment: negative descriptor-generation case is pinned"
assert_grep_present "expired claim" \
    docs/plans/phase-238-app-only-callback-catalog.md \
    "amendment: negative expiry case is pinned"
assert_grep_present "tampered claim" \
    docs/plans/phase-238-app-only-callback-catalog.md \
    "amendment: negative tamper case is pinned"
assert_grep_present "erased session/agent" \
    docs/plans/phase-238-app-only-callback-catalog.md \
    "amendment: negative erasure case is pinned"
assert_grep_present "paused/disabled server" \
    docs/plans/phase-238-app-only-callback-catalog.md \
    "amendment: negative paused-disabled case is pinned"
assert_grep_present "zero callback executions" \
    docs/plans/phase-238-app-only-callback-catalog.md \
    "amendment: refusal yields zero callback executions"
assert_grep_present "zero originating-tool rerun" \
    docs/plans/phase-238-app-only-callback-catalog.md \
    "amendment: refusal yields zero originating-tool rerun"
assert_grep_present "N≥100 concurrent reopen/isolation" \
    docs/plans/phase-238-app-only-callback-catalog.md \
    "amendment: N≥100 isolation is pinned"
assert_grep_present "shared-KEK" \
    docs/plans/phase-238-app-only-callback-catalog.md \
    "amendment: shared-KEK admission is pinned"
assert_grep_present "fails readiness loud" \
    docs/plans/phase-238-app-only-callback-catalog.md \
    "amendment: readiness-loud on missing shared sealing authority is pinned"
assert_grep_present "ResolveAppTool" \
    docs/plans/phase-238-app-only-callback-catalog.md \
    "amendment: same-server ResolveAppTool dispatch is pinned"
assert_grep_present "no new HA, phase, or decision" \
    docs/plans/phase-238-app-only-callback-catalog.md \
    "amendment: no new HA / phase / decision is allocated"

# Decision: D-412 carries the amendment record in place.
assert_grep_present "corrected fresh render-admission contract" \
    docs/decisions.md \
    "amendment: D-412 amendment record exists"
assert_grep_present "Pending v1.28" \
    docs/decisions.md \
    "amendment: D-412 amendment is marked Pending v1.28"
assert_grep_present "no transcript impersonation" \
    docs/decisions.md \
    "amendment: transcript-impersonation non-goal is pinned"

# RFC: the host-approved orientation section (§6.10) records the contract.
assert_grep_present "Fresh render-admission contract" \
    RFC-001-Harbor.md \
    "amendment: RFC §6.10 records the fresh render-admission contract"

# Master plan + register: status flips on both surfaces.
assert_grep_present "fresh render-admission amendment Pending \(v1.28\)" \
    docs/plans/README.md \
    "amendment: master-plan index row + detail block mark the amendment Pending (v1.28)"
assert_grep_present "fresh render-admission amendment Pending \(v1.28\)" \
    docs/notes/downstream-asks.md \
    "amendment: HA-56 register row marks the amendment Pending (v1.28)"

# Glossary: the new term lands beside the retained App dispatch catalog entry.
assert_grep_present "Fresh render admission" \
    docs/glossary.md \
    "amendment: glossary records the Fresh render admission term"

# Operator skill: the Playground render surface documents both paths.
assert_grep_present "Fresh render admission" \
    docs/skills/drive-the-playground/SKILL.md \
    "amendment: drive-the-playground skill records the fresh render-admission reopen contract"

# Config/example docs: enabled surface without a shared sealing authority
# fails readiness loud; restart/multi-replica use the shared authority.
assert_grep_present "shared sealing authority" \
    docs/CONFIG.md \
    "amendment: CONFIG.md documents the shared sealing authority consequence"
assert_grep_present "shared sealing authority" \
    examples/harbor.yaml \
    "amendment: example config documents the shared sealing authority consequence"

smoke_summary
