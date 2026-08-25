#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 263 smoke — content-free user-scoped turn usage projection.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file "docs/plans/phase-263-content-free-turn-usage-projection.md" "phase 263 plan exists"
assert_grep_present '^|263 | Content-free user-scoped turn usage projection' "docs/plans/README.md" "phase 263 canonical index row exists"
assert_grep_present '## D-443' "docs/decisions.md" "D-443 decision exists"
assert_grep_present 'ProjectionUsage Projection = "usage"' "internal/sessions/turns/protocol/protocol.go" "usage selector is explicit"
assert_grep_present 'type SessionUsageTurnRow struct' "internal/protocol/types/session_turns.go" "usage wire row is structurally distinct"
assert_grep_present 'TestSessionUsageTurnRow_JSONContentFreeContract' "internal/protocol/types/session_turns_usage_test.go" "content-free JSON contract is pinned"
assert_grep_present 'foreign usage get with admin scope' "internal/protocol/transports/stream/session_turns_handler_test.go" "admin widening is refused"
assert_grep_present 'row, err := s.projector.Get' "internal/sessions/turns/protocol/protocol.go" "usage reuses one indexed turn read"
assert_grep_present 'SessionUsageTurnRow' "docs/site/protocol/types.md" "generated Protocol reference includes usage row"
assert_grep_present 'projection: "usage"' "docs/skills/use-the-harbor-protocol/SKILL.md" "operator skill documents usage selector"

smoke_summary
