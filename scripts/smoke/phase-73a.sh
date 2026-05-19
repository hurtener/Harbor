#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 73a — Console Overview page (UI composition).
#
# This phase ships no new Protocol method — it composes Stage-1 primitives
# from 72d / 72e / 72f / 72a + Stage-2 73d's tasks.list + shipped approve /
# reject (Phase 54). Smoke probes the upstream methods (SKIPped today via
# protocol_call) and the page route (SKIPped until 73m's harbor console
# subcommand lands).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Upstream primitives consumed by the Overview page.
protocol_call 'runtime/counters' '{}' \
  'phase 73a: runtime.counters surface (Phase 72f) consumed by counter row'
protocol_call 'runtime/health' '{}' \
  'phase 73a: runtime.health surface (Phase 72f) consumed by health chips'
protocol_call 'pause/list' '{}' \
  'phase 73a: pause.list surface (Phase 72e) consumed by intervention queue'
protocol_call 'events/subscribe' \
  '{"filter": {"event_types": ["notification.task_failed", "tool.failed"]}}' \
  'phase 73a: events.subscribe with notification filter (Phase 72d) consumed by alert ribbon'
protocol_call 'tasks/list' '{"filter": {"statuses": ["running"]}}' \
  'phase 73a: tasks.list (Phase 73d) consumed by Tasks Running counter'

# Page route — lands with 73m harbor console subcommand.
skip 'phase 73a: /console/overview route lands with 73m harbor console subcommand'

# Surface-existence probe — flips from SKIP to OK once the Protocol layer ships.
skip_if_404 "$(api_url /protocol/runtime/counters)" \
  'phase 73a: runtime.counters route absent until 72f ships' || true

smoke_summary
