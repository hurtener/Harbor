// memory_fetch.go — the promoted, single-home run-loop memory step.
//
// Both run-loop drivers (cmd/harbor and harbortest/devstack) previously
// contained an identical inline block that called GetLLMContext and then
// ProjectMemoryBlocks. This file promotes that logic into one function
// so the block exists in exactly one place and both drivers become thin
// callers. When semantic recall is enabled the helper additionally calls
// SearchTurns, applies the similarity floor, deduplicates against the
// recent-turn window already in the patch, caps each turn's text, and
// populates MemoryBlocks.External.
//
// When semantic recall is off the output is byte-for-byte identical to
// what the two inline blocks previously produced — zero behavioural change
// unless the operator explicitly opts in.

package runctx

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/planner"
)

// recalledTurnTextCap is the per-side (user / assistant) byte ceiling
// applied to each recalled turn's text before it enters the prompt.
// This bounds each turn, not the aggregate: the injected External
// block is at most recall.TopK × 2 × this cap, so a small TopK keeps
// it well under the context-window heavy-output threshold. A large
// operator-set TopK can push the aggregate over that threshold — at
// which point the LLM-edge context-leak guard fails the run loudly
// with ErrContextLeak (never a silent leak; §13). That guard is the
// authoritative backstop; this per-turn cap is the first line.
const recalledTurnTextCap = 2048

// FetchMemoryBlocks fetches the session's memory patch, optionally
// recalls semantically similar past turns for the given query, and
// projects both into the planner's MemoryBlocks. This is the single
// production home for the memory step of run-loop RunContext population.
//
// When recall is disabled (RecallSettings.Enabled == false) the output
// is identical to calling GetLLMContext + ProjectMemoryBlocks directly —
// the External tier remains nil and SearchTurns is never called.
//
// When recall is enabled:
//   - SearchTurns is called with the session-scoped identity triple
//     (RunID zeroed) and the caller-supplied query.
//   - Turns scoring below RecallSettings.MinScore are dropped.
//   - Turns whose UserMessage is already present in the patch's
//     RecentTurns window are deduped out.
//   - Each remaining turn's user and assistant text are capped at
//     recalledTurnTextCap bytes (with a "…[truncated]" marker).
//   - The resulting slice is marshalled as
//     map["recalled_turns"][]map[string]any and stored in
//     MemoryBlocks.External alongside the Conversation tier.
//
// An error from GetLLMContext or SearchTurns is returned unwrapped so
// the caller can fail the run loudly via MarkFailed(runtime_fetch_error).
// There is no silent fallback to rolling-summary-only on a recall error.
//
// A Debug line is emitted on the supplied logger when recall fires
// (count + top score) so operators can see retrieval activity without
// seeing turn content. A nil logger falls back to slog.Default().
func FetchMemoryBlocks(
	ctx context.Context,
	store memory.MemoryStore,
	id identity.Quadruple,
	query string,
	recall memory.RecallSettings,
	logger *slog.Logger,
) (*planner.MemoryBlocks, error) {
	if logger == nil {
		logger = slog.Default()
	}
	patch, err := store.GetLLMContext(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("memory.GetLLMContext: %w", err)
	}

	mb := ProjectMemoryBlocks(patch)

	if !recall.Enabled {
		return mb, nil
	}

	// Semantic recall path. Build a set of recent-turn user messages for
	// fast dedup (the recent window is already in the Conversation tier;
	// injecting duplicates wastes tokens and confuses ordering).
	recentSet := make(map[string]struct{}, len(patch.RecentTurns))
	for _, t := range patch.RecentTurns {
		if t.UserMessage != "" {
			recentSet[t.UserMessage] = struct{}{}
		}
	}

	topK := recall.TopK
	scored, sErr := store.SearchTurns(ctx, id, query, topK)
	if sErr != nil {
		return nil, fmt.Errorf("memory.SearchTurns: %w", sErr)
	}

	// Filter, dedup, cap.
	recalled := make([]map[string]any, 0, len(scored))
	for _, st := range scored {
		if st.Score < recall.MinScore {
			continue
		}
		if _, inRecent := recentSet[st.Turn.UserMessage]; inRecent {
			continue
		}
		entry := map[string]any{
			"user":      capText(st.Turn.UserMessage),
			"assistant": capText(st.Turn.AssistantResponse),
			"score":     st.Score,
		}
		if !st.Turn.Timestamp.IsZero() {
			entry["timestamp"] = st.Turn.Timestamp.UTC().Format("2006-01-02T15:04:05Z")
		}
		recalled = append(recalled, entry)
	}

	if len(recalled) > 0 {
		logger.Debug("runctx: semantic recall fired",
			slog.Int("count", len(recalled)),
			slog.Float64("top_score", scored[0].Score))

		external := map[string]any{
			"recalled_turns": recalled,
		}
		if mb == nil {
			mb = &planner.MemoryBlocks{}
		}
		mb.External = external
	}

	return mb, nil
}

// capText truncates text to recalledTurnTextCap bytes, appending a
// truncation marker when the text is actually shortened. The cut is
// on a valid UTF-8 boundary so the marker is never mid-rune.
func capText(text string) string {
	if len(text) <= recalledTurnTextCap {
		return text
	}
	// Walk back to the nearest valid UTF-8 boundary.
	cut := recalledTurnTextCap
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	var sb strings.Builder
	sb.WriteString(text[:cut])
	sb.WriteString("…[truncated]")
	return sb.String()
}
