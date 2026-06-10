#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 113b smoke — Protocol adoption track: the pause + versioning
# choreographies, build-a-client, and conformance certification.
# (RFC §5, §3.3, §6.3; docs/plans/phase-113b-protocol-choreographies-and-certification.md)
#
# At ship this script carries (all static — flip the header to
# live-server only if wire-touching assertions are added):
#   1. Site trip-wires: docs/site/protocol/{pause-model,
#      versioning-and-compatibility,build-a-client,
#      conformance-certification}.md exist; the Protocol nav section in
#      docs/site/.vitepress/config.ts lists them.
#   2. Lockstep greps: every method (approve, reject, resume,
#      pause.list, ...), event (pause.requested, pause.resumed), and
#      route (/v1/tools/oauth/callback) the pause guide names resolves
#      against 113a's generated reference; the versioning guide names
#      the pinned-version surface.
#   3. The Q3 guard: the certification page does NOT promise an
#      sdk-exported runner (grep-absent).
#   4. The worked-client compile gate: build
#      examples/protocol-clients/event-viewer/ (bounded; the
#      phase-112b external-gate pattern at miniature scale).
#
# Conventions (AGENTS.md §4.2): missing surface → SKIP on pre-113b
# builds; once shipped, OK ≥ count(acceptance criteria covered), FAIL = 0.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 113b: smoke skeleton — replace with the site trip-wires + the worked-client compile gate when the phase implements its surface"

smoke_summary
