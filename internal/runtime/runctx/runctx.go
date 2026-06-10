// Package runctx exports the RunContext-population helpers a run-loop
// driver applies between "task spawned" and "planner.Next" — the code
// that turns subsystem state (memory, skills, artifacts, the terminal
// Finish) into what the planner actually sees (Phase 110b — D-195).
//
// Brief 02's contract is that the planner never imports runtime
// internals: everything it can see arrives through `planner.RunContext`.
// The projection code that POPULATES RunContext is therefore the
// runtime's half of the planner contract. Before 110b that half lived
// as unexported `package main` helpers in `cmd/harbor`, hand-duplicated
// in `harbortest/devstack` (the D-094 mirror tax — the SDK friction
// audit found a THIRD drifting copy of the keyword shaper). This
// package is the promotion that makes the contract real for every
// caller: `cmd/harbor`, the devstack test kit, and a headless SDK
// consumer building its own RunSpec all compose the SAME projections.
//
// Import direction (recorded in the phase plan): `internal/runtime/*`
// may import `planner` / `memory` / `skills` / `artifacts`;
// `internal/planner` gains NO new imports — in particular no `memory`
// import.
//
// All five helpers are pure functions (or functions over their
// explicit dependencies) with no package-level mutable state — they
// are trivially safe for concurrent use (D-025).
//
// Phase 111d (D-201) executed the D-195 deprecation notice: the
// `ExtractSkillKeywords` query shaper is DELETED along with the
// raw-Search `<skills_context>` injection path it served; the
// Phase-39 `skills.Directory` view (projected via
// ProjectSkillsDirectory) is the canonical producer.
package runctx

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/skills"
)

// ProjectMemoryBlocks shapes a memory.LLMContextPatch into the
// JSON-encodable map the planner's `<read_only_conversation_memory>`
// wrapper renders (Phase 83f — D-149; promoted by Phase 110b).
// Returns nil when the patch is empty — the wrapper is omitted
// entirely. V1.1 ships only the Conversation tier; the External tier
// remains nil pending a long-term memory phase.
func ProjectMemoryBlocks(patch memory.LLMContextPatch) *planner.MemoryBlocks {
	if len(patch.RecentTurns) == 0 && patch.Summary == "" {
		return nil
	}
	recent := make([]map[string]any, 0, len(patch.RecentTurns))
	for _, turn := range patch.RecentTurns {
		recent = append(recent, map[string]any{
			"user":      turn.UserMessage,
			"assistant": turn.AssistantResponse,
		})
	}
	conversation := map[string]any{
		"strategy":     string(patch.Strategy),
		"recent_turns": recent,
	}
	if patch.Summary != "" {
		conversation["summary"] = patch.Summary
	}
	return &planner.MemoryBlocks{Conversation: conversation}
}

// ProjectSkillsContext shapes a []skills.RankedSkill into the []any
// the planner's `<skills_context>` wrapper renders (Phase 83f —
// D-149; promoted by Phase 110b). Each element is a small map
// carrying the body fields the LLM consumes (name / title /
// description / steps). An empty input returns nil so the wrapper is
// omitted.
func ProjectSkillsContext(ranked []skills.RankedSkill) []any {
	if len(ranked) == 0 {
		return nil
	}
	out := make([]any, 0, len(ranked))
	for _, r := range ranked {
		entry := map[string]any{
			"name":  r.Skill.Name,
			"title": r.Skill.Title,
		}
		if r.Skill.Description != "" {
			entry["description"] = r.Skill.Description
		}
		if len(r.Skill.Steps) > 0 {
			entry["steps"] = r.Skill.Steps
		}
		out = append(out, entry)
	}
	return out
}

// ProjectSkillsDirectory shapes a Phase-39 `skills.Directory.View`
// snapshot into the []any the planner's `<skills_context>` wrapper
// renders (Phase 111d — D-201, executing the D-195 deprecation
// notice: the keyword-shaped raw-Search injection path and its
// `ExtractSkillKeywords` helper are deleted; the Directory's bounded,
// pinned-then-recent, capability-filtered, redacted browse window is
// the canonical producer). Each element is the compact
// `skills.SkillView` projection (name / title / trigger / task_type /
// pinned) — full skill content stays behind the `skill_get`
// meta-tool. An empty input returns nil so the wrapper is omitted.
func ProjectSkillsDirectory(views []skills.SkillView) []any {
	if len(views) == 0 {
		return nil
	}
	out := make([]any, 0, len(views))
	for _, v := range views {
		out = append(out, v)
	}
	return out
}

// ExtractAssistantAnswer pulls the planner's natural-language answer
// out of a terminal Finish for the memory.AddTurn writeback and the
// `planner.AnswerEnvelope` projection (Phase 83i — D-152; promoted by
// Phase 110b). The react planner's FinishGoal carries
// Payload = map[string]any{"answer": "<the LLM's answer>"}. Other
// planners may shape Payload differently; we accept any string-valued
// "answer" key and otherwise fall back to a Sprintf so something
// always lands in memory (matching CLAUDE.md §5 fail-loud — silent
// "nothing written" would lose the run's outcome).
func ExtractAssistantAnswer(fin planner.Finish) string {
	switch p := fin.Payload.(type) {
	case string:
		return p
	case map[string]any:
		if v, ok := p["answer"]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	if fin.Payload == nil {
		return string(fin.Reason)
	}
	return fmt.Sprintf("%v", fin.Payload)
}

// ResolveInputArtifacts pre-fetches the metadata (+ bytes for
// `image/*`) for every operator-uploaded artifact ID on a task,
// producing the `planner.InputArtifactView` slice the run loop hands
// to the planner's first turn (Round-7 F11 / D-166; promoted by
// Phase 110b). The synchronous pre-resolution keeps the planner's
// prompt assembly I/O-free.
//
// Failures are bounded:
//   - Nil artifact store with non-empty IDs → empty slice + Warn log
//     (the LLM still sees a text-only prompt; the operator can re-
//     attach after wiring the store). Avoids a hard fail-loud here
//     because the artifact-store dependency is genuinely optional in
//     some dev postures.
//   - `GetRef` not-found / errored → skip that ID + Warn (the rest
//     of the slice survives; the artifact may have been GC'd between
//     spawn and run).
//   - `Get` (bytes fetch) errored on an image/* → keep the entry but
//     leave Bytes nil. The materializer falls back to a stub-text
//     part for missing-bytes images.
//
// The scope on every store call is the run's identity tuple — the
// artifact store enforces tenant isolation on read. A nil logger
// defaults to slog.Default() so the Warn paths above stay loud for
// every caller.
func ResolveInputArtifacts(
	ctx context.Context,
	store artifacts.ArtifactStore,
	q identity.Quadruple,
	ids []string,
	logger *slog.Logger,
) []planner.InputArtifactView {
	if len(ids) == 0 {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	if store == nil {
		logger.Warn("runctx: input artifacts ignored — no artifact store wired",
			slog.String("run_id", q.RunID),
			slog.Int("count", len(ids)))
		return nil
	}
	scope := artifacts.ArtifactScope{
		TenantID:  q.TenantID,
		UserID:    q.UserID,
		SessionID: q.SessionID,
	}
	out := make([]planner.InputArtifactView, 0, len(ids))
	for _, id := range ids {
		ref, found, gerr := store.GetRef(ctx, scope, id)
		if gerr != nil {
			logger.Warn("runctx: artifact GetRef failed; skipping",
				slog.String("run_id", q.RunID),
				slog.String("artifact_id", id),
				slog.String("err", gerr.Error()))
			continue
		}
		if !found || ref == nil {
			logger.Warn("runctx: artifact not found; skipping",
				slog.String("run_id", q.RunID),
				slog.String("artifact_id", id))
			continue
		}
		view := planner.InputArtifactView{
			ID:        ref.ID,
			MIME:      ref.MimeType,
			SizeBytes: ref.SizeBytes,
			Filename:  ref.Filename,
		}
		// Image MIMEs need the bytes inline (Path 1 — DataURL).
		// Everything else stays as a ref the materializer renders as
		// an `ArtifactStub`.
		if strings.HasPrefix(ref.MimeType, "image/") {
			bytesPayload, getFound, berr := store.Get(ctx, scope, id)
			if berr != nil || !getFound || len(bytesPayload) == 0 {
				logger.Warn("runctx: image artifact bytes missing; emitting ref-only fallback",
					slog.String("run_id", q.RunID),
					slog.String("artifact_id", id))
			} else {
				view.Bytes = bytesPayload
			}
		}
		out = append(out, view)
	}
	return out
}
