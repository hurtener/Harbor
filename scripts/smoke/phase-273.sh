#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
# Phase 273 — immutable native task-output provenance.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
source scripts/smoke/common.sh
assert_file docs/plans/phase-273-native-output-provenance.md "native output provenance plan"
assert_grep_present '^## D-489 ' docs/decisions.md "native output provenance decision"
log_file="$(mktemp)"
trap 'rm -f "$log_file"' EXIT
assert_go_tests_pass "$log_file" '-race -p 1 ./internal/tasks/engine ./internal/tasks/drivers/durable ./internal/runtime/dispatch ./internal/tools/drivers/mcp ./internal/sessions/turns' \
 'native binary provenance and immutable projection' \
 TestOutputWitness_PersistenceUncertaintyStopsCompletion \
 TestDurable_OutputWitnessRestartStorageTriad \
 TestOutputWitness_NativeAndForgedAndObservationGap \
 TestOutputWitness_SharedExecutorParallel100 \
 TestNativeMCPBinaryOutputWitness_FirstConsumer \
 TestOutputManifest_UnknownEmptyAndImmutable
smoke_summary
