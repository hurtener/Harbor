#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 258 smoke — stock coordinator receipt transport and runtime readiness.
# Static-only: no coordinator, provider, PostgreSQL, or network is contacted.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file "docs/plans/phase-258-stock-coordinator-receipt-transport.md" "phase 258 plan exists"
assert_grep_present '## D-438' "docs/decisions.md" "D-438 decision exists"
assert_grep_present '\*\*Stock transport/readiness completion\.\*\* Phase 258' "docs/notes/downstream-asks.md" "HA-72 transport register exists"
assert_file "internal/llm/receipts/httptransport/client.go" "stock authenticated transport exists"
assert_grep_present 'type BatchDelivery interface' "internal/llm/receipts/outbox.go" "bounded batch extension exists"
assert_grep_present 'TestOutbox_BatchPartialAckAndResponseLossReplayOnlyUnackedFacts' "internal/llm/receipts/outbox_test.go" "partial ACK/response-loss replay covered"
assert_grep_present 'TestClient_DeliverBatch_StrictFailureMatrix' "internal/llm/receipts/httptransport/client_test.go" "strict ACK parser covered"
assert_grep_present 'TestClient_ConcurrentReuse' "internal/llm/receipts/httptransport/client_test.go" "concurrent reuse covered"
assert_grep_present 'TestConfigureStockExternalGrant_DisabledDoesNoWork' "internal/runtime/serve/external_grant_transport_test.go" "disabled zero-work path covered"
assert_grep_present 'TestExternalGrantReadiness_ReportsModeRoutesAndConcreteWiring' "internal/runtime/serve/external_grant_transport_test.go" "route readiness covered"
assert_grep_present 'TestExternalGrantReadiness_StockOutboxDegradesAndRecovers' "internal/runtime/serve/external_grant_transport_test.go" "outbox readiness degradation and recovery covered"
assert_grep_present 'TestOutboxAcknowledgedLifetimeHistoryNeverReentersLegacyScan' "internal/llm/receipts/outbox_due_test.go" "ACKed lifetime history stays outside upgrade reconciliation"
assert_grep_present 'TestOutboxReconcile_EnqueueBeforePendingAckConvergesAfterResponseLoss' "internal/llm/receipts/outbox_due_test.go" "enqueue-before-pending-ACK response loss converges"
assert_grep_present 'TestStore_PendingReceiptHandoffRecoversAllTerminalOutcomesAcrossDrivers' "internal/llm/leases/manager_test.go" "atomic terminal receipt handoff recovery covered"
assert_grep_present 'external_grant\?: ExternalGrantReadiness' "web/console/src/lib/protocol/settings.ts" "Console runtime.info optional mirror updated"
assert_grep_present 'llm.external_grant.coordinator.receipt_url' "docs/CONFIG.md" "operator config documented"
assert_grep_present 'never require coordinator provider credentials or a model catalog' "docs/plans/phase-258-stock-coordinator-receipt-transport.md" "runtime-default independence explicit"

smoke_summary
