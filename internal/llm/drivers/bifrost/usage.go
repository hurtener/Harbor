package bifrost

import (
	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/llm"
)

// Streaming usage is cumulative, not additive. Sparse updates replace only
// their available categories: a later cost-only chunk must not erase tokens,
// and missing detail objects must not erase earlier cache/reasoning counts.
func mergeStreamAccounting(resp *bfschemas.BifrostChatResponse, usage *llm.Usage, cost *llm.Cost) {
	if resp == nil || resp.Usage == nil {
		return
	}
	next, nextCost := extractUsageAndCost(resp)
	// The SDK normalizes absent scalar totals to zero. Preserve an earlier
	// report on an empty/cost-only update rather than treating absence as an
	// authoritative reset. A first all-zero report still records presence.
	if !usage.ReportPresent || next.PromptTokens != 0 {
		usage.PromptTokens = next.PromptTokens
	}
	if !usage.ReportPresent || next.CompletionTokens != 0 {
		usage.CompletionTokens = next.CompletionTokens
	}
	if !usage.ReportPresent || next.TotalTokens != 0 {
		usage.TotalTokens = next.TotalTokens
	}
	usage.ReportPresent = true
	if next.LatencyMS != 0 {
		usage.LatencyMS = next.LatencyMS
	}
	if next.PromptDetailsPresent {
		usage.PromptDetailsPresent = true
		usage.CacheReadTokens = next.CacheReadTokens
		usage.CacheWriteTokens = next.CacheWriteTokens
	}
	if next.CompletionDetailsPresent {
		usage.CompletionDetailsPresent = true
		usage.ReasoningTokens = next.ReasoningTokens
	}
	if nextCost.ReportPresent {
		*cost = nextCost
	}
}
