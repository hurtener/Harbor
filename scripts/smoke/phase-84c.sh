#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 84c — Provider-native multimodal mechanism (D-190).
#
# Static assertions pin the shipped surface: the part-level
# `ProviderNative` flag + `ProviderFileID` fields, the
# `llm.provider_file.uploaded` canonical event (registered + in the
# generated Protocol docs), the bifrost driver's internal upload pass,
# and the planner capability pass honouring `provider_native` (the
# 84b-era degradation is retired).
#
# Live assertions drive the booted dev server: an attachment started
# with disposition `provider_native` resolves WITHOUT degradation —
# the `task.input_disposition.resolved` event reports
# disposition=provider_native, degraded=false. The provider upload
# event itself (`llm.provider_file.uploaded`) cannot fire here: the
# preflight server runs the dev-only mock LLM driver, and the upload
# pass is the bifrost driver's. The real-provider upload is covered by
# the HARBOR_LIVE_LLM-gated conformance table
# (internal/llm/drivers/bifrost/conformance_test.go).
#
# Per §4.2 the smoke is shipping-progress aware: every assertion SKIPs
# (via the 404/405/501 convention + static probes) on a pre-84c build.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Convenience wrapper — the phase-84b shape.
assert_or_skip() {
    local pattern="$1" file="$2" desc="$3"
    if [ ! -f "${file}" ]; then
        skip "${desc}: ${file} not found (Phase 84c not yet implemented)"
        return
    fi
    if grep -qE "${pattern}" "${file}" 2>/dev/null; then
        ok "${desc}"
    else
        skip "${desc}: pattern '${pattern}' absent (Phase 84c not yet implemented)"
    fi
}

# ----------------------------------------------------------------------------
# Static assertions — the mechanism's surface.
# ----------------------------------------------------------------------------

assert_or_skip 'ProviderNative bool' \
    "internal/llm/llm.go" \
    "static: the part-level ProviderNative flag exists on the content sum-type"

assert_or_skip 'ProviderFileID string' \
    "internal/llm/llm.go" \
    "static: the opaque ProviderFileID reference field exists"

assert_or_skip 'DocumentType string' \
    "internal/llm/llm.go" \
    "static: FilePart.DocumentType disambiguates structured documents"

assert_or_skip 'llm\.provider_file\.uploaded' \
    "internal/llm/events.go" \
    "static: the llm.provider_file.uploaded canonical event is declared"

assert_or_skip 'applyProviderNative' \
    "internal/llm/drivers/bifrost/providerfiles.go" \
    "static: the bifrost driver carries the internal upload pass"

assert_or_skip 'providerFileBlock' \
    "internal/llm/drivers/bifrost/translate.go" \
    "static: the translator emits the provider file-reference block"

assert_or_skip 'return DispositionProviderNative, nil' \
    "internal/planner/disposition.go" \
    "static: EffectiveDisposition honours provider_native (84b degradation retired)"

assert_or_skip 'EventTypeProviderFileUploaded' \
    "cmd/harbor-gen-protocol-docs/events.go" \
    "static: the generated Protocol docs carry the event join row"

assert_or_skip 'llm\.provider_file\.uploaded' \
    "docs/site/protocol/events.md" \
    "static: the generated events reference documents the new event"

# ----------------------------------------------------------------------------
# Live-server assertions — provider_native resolves without degradation.
# ----------------------------------------------------------------------------

if ! grep -qE 'return DispositionProviderNative, nil' internal/planner/disposition.go 2>/dev/null; then
    skip "live provider_native resolution: skipped — Phase 84c not yet implemented"
    smoke_summary
    exit 0
fi

BOOTSTRAP_URL="$(api_url /v1/dev/bootstrap.json)"
BOOTSTRAP_RESULT="$(curl -sS -X POST -d '{}' "${BOOTSTRAP_URL}" || true)"
TOKEN="$(echo "${BOOTSTRAP_RESULT}" | jq -r '.token // empty' 2>/dev/null || true)"
if [ -z "${TOKEN}" ]; then
    skip "live provider_native resolution: could not bootstrap a dev token"
    smoke_summary
    exit 0
fi

ID_HEADERS=(
    -H "Authorization: Bearer ${TOKEN}"
    -H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev"
    -H "Content-Type: application/json"
)

# Open an SSE subscription in the background; capture events.
EV_FILE="$(mktemp -t harbor-phase84c-events-XXXXXX)"
trap 'rm -f "${EV_FILE}" 2>/dev/null; kill ${SSE_PID:-} 2>/dev/null || true' EXIT
SUBSCRIBE_URL="$(api_url /v1/events)"
curl -sS -N "${ID_HEADERS[@]}" "${SUBSCRIBE_URL}" > "${EV_FILE}" 2>&1 &
SSE_PID=$!
sleep 1

# 1. Upload a small PNG attachment (a 1×1 PNG; size is irrelevant —
#    the disposition declares the path, not the threshold).
PNG_B64="iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
PUT_RESP="$(curl -sS -X POST "$(api_url /v1/control/artifacts.put)" "${ID_HEADERS[@]}" \
    -d "{\"scope\":{\"tenant\":\"dev\",\"user\":\"dev\",\"session\":\"dev\"},\"bytes\":\"${PNG_B64}\",\"opts\":{\"mime_type\":\"image/png\",\"filename\":\"smoke-84c.png\"}}")"
ART_ID="$(echo "${PUT_RESP}" | jq -r '.ref.id // empty')"
if [ -z "${ART_ID}" ]; then
    skip "live provider_native resolution: artifacts.put returned no ref id (surface absent?)"
    smoke_summary
    exit 0
fi
ok "live: artifacts.put stored the smoke attachment (id=${ART_ID})"

# 2. start with the provider_native per-attachment hint.
START_RESP="$(curl -sS -X POST "$(api_url /v1/control/start)" "${ID_HEADERS[@]}" \
    -d "{\"query\":\"phase-84c provider-native smoke\",\"description\":\"phase-84c smoke\",\"input_artifact_ids\":[\"${ART_ID}\"],\"input_artifact_dispositions\":{\"${ART_ID}\":\"provider_native\"}}")"
TASK_ID="$(echo "${START_RESP}" | jq -r '.task_id // empty')"
if [ -z "${TASK_ID}" ]; then
    fail "live: start with a provider_native hint returned no task id — ${START_RESP}"
    smoke_summary
    exit 0
fi
ok "live: start accepted the provider_native disposition hint (task=${TASK_ID})"

# 3. The resolution event reports provider_native HONOURED — no
#    degradation (pre-84c builds emitted degraded=true with reason
#    provider_native_unavailable; that vocabulary is retired).
RESOLVED_SEEN=0
for _ in $(seq 1 15); do
    if grep -q 'task\.input_disposition\.resolved' "${EV_FILE}" 2>/dev/null \
        && grep -q '"disposition":"provider_native"' "${EV_FILE}" 2>/dev/null; then
        RESOLVED_SEEN=1
        break
    fi
    sleep 1
done
if [ "${RESOLVED_SEEN}" -eq 1 ]; then
    ok "live: task.input_disposition.resolved reports disposition=provider_native"
else
    fail "live: no provider_native resolution event observed within 15s"
fi

if grep -q 'provider_native_unavailable' "${EV_FILE}" 2>/dev/null; then
    fail "live: the retired provider_native_unavailable degradation still fired"
else
    ok "live: no provider_native degradation — the mechanism is honoured end-to-end"
fi

# 4. tasks.get still reflects the declared hint on the wire (84b
#    surface, exercised with the 84c value).
DETAIL="$(curl -sS -X POST "$(api_url /v1/tasks/get)" "${ID_HEADERS[@]}" \
    -d "{\"identity\":{\"tenant\":\"dev\",\"user\":\"dev\",\"session\":\"dev\"},\"id\":\"${TASK_ID}\"}")"
GOT_DISP="$(echo "${DETAIL}" | jq -r '.input_artifacts[0].disposition // empty')"
if [ "${GOT_DISP}" = "provider_native" ]; then
    ok "live: tasks.get reflects input_artifacts[0].disposition=provider_native"
else
    fail "live: tasks.get disposition = '${GOT_DISP}', want provider_native"
fi

smoke_summary
