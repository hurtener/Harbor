#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 83h — dev-binary fixes (D-151). Two bugs from the v1.1
# operator validation: V1 (hot-reload watcher reboot-loops on SQLite
# WAL/SHM/journal sidecars) and V2 (LLM safety wrapper rejects
# requests with empty Model — react planner never sets it). Both
# fixes covered by unit tests; this static smoke pins the surfaces.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# V1 — hot-reload watcher skips DB sidecars.
# ----------------------------------------------------------------------------
assert_grep_present 'dbSidecarSuffixes' "cmd/harbor/cmd_dev_hot_reload.go" \
    "hot-reload watcher declares the dbSidecarSuffixes list (D-151 V1)"
assert_grep_present 'isDBSidecar' "cmd/harbor/cmd_dev_hot_reload.go" \
    "hot-reload watcher uses isDBSidecar in shouldTrigger (D-151 V1)"
assert_grep_present '\.sqlite-wal' "cmd/harbor/cmd_dev_hot_reload.go" \
    "dbSidecarSuffixes covers the SQLite WAL extension"
assert_grep_present 'TestShouldTrigger_SkipsDBSidecars' \
    "cmd/harbor/cmd_dev_hot_reload_test.go" \
    "unit test for the DB-sidecar skip exists"

# ----------------------------------------------------------------------------
# V2 — LLM safety wrapper defaults req.Model from cfg.Model.
# ----------------------------------------------------------------------------
assert_grep_present 'req\.Model = c\.cfg\.Model' "internal/llm/safety.go" \
    "safety.go defaults CompleteRequest.Model from cfg.Model (D-151 V2)"
assert_grep_present 'TestSafety_DefaultsModelFromConfigSnapshot' \
    "internal/llm/safety_test.go" \
    "unit test for the default-Model fill exists"

smoke_summary
