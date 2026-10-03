#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
# Phase 270 — exact recipient-admitted direct artifact copies.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
# shellcheck source=scripts/smoke/common.sh
source scripts/smoke/common.sh
assert_file docs/plans/phase-270-recipient-artifact-transfer.md "recipient transfer plan"
assert_grep_present '^## D-486 ' docs/decisions.md "recipient transfer decision"
log_file="$(mktemp)"
trap 'rm -f "$log_file"' EXIT
assert_go_tests_pass "$log_file" '-race -p 1 ./internal/artifacts/transfer ./internal/protocol/transports/control ./internal/sessions ./internal/runtime/serve' \
  'direct native byte movement and erasure fences' \
  TestFinalAnswer_ProductionWiringExactSealedProvenance \
  TestTransfer_StorageTriadDurableReplayAndErasure \
  TestTransfer_DirectHTTP_ByteExactReplayAndIsolation \
  TestTransfer_ConcurrentOwners \
  TestTransfer_ErasureBlocksLateReceiptPersistence \
  TestMaterializeAnswer_ExactBytesReplayAndDeletedOutput \
  TestArtifactTransfer_AuthenticatedNativeProtocolRoundTrip \
  TestCascadeErasure_FencesPausedArtifactWriter
# Anonymous attempts may never admit transfers or materialize final answers.
# Configured-peer positive coverage is exercised by the actual HTTP test above.
for method in prepare_import transfer transfer_status revoke_transfer export_answer; do
  assert_post_status 401 "$(api_url "/v1/control/artifacts.${method}")" '{}' \
    "artifact ${method} requires authenticated owner"
done
smoke_summary
