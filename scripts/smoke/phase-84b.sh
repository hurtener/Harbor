#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 84b — Multimodal attachment disposition policy (D-189).
#
# Asserts the per-attachment `disposition` hint round-trips through
# the Protocol: `artifacts.put` → `start` (with
# `input_artifact_dispositions`) → `tasks.get` reflects the hint on
# `input_artifacts[].disposition` — and the edge rejects a non-grammar
# value with CodeInvalidRequest (400).
#
# Per §4.2 the smoke is shipping-progress aware: every assertion SKIPs
# (via the 404/405/501 convention + static probes) on a pre-84b build.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Convenience wrapper — the phase-107a shape.
assert_or_skip() {
    local pattern="$1" file="$2" desc="$3"
    if [ ! -f "${file}" ]; then
        skip "${desc}: ${file} not found (Phase 84b not yet implemented)"
        return
    fi
    if grep -qE "${pattern}" "${file}" 2>/dev/null; then
        ok "${desc}"
    else
        skip "${desc}: pattern '${pattern}' absent (Phase 84b not yet implemented)"
    fi
}

# ----------------------------------------------------------------------------
# Static assertions — the policy core + the carriers.
# ----------------------------------------------------------------------------

assert_or_skip 'func ResolveDisposition' \
    "internal/planner/disposition.go" \
    "static: planner.ResolveDisposition is the exported pure resolver"

assert_or_skip 'type DispositionPolicy struct' \
    "internal/planner/disposition.go" \
    "static: planner.DispositionPolicy is the planner-homed policy type"

assert_or_skip 'InputArtifactDispositions' \
    "internal/protocol/types/control.go" \
    "static: StartRequest carries input_artifact_dispositions"

assert_or_skip 'TaskInputArtifact' \
    "internal/protocol/types/tasks.go" \
    "static: TaskDetail surfaces input_artifacts on tasks.get"

assert_or_skip 'MultimodalConfig' \
    "internal/config/config.go" \
    "static: the multimodal.disposition config block exists"

assert_or_skip 'chat-attachment-disposition' \
    "web/console/src/lib/chat/ChatComposer.svelte" \
    "static: the Playground composer carries the disposition selector"

# ----------------------------------------------------------------------------
# Live-server assertions — disposition hint round-trip.
# ----------------------------------------------------------------------------

if ! grep -qE 'InputArtifactDispositions' internal/protocol/types/control.go 2>/dev/null; then
    skip "live disposition round-trip: skipped — Phase 84b not yet implemented"
    smoke_summary
    exit 0
fi

BOOTSTRAP_URL="$(api_url /v1/dev/bootstrap.json)"
BOOTSTRAP_RESULT="$(curl -sS -X POST -d '{}' "${BOOTSTRAP_URL}" || echo '{}')"
TOKEN="$(echo "${BOOTSTRAP_RESULT}" | jq -r '.token // empty')"
if [ -z "${TOKEN}" ]; then
    skip "live disposition round-trip: could not bootstrap a dev token"
    smoke_summary
    exit 0
fi

ID_HEADERS=(
    -H "Authorization: Bearer ${TOKEN}"
    -H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev"
    -H "Content-Type: application/json"
)

# 1. Upload a small artifact (a missing surface yields no ref id →
#    SKIP, the §4.2 coexistence convention).
PUT_RESP="$(curl -sS -X POST "$(api_url /v1/control/artifacts.put)" "${ID_HEADERS[@]}" \
    -d '{"scope":{"tenant":"dev","user":"dev","session":"dev"},"bytes":"JVBERi0xLjQK","opts":{"mime_type":"application/pdf","filename":"smoke-84b.pdf"}}')"
ART_ID="$(echo "${PUT_RESP}" | jq -r '.ref.id // empty')"
if [ -z "${ART_ID}" ]; then
    skip "live disposition round-trip: artifacts.put returned no ref id (surface absent?)"
    smoke_summary
    exit 0
fi
ok "live: artifacts.put stored the smoke attachment (id=${ART_ID})"

# 2. start with a per-attachment disposition hint.
START_RESP="$(curl -sS -X POST "$(api_url /v1/control/start)" "${ID_HEADERS[@]}" \
    -d "{\"query\":\"phase-84b disposition smoke\",\"description\":\"phase-84b smoke\",\"input_artifact_ids\":[\"${ART_ID}\"],\"input_artifact_dispositions\":{\"${ART_ID}\":\"tool:pdf.extract\"}}")"
TASK_ID="$(echo "${START_RESP}" | jq -r '.task_id // empty')"
if [ -z "${TASK_ID}" ]; then
    fail "live: start with a disposition hint returned no task id — ${START_RESP}"
    smoke_summary
    exit 0
fi
ok "live: start accepted the per-attachment disposition hint (task=${TASK_ID})"

# 3. tasks.get reflects the hint on the input-artifact view.
DETAIL="$(curl -sS -X POST "$(api_url /v1/tasks/get)" "${ID_HEADERS[@]}" \
    -d "{\"identity\":{\"tenant\":\"dev\",\"user\":\"dev\",\"session\":\"dev\"},\"id\":\"${TASK_ID}\"}")"
GOT_ID="$(echo "${DETAIL}" | jq -r '.input_artifacts[0].id // empty')"
GOT_DISP="$(echo "${DETAIL}" | jq -r '.input_artifacts[0].disposition // empty')"
if [ "${GOT_ID}" = "${ART_ID}" ] && [ "${GOT_DISP}" = "tool:pdf.extract" ]; then
    ok "live: tasks.get reflects input_artifacts[0].disposition=tool:pdf.extract"
else
    fail "live: tasks.get did not round-trip the disposition (id=${GOT_ID}, disposition=${GOT_DISP})"
fi

# 4. The edge rejects a non-grammar disposition value (fail-closed).
BAD_STATUS="$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$(api_url /v1/control/start)" "${ID_HEADERS[@]}" \
    -d "{\"query\":\"phase-84b bad disposition\",\"input_artifact_ids\":[\"${ART_ID}\"],\"input_artifact_dispositions\":{\"${ART_ID}\":\"bogus\"}}")"
if [ "${BAD_STATUS}" = "400" ]; then
    ok "live: start rejects a non-grammar disposition value with 400"
else
    fail "live: start accepted a bogus disposition (status=${BAD_STATUS}, want 400)"
fi

# 5. The edge rejects a hint keyed by an unattached artifact id.
ORPHAN_STATUS="$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$(api_url /v1/control/start)" "${ID_HEADERS[@]}" \
    -d '{"query":"phase-84b orphan hint","input_artifact_dispositions":{"art_never_attached":"ref"}}')"
if [ "${ORPHAN_STATUS}" = "400" ]; then
    ok "live: start rejects a disposition hint for an unattached artifact id with 400"
else
    fail "live: start accepted an orphan disposition hint (status=${ORPHAN_STATUS}, want 400)"
fi

smoke_summary
