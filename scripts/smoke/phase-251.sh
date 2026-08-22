#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 251 smoke — v1.29.1 event-index and PostgreSQL fleet safety (HA-69).
# This planning skeleton only checks governance artifacts. Implementation
# assertions are added when the phase surface lands; local preflight is
# intentionally deferred to hosted CI for the emergency release.
#
# The script remains static-only; it never contacts a live server.
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.
#
# Classification (D-104 — the `# PREFLIGHT_REQUIRES:` header above):
#   - static-only — pure file/text greps, golden compares, file-existence
#     assertions. Runs in the parallel batch BEFORE the dev server boots.
#   - live-server — hits the booted dev server over HTTP (`api_url`,
#     `assert_status`, `skip_if_404`, `assert_json_path`) or reads the
#     preflight server log. Runs serially against the booted instance.
#   - unit-tests — runs `go test` for one or more packages. Parallelisable;
#     `go test` schedules its own internal parallelism.
#
# Pick `live-server` whenever the smoke depends on `HARBOR_BIND` /
# `HARBOR_BASE_URL` / `HARBOR_DEV_TOKEN` / `${HARBOR_DATA_DIR}/server.log`
# or invokes the built `bin/harbor` against a network endpoint. When in
# doubt, `live-server` is the safe default — misclassifying a
# server-touching smoke as `static-only` produces nondeterministic flakes.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Governance assertions remain useful before implementation and avoid
# claiming that the runtime surfaces have landed.
assert_file \
    "docs/plans/phase-251-v1291-events-postgres-fleet-safety.md" \
    "phase 251 plan exists"
assert_grep_present \
    'HA-69' \
    "docs/notes/downstream-asks.md" \
    "HA-69 is registered"
assert_grep_present \
    '## D-431' \
    "docs/decisions.md" \
    "D-431 decision exists"
assert_grep_present \
    'postgres\.pool\.max_open' \
    "docs/plans/phase-251-v1291-events-postgres-fleet-safety.md" \
    "aggregate pool configuration is bound"
assert_grep_present \
    'max_connections=103' \
    "docs/plans/phase-251-v1291-events-postgres-fleet-safety.md" \
    "Basic-4GB connection cap is documented"
# ----------------------------------------------------------------------------

skip "phase 251: implementation smoke assertions are pending; hosted CI is the broad gate"

smoke_summary
