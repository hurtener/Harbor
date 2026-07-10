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
# MarkComplete carries the canonical answer envelope via the ONE shared
# runctx builder (no hand-rolled envelope literal); the focused test
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
# Phase 111d (D-201) executed the deprecation notice this smoke used
# to pin: ExtractSkillKeywords is DELETED (the Directory is the
# `<skills_context>` producer). Assert the deletion held.
assert_grep_absent 'func ExtractSkillKeywords' "internal/runtime/runctx/runctx.go" \
    "ExtractSkillKeywords deleted per its D-195 deprecation notice (Phase 111d, D-201)"

# 2. cmd/harbor no longer DEFINES the five helpers (it calls the
#    promoted surface). Patterns pin the definitions so the thin
#    `runctx.*` call sites don't trip the absence check.
for def in 'func projectMemoryBlocks' 'func projectSkillsContext' \
    'func extractSkillKeywords' 'func extractAssistantAnswer' \
    ') resolveInputArtifacts(' 'skillKeywordStopwords'; do
    assert_grep_absent "${def}" "internal/runtime/serve/runloop.go" \
        "cmd runloop no longer defines '${def}'"
done
assert_grep_present 'runctx\.FetchMemoryBlocks' "internal/runtime/serve/runloop.go" \
    "cmd runloop calls runctx.FetchMemoryBlocks (promotes ProjectMemoryBlocks + semantic recall)"
assert_grep_present 'runctx\.ResolveInputArtifacts' "internal/runtime/serve/runloop.go" \
    "cmd runloop calls runctx.ResolveInputArtifacts"
assert_grep_present 'events\.IdentityStampingEmitter' "internal/runtime/serve/runloop.go" \
    "cmd runloop builds Emit via events.IdentityStampingEmitter"
assert_grep_present 'llm\.NewChunkPublisher' "internal/runtime/serve/runloop.go" \
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
#    carries the 110a answer envelope instead of TaskResult{}. The
#    envelope construction was later promoted into the ONE shared
#    builder (runctx.FinishAnswerEnvelope — the per-task structured-
#    output phase), so the invariant is pinned transitively: devstack
#    builds its completion envelope through the shared builder, the
#    shared builder constructs the canonical planner.AnswerEnvelope,
#    and no hand-rolled envelope literal survives in devstack.
assert_grep_present 'OnChunk:' "internal/runtime/serve/runloop.go" \
    "devstack RunSpec wires OnChunk (streaming parity)"
assert_grep_present 'Emit:' "internal/runtime/serve/runloop.go" \
    "devstack RunSpec wires Emit (planner-telemetry parity)"
assert_grep_present 'runctx\.FinishAnswerEnvelope' "internal/runtime/serve/runloop.go" \
    "devstack MarkComplete builds the envelope via the shared runctx builder"
assert_grep_present 'planner\.AnswerEnvelope' "internal/runtime/runctx/answer_envelope.go" \
    "the shared builder constructs the canonical planner.AnswerEnvelope"
assert_grep_absent 'planner\.AnswerEnvelope{' "internal/runtime/serve/runloop.go" \
    "no hand-rolled envelope literal survives in devstack (§13 one-builder rule)"
assert_grep_absent 'MarkComplete(taskCtx, taskID, tasks.TaskResult{})' "internal/runtime/serve/runloop.go" \
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
