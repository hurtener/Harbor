#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 73i — Console Flows page (Protocol + UI).
#
# Surface assertions (404/501 auto-SKIP per AGENTS.md §4.2):
#   1. The six `POST /v1/flows/*` routes are mounted and identity-gated.
#      An unauthenticated POST reaches the handler and fails closed with
#      HTTP 401 (CodeIdentityRequired) — proving the route is alive AND
#      that identity is mandatory (CLAUDE.md §6 rule 9).
#   2. Page route /console/flows — SKIPped until 73m's `harbor console`
#      subcommand lands.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. The six flows.* routes are mounted + identity-mandatory. Without a
#    bearer JWT the handler resolves no identity and fails closed 401.
assert_post_status 401 "$(api_url /v1/flows/list)" '{}' \
  'phase 73i: flows.list route mounted + identity-mandatory'
assert_post_status 401 "$(api_url /v1/flows/describe)" '{"id":"f1"}' \
  'phase 73i: flows.describe route mounted + identity-mandatory'
assert_post_status 401 "$(api_url /v1/flows/runs/list)" '{"flow_id":"f1"}' \
  'phase 73i: flows.runs.list route mounted + identity-mandatory'
assert_post_status 401 "$(api_url /v1/flows/runs/describe)" '{"run_id":"r1"}' \
  'phase 73i: flows.runs.describe route mounted + identity-mandatory'
assert_post_status 401 "$(api_url /v1/flows/run)" '{"flow_id":"f1","inputs":{}}' \
  'phase 73i: flows.run route mounted + identity-mandatory'
assert_post_status 401 "$(api_url /v1/flows/metrics)" '{"flow_id":"f1"}' \
  'phase 73i: flows.metrics route mounted + identity-mandatory'

# 2. Console page route — lands with the 73m `harbor console` subcommand.
skip 'phase 73i: /console/flows route lands with 73m harbor console subcommand'

smoke_summary
