package strategy

import (
	"context"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
)

// EchoSummarizer is the test-grade `memory.Summarizer` stub Phase
// 24 ships. It concatenates the previous summary + the joined
// user/assistant turns into a deterministic string so tests can
// pin expected outputs without an LLM backend.
//
// The real LLM-backed `Summarizer` lands at Phase 32+ in the
// `internal/llm` subsystem; tests in Phase 24 reach for
// `EchoSummarizer` because (a) it is fully deterministic, and
// (b) it never hits the network so the test suite stays hermetic.
//
// Concurrent-reuse contract (D-025): no mutable state on the
// struct; safe to share across N goroutines.
type EchoSummarizer struct{}

// Summarize implements `memory.Summarizer`.
func (EchoSummarizer) Summarize(_ context.Context, _ identity.Quadruple, req memory.SummarizeRequest) (memory.SummarizeResponse, error) {
	var b strings.Builder
	if req.PreviousSummary != "" {
		b.WriteString(req.PreviousSummary)
		b.WriteByte('\n')
	}
	for i, t := range req.Turns {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("U:")
		b.WriteString(t.UserMessage)
		b.WriteString(" A:")
		b.WriteString(t.AssistantResponse)
	}
	return memory.SummarizeResponse{Summary: b.String()}, nil
}

// Compile-time assertion that EchoSummarizer satisfies
// memory.Summarizer.
var _ memory.Summarizer = EchoSummarizer{}
