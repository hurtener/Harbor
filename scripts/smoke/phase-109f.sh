#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 109f smoke — render heavy MCP App documents (fetch the offloaded
# artifact) + the operator "pop to side-by-side" affordance.
#
# Classification: static-only — pure file-existence + text greps against the
# Console source. Runs in the parallel batch BEFORE the dev server boots; this
# phase ships no Runtime endpoint or Protocol method (Console-only).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

RENDERER="web/console/src/lib/chat/renderers/mcp-app.svelte"
HOST_IFACE="web/console/src/lib/chat/renderers/app-bridge-host.ts"
ADAPTER="web/console/src/lib/mcp-app-host-client.ts"
GUARD="web/console/src/routes/(console)/playground/[session_id]/mcp-app-discovery.spec.ts"

# ----------------------------------------------------------------------------
# (1) Gap A — the heavy `ui://` document renders by FETCHING the offloaded
#     artifact (D-026 by-reference), not by refusing it as a "server bug".
# ----------------------------------------------------------------------------
assert_grep_present 'resolveArtifact\(artifactID: string\)' "${HOST_IFACE}" \
    "phase-109f: MCPAppHostClient gains the artifact-fetch seam (resolveArtifact)"
assert_grep_present 'resolveArtifact\(resource\.artifactRef\.id\)' "${RENDERER}" \
    "phase-109f: the renderer resolves the by-reference stub instead of throwing"
assert_grep_present 'wrapAppDocument\(documentHTML' "${RENDERER}" \
    "phase-109f: the fetched bytes feed the same sandboxed srcdoc as the inline path"
assert_grep_present 'res.presigned_url' "${ADAPTER}" \
    "phase-109f: the adapter maps resolveArtifact onto artifacts.get_ref (presigned_url)"

# The wrong "server bug" comment must be gone.
if grep -q 'is a server bug for' "${RENDERER}" 2>/dev/null; then
    fail "phase-109f: the renderer still calls a heavy app document a 'server bug' (Gap A not fixed)"
else
    ok "phase-109f: the wrong 'server bug' comment is corrected"
fi

# ----------------------------------------------------------------------------
# (2) Gap B — the host-side operator "pop to side-by-side" affordance dispatches
#     through the INJECTED display-mode callback (never the page; D-091).
# ----------------------------------------------------------------------------
assert_grep_present 'mcp-app-expand-pip' "${RENDERER}" \
    "phase-109f: the inline frame carries the operator expand affordance"
assert_grep_present 'requested: mode, granted: mode' "${RENDERER}" \
    "phase-109f: the affordance dispatches via the injected onDisplayModeRequest seam"

# ----------------------------------------------------------------------------
# (3) Chat-module encapsulation (D-091): the renderer reaches NO Console
#     internal — the artifact fetch goes through the injected client, not a
#     $lib/protocol import. Spec files are exempt.
# ----------------------------------------------------------------------------
chat_leaks="$(grep -rnE "from ['\"]\\\$lib/(protocol|connection|stores|components)" \
    web/console/src/lib/chat/ --include='*.ts' --include='*.svelte' 2>/dev/null \
    | grep -vE '\.(spec|test)\.ts:' || true)"
if [ -n "${chat_leaks}" ]; then
    fail "phase-109f: chat module imports a Console internal (D-091 encapsulation): ${chat_leaks}"
else
    ok "phase-109f: chat module imports no Console internals (artifact fetch is injected)"
fi

# ----------------------------------------------------------------------------
# (4) The always-on Vitest guards exist (Gap A heavy-fetch + Gap B affordance).
# ----------------------------------------------------------------------------
assert_file "${GUARD}" \
    "phase-109f: the heavy-app-document + expand-affordance regression guard landed"
assert_grep_present 'Gap A — heavy MCP-App document renders via the offloaded artifact' "${GUARD}" \
    "phase-109f: the Gap-A heavy-document fetch test is present"
assert_grep_present 'operator expand affordance pops the inline app to side-by-side' "${GUARD}" \
    "phase-109f: the Gap-B expand-affordance test is present"

smoke_summary
