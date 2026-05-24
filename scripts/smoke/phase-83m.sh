#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 83m — eight WARN-cleanup items. D-156.
#
# Bucket A: items 1, 2, 3, 4, 6 (cmd/harbor + tool drivers).
# Bucket B: items 5, 7, 8 (internal/llm + tasks + steering + planner).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Item 1 — MCP DefaultIdentity per-push.
# ----------------------------------------------------------------------------
assert_grep_present 'identity\.From\(ctx\)' "internal/tools/drivers/mcp/mcp.go" \
    "mcp driver reads identity from ctx (item 1: per-push instead of cached DefaultIdentity)"

# ----------------------------------------------------------------------------
# Item 2 — sqlite-main-file watcher extends skip list.
# ----------------------------------------------------------------------------
assert_grep_present '"\.sqlite"' "cmd/harbor/cmd_dev_hot_reload.go" \
    "hot-reload watcher skips main .sqlite files (item 2)"
assert_grep_present '"\.db"' "cmd/harbor/cmd_dev_hot_reload.go" \
    "hot-reload watcher skips main .db files (item 2)"

# ----------------------------------------------------------------------------
# Item 3 — draftStore + agentRegistry closers wired.
# ----------------------------------------------------------------------------
assert_grep_present 'draftStore\.Close' "cmd/harbor/cmd_dev.go" \
    "bootDevStack registers draftStore.Close in the closer chain (item 3)"
assert_grep_present 'agentRegistry\.Close' "cmd/harbor/cmd_dev.go" \
    "bootDevStack registers agentRegistry.Close in the closer chain (item 3)"

# ----------------------------------------------------------------------------
# Item 4 — Skills query keyword extraction.
# ----------------------------------------------------------------------------
assert_grep_present 'extractSkillKeywords' "cmd/harbor/cmd_dev_runloop.go" \
    "skills query keyword extractor declared (item 4)"
assert_grep_present 'extractSkillKeywords' "harbortest/devstack/devstack.go" \
    "devstack mirror uses extractSkillKeywords (D-094 mirror, item 4)"

# ----------------------------------------------------------------------------
# Item 5 — Per-call LLM timeout uses cfg.Timeout.
# ----------------------------------------------------------------------------
assert_grep_present 'c\.cfg\.Timeout' "internal/llm/safety.go" \
    "safety wrapper prefers cfg.Timeout over the default fallback (item 5)"

# ----------------------------------------------------------------------------
# Item 6 — Catalog GrantedScopes plumb-through.
# ----------------------------------------------------------------------------
assert_grep_present 'GrantedScopes\s*\[\]string' "internal/config/config.go" \
    "ToolsConfig.GrantedScopes field declared (item 6)"
assert_grep_present 'cfg\.Tools\.GrantedScopes' "cmd/harbor/cmd_dev.go" \
    "bootDevStack reads GrantedScopes from config (item 6)"
assert_grep_present 'grantedScopes' "cmd/harbor/cmd_dev_runloop.go" \
    "cmd_dev_runloop threads grantedScopes through to runtimeCatalogView (item 6)"
assert_grep_present '### tools\.granted_scopes' "docs/CONFIG.md" \
    "CONFIG.md documents the new tools.granted_scopes field (item 6)"

# ----------------------------------------------------------------------------
# Item 7 — task.tool_count.
# ----------------------------------------------------------------------------
assert_grep_present 'ToolCount\s*int' "internal/tasks/tasks.go" \
    "tasks.Task.ToolCount field declared (item 7)"
assert_grep_present 'IncrementToolCount' "internal/tasks/tasks.go" \
    "TaskRegistry.IncrementToolCount method declared (item 7)"
assert_grep_present 'IncrementToolCount' "internal/tasks/drivers/inprocess/inprocess.go" \
    "inprocess driver implements IncrementToolCount (item 7)"
assert_grep_present 'ToolCount' "internal/tasks/protocol/registry_projector.go" \
    "projectRow projects ToolCount onto the wire (item 7)"

# ----------------------------------------------------------------------------
# Item 8 — reasoning trace round-trip.
# ----------------------------------------------------------------------------
assert_grep_present 'ReasoningTrace' "internal/runtime/steering/runloop.go" \
    "runloop populates Step.ReasoningTrace on trajectory append (item 8)"

smoke_summary
