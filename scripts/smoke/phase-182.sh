#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 182 — TUI conversation and session experience (D-318).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 182: smoke skeleton — replace with attach, conversation, session, and bounded PTY assertions"

smoke_summary
