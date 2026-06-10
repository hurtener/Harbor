#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 110b smoke — RunContext population + event-closure promotion
# (D-195).
#
# Static guards: the promoted internal/runtime/runctx package + the
# two event-closure constructors exist; every package-main / devstack
# duplicate of the five helpers is deleted (grep-asserted on the
# definitions); devstack's RunSpec wires Emit + OnChunk and its
# MarkComplete carries the planner.AnswerEnvelope; the focused test
# slice passes under -race.
#
# Conventions (AGENTS.md §4.2): 404/405/501 → SKIP; ≥1 OK once shipped;
# use scripts/smoke/common.sh helpers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. The promoted package + the two constructor files exist.
assert_file "internal/runtime/runctx/runctx.go" "promoted runctx package exists"
assert_file "internal/events/emitter.go" "events.IdentityStampingEmitter file exists"
assert_file "internal/llm/chunk_publisher.go" "llm.NewChunkPublisher file exists"
assert_grep_present 'func IdentityStampingEmitter' "internal/events/emitter.go" \
    "events.IdentityStampingEmitter exported"
assert_grep_present 'func NewChunkPublisher' "internal/llm/chunk_publisher.go" \
    "llm.NewChunkPublisher exported"
assert_grep_present 'scheduled for deletion by Phase 111d \(D-201\)' "internal/runtime/runctx/runctx.go" \
    "ExtractSkillKeywords carries the 111d deprecation notice (owner decision 2026-06-09)"

# 2. cmd/harbor no longer DEFINES the five helpers (it calls the
#    promoted surface). Patterns pin the definitions so the thin
#    `runctx.*` call sites don't trip the absence check.
for def in 'func projectMemoryBlocks' 'func projectSkillsContext' \
    'func extractSkillKeywords' 'func extractAssistantAnswer' \
    ') resolveInputArtifacts(' 'skillKeywordStopwords'; do
    assert_grep_absent "${def}" "cmd/harbor/cmd_dev_runloop.go" \
        "cmd runloop no longer defines '${def}'"
done
assert_grep_present 'runctx\.ProjectMemoryBlocks' "cmd/harbor/cmd_dev_runloop.go" \
    "cmd runloop calls runctx.ProjectMemoryBlocks"
assert_grep_present 'runctx\.ResolveInputArtifacts' "cmd/harbor/cmd_dev_runloop.go" \
    "cmd runloop calls runctx.ResolveInputArtifacts"
assert_grep_present 'events\.IdentityStampingEmitter' "cmd/harbor/cmd_dev_runloop.go" \
    "cmd runloop builds Emit via events.IdentityStampingEmitter"
assert_grep_present 'llm\.NewChunkPublisher' "cmd/harbor/cmd_dev_runloop.go" \
    "cmd runloop builds OnChunk via llm.NewChunkPublisher"

# 3. devstack no longer defines its duplicate copies (including the
#    THIRD extractSkillKeywords copy the audit found).
for def in 'devStackProjectMemoryBlocks' 'devStackProjectSkillsContext' \
    'devStackExtractSkillKeywords' 'devStackExtractAssistantAnswer' \
    'devStackSkillKeywordStopwords' ') resolveInputArtifacts('; do
    assert_grep_absent "${def}" "harbortest/devstack/devstack.go" \
        "devstack no longer defines '${def}'"
done

# 4. Devstack parity: RunSpec wires Emit + OnChunk; MarkComplete
#    carries the 110a answer envelope instead of TaskResult{}.
assert_grep_present 'OnChunk:' "harbortest/devstack/devstack.go" \
    "devstack RunSpec wires OnChunk (streaming parity)"
assert_grep_present 'Emit:' "harbortest/devstack/devstack.go" \
    "devstack RunSpec wires Emit (planner-telemetry parity)"
assert_grep_present 'planner\.AnswerEnvelope' "harbortest/devstack/devstack.go" \
    "devstack MarkComplete carries planner.AnswerEnvelope"
assert_grep_absent 'MarkComplete(taskCtx, taskID, tasks.TaskResult{})' "harbortest/devstack/devstack.go" \
    "devstack empty-TaskResult{} completion drift closed"

# 5. The spawn-depth default is single-sourced (D-196 call 4 — the
#    Stage-1 handoff one-liner): dispatch references
#    config.DefaultSpawnDepthCap, no duplicated literal const.
assert_grep_present 'config\.DefaultSpawnDepthCap' "internal/runtime/dispatch/dispatch.go" \
    "dispatch spawn-depth default references config.DefaultSpawnDepthCap"
assert_grep_absent 'defaultMaxSpawnDepth' "internal/runtime/dispatch/dispatch.go" \
    "dispatch duplicated spawn-depth literal deleted"

# 6. The promotion's focused test slice passes under -race.
if go test ./internal/runtime/runctx/ ./internal/events/ ./internal/llm/ \
    -run 'Project|ExtractSkill|ExtractAssistant|ResolveInputArtifacts|IdentityStamping|ChunkPublisher|EmitterAndPublisher' \
    -race -count=1 >/dev/null 2>&1; then
    ok "runctx/emitter/chunk-publisher test slice passes (-race)"
else
    fail "runctx/emitter/chunk-publisher test slice failed (-race)"
fi

smoke_summary
