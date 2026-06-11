#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 113b smoke — Protocol adoption track: the pause + versioning
# choreographies, build-a-client, and conformance certification.
# (RFC §5, §3.3, §6.3; docs/plans/phase-113b-protocol-choreographies-and-certification.md)
#
# Assertion classes (all static — no booted-server dependency):
#   1. Site trip-wires: the four 113b pages exist; the Protocol nav
#      section in docs/site/.vitepress/config.ts lists them.
#   2. Lockstep greps: every method a 113b guide demonstrates resolves
#      against 113a's generated methods.md (the "> Methods
#      demonstrated:" convention); every pause event the guide names
#      resolves against the generated events.md; the OAuth callback
#      route quoted by the pause guide matches the exported
#      auth.CallbackPath constant (the route is a provider-redirect
#      mount, deliberately NOT a methods.md row); the versioning guide
#      names the pinned-version surface.
#   3. The Q3 guard: the certification page does NOT promise an
#      sdk-exported runner (grep-absent on sdk/ phrasing).
#   4. The worked-client compile gate: `go build` of
#      examples/protocol-clients/event-viewer (bounded; the
#      phase-112b external-gate pattern at miniature scale — in-module
#      here, since the client is stdlib-only by design).
#
# Conventions (AGENTS.md §4.2): missing surface → SKIP on pre-113b
# builds; once shipped, OK ≥ count(acceptance criteria covered), FAIL = 0.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

PROTO_DIR="docs/site/protocol"
PAUSE_PAGE="${PROTO_DIR}/pause-model.md"

if [ ! -f "${PAUSE_PAGE}" ]; then
    skip "phase 113b: ${PAUSE_PAGE} not yet present (phase not implemented)"
    smoke_summary
    exit 0
fi

# ----------------------------------------------------------------------------
# 1. Static — the four 113b pages exist; the nav lists them.
# ----------------------------------------------------------------------------
for page in pause-model.md versioning-and-compatibility.md build-a-client.md \
            conformance-certification.md; do
    assert_file "${PROTO_DIR}/${page}" "phase 113b: ${page} present"
done

CONFIG='docs/site/.vitepress/config.ts'
for link in '/protocol/pause-model' '/protocol/versioning-and-compatibility' \
            '/protocol/build-a-client' '/protocol/conformance-certification'; do
    assert_grep_present "${link}" "${CONFIG}" "phase 113b: config.ts nav entry ${link}"
done

# ----------------------------------------------------------------------------
# 2a. Lockstep — every method a 113b guide demonstrates resolves against
#     the generated methods.md (the 113a "> Methods demonstrated:"
#     convention, continued).
# ----------------------------------------------------------------------------
for guide in pause-model.md versioning-and-compatibility.md build-a-client.md; do
    path="${PROTO_DIR}/${guide}"
    demonstrated=$(awk '/^> Methods demonstrated:/,/^$/' "${path}" \
        | grep -oE '`[a-z0-9_.]+`' | tr -d '`' | sort -u || true)
    if [ -z "${demonstrated}" ]; then
        fail "phase 113b: ${guide} declares no 'Methods demonstrated' line"
        continue
    fi
    for m in ${demonstrated}; do
        if grep -qF "| \`${m}\` |" "${PROTO_DIR}/methods.md"; then
            ok "phase 113b: ${guide} method \`${m}\` resolves in methods.md"
        else
            fail "phase 113b: ${guide} demonstrates \`${m}\` but methods.md has no such row"
        fi
    done
done

# 2b. Lockstep — every pause event the guide narrates is a catalogued
#     canonical event type in the generated events.md.
for ev in 'pause.requested' 'pause.resumed' 'tool.approval_requested' \
          'tool.approved' 'tool.rejected' 'tool.auth_required' \
          'tool.auth_completed' 'notification.pause_requested'; do
    if ! grep -qF "\`${ev}\`" "${PAUSE_PAGE}"; then
        fail "phase 113b: pause guide no longer narrates ${ev} (guide drifted from this smoke)"
        continue
    fi
    if grep -qF "## \`${ev}\`" "${PROTO_DIR}/events.md"; then
        ok "phase 113b: pause guide event \`${ev}\` resolves in events.md"
    else
        fail "phase 113b: pause guide names \`${ev}\` but events.md has no such section"
    fi
done

# 2c. Lockstep — the typed Decision values the guide teaches clients to
#     branch on are the canonical pauseresume.Decision set.
for d in approve reject resume timeout; do
    if grep -qF "\`${d}\`" "${PAUSE_PAGE}" \
        && grep -qF "Decision = \"${d}\"" internal/runtime/pauseresume/decision.go; then
        ok "phase 113b: pause guide Decision \`${d}\` matches pauseresume.Decision"
    else
        fail "phase 113b: pause guide Decision \`${d}\` does not match internal/runtime/pauseresume/decision.go"
    fi
done

# 2d. Lockstep — the OAuth callback route the guide quotes IS the
#     exported auth.CallbackPath constant (a provider-redirect mount;
#     deliberately not a methods.md row).
CB_ROUTE='/v1/tools/oauth/callback'
assert_grep_present "GET ${CB_ROUTE}" "${PAUSE_PAGE}" \
    'phase 113b: pause guide quotes the OAuth callback route'
assert_grep_present "CallbackPath = \"${CB_ROUTE}\"" internal/tools/auth/callback.go \
    'phase 113b: the quoted callback route matches auth.CallbackPath'

# 2e. Lockstep — the versioning guide names the pinned-version surface:
#     the version pin's home and the live protocol_version value.
VERS_PAGE="${PROTO_DIR}/versioning-and-compatibility.md"
assert_grep_present 'internal/protocol/types/version.go' "${VERS_PAGE}" \
    'phase 113b: versioning guide names the version pin home'
PINNED=$(grep -oE 'ProtocolVersion = "[0-9.]+"' internal/protocol/types/version.go \
    | grep -oE '[0-9.]+' || true)
if [ -n "${PINNED}" ] && grep -qF "\`${PINNED}\`" "${VERS_PAGE}"; then
    ok "phase 113b: versioning guide carries the pinned version ${PINNED}"
else
    fail "phase 113b: versioning guide does not carry the pinned Protocol version (${PINNED:-unreadable})"
fi

# ----------------------------------------------------------------------------
# 3. The Q3 guard — the certification page documents the IN-REPO suite
#    and must not promise an sdk-exported runner.
# ----------------------------------------------------------------------------
CERT_PAGE="${PROTO_DIR}/conformance-certification.md"
assert_grep_present 'internal/protocol/conformance' "${CERT_PAGE}" \
    'phase 113b: certification page names the in-repo suite'
assert_grep_absent 'sdk/' "${CERT_PAGE}" \
    'phase 113b: certification page promises no sdk-exported runner (Q3 guard)'

# ----------------------------------------------------------------------------
# 4. The worked-client compile gate — the build-a-client listing cannot
#    rot. Bounded; module-cached Go build of an in-module main package.
# ----------------------------------------------------------------------------
EV_DIR='examples/protocol-clients/event-viewer'
assert_file "${EV_DIR}/main.go" 'phase 113b: worked event-viewer source present'
if ! command -v go >/dev/null 2>&1; then
    skip 'phase 113b: go toolchain unavailable — compile gate skipped'
else
    GATE_LOG="$(mktemp -t harbor-113b-gate-XXXXXX)"
    GATE_BIN="$(mktemp -d -t harbor-113b-bin-XXXXXX)"
    trap 'rm -rf "${GATE_LOG}" "${GATE_BIN}"' EXIT
    if go build -o "${GATE_BIN}/event-viewer" "./${EV_DIR}" >"${GATE_LOG}" 2>&1; then
        ok 'phase 113b: COMPILE GATE green — the worked event-viewer builds'
    else
        fail "phase 113b: the worked event-viewer does not compile (see ${GATE_LOG})"
        sed 's/^/    /' "${GATE_LOG}" || true
    fi
    # The client is SDK-free by design — a Harbor import sneaking in
    # would quietly invalidate the guide's whole premise.
    assert_grep_absent 'hurtener/Harbor' "${EV_DIR}/main.go" \
        'phase 113b: the event-viewer stays SDK-free (no Harbor import)'
fi

smoke_summary
