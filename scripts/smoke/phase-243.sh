#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 243 smoke — Verified-caller two-phase skill-package import (HA-61,
# D-422). Shipped (v1.28): this smoke pins the shipped surface's contracts —
# the two canonical methods, the STATELESS sealed-token validate contract
# (ZERO writes of any kind, including zero proposal-ledger writes), the
# commit-phase durable idempotency, the one conditional package write, the
# mandatory skillpkg:// resolver, and the typed refusal codes — so a
# regression that reverts the accepted contract fails the gate. The live
# assertions (upload -> validate -> no skill before commit -> commit -> reopen
# -> skill survives staging cleanup) are exercised by the phase's in-package
# integration suite
# (internal/runtime/agentcfg/protocol/userskillimport_test.go), not duplicated
# here.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
source scripts/smoke/common.sh
assert_file docs/plans/phase-243-consumer-skill-package-import.md "phase 243 plan exists"
assert_grep_present "D-422" docs/decisions.md "D-422 is recorded (HA-61)"
assert_grep_present "Shipped (v1.28)" docs/plans/README.md "phase 243 is Shipped (v1.28) in the master plan"
assert_grep_present "import_validate" docs/site/protocol/methods.md "import_validate is a canonical Protocol method"
assert_grep_present "import_commit" docs/site/protocol/methods.md "import_commit is a canonical Protocol method"

# Stateless validation: validate performs ZERO writes of any kind — no
# SkillStore body/package write, no agent-config membership write, and no
# StateStore proposal-ledger write — and returns a bounded opaque versioned
# sealed proposal token. Pin the exact contract sentences across the owned
# surfaces so the guard dies if the accepted contract is diluted.
assert_grep_present "ZERO writes of any kind" docs/plans/phase-243-consumer-skill-package-import.md "plan pins the zero-write validate contract"
assert_grep_present "proposal-ledger write" docs/plans/phase-243-consumer-skill-package-import.md "plan pins zero proposal-ledger writes"
assert_grep_present "proposal-ledger write" docs/plans/README.md "master plan pins zero proposal-ledger writes"
assert_grep_present "proposal-ledger write" docs/decisions.md "D-422 pins zero proposal-ledger writes"
assert_grep_present "sealed proposal token" docs/plans/phase-243-consumer-skill-package-import.md "plan names the bounded opaque versioned sealed proposal token"
assert_grep_present "proposal_token" docs/site/protocol/types.md "the wire carries the sealed proposal_token"
assert_grep_present "only in the commit phase" docs/plans/phase-243-consumer-skill-package-import.md "durable idempotency begins only in the commit phase"
assert_grep_present "ONE conditional" docs/decisions.md "D-422 pins the one conditional package write"

# The durable personal-skill package surface: PackageHash, the mandatory
# skillpkg:// resolver, forced ScopeUser + effective agent.
assert_grep_present "PackageHash" docs/plans/phase-243-consumer-skill-package-import.md "versioned PackageHash binds the review"
assert_grep_present "skillpkg://" docs/plans/phase-243-consumer-skill-package-import.md "durable skillpkg reference is planned"
assert_grep_present "ScopeUser" docs/plans/phase-243-consumer-skill-package-import.md "server-forced ScopeUser/owner is planned"

# Typed refusals on the canonical error registry.
assert_grep_present "skill_import_package_invalid" docs/site/protocol/errors.md "invalid-package refusal is canonical"
assert_grep_present "skill_import_proposal_expired" docs/site/protocol/errors.md "expired-token refusal is canonical"
assert_grep_present "skill_import_proposal_invalid" docs/site/protocol/errors.md "invalid-token refusal is canonical"
assert_grep_present "skill_import_replace_required" docs/site/protocol/errors.md "replace-consent refusal is canonical"

# Negative matrix: cross-session token use, traversal archives, forbidden
# origin pairs, and implicit replacement fail typed with no mutation.
assert_grep_present "cross-session token use" docs/plans/phase-243-consumer-skill-package-import.md "cross-session token use fails (no mutation)"
assert_grep_present "traversal" docs/plans/phase-243-consumer-skill-package-import.md "traversal archives are refused"
assert_grep_present "no mutation" docs/plans/phase-243-consumer-skill-package-import.md "refused pairs leave no mutation"

# The glossary records the stateless sealed-token contract, not the stale
# durable-proposal wording.
assert_grep_present "sealed proposal token" docs/glossary.md "glossary records the stateless reviewed skill-package proposal"
smoke_summary
