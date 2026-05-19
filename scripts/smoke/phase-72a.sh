#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 72a — events.subscribe filter extensions + events.aggregate.
#
# Surface assertions (executed only when the surface is live; 404/405/501
# auto-SKIP per AGENTS.md §4.2):
#   1. events/subscribe accepts an EventFilter and returns a cursor.
#   2. events/aggregate returns time-bucketed counts.
#   3. cross-tenant filter without auth.ScopeAdmin claim → 403.
#   4. missing identity in subscriber context → 401.
#
# Until the phase ships, `protocol_call` stubs the calls (SKIP); when the
# phase lands, replace `protocol_call` invocations with real `curl` /
# `assert_status` / `assert_json_path` calls per the assertions above.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. events/subscribe with filter — happy path.
protocol_call 'events/subscribe' \
  '{"filter": {"event_types": ["tool.failed"]}}' \
  'phase 72a: events.subscribe accepts EventFilter'

# 2. events/aggregate — bucket count.
protocol_call 'events/aggregate' \
  '{"filter": {"event_types": ["tool.failed"]}, "window": "1h", "bucket": "1m"}' \
  'phase 72a: events.aggregate returns bucket series'

# 3. cross-tenant filter without scope claim → expect 403.
protocol_call 'events/subscribe' \
  '{"filter": {"tenant_ids": ["t1", "t2"]}}' \
  'phase 72a: events.subscribe rejects cross-tenant filter without auth.ScopeAdmin claim'

# 4. missing identity context → expect 401.
protocol_call 'events/aggregate' \
  '{}' \
  'phase 72a: events.aggregate rejects missing identity context'

# Surface-existence probe — when the protocol layer ships, this flips from
# SKIP to OK and the protocol_call stubs above are replaced with real
# assert_status/assert_json_path calls.
skip_if_404 "$(api_url /protocol/events/aggregate)" \
  'phase 72a: events.aggregate route absent until Protocol layer ships' || true

smoke_summary
