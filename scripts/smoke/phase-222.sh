#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 222 — `ExtraSystemBlocks` on the agent-config payload (D-367).
#
# The phase adds a durable, per-agent, ORDERED list of named prompt blocks to
# the agent-config payload, with its own admin verb, so N independent
# capability sources can each contribute — and later remove — exactly their
# own text without re-deriving the whole composition.
#
# Asserts:
#   - static: the wire types exist (the PHASE GATE the live legs branch on);
#   - static: the carrier is a SLICE and no map appears on the blocks path —
#     ordering is deterministic BY CONSTRUCTION, not by a sort;
#   - static: the normalizer does NOT sort blocks. This is the deliberate
#     asymmetry with Skills.Names (sortDedup) and OAuthProviders (sorted by
#     name): for those, order is not semantic and a re-ordering must NOT
#     perturb the content hash; for blocks, order IS the render order, so a
#     re-ordering MUST be a new revision. Adding a sort here would silently
#     re-order an operator's prompt;
#   - static: the verb is in BOTH the canonical and the ADMIN method sets.
#     This is the guard the whole verbatim-render decision rests on — the
#     blocks are unescaped precisely because only the admin tier writes them;
#   - static: the lower (claim-free) session verb still writes only
#     PromptLayers and cannot reach the section;
#   - static: the renderer does NOT pass block bodies through
#     escapeUntrustedSection, and renders them inside buildAdditionalGuidance;
#   - static: the single-source + Console hand-mirror lockstep (D-223);
#   - live: a two-block write round-trips IN THE WRITTEN ORDER; a duplicate
#     name and a malformed name are refused; the section-preservation
#     invariant holds in BOTH directions against set_prompt_layers; identity
#     stays mandatory;
#   - unit-tests: the normalizer + hash asymmetry, the write door + the
#     all-direction preservation matrix, the projection, the verbatim render +
#     byte-identity + determinism, and the integration seam — all under -race.
#
# Every non-probe assertion FAILS (never SKIPs) when its guard is removed. The
# only SKIP arms describe the HARNESS, not the surface: no reachable server, no
# bearer/jq/curl, no go toolchain. In particular the guard-test legs name each
# test EXPLICITLY through `assert_go_tests_pass`, so a renamed or deleted test
# is a FAIL — `go test -run` whose filter matches nothing exits ZERO.
#
# The agent id is per-invocation, so the script is idempotent against a
# long-lived preflight server.
#
# Done-definition: OK >= 12, FAIL = 0.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

WIRE_GO='internal/protocol/types/agentconfig.go'
DOMAIN_GO='internal/agentcfg/agentcfg.go'
METHODS_GO='internal/protocol/methods/methods.go'
SINGLESOURCE_GO='internal/protocol/singlesource/singlesource.go'
USER_GO='internal/runtime/agentcfg/protocol/user.go'
PROMPT_GO='internal/planner/react/prompt.go'
AGENTCFG_TS='web/console/src/lib/protocol/agentconfig.ts'

# assert_grep <file> <extended-regexp> <desc>
#
# OK when the pattern matches, FAIL when it does not. Deliberately NOT a skip:
# the 404/405/501 -> SKIP convention is for a forward-phase script running
# against an older build's HTTP surface, not for source guards. Patterns use
# POSIX classes ([[:space:]]) — never \t / \d, which BSD grep matches and GNU
# grep does not.
assert_grep() {
    local file="$1" pattern="$2" desc="$3"
    if [ ! -f "${file}" ]; then
        fail "${desc}: ${file} does not exist"
        return
    fi
    if grep -qE "${pattern}" "${file}"; then
        ok "${desc}"
    else
        fail "${desc}: no match for /${pattern}/ in ${file}"
    fi
}

# assert_not_grep <file> <extended-regexp> <desc>
assert_not_grep() {
    local file="$1" pattern="$2" desc="$3"
    if [ ! -f "${file}" ]; then
        fail "${desc}: ${file} does not exist"
        return
    fi
    if grep -qE "${pattern}" "${file}"; then
        fail "${desc}: unexpected match for /${pattern}/ in ${file}"
    else
        ok "${desc}"
    fi
}

# ----------------------------------------------------------------------------
# 1. The phase gate: are the wire types declared?
# ----------------------------------------------------------------------------

# The phase has LANDED, so this is a FAIL arm, not a phase gate. A skeleton
# SKIP here would fire on the feature's ABSENCE and report green — exactly the
# instrument the wave's Gate-0 found four of. Every downstream guard below is
# unconditional for the same reason.
assert_grep "${WIRE_GO}" \
    'type AgentConfigExtraSystemBlocks struct' \
    'phase 222: the agent-config payload declares the AgentConfigExtraSystemBlocks section'

# ----------------------------------------------------------------------------
# 2. Ordering is deterministic BY CONSTRUCTION.
# ----------------------------------------------------------------------------

assert_grep "${WIRE_GO}" \
    'Blocks[[:space:]]+\[\]AgentConfigNamedBlock' \
    'phase 222: the wire carrier is an ORDERED SLICE (declared order is render order)'

assert_grep "${DOMAIN_GO}" \
    'Blocks[[:space:]]+\[\]NamedBlock' \
    'phase 222: the domain carrier is an ORDERED SLICE too'

# No map may appear on the blocks path. A map[string]string keyed by name is
# the nondeterminism defect phase 217 removed elsewhere; it must not return
# here under a different name.
#
# `.` and not `[^\n]`: in an ERE a bracket expression has no C escapes, so
# `[^\n]` is the class "neither a backslash nor the letter n" — which would
# refuse to match `ExtraSystemBlocks map[string]string` (the `n` in `string`).
# grep is line-oriented, so plain `.` already cannot cross a newline.
assert_not_grep "${WIRE_GO}" \
    'ExtraSystemBlocks.*map\[' \
    'phase 222: no map carries the blocks on the wire (map iteration order is not a composition order)'

assert_not_grep "${DOMAIN_GO}" \
    'ExtraSystemBlocks.*map\[' \
    'phase 222: no map carries the blocks in the domain type'

# THE MUST-NOT-SORT GUARD. Skills.Names is sortDedup'd and OAuthProviders is
# sorted by name, both because a re-ordering of a SET must not perturb the
# content hash. Blocks are the opposite: order IS the render order, so a
# re-ordering MUST be a new revision. Sorting them would silently re-order an
# operator's prompt and hide the change from the diff.
if sed -n '/func NormalizePayload(/,/^}/p' "${DOMAIN_GO}" \
     | grep -A 8 'ExtraSystemBlocks' \
     | grep -qE 'sortDedup|sort\.'; then
    fail 'phase 222: NormalizePayload SORTS the blocks — order is semantic here (unlike Skills.Names / OAuthProviders); a sort silently re-orders the operator prompt and hides it from the content hash'
else
    ok 'phase 222: NormalizePayload preserves block order (the deliberate asymmetry with the sorted sibling sections)'
fi

# ----------------------------------------------------------------------------
# 3. The trust argument — the verbatim render rests on the ADMIN write door.
# ----------------------------------------------------------------------------

assert_grep "${METHODS_GO}" \
    'MethodAgentConfigSetExtraSystemBlocks[[:space:]]+Method[[:space:]]*=' \
    'phase 222: the set_extra_system_blocks verb is declared'

# The verb must be in BOTH closed sets. Dropping it from the ADMIN set would
# leave the section writable by a non-admin caller WHILE its bodies are still
# rendered verbatim into the operator-trusted position — the single mutation
# that would turn this phase into a prompt-injection channel.
if sed -n '/^var canonicalAgentConfigMethods = map/,/^}/p' "${METHODS_GO}" \
     | grep -qE 'MethodAgentConfigSetExtraSystemBlocks'; then
    ok 'phase 222: the verb is in the canonical agent-config method set'
else
    fail 'phase 222: the verb is missing from canonicalAgentConfigMethods — it would not route'
fi

if sed -n '/^var canonicalAgentConfigAdminMethods = map/,/^}/p' "${METHODS_GO}" \
     | grep -qE 'MethodAgentConfigSetExtraSystemBlocks'; then
    ok 'phase 222: the verb is in the ADMIN method set — the tier the verbatim-render decision rests on'
else
    fail 'phase 222: the verb is NOT admin-gated, yet its bodies render VERBATIM into the operator-trusted position — this is the mutation that turns the section into a prompt-injection channel (D-367)'
fi

# The lower, CLAIM-FREE session tier must stay unable to reach the section.
assert_not_grep "${USER_GO}" \
    'ExtraSystemBlocks' \
    'phase 222: the claim-free session-user path still writes only PromptLayers — it cannot reach the blocks section'

# The trust obligation must be stated where a writer will read it.
if sed -n '/AgentConfigExtraSystemBlocks is/,/type AgentConfigExtraSystemBlocks struct/p' "${WIRE_GO}" \
     | grep -qE 'VERBATIM|verbatim'; then
    ok 'phase 222: the wire godoc states the verbatim (operator-trusted, unescaped) property'
else
    fail 'phase 222: the AgentConfigExtraSystemBlocks godoc does not state the verbatim property — the trust obligation is undocumented'
fi

# ----------------------------------------------------------------------------
# 4. The render slot.
# ----------------------------------------------------------------------------

assert_grep "${PROMPT_GO}" \
    'renderExtraSystemBlocks' \
    'phase 222: the prompt builder renders the blocks'

# Blocks compose into the EXISTING additive section — the taxonomy is
# untouched, and this is also what gives them the survives-a-replace property
# for free (buildAdditionalGuidance is reached on both branches of baseRequest).
if sed -n '/func buildAdditionalGuidance(/,/^}/p' "${PROMPT_GO}" \
     | grep -qE 'renderExtraSystemBlocks|blocks'; then
    ok 'phase 222: blocks render inside buildAdditionalGuidance (no new section; survives SystemPromptOverride)'
else
    fail 'phase 222: buildAdditionalGuidance does not render the blocks — either the taxonomy grew a section or the blocks are inert'
fi

# The blocks must NOT be escaped. Escaping them would defend against a writer
# who can already replace the whole spine via PromptLayers.Base — and would
# silently mangle an operator's angle brackets.
if sed -n '/func renderExtraSystemBlocks(/,/^}/p' "${PROMPT_GO}" \
     | grep -qE 'escapeUntrustedSection'; then
    fail 'phase 222: block bodies are routed through escapeUntrustedSection — blocks are admin-written and operator-trusted (D-367); escaping is incoherent with PromptLayers.Base being verbatim, and mangles operator text'
else
    ok 'phase 222: block bodies render VERBATIM (the position is operator-trusted; the boundary is the admin write door, not an escaper)'
fi

# ----------------------------------------------------------------------------
# 5. Single-source + Console hand-mirror lockstep (D-223 / §4.5 item 5).
# ----------------------------------------------------------------------------

assert_grep "${SINGLESOURCE_GO}" \
    'agent_config\.set_extra_system_blocks' \
    'phase 222: the method is registered in the single-source canonical set'

assert_grep "${SINGLESOURCE_GO}" \
    'AgentConfigExtraSystemBlocks' \
    'phase 222: the wire type is registered in the single-source type map'

assert_grep "${AGENTCFG_TS}" \
    'AgentConfigExtraSystemBlocks|extra_system_blocks' \
    'phase 222: the Console typed client mirrors the section by hand'

# ----------------------------------------------------------------------------
# 6. Live-wire assertions against the preflight-booted dev server.
# ----------------------------------------------------------------------------

SET_BLOCKS_URL="$(api_url /v1/agent_config/set_extra_system_blocks)"
SET_LAYERS_URL="$(api_url /v1/agent_config/set_prompt_layers)"
GET_URL="$(api_url /v1/agent_config/get)"

PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${SET_BLOCKS_URL}" 2>/dev/null || true)
LIVE=1
case "${PROBE:-000}" in
    000|'')
        # No server reachable at all — a standalone run outside preflight.
        skip 'phase 222: no dev server reachable; live assertions skipped (run under make preflight)'
        LIVE=0
        ;;
    404|405|501)
        # The route ships in the SAME PR as this script, so its absence on a
        # reachable server is an un-mounted REGRESSION, not a future surface.
        # The 404 -> SKIP convention exists for phase-N+1 scripts running
        # against a phase-N build; it does not apply here.
        fail "phase 222: agent_config.set_extra_system_blocks answered ${PROBE} on a reachable server — the route did not mount"
        LIVE=0
        ;;
esac

# THE BEARER COMES FROM common.sh's dev_bearer, NEVER from HARBOR_DEV_TOKEN
# directly (issue #624): a smoke that reads the env var itself SKIPs outside
# preflight while exiting 0, so it reports green having asserted nothing.
# dev_bearer falls back to the dev server's log, so a standalone run against a
# hand-booted `harbor dev` exercises the live legs too.
TOKEN="$(dev_bearer)"

if [ "${LIVE}" = "0" ]; then
    : # live assertions skipped; the guard tests below still run.
elif [ -z "${TOKEN}" ] || ! command -v jq >/dev/null 2>&1; then
    skip "phase 222: no dev bearer (dev_bearer) or jq — live assertions skipped (run under 'make preflight' or against a booted 'harbor dev')"
elif ! command -v curl >/dev/null 2>&1; then
    skip 'phase 222: curl not available; cannot exercise live wire'
else
    ID_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev")
    AGENT_ID="phase222-smoke-agent-$$-$(date +%s)"

    post_code() {
        curl -s -o /dev/null -w '%{http_code}' --max-time 10 -X POST "$1" \
            -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
            -H 'Content-Type: application/json' -d "$2" 2>/dev/null || true
    }
    post_body() {
        curl -sS --max-time 10 -X POST "$1" \
            -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
            -H 'Content-Type: application/json' -d "$2" 2>/dev/null
    }
    blocks_payload() {
        printf '{"agent_id":"%s","extra_system_blocks":{"blocks":%s}}' "${AGENT_ID}" "$1"
    }
    # Echo the active revision's block names, one per line, IN ORDER.
    read_block_names() {
        post_body "${GET_URL}" "{\"agent_id\":\"${AGENT_ID}\"}" \
            | jq -r '.revision.payload.extra_system_blocks.blocks[]?.name' 2>/dev/null || true
    }

    # DELIBERATELY REVERSE-ALPHABETICAL. With an already-sorted pair the
    # ordering assertion below is INERT: a sort anywhere on the write ->
    # normalize -> hash -> project -> render path leaves [alpha, beta]
    # untouched and the guard reports OK. Mutation-verified: adding a sort to
    # payloadToWire turns this leg OK -> FAIL only with this fixture.
    TWO_BLOCKS='[{"name":"zulu","body":"Prefer SI units."},{"name":"alpha","body":"Cite <sources> inline."}]'

    # --- A two-block write is accepted.
    CODE=$(post_code "${SET_BLOCKS_URL}" "$(blocks_payload "${TWO_BLOCKS}")")
    if [ "${CODE}" = "200" ]; then
        ok 'phase 222: a two-block set_extra_system_blocks write is accepted (200)'
    else
        fail "phase 222: the block write returned ${CODE}, want 200"
    fi

    # --- THE ORDERING ASSERTION, over the wire. Declared order is render
    #     order, so the read-back must preserve it. A sorted or reversed
    #     result is a FAIL — it would mean the composition order is not the
    #     operator's.
    NAMES="$(read_block_names | tr '\n' ',' | sed 's/,$//')"
    if [ "${NAMES}" = "zulu,alpha" ]; then
        ok 'phase 222: agent_config.get returns the blocks IN THE WRITTEN ORDER (zulu,alpha — NOT sorted)'
    else
        fail "phase 222: block order round-tripped as '${NAMES}', want 'zulu,alpha' — the declared order is not preserved (a sort on the path would yield 'alpha,zulu')"
    fi

    # --- A duplicate block name is refused: remove-by-name must stay
    #     unambiguous, which is the whole composability property.
    CODE=$(post_code "${SET_BLOCKS_URL}" \
        "$(blocks_payload '[{"name":"dup","body":"x"},{"name":"dup","body":"y"}]')")
    if [ "${CODE}" = "400" ]; then
        ok 'phase 222: a duplicate block name is refused (400) — remove-by-name stays unambiguous'
    else
        fail "phase 222: a duplicate block name returned ${CODE}, want 400"
    fi

    # --- A malformed block name is refused (empty name).
    CODE=$(post_code "${SET_BLOCKS_URL}" "$(blocks_payload '[{"name":"","body":"x"}]')")
    if [ "${CODE}" = "400" ]; then
        ok 'phase 222: an empty block name is refused (400)'
    else
        fail "phase 222: an empty block name returned ${CODE}, want 400"
    fi

    # --- THE SECTION-PRESERVATION INVARIANT, both directions. A sibling
    #     section write must not clear the blocks, and a block write must not
    #     clear a sibling. Only one direction is the classic drop.
    CODE=$(post_code "${SET_LAYERS_URL}" \
        "{\"agent_id\":\"${AGENT_ID}\",\"prompt_layers\":{\"base\":\"You are a smoke-test agent.\"}}")
    if [ "${CODE}" = "200" ]; then
        NAMES="$(read_block_names | tr '\n' ',' | sed 's/,$//')"
        if [ "${NAMES}" = "zulu,alpha" ]; then
            ok 'phase 222: a set_prompt_layers write PRESERVES the blocks section, in order (direction 1)'
        else
            fail "phase 222: after set_prompt_layers the blocks read back as '${NAMES}', want 'zulu,alpha' — a sibling verb dropped or re-ordered the new section"
        fi
    else
        fail "phase 222: the set_prompt_layers write returned ${CODE}, want 200 (cannot test preservation)"
    fi

    CODE=$(post_code "${SET_BLOCKS_URL}" "$(blocks_payload "${TWO_BLOCKS}")")
    if [ "${CODE}" = "200" ]; then
        BASE="$(post_body "${GET_URL}" "{\"agent_id\":\"${AGENT_ID}\"}" \
            | jq -r '.revision.payload.prompt_layers.base // ""' 2>/dev/null || true)"
        if [ -n "${BASE}" ]; then
            ok 'phase 222: a block write PRESERVES the prompt-layer section (direction 2)'
        else
            fail 'phase 222: the block write cleared prompt_layers.base — the section-preservation invariant is one-directional'
        fi
    else
        fail "phase 222: the second block write returned ${CODE}, want 200"
    fi

    # --- Identity stays mandatory.
    CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 -X POST "${SET_BLOCKS_URL}" \
        -H 'Content-Type: application/json' -d "$(blocks_payload "${TWO_BLOCKS}")" 2>/dev/null || true)
    if [ "${CODE}" = "401" ]; then
        ok 'phase 222: an unauthenticated block write is refused 401 (CLAUDE.md §6 — identity is mandatory)'
    else
        fail "phase 222: the unauthenticated block write returned ${CODE}, want 401"
    fi
fi

# ----------------------------------------------------------------------------
# 7. Guard tests (each FAILS, never SKIPs, when its guard is removed).
#
# Through common.sh's `assert_go_tests_pass`, which greps the `-v` output for a
# `--- PASS:` line per NAMED test. The private `run_filtered_tests` this
# replaced trusted the exit code and turned a rename into a SKIP — better than
# a silent OK, but still the §4.2 item 5 shape, and it contradicted this file's
# own claims at the header and at section 7 that every non-probe assertion
# FAILs when its guard is removed. It also could not see a HALF-dead filter:
# each leg's alternation carried a name that matched NOTHING —
# `TestContentHash_BlockOrderIsSemantic` (really
# `TestContentHash_ExtraSystemBlocks_OrderIsSemantic`), `TestDiff_ExtraSystemBlocks`
# (really `TestDiffExtraSystemBlocks`), `TestSectionPreservation_ExtraSystemBlocks`
# and `TestComposition_ExtraSystemBlocks` (which never existed; the
# all-direction preservation matrix is `TestRebuildCompleteness_EverySetter_
# PreservesEverySibling` + `TestVerbs_PreserveAllSiblingSections`) — while a
# surviving sibling in the same alternation kept the leg green. Every leg's
# description therefore named coverage the leg did not run. Naming each test
# explicitly is what makes that state visible.
#
# `-race -count=1` rides in the go-test-args parameter. NO CGO_ENABLED=0: the
# race detector needs cgo on Linux, where `CGO_ENABLED=0 go test -race` fails
# to build. Harbor's CGo ban governs the shipped BINARY, not the
# race-instrumented test binary.
# ----------------------------------------------------------------------------

if ! command -v go >/dev/null 2>&1; then
    skip 'phase 222: go toolchain unavailable — guard tests skipped'
else
    P222_GOLOG="$(mktemp "${TMPDIR:-/tmp}/phase222-gotest.XXXXXX")"
    trap 'rm -f "${P222_GOLOG}"' EXIT

    assert_go_tests_pass "${P222_GOLOG}" '-race -count=1 ./internal/agentcfg/' \
        "phase 222: the order-preserving normalizer, the order-is-semantic hash property and the block diff (agentcfg)" \
        TestNormalizePayload_ExtraSystemBlocks_PreservesDeclaredOrder \
        TestNormalizePayload_ExtraSystemBlocks_DropsPhantomsAndDuplicates \
        TestNormalizePayload_ExtraSystemBlocks_AllEmptySectionDropsOut \
        TestContentHash_ExtraSystemBlocks_OrderIsSemantic \
        TestContentHash_ExtraSystemBlocks_AbsentIsByteIdentical \
        TestDiffExtraSystemBlocks

    assert_go_tests_pass "${P222_GOLOG}" '-race -count=1 ./internal/runtime/agentcfg/protocol/' \
        "phase 222: the write door, the all-direction section-preservation matrix and the concurrent-reuse run (agentcfg/protocol)" \
        TestSetExtraSystemBlocks_RecordsRevision_InDeclaredOrder \
        TestSetExtraSystemBlocks_ReorderIsANewRevision \
        TestSetExtraSystemBlocks_RefusesMalformedInput \
        TestSetExtraSystemBlocks_EmptyListClearsTheSection \
        TestSetExtraSystemBlocks_HonoursTheExpectedRevisionToken \
        TestSetExtraSystemBlocks_MethodIsAdminTier \
        TestSetExtraSystemBlocks_ConcurrentReuse_NoCrossTalk \
        TestSetRevision_RefusesMalformedExtraSystemBlocks \
        TestSessionUserPrompt_CannotWriteExtraSystemBlocks \
        TestActiveExtraSystemBlocks_IsIdentityScoped \
        TestRebuildCompleteness_EverySetter_PreservesEverySibling \
        TestVerbs_PreserveAllSiblingSections

    assert_go_tests_pass "${P222_GOLOG}" '-race -count=1 ./internal/runtime/agentcfg/projection/' \
        "phase 222: run-start projection of the active revision's blocks (agentcfg/projection)" \
        TestActiveExtraSystemBlocks_ResolvesInDeclaredOrder \
        TestActiveExtraSystemBlocks_NotSetPaths \
        TestActiveExtraSystemBlocks_IdentityScoped \
        TestActiveExtraSystemBlocks_ReturnsAFreshCopy \
        TestApplyPromptLayers_CarriesExtraSystemBlocks

    assert_go_tests_pass "${P222_GOLOG}" '-race -count=1 ./internal/planner/react/' \
        "phase 222: the verbatim render, the declared order, the byte-identity pin and determinism (planner/react)" \
        TestRenderExtraSystemBlocks_DeclaredOrderWithLabels \
        TestRenderExtraSystemBlocks_NameIsNeverATag \
        TestRenderExtraSystemBlocks_BodyIsVerbatim \
        TestRenderExtraSystemBlocks_UserLayerStaysEscaped \
        TestRenderExtraSystemBlocks_SlotIsBelowBakedGuidanceAboveExtraInstructions \
        TestRenderExtraSystemBlocks_SurvivesSystemPromptOverride \
        TestRenderExtraSystemBlocks_AbsentIsByteIdentical \
        TestRenderExtraSystemBlocks_NoWrapperWhenEverythingElseIsEmpty \
        TestRenderExtraSystemBlocks_Deterministic

    assert_go_tests_pass "${P222_GOLOG}" '-race -count=1 ./test/integration/' \
        "phase 222: the N-contributor seam with identity propagation and four failure modes (integration)" \
        TestE2E_AgentConfigExtraSystemBlocks_NContributorRoundTrip \
        TestE2E_AgentConfigExtraSystemBlocks_EmitsConfigRevised \
        TestE2E_AgentConfigExtraSystemBlocks_ReachesTheBuiltPrompt \
        TestE2E_AgentConfigExtraSystemBlocks_IdentityPropagation \
        TestE2E_AgentConfigExtraSystemBlocks_FailureModes \
        TestE2E_AgentConfigExtraSystemBlocks_SessionTierCannotReachTheSection \
        TestE2E_AgentConfigExtraSystemBlocks_ReorderIsADiffableRevision
fi

smoke_summary
