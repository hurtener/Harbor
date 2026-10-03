#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source scripts/smoke/common.sh
if go test -race -p 1 ./internal/tasks/engine ./internal/runtime/steering ./internal/protocol ./internal/protocol/transports/control ./internal/protocol/client ./internal/tasks/drivers/inprocess ./internal/tasks/drivers/durable ./internal/runtime/serve \
  -run 'TestEngine_Input|TestRun_InputReceipts|TestDispatch_InputReceipt|TestServeHTTP_InputReceipt|TestClient_ControlReceipt|Test.*Conformance/Input|TestDurable_Input|TestInputReceiptCapability_' -count=1; then
  ok "exact-task input receipts, replay, cancellation, and sealed revision regressions pass"
else
  fail "exact-task input receipt regression failed"
fi
assert_post_status_auth 401 "$(api_url /v1/control/control.receipt)"   '{"identity":{},"event_id":"phase-271-missing-run"}' "$(dev_bearer)"   "input receipt lookup requires an exact task identity"
smoke_summary
