#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file "docs/plans/phase-233c-bifrost-reasoning-fidelity.md" "phase 233c plan exists"
assert_grep_present 'HA-51' "docs/plans/phase-233c-bifrost-reasoning-fidelity.md" "HA-51 is planned"
assert_grep_present 'D-402' "docs/decisions.md" "D-402 is recorded"
assert_grep_present 'byte-exact concatenation' "docs/plans/phase-233c-bifrost-reasoning-fidelity.md" "raw source bytes are authoritative"
assert_grep_present 'decoded JSON/SSE' "docs/plans/phase-233c-bifrost-reasoning-fidelity.md" "decoder regression is binding"
assert_grep_present 'durable `state.history`' "docs/plans/phase-233c-bifrost-reasoning-fidelity.md" "restart oracle is durable"
assert_grep_present 'No Protocol method, wire type' "docs/plans/phase-233c-bifrost-reasoning-fidelity.md" "scope stays zero-wire"

skip "phase 233c: implementation tests pending; static D-402 design guards ran"
smoke_summary
