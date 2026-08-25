#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 260 smoke — reach-admitted agent-bound external grants.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file "docs/plans/phase-260-agent-bound-external-grants.md" "phase 260 plan exists"
assert_grep_present '^|260 | Reach-admitted agent-bound external grants' "docs/plans/README.md" "phase 260 canonical index row exists"
assert_grep_present '## D-440' "docs/decisions.md" "D-440 decision exists"
assert_grep_present '## HA-75' "docs/notes/downstream-asks.md" "HA-75 register entry exists"
assert_grep_present 'ExternalGrantVersionAgentBound' "sdk/llm/llm.go" "public v2 grant constant exists"
assert_grep_present 'EffectiveAgentConfigFrom' "internal/llm/grant/grant.go" "reference verifier consumes admitted agent"
assert_grep_present 'legacy grant cannot carry an unsigned agent binding' "internal/llm/grant/grant.go" "v1 cannot pretend agent authority"
assert_grep_present 'grant agent binding mismatch' "internal/llm/external_grant.go" "receipt binds exact v2 agent"
assert_grep_present 'TestRunLoop_V2GrantBindsExplicitAndDefaultReachAdmissionsAndRejectsForgedTaskAgent' "internal/runtime/serve/external_grant_agent_binding_test.go" "real explicit/default/forged run-loop acceptance exists"
assert_grep_present 'TestDispatchStart_NamedAgent_TwoCheckRule' "internal/protocol/control_agent_test.go" "control.start explicit/default sealed reach producer remains pinned"
assert_grep_present 'TestSignerVerifier_V2RequiresExactReachAdmittedAgentForBothRoutes' "internal/llm/grant/grant_test.go" "both v2 route modes are covered"
assert_grep_present 'TestVerifier_V2ConcurrentAgentsDoNotBleed' "internal/llm/grant/grant_test.go" "multi-agent concurrent isolation exists"
assert_grep_present 'TestUnmarshalCanonicalAttemptUsageReceipt_RoundTripsAgentBoundWireWithoutChangingOldBytes' "internal/llm/external_grant_receipt_parse_test.go" "old/new canonical receipt compatibility exists"
assert_grep_present 'SupportedGrantVersions' "internal/protocol/types/posture.go" "readiness advertises grant versions"
assert_grep_present 'AgentBinding' "internal/protocol/types/posture.go" "readiness advertises agent binding"
assert_grep_present 'No claim that arbitrary ungranted auxiliary or embedder LLM calls' "docs/plans/phase-260-agent-bound-external-grants.md" "scope boundary remains honest"

smoke_summary
