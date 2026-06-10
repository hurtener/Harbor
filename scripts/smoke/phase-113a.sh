#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 113a smoke — Protocol adoption track: the generated contract
# reference + the executed quickstart.
# (RFC §5, §3.6; docs/plans/phase-113a-protocol-reference-and-quickstart.md)
#
# At ship this script carries two assertion classes:
#   1. Static, phase-103-style site trip-wires: the four generated pages
#      under docs/site/protocol/ exist AND carry the generated-file
#      header; the hand-written quickstart + choreography guides 1-3
#      exist; the Protocol nav section is wired in
#      docs/site/.vitepress/config.ts; the Makefile has
#      `protocol-docs-gen-check:`; the docs workflow invokes it;
#      CLAUDE.md §18 names the generated reference; the README Docs
#      table carries the Protocol row; each choreography guide's
#      demonstrated method names resolve against the generated
#      methods.md (lockstep greps).
#   2. Live — the EXECUTED quickstart (the recipe-cannot-lie pattern):
#      extract the quickstart's marker-tagged curl blocks and run them
#      in order against the booted preflight dev server
#      (HARBOR_BASE_URL + HARBOR_DEV_TOKEN): bootstrap → start → SSE
#      tail (bounded) → tasks.get → one steering call, asserting status
#      + JSON shape per step.
#
# Conventions (AGENTS.md §4.2): 404/405/501 → SKIP on pre-113a builds;
# once shipped, OK ≥ the quickstart's step count and FAIL = 0.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 113a: smoke skeleton — replace with the site trip-wires + the executed quickstart when the phase implements its surface"

smoke_summary
