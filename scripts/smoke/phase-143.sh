#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 143 smoke — run-level structured output (WithOutputSchema +
# answer_payload, D-272).
#
# There is no new HTTP / Protocol surface: the option threads through
# RunOnce and the additive answer_payload rides the existing envelope as
# opaque JSON. Correctness is verified by the race-enabled Go test suite
# (the option validation + runtime-edge validation + stream suppression +
# the D-025 mixed-traffic stress + the §13 consumer E2E). A static grep
# pins that answer_payload appears in exactly one wire-shape definition.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. The assemble runner + envelope + planner schema surface under -race.
if go test -race -count=1 -timeout 120s \
	./internal/runtime/assemble/... \
	./internal/runtime/runctx/... \
	./internal/planner/ >/dev/null 2>&1; then
	ok 'phase 143: assemble + runctx + planner output-schema tests pass under -race'
else
	fail 'phase 143: output-schema unit tests failed (run `go test -race ./internal/runtime/assemble/... ./internal/planner/`)'
fi

# 2. The §13 primitive-with-consumer E2E (happy / corrective-retry /
#    exhaustion / planner-agnostic deterministic leg) under -race.
if go test -race -count=1 -timeout 120s -run '^TestE2E_Phase143_' ./test/integration/... >/dev/null 2>&1; then
	ok 'phase 143: structured-output consumer E2E passes (TestE2E_Phase143_*)'
else
	fail 'phase 143: consumer E2E failed (run `go test -race -run TestE2E_Phase143_ ./test/integration/...`)'
fi

# 3. Static: answer_payload is defined in EXACTLY ONE wire-shape (the
#    AnswerEnvelope). A second `json:"answer_payload..."` tag would mean a
#    duplicate wire definition (§13: single source for a wire shape).
payload_tag_count="$(grep -rn 'json:"answer_payload' internal/ | grep -c 'AnswerPayload' || true)"
if [ "${payload_tag_count}" = "1" ]; then
	ok 'phase 143: answer_payload wire tag defined in exactly one place (AnswerEnvelope)'
else
	fail "phase 143: answer_payload wire tag appears ${payload_tag_count} times, want exactly 1 (single wire-shape source)"
fi

# 4. Static: a schema-invalid answer maps to the typed ErrOutputInvalid at
#    the runtime edge (no silent text fallback, §13). Assert the sentinel
#    exists and the runner references it.
if grep -q 'ErrOutputInvalid' internal/planner/output_schema.go &&
	grep -q 'planner.ErrOutputInvalid' internal/runtime/assemble/runonce.go; then
	ok 'phase 143: schema-invalid answer maps to the typed ErrOutputInvalid (no silent text fallback)'
else
	fail 'phase 143: ErrOutputInvalid wiring missing from the runtime edge'
fi

smoke_summary
