#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 84a smoke — runtime-capability gate + session aggregates
# (round-8 F1 + F8 closeout).
#
# Asserts:
#   1. `runtime.info` response includes a `capabilities` array.
#   2. On a planner/RunLoop runtime (the dev posture), `topology_snapshot`
#      is ABSENT from `capabilities` — the gate the Console reads to
#      skip its `topology.snapshot` fetch on Live Runtime + Playground.
#   3. `topology.snapshot` itself still returns 404 / `unknown_method`
#      — the advertisement is the Console gate, the wire stays
#      unchanged.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

INFO_URL="$(api_url /v1/control/runtime.info)"
INFO_BODY='{"identity":{"tenant":"dev","user":"dev","session":"dev"}}'

# 1 — capabilities array present + 2 — topology_snapshot absent on dev.
assert_json_truthy \
  "${INFO_URL}" \
  "${INFO_BODY}" \
  ".capabilities | type == \"array\" and (index(\"topology_snapshot\") | not)" \
  "runtime.info advertises capabilities[] with topology_snapshot ABSENT (planner/RunLoop dev runtime)"

# 3 — topology.snapshot wire response stays the unknown_method shape.
TOPO_URL="$(api_url /v1/control/topology.snapshot)"
TOPO_BODY='{"identity":{"tenant":"dev","user":"dev","session":"dev"}}'
skip_if_404 \
  "${TOPO_URL}" \
  "${TOPO_BODY}" \
  "topology.snapshot stays 404 on planner/RunLoop runtime (advertisement is the Console gate)"

smoke_summary
