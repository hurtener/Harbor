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
package runctx

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode"

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

// skillKeywordStopwords lists the common English stopwords the
// keyword extractor drops before handing the query to the FTS5
// skills driver. The list is intentionally CONSERVATIVE — domain
// keywords ("api", "config", "auth", "tool") survive because they
// drive the BM25 ranker's signal. The list mirrors the standard
// short-stopword sets shipped with SQLite FTS5 tokenizers; it is
// fixed (operator-tunable lists are a Phase 91+ concern).
// Phase 83m (Item 4, D-156).
var skillKeywordStopwords = map[string]struct{}{
	"a": {}, "an": {}, "the": {}, "and": {}, "or": {}, "but": {},
	"if": {}, "is": {}, "are": {}, "was": {}, "were": {}, "be": {},
	"been": {}, "being": {}, "have": {}, "has": {}, "had": {},
	"do": {}, "does": {}, "did": {}, "of": {}, "to": {}, "in": {},
	"on": {}, "at": {}, "for": {}, "with": {}, "by": {}, "from": {},
	"as": {}, "into": {}, "that": {}, "this": {}, "it": {}, "i": {},
	"you": {}, "we": {}, "they": {}, "my": {}, "your": {},
}

// maxSkillKeywords caps the number of terms the helper returns. A
// longer term list dilutes the BM25 signal without improving recall;
// 10 mirrors the standard search-keyword cap.
const maxSkillKeywords = 10

// ExtractSkillKeywords turns a raw task Query (a full sentence, with
// punctuation + articles + stopwords) into the keyword-shaped string
// the SQLite skills driver's FTS5 ranker performs best on. The
// pipeline is intentionally CONSERVATIVE: tokens that look like
// domain vocabulary survive; only the highest-noise common-English
// stopwords + 1-char tokens get dropped. Phase 83m (Item 4, D-156);
// promoted by Phase 110b.
//
// DEPRECATION NOTICE: scheduled for deletion by Phase 111d (D-201);
// add no new consumers. The 111d skills Directory wiring replaces the
// raw-Search injection path this helper shapes queries for, deleting
// this function and its call sites. It is promoted anyway because the
// cmd↔devstack mirror collapse must not wait on 111d and landing
// order is not guaranteed; the deletion rides 111d regardless of
// which phase lands first.
//
// Steps (in order):
//
//  1. Lowercase the input so the case-insensitive token comparison
//     matches the FTS5 tokenizer's default case-folding.
//  2. Split on whitespace + punctuation (every rune that is neither a
//     letter nor a digit acts as a separator). Apostrophes inside a
//     word ("operator's") are split — the driver tokenizes the same
//     way, so the result is a single contiguous letter run rather
//     than the contraction.
//  3. Drop tokens in the conservative English stopword set.
//  4. Drop 1-character tokens — they carry no signal at the BM25
//     edge.
//  5. Deduplicate while preserving order — the first occurrence wins
//     so the operator-visible word order is preserved.
//  6. Cap at 10 terms.
//
// Returns the space-joined keyword string. An empty result is
// possible for a pathological all-stopword input; the caller MUST
// fall back to the raw Query so Search still has signal.
func ExtractSkillKeywords(query string) string {
	if query == "" {
		return ""
	}
	lower := strings.ToLower(query)
	// Token boundary: any rune that is not a letter or a digit.
	tokens := strings.FieldsFunc(lower, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	seen := make(map[string]struct{}, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		if len(tok) <= 1 {
			continue
		}
		if _, drop := skillKeywordStopwords[tok]; drop {
			continue
		}
		if _, dup := seen[tok]; dup {
			continue
		}
		seen[tok] = struct{}{}
		out = append(out, tok)
		if len(out) >= maxSkillKeywords {
			break
		}
	}
	return strings.Join(out, " ")
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
