#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 275 — signed method restriction and durable session admission.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
source scripts/smoke/common.sh
assert_file docs/plans/phase-275-scoped-session-admission.md "scoped admission plan exists"
if go test -race -p 1 ./internal/protocol/auth ./internal/runtime/sessionadmission ./internal/protocol ./internal/protocol/client ./internal/tools/auth ./internal/runtime/runs/protocol ./internal/sessions/protocol ./test/integration \
 -run 'Test(.*(MethodReach|MethodMiddleware|SessionAdmission|ScopedAudience|ScopedToken|Restricted|Admission|SessionsSetAdmission|CallbackHandler_Enrolled)|UserSignedOAuthMCPCapability_MethodReach)' -count=1; then
 ok "signed method, audience, session acceptance and native callback contracts pass"
else
 fail "scoped session admission regression"
fi
smoke_summary
