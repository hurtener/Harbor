#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 111f smoke — telemetry assembly + approval-gate authorizer seam
# (D-203). (RFC §6.14 / §6.4 / §5.1;
# docs/plans/phase-111f-telemetry-assembly-approval-seam.md)
#
# Static + unit-test assertions:
#   1. Direction-rule tripwire: `internal/tools/approval` imports NO
#      `internal/protocol/auth`; apply.go carries no
#      `protocolauth.WithScopes` self-elevation. (The mechanical gate
#      until a depguard rule lands — D-203.)
#   2. Telemetry assembled in production: `telemetry.New` +
#      `telemetry.NewTracer` + `BridgeBusToTracer` are called from the
#      promoted assembly; the flow seam forwards the run-error handler.
#   3. The seam surfaces exist: the trace bridge, the approval
#      ResolveAuthorizer + identity default, the Protocol-side adapter
#      injected by BOTH serving callers (cmd + devstack).
#   4. The recipe ships and is indexed, referencing real symbols.
#   5. The focused unit + integration slices pass under -race.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# --- 1. Direction rule: protocol auth OUT of the runtime gate --------------

# Import-line grep (godoc prose may NAME the package while explaining
# the rule; the import path string is what the rule forbids).
if grep -rn '"github.com/hurtener/Harbor/internal/protocol/auth"' internal/tools/approval/*.go >/dev/null 2>&1; then
    fail 'phase 111f: internal/tools/approval still imports internal/protocol/auth (D-203 direction rule violated)'
else
    ok 'phase 111f: internal/tools/approval has NO protocol/auth import (D-203 direction rule)'
fi
assert_grep_absent 'protocolauth\.WithScopes' internal/runtime/steering/apply.go \
    'phase 111f: the steering bridge self-elevation is deleted'
assert_grep_absent 'github.com/hurtener/Harbor/internal/protocol/auth' internal/runtime/steering/apply.go \
    'phase 111f: apply.go imports no protocol auth vocabulary'

# --- 2. Telemetry assembled in production ----------------------------------

assert_grep_present 'telemetry\.New\(cfg\.Telemetry, red' internal/runtime/assemble/assemble.go \
    'phase 111f: telemetry.New has its production caller (the assembly)'
assert_grep_present 'telemetry\.NewTracer\(cfg\.Telemetry' internal/runtime/assemble/assemble.go \
    'phase 111f: NewTracer constructed on the production path'
assert_grep_present 'telemetry\.BridgeBusToTracer\(' internal/runtime/assemble/assemble.go \
    'phase 111f: the bus→tracer bridge starts in the assembly'
assert_grep_present 'RunErrorHandler' internal/runtime/assemble/assemble.go \
    'phase 111f: the engine run-error handler is built by the assembly'
assert_grep_present 'func WithRunErrorHandler\(h engine\.RunErrorHandler\) ComposeOption' internal/runtime/flow/flow.go \
    'phase 111f: the flow seam forwards a run-error handler'

# --- 3. The seam surfaces exist --------------------------------------------

assert_file internal/telemetry/tracebridge.go 'phase 111f: BridgeBusToTracer file'
assert_grep_present 'func BridgeBusToTracer\(ctx context\.Context, bus events\.EventBus, tracer \*Tracer, f events\.Filter\) \(stop func\(\), err error\)' \
    internal/telemetry/tracebridge.go 'phase 111f: BridgeBusToTracer signature (symmetric with metrics)'
assert_file internal/tools/approval/authorizer.go 'phase 111f: the ResolveAuthorizer seam file'
assert_grep_present 'AuthorizeResolve\(ctx context\.Context, pending PendingInfo\) error' \
    internal/tools/approval/authorizer.go 'phase 111f: ResolveAuthorizer interface'
assert_grep_present 'Authorizer ResolveAuthorizer' internal/tools/approval/gate.go \
    'phase 111f: GateDeps carries the injected authorizer'
assert_file internal/server/approval_authorizer.go 'phase 111f: the Protocol-side adapter (server-owned)'
assert_grep_present 'server\.NewProtocolScopeAuthorizer\(toolapproval\.NewIdentityAuthorizer\(\)\)' cmd/harbor/cmd_dev.go \
    'phase 111f: cmd injects the wire-side authorizer at gate assembly'
assert_grep_present 'server\.NewProtocolScopeAuthorizer\(toolapproval\.NewIdentityAuthorizer\(\)\)' harbortest/devstack/devstack.go \
    'phase 111f: devstack mirrors the production injection'

# --- 4. The recipe ships ----------------------------------------------------

assert_file docs/recipes/observe-an-embedded-runtime.md 'phase 111f: the observability recipe'
assert_grep_present 'telemetry\.BridgeBusToTracer' docs/recipes/observe-an-embedded-runtime.md \
    'phase 111f: recipe wires the trace bridge'
assert_grep_present 'telemetry\.WithBusEmitter\(eventbus\.New\(bus\)\)' docs/recipes/observe-an-embedded-runtime.md \
    'phase 111f: recipe wires the bus-paired Logger'
assert_grep_present 'observe-an-embedded-runtime' docs/recipes/README.md 'phase 111f: recipe indexed'

# --- 5. The focused unit + integration slices pass under -race --------------

if go test ./internal/telemetry/ ./internal/tools/approval/ ./internal/server/ \
    -run 'Bridge|Authoriz' -race -count=1 -timeout 300s >/dev/null 2>&1; then
    ok 'phase 111f: trace-bridge + authorizer unit slices pass under -race'
else
    fail 'phase 111f: trace-bridge / authorizer unit slices failed (run the go test line in scripts/smoke/phase-111f.sh for detail)'
fi

if go test ./test/integration/ -run 'Phase111f' -race -count=1 -timeout 600s >/dev/null 2>&1; then
    ok 'phase 111f: telemetry + approval-seam E2Es pass under -race'
else
    fail 'phase 111f: Phase111f integration E2Es failed (run: go test ./test/integration/ -run Phase111f -race)'
fi

smoke_summary
