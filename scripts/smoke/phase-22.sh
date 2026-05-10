#!/usr/bin/env bash
# Phase 22 smoke skeleton — MessageBus + RemoteTransport (V1 contracts).
#
# Phase 22 will ship internal/distributed: the at-least-once
# MessageBus interface + an in-process loopback driver, AND the
# RemoteTransport interface designed against the full A2A v1 spec
# (vendored at docs/specifications/a2a.proto). The actual A2A wire
# driver lands in Phase 29 (southbound). Until the implementation
# lands, this smoke is skip-only. Replaces with:
#
#   if go test -race -count=1 -timeout 90s ./internal/distributed/... >/dev/null 2>&1; then
#       ok 'phase 22: internal/distributed tests pass under -race'
#   else
#       fail 'phase 22: internal/distributed tests failed'
#   fi

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 22: distributed contracts (A2A v1) — implementation pending; smoke skeleton awaits go-test wiring"
skip "phase 22: distributed contracts have no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
