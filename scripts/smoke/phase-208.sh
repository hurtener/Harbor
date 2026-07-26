#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 208 smoke — the reconciled artifact read key (D-352).
#
# The phase has NO HTTP / Protocol surface: it narrows the ArtifactStore
# read key onto the isolation triple across all five drivers. So this
# script has two halves.
#
#   1. The behavioural gate: the artifacts package suite (which carries
#      the conformance rows every driver runs) plus the cross-subsystem
#      integration test, both under -race.
#   2. Static guards on the load-bearing declarations. Each one is
#      written so that breaking the thing it guards turns it into a FAIL
#      rather than a SKIP (AGENTS.md §4.2 item 5) — the mutation sweep is
#      recorded in the phase PR.
#
# Portability note (both traps cost this project a release):
#   - `\t` / `\d` are NOT used inside any grep -E pattern. BSD grep
#     matches them, GNU grep does not, so such a guard is silently inert
#     on Linux CI. `[[:space:]]` / `[[:digit:]]` only.
#   - `go test -race` is run WITHOUT CGO_ENABLED=0: the race detector
#     needs cgo on Linux and that combination fails to build.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ---------------------------------------------------------------------------
# 1. Behavioural gate
# ---------------------------------------------------------------------------

if go test -race -count=1 -timeout 300s ./internal/artifacts/... >/dev/null 2>&1; then
    ok 'phase 208: internal/artifacts passes under -race (conformance across inmem/fs/sqlite; postgres+s3 self-skip without a backend)'
else
    fail 'phase 208: internal/artifacts failed (run `go test -race ./internal/artifacts/...` for detail)'
fi

if go test -race -count=1 -timeout 300s -run 'TestE2E_ArtifactReadKey' ./test/integration/ >/dev/null 2>&1; then
    ok 'phase 208: TestE2E_ArtifactReadKey_* passes — the session-artifact manifest lists only refs artifact_fetch resolves'
else
    fail 'phase 208: TestE2E_ArtifactReadKey_* failed (run `go test -race -run TestE2E_ArtifactReadKey ./test/integration/`)'
fi

# ---------------------------------------------------------------------------
# 2. Static guards — the read key
#
# A single-row read/write/delete must not carry a `task` predicate. These
# are absence checks over the SQL text, which is why they name the whole
# clause: a partial pattern would match the List builder's conditional
# `task = ?` and report a false FAIL.
# ---------------------------------------------------------------------------

assert_grep_absent 'session = \? AND task = \? AND id = \?' \
    internal/artifacts/drivers/sqlite/sqlite.go \
    'phase 208: sqlite single-row clauses key on the triple, not the task'

assert_grep_absent 'session = \$3 AND task = \$4 AND id = \$5' \
    internal/artifacts/drivers/postgres/postgres.go \
    'phase 208: postgres single-row clauses key on the triple, not the task'

assert_grep_present 'WHERE tenant = \? AND user = \? AND session = \? AND id = \?' \
    internal/artifacts/drivers/sqlite/sqlite.go \
    'phase 208: sqlite reads resolve on (tenant, user, session, id)'

assert_grep_present 'WHERE tenant = \$1 AND "user" = \$2 AND session = \$3 AND id = \$4' \
    internal/artifacts/drivers/postgres/postgres.go \
    'phase 208: postgres reads resolve on (tenant, user, session, id)'

# The write/dedup key narrows with the read key, or a re-Put under a
# differing task stores a second row and the collapse property is a lie.
assert_grep_present 'ON CONFLICT\(tenant, user, session, namespace, id\) DO NOTHING' \
    internal/artifacts/drivers/sqlite/sqlite.go \
    'phase 208: sqlite dedups on the triple + namespace + id'

assert_grep_present 'ON CONFLICT \(tenant, "user", session, namespace, id\) DO NOTHING' \
    internal/artifacts/drivers/postgres/postgres.go \
    'phase 208: postgres dedups on the triple + namespace + id'

# The two index-keyed drivers must not carry a Task field on their
# composite key. `[[:space:]]` rather than a tab escape — see the header.
assert_grep_absent '^[[:space:]]+Task[[:space:]]+string$' \
    internal/artifacts/drivers/inmem/inmem.go \
    'phase 208: inmem indexKey carries no Task field'

assert_grep_absent '^[[:space:]]+Task[[:space:]]+string$' \
    internal/artifacts/drivers/fs/fs.go \
    'phase 208: fs indexKey carries no Task field'

# The object-store key is the read key: a task segment there is what let
# N concurrent writers of identical bytes create N objects.
assert_grep_absent 'scope.TenantID, scope.UserID, scope.SessionID, task, namespace, id' \
    internal/artifacts/drivers/s3/s3.go \
    'phase 208: the s3 object key folds in no task segment'

assert_grep_present 'parts = append\(parts, scope.TenantID, scope.UserID, scope.SessionID, namespace, id\)' \
    internal/artifacts/drivers/s3/s3.go \
    'phase 208: the s3 object key is the triple + namespace + id'

# ---------------------------------------------------------------------------
# 3. Static guards — the facade and the List precondition
# ---------------------------------------------------------------------------

assert_grep_present 'ref.Scope.EqualTriple\(s.scope\)' \
    internal/artifacts/scoped.go \
    'phase 208: ScopedArtifacts.GetRef compares the isolation triple, not the whole scope'

assert_grep_present 's.store.List\(ctx, s.scope.Triple\(\)\)' \
    internal/artifacts/scoped.go \
    'phase 208: ScopedArtifacts.List filters on the triple, so listing and reading agree'

# Every driver validates List's tenant precondition — it was the one
# ArtifactStore method no driver validated.
for driver in inmem fs sqlite postgres s3; do
    assert_grep_present 'filter.ValidateFilter\(\)' \
        "internal/artifacts/drivers/${driver}/${driver}.go" \
        "phase 208: ${driver}.List validates the filter's tenant"
done

assert_grep_present 'func \(s ArtifactScope\) ValidateFilter\(\) error' \
    internal/artifacts/artifacts.go \
    'phase 208: ArtifactScope.ValidateFilter is declared'

assert_grep_present 'func \(s ArtifactScope\) EqualTriple\(other ArtifactScope\) bool' \
    internal/artifacts/artifacts.go \
    'phase 208: ArtifactScope.EqualTriple is declared'

# ---------------------------------------------------------------------------
# 4. Static guards — the forward-only migrations
# ---------------------------------------------------------------------------

assert_file internal/artifacts/drivers/sqlite/migrations/0002_read_key_is_the_triple.sql \
    'phase 208: sqlite migration 0002 exists'
assert_file internal/artifacts/drivers/postgres/migrations/0002_read_key_is_the_triple.sql \
    'phase 208: postgres migration 0002 exists'

assert_grep_present 'PRIMARY KEY \(tenant, user, session, namespace, id\)' \
    internal/artifacts/drivers/sqlite/migrations/0002_read_key_is_the_triple.sql \
    'phase 208: sqlite migration re-keys the table onto the triple'

assert_grep_present 'PRIMARY KEY \(tenant, "user", session, namespace, id\)' \
    internal/artifacts/drivers/postgres/migrations/0002_read_key_is_the_triple.sql \
    'phase 208: postgres migration re-keys the table onto the triple'

# 0001 is merged and therefore frozen (AGENTS.md §13, append-only).
assert_grep_present 'PRIMARY KEY \(tenant, user, session, task, namespace, id\)' \
    internal/artifacts/drivers/sqlite/migrations/0001_init.sql \
    'phase 208: sqlite migration 0001 is unedited (append-only migrations)'

# ---------------------------------------------------------------------------
# 5. Static guards — the honest consequences are stated where a reader meets
#    them, not only in the decision log.
# ---------------------------------------------------------------------------

assert_grep_present 'THE TaskID FILTER IS LOSSY' \
    internal/artifacts/artifacts.go \
    'phase 208: the first-writer property is stated AT the List filter'

assert_grep_present 'PROVENANCE ANNOTATION' \
    internal/artifacts/artifacts.go \
    'phase 208: TaskID is documented as a provenance annotation on the store surface'

smoke_summary
