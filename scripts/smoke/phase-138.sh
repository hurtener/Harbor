#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 138 smoke — `harbor dev` hot-reload Go-source honesty (D-268).
#
# The LIVE edit-and-observe assertion (write a `.go` file under a watched
# root against the running preflight dev server; assert the honest WARN
# fires and NO in-process rebuild / `dev.hot_reload.completed` happens)
# lives in scripts/smoke/phase-65.sh — the hot-reload supervisor's smoke
# — because that is the live-server surface this phase corrects. A static
# `strings` grep CANNOT distinguish the false-success (the `completed`
# string stays in the binary for the YAML rebuild path), so the live
# assertion is the load-bearing gate.
#
# This static guard pins the SOURCE routing so the honest path cannot be
# silently removed, and asserts the live assertion still exists in
# phase-65.sh so the live gate cannot be silently dropped. The behavioural
# proof is in cmd/harbor/cmd_dev_hot_reload_test.go
# (TestHotReloadSupervisor_GoSourceChange_WarnsNoRebuild).
#
# Conventions (AGENTS.md §4.2): use scripts/smoke/common.sh helpers; FAIL = 0.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

HR_SRC="cmd/harbor/cmd_dev_hot_reload.go"

# 1. The watcher classifies a `.go` change to the WARN-and-guide path.
assert_grep_present 'reloadGoSourceWarn' "${HR_SRC}" \
    'phase 138: classifyEvent routes .go changes to reloadGoSourceWarn'

# 2. The honest WARN guidance (the operator-visible log line) exists.
assert_grep_present 'Go source change detected' "${HR_SRC}" \
    'phase 138: honest .go WARN guidance present (no false hot-reload success)'

# 3. The WARN helper exists and is wired (does NOT reboot / emit completed).
assert_grep_present 'func \(s \*hotReloadSupervisor\) warnGoSourceChange' "${HR_SRC}" \
    'phase 138: warnGoSourceChange helper declared'

# 4. shouldTrigger is the rebuild-gate predicate over classifyEvent (so a
#    `.go` change cannot reach the rebuild path).
assert_grep_present 'classifyEvent\(ev\) == reloadRebuild' "${HR_SRC}" \
    'phase 138: shouldTrigger gates rebuilds on reloadRebuild only'

# 5. The LIVE edit-and-observe assertion lives in phase-65.sh — guard it so
#    the live gate is not silently dropped.
assert_grep_present 'hot-reload: Go source change detected' "scripts/smoke/phase-65.sh" \
    'phase 138: live .go-edit honesty assertion present in phase-65.sh'

smoke_summary
