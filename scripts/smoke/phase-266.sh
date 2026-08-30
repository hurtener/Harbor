#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 266 smoke — indexed fan-out and ordered observability event path.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"
assert_file "docs/plans/phase-266-ordered-observability-event-path.md" "phase 266 plan exists"
assert_grep_present '^\|266 \| Indexed fan-out and ordered observability event path' "docs/plans/README.md" "phase 266 index exists"
assert_grep_present '^## D-453 ' "docs/decisions.md" "D-453 exists"
assert_grep_present '^## D-454 ' "docs/decisions.md" "D-454 exists"
assert_grep_present '^## D-455 ' "docs/decisions.md" "D-455 exists"
assert_grep_present 'one bounded.*FIFO|one ordered bounded.*FIFO' "docs/decisions.md" "shared FIFO is explicit"
assert_grep_present 'Abrupt process loss before commit' "docs/decisions.md" "process-loss limit is explicit"
assert_grep_present 'no second transcript|No second transcript' "docs/plans/phase-266-ordered-observability-event-path.md" "no second transcript"
assert_grep_present 'sessions.turns.\*.*live_resume_seq' "docs/plans/phase-266-ordered-observability-event-path.md" "turns/resume boundary"
assert_file "internal/llm/phase266_async_telemetry_test.go" "async cost gate exists"
assert_file "test/integration/phase266_event_ordering_test.go" "ordering gate exists"
assert_file "test/integration/phase266_tool_lifecycle_test.go" "tool gate exists"
assert_file "test/benchmarks/phase266_fanout_bench_test.go" "fanout benchmark exists"
assert_grep_present 'TestPhase266_TerminalPublishCannotOvertakeQueuedTelemetry' "internal/llm/phase266_async_telemetry_test.go" "terminal barrier gate"
assert_grep_present 'BenchmarkPhase266FanOutIdentityIsolation' "test/benchmarks/phase266_fanout_bench_test.go" "identity benchmark"
smoke_summary
