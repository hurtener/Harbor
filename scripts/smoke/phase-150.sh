#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 150 smoke — run-completion hook: transcript egress through the
# tool catalog (D-280). docs/plans/phase-150-run-completion-hook.md
#
# Assertions:
#   1. go test -race for internal/runtime/steering (completion-hook fire on
#      every terminal outcome, transcript assembly ordering golden,
#      run.hook_dispatched / run.hook_failed events, D-274 counters untouched
#      by a hook dispatch, N>=10 concurrent no-bleed).
#   2. go test -race for internal/agentcfg + the run-start projection (hooks
#      section revision round-trip, diff arm, section-merge preservation,
#      agentcfg-over-yaml precedence).
#   3. go test -race for internal/config (runtime.hooks validation).
#   4. go test -race for the phase 150 E2E (test/integration/ — hook receives
#      the full ordered transcript incl. a steering-injected mid-run user
#      message; identity at the receiving tool; hook error leaves the run
#      outcome unchanged; cancelled run still fires with outcome=cancelled),
#      with a `go test -list` no-match-fails guard.
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 -> SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh -- don't roll new curl wrappers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

TMPDIR="$(mktemp -d -t harbor-phase150-XXXXXX)"
trap 'rm -rf "${TMPDIR}"' EXIT

# --- 1. steering -- the run-completion hook surface -------------------------

assert_file internal/runtime/steering/completion.go 'phase 150: the run-completion hook implementation exists'
assert_grep_present 'TestRun_CompletionHook_ConcurrentNoBleed' \
    internal/runtime/steering/completion_test.go \
    'phase 150: the concurrent no-bleed hook test exists'
assert_grep_present 'TestBuildRunCompletionPayload_Golden' \
    internal/runtime/steering/completion_test.go \
    'phase 150: the golden payload test exists'

steering_log="${TMPDIR}/steering-test.log"
if go test -race -count=1 -timeout 180s ./internal/runtime/steering/ >"${steering_log}" 2>&1; then
    ok 'phase 150: internal/runtime/steering tests pass under -race (fire-on-all-terminals, golden, events, D-274, no-bleed)'
else
    fail 'phase 150: internal/runtime/steering tests FAILED under -race (see tail)'
    tail -40 "${steering_log}" | sed 's/^/    /'
fi

# --- 2. agentcfg hooks section + the run-start projection precedence --------

agentcfg_log="${TMPDIR}/agentcfg-test.log"
if go test -race -count=1 -timeout 180s ./internal/agentcfg/... ./internal/runtime/agentcfg/... >"${agentcfg_log}" 2>&1; then
    ok 'phase 150: agentcfg hooks section + projection tests pass (round-trip, diff arm, agentcfg-over-yaml precedence)'
else
    fail 'phase 150: agentcfg / projection hooks tests FAILED (see tail)'
    tail -40 "${agentcfg_log}" | sed 's/^/    /'
fi

# --- 3. config -- runtime.hooks validation ----------------------------------

config_log="${TMPDIR}/config-test.log"
if go test -race -count=1 -timeout 120s -run 'RunCompletionHook|TableDriven' ./internal/config/ >"${config_log}" 2>&1; then
    ok 'phase 150: internal/config runtime.hooks validation tests pass under -race'
else
    fail 'phase 150: internal/config runtime.hooks validation tests FAILED (see tail)'
    tail -40 "${config_log}" | sed 's/^/    /'
fi

# --- 4. the primitive-with-consumer integration E2E -------------------------

assert_file test/integration/phase150_run_completion_hook_test.go 'phase 150: the run-completion-hook E2E test exists'

# no-match-fails guard: the E2E tests MUST exist (a silently-renamed test
# would make the run below a vacuous pass). Capture to a variable first --
# piping into `grep -q` closes the pipe early and, under `set -o pipefail`,
# the SIGPIPE-d `go test` would flip the pipeline exit code.
listed_e2e="$(go test -list 'TestE2E_Phase150' ./test/integration/... 2>/dev/null || true)"
if printf '%s\n' "${listed_e2e}" | grep -q 'TestE2E_Phase150'; then
    ok 'phase 150: TestE2E_Phase150* integration tests are present (no-match-fails guard)'
else
    fail 'phase 150: no TestE2E_Phase150* tests found -- the E2E guard would be vacuous'
fi

e2e_log="${TMPDIR}/e2e-test.log"
if go test -race -count=1 -timeout 240s -run 'TestE2E_Phase150' ./test/integration/... >"${e2e_log}" 2>&1; then
    ok 'phase 150: run-completion-hook E2E passes under -race (ordered transcript + mid-run steering, identity at the sink, hook-error + cancelled legs, no-bleed)'
else
    fail 'phase 150: run-completion-hook E2E FAILED under -race (see tail)'
    tail -40 "${e2e_log}" | sed 's/^/    /'
fi

# --- 5. dependency-direction + contract greps -------------------------------

# The hook dispatches through the ToolExecutor seam -- steering never imports a
# concrete tool driver (the egress is the catalog, resolved via the interface).
assert_grep_absent '"github.com/hurtener/Harbor/internal/tools/drivers' \
    internal/runtime/steering/completion.go \
    'phase 150: steering completion.go imports no concrete tool driver (egress via the ToolExecutor interface)'

# The payload format version is pinned (a golden public contract).
assert_grep_present 'RunCompletionPayloadFormatVersion = 1' \
    internal/runtime/steering/completion.go \
    'phase 150: the RunCompletionPayload format_version is pinned to 1'

# Both run-loop driver twins resolve the hook via the ONE shared projection.
assert_grep_present 'projection.ActiveRunCompletionHook' \
    cmd/harbor/cmd_dev_runloop.go \
    'phase 150: the production driver resolves the hook via the shared projection'
assert_grep_present 'projection.ActiveRunCompletionHook' \
    harbortest/devstack/devstack.go \
    'phase 150: the devstack twin resolves the hook via the shared projection (17.6 twin discipline)'

# The EMBED run type is covered too: RunOnce + the devstack twin apply the
# SAME shared yaml projection (the silently-uncovered-embed gap the review
# pinned must never regress), with the RunOption override + its tests.
assert_grep_present 'projection.RunCompletionHookFromConfig' \
    internal/runtime/assemble/runonce.go \
    'phase 150: Stack.RunOnce (the embed run type) resolves the yaml hook via the shared projection'
assert_grep_present 'projection.RunCompletionHookFromConfig' \
    harbortest/devstack/devstack.go \
    'phase 150: the devstack twin applies the shared yaml hook projection'
assert_grep_present 'func WithCompletionHook' \
    internal/runtime/assemble/runonce.go \
    'phase 150: the WithCompletionHook RunOption exists (embed per-call override / explicit-nil disable)'

embed_log="${TMPDIR}/embed-test.log"
if go test -race -count=1 -timeout 180s -run 'TestRunOnce_CompletionHook' ./internal/runtime/assemble/ >"${embed_log}" 2>&1; then
    ok 'phase 150: embed RunOnce hook tests pass under -race (yaml fires, option overrides, nil disables, no-hook byte-identical)'
else
    fail 'phase 150: embed RunOnce hook tests FAILED under -race (see tail)'
    tail -40 "${embed_log}" | sed 's/^/    /'
fi

smoke_summary
