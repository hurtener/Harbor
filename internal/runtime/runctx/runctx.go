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
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/events"
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

// InputArtifactOptions carries the Phase 84b (D-189) disposition
// inputs to [ResolveInputArtifacts]. The zero value reproduces the
// pre-84b behaviour exactly: no hints, no policy (the runtime default
// resolves `image/*` → inline, everything else → ref), no catalog
// validation of `tool:<name>` values, and no event emission.
type InputArtifactOptions struct {
	// Hints maps an artifact ID to its per-attachment caller hint —
	// the top precedence layer. The dev run loop populates it from
	// `tasks.Task.InputArtifactDispositions` (the Protocol
	// `disposition` carrier); a headless caller constructs it
	// directly (or skips this helper entirely and sets
	// `InputArtifactView.Disposition` on hand-built views).
	Hints map[string]planner.AttachmentDisposition
	// Policy is the per-agent disposition policy map — the middle
	// precedence layer (`harbor.yaml` `multimodal.disposition`, or a
	// programmatic `planner.DispositionPolicy`).
	Policy planner.DispositionPolicy
	// Catalog, when non-nil, validates `tool:<name>` dispositions
	// against the run's visible tool set: an unknown name degrades
	// to `ref` with a logged + emitted degradation fact.
	Catalog planner.ToolCatalogView
	// Emit, when non-nil, publishes one
	// [EventTypeInputDispositionResolved] event per resolved
	// artifact (the dev run loop passes its identity-stamping
	// emitter). Nil skips emission; the slog records still land.
	Emit func(events.Event)
}

// DispositionHints converts the string-typed per-attachment hint map
// a task record carries (`tasks.Task.InputArtifactDispositions` — the
// Protocol carrier, validated at the edge via
// `planner.ParseDisposition`) into the typed hint map
// [InputArtifactOptions.Hints] expects. Nil in, nil out.
func DispositionHints(raw map[string]string) map[string]planner.AttachmentDisposition {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]planner.AttachmentDisposition, len(raw))
	for id, v := range raw {
		out[id] = planner.AttachmentDisposition(v)
	}
	return out
}

// ResolveInputArtifacts pre-fetches the metadata (+ bytes for inline
// dispositions) for every operator-uploaded artifact ID on a task,
// producing the `planner.InputArtifactView` slice the run loop hands
// to the planner's first turn (Round-7 F11 / D-166; promoted by
// Phase 110b). The synchronous pre-resolution keeps the planner's
// prompt assembly I/O-free.
//
// Phase 84b (D-189): each view's `Disposition` is resolved here via
// the pure `planner.ResolveDisposition` (per-attachment caller hint >
// per-agent policy map > runtime default) + capability normalisation
// via `planner.EffectiveDisposition`. This helper is a THIN caller —
// the precedence logic lives in `internal/planner`; the helper adds
// only the I/O (byte fetch for inline) and the logging/emission of
// the returned winning-layer + degradation facts (the resolver is
// pure and never logs).
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
//   - `Get` (bytes fetch) errored on an inline disposition → keep
//     the entry but leave Bytes nil. The materializer falls back to
//     a stub-text part for missing-bytes inlines.
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
	opts InputArtifactOptions,
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
		// Phase 84b (D-189): thin calls into the planner-homed pure
		// policy core; this caller logs/emits the returned facts.
		resolved, layer := planner.ResolveDisposition(opts.Hints[id], opts.Policy, ref.MimeType)
		effective, degradation := planner.EffectiveDisposition(resolved, ref.MimeType, opts.Catalog)
		view := planner.InputArtifactView{
			ID:          ref.ID,
			MIME:        ref.MimeType,
			SizeBytes:   ref.SizeBytes,
			Filename:    ref.Filename,
			Disposition: effective,
		}
		// Inline dispositions need the bytes (Path 1 — DataURL; the
		// runtime default for `image/*`, byte-for-byte the pre-84b
		// fetch condition). Everything else stays a ref the
		// materializer renders as an `ArtifactStub`.
		if effective == planner.DispositionInline {
			bytesPayload, getFound, berr := store.Get(ctx, scope, id)
			if berr != nil || !getFound || len(bytesPayload) == 0 {
				logger.Warn("runctx: inline artifact bytes missing; emitting ref-only fallback",
					slog.String("run_id", q.RunID),
					slog.String("artifact_id", id))
			} else {
				view.Bytes = bytesPayload
			}
		}
		logResolvedDisposition(logger, q, id, ref.MimeType, effective, layer, degradation)
		emitResolvedDisposition(opts.Emit, q, id, ref.MimeType, effective, layer, degradation)
		out = append(out, view)
	}
	return out
}

// logResolvedDisposition records which disposition fired and why
// (which precedence layer won) — Debug on the honoured path, Warn on
// a degradation (fail-loud, never a silent drop — CLAUDE.md §13).
func logResolvedDisposition(
	logger *slog.Logger,
	q identity.Quadruple,
	artifactID, mime string,
	effective planner.AttachmentDisposition,
	layer planner.DispositionLayer,
	degradation *planner.DispositionDegradation,
) {
	if degradation != nil {
		logger.Warn("runctx: attachment disposition degraded",
			slog.String("run_id", q.RunID),
			slog.String("artifact_id", artifactID),
			slog.String("mime", mime),
			slog.String("requested", string(degradation.From)),
			slog.String("disposition", string(degradation.To)),
			slog.String("layer", string(layer)),
			slog.String("reason", string(degradation.Reason)),
			slog.String("tool", degradation.Tool))
		return
	}
	logger.Debug("runctx: attachment disposition resolved",
		slog.String("run_id", q.RunID),
		slog.String("artifact_id", artifactID),
		slog.String("mime", mime),
		slog.String("disposition", string(effective)),
		slog.String("layer", string(layer)))
}

// emitResolvedDisposition publishes the
// `task.input_disposition.resolved` event through the caller's
// emitter. A nil emit is a no-op (headless callers without a bus).
func emitResolvedDisposition(
	emit func(events.Event),
	q identity.Quadruple,
	artifactID, mime string,
	effective planner.AttachmentDisposition,
	layer planner.DispositionLayer,
	degradation *planner.DispositionDegradation,
) {
	if emit == nil {
		return
	}
	payload := InputDispositionResolvedPayload{
		Identity:    q,
		TaskID:      q.RunID,
		ArtifactID:  artifactID,
		MIME:        mime,
		Disposition: string(effective),
		Layer:       string(layer),
		OccurredAt:  time.Now(),
	}
	if degradation != nil {
		payload.Degraded = true
		payload.DegradedFrom = string(degradation.From)
		payload.DegradationReason = string(degradation.Reason)
		payload.Tool = degradation.Tool
	}
	emit(events.Event{
		Type:       EventTypeInputDispositionResolved,
		Identity:   q,
		OccurredAt: payload.OccurredAt,
		Payload:    payload,
	})
}
