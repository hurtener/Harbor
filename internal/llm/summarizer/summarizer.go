// Package summarizer is Harbor's production LLM-backed `memory.Summarizer`
// — the §13 "test stubs as production defaults" amendment closure for the
// memory subsystem's `rolling_summary` strategy.
//
// Before Phase 64 / D-089 the only `memory.Summarizer` Harbor shipped
// was `strategy.EchoSummarizer` — a deterministic test stub that
// concatenated turns into a single string. The runtime's
// `rolling_summary` strategy required an *injected* Summarizer with no
// default, so a `harbor dev` boot against `memory.strategy:
// rolling_summary` would have crashed with `nil Summarizer` (or worse,
// silently fallen back to a test stub if a future PR wired one).
//
// Phase 64 closes that seam: `summarizer.New(client)` composes an
// `llm.LLMClient` with a versioned compaction prompt and returns a
// `memory.Summarizer` ready to wire into `memory.Open(...,
// Deps{Summarizer: ...})`. The dev server wires this when an operator
// configures `memory.strategy: rolling_summary` (the same conditional
// the Phase 24 spec laid down).
//
// # The compaction prompt
//
// The prompt is versioned (`PromptVersion`) so a later prompt-craft
// improvement bumps the constant and the prior version stays available
// for golden-test pinning. The template uses an instruction-tuned
// shape that works against every Phase 33 native provider:
//
//   - One system message anchors the summariser persona: "You are a
//     concise meeting-minutes summariser. Compress the conversation
//     turns below into a short, factual rolling summary..."
//   - One user message stitches the previous summary (if any) + the
//     evicted turns into a single payload. The Summarize implementation
//     uses the request's `PreviousSummary` + `Turns` directly — no
//     reformatting beyond a stable line-prefix scheme so the LLM sees
//     consistent input shape across calls.
//
// # Concurrent reuse (D-025)
//
// `Summarizer` is a compiled artifact: the embedded `llm.LLMClient`
// and the compaction prompt template are set once at construction
// (`New`). One Summarizer is safe to share across N concurrent
// `Summarize` goroutines; the per-call state lives on the function
// stack and in `ctx`. Concurrent_test.go pins N≥100 under -race.
//
// # Identity (CLAUDE.md §6 rule 9)
//
// `Summarize` is identity-mandatory: the request's
// `identity.Quadruple` is propagated into ctx via `identity.With`
// before the LLM call so the safety pass (Phase 32) sees the same
// identity the memory subsystem's caller supplied. The LLM-edge audit
// + governance + retry layers all key on this identity; without it,
// the safety pass rejects with `ErrIdentityRequired`.
//
// # Fail-loud (CLAUDE.md §5)
//
// An LLM call that fails surfaces the wrapped error verbatim — the
// summariser does NOT silently degrade to an echo or a truncation.
// Callers handle `Summarize` errors via the
// `memory.strategy.rolling_summary` health FSM (Phase 24): the strategy
// transitions from healthy → retry → degraded as failures accumulate.
//
// # §13 primitive-with-consumer
//
// Phase 64's `harbor dev` subcommand is the §13 first consumer of
// this Summarizer. The dev wiring constructs `summarizer.New(client)`
// and passes it through `memory.Deps.Summarizer` when the operator
// configured `memory.strategy: rolling_summary`. Without Phase 64 the
// summariser would be a dormant primitive; with Phase 64 it is wired
// end-to-end on the production code path.
package summarizer

import (
	"context"
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/memory"
)

// PromptVersion is the stable identifier for the compaction prompt
// template. Tests pin this; a later prompt-craft improvement bumps
// the constant. Format: `vN` where N is a monotonically-increasing
// integer.
const PromptVersion = "v1"

// systemPromptV1 is the instruction the summariser anchors on. Kept
// short, declarative, and provider-neutral. Future versions add a
// `v2` constant and the template selector reads the constant to pick.
const systemPromptV1 = `You are Harbor's rolling conversation summariser. Compress the conversation turns into a short, factual rolling summary that preserves user goals, key decisions, and unresolved questions. Be concise: prefer bullet-point style; drop pleasantries; never speculate beyond the turns provided.`

// userPromptHeader is the stable prefix for the user-side payload.
// The summarizer appends the previous summary + the new turns under
// fixed labels so the LLM sees a consistent shape across calls.
const userPromptHeader = `Compress the following conversation excerpt into an updated rolling summary.`

// Summarizer is the production LLM-backed `memory.Summarizer` Phase
// 64 ships. Construct via `New`; do not construct directly.
//
// `Summarizer` is a compiled artifact (D-025): every field is set
// once at construction and never mutated. One Summarizer is safe to
// share across N concurrent Summarize goroutines.
type Summarizer struct {
	client llm.LLMClient
	model  string
}

// Option configures a Summarizer at construction time.
type Option func(*Summarizer)

// WithModel pins the model name the Summarizer requests against the
// LLMClient. Empty model falls back to the client's resolved
// `ConfigSnapshot.Model` at request time (the snapshot's default
// model). Pinning at construction is useful for operators who want
// a cheap model for compaction independent of the planner's model
// choice.
func WithModel(model string) Option {
	return func(s *Summarizer) {
		if model != "" {
			s.model = model
		}
	}
}

// New constructs a production Summarizer. The client is mandatory; a
// nil client returns an error rather than building a Summarizer that
// would nil-panic on the first Summarize call.
//
// The returned Summarizer satisfies `memory.Summarizer`. Wire it via
// `memory.Open(..., Deps{Summarizer: s})` when configuring the
// `rolling_summary` strategy.
func New(client llm.LLMClient, opts ...Option) (*Summarizer, error) {
	if client == nil {
		return nil, fmt.Errorf("summarizer: New requires a non-nil llm.LLMClient")
	}
	s := &Summarizer{client: client}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Summarize implements `memory.Summarizer`. It composes a chat
// request from the previous summary + the new turns, sends it
// through the LLM client (which runs through the Phase 32 safety
// pass + Phase 34 corrections + Phase 35 downgrade + Phase 36 retry
// + Phase 36a/36b governance chain), and returns the assistant's
// reply verbatim as the new rolling summary.
//
// Identity is mandatory: `id` is injected into ctx via
// `identity.With` so the LLM-edge safety pass sees the same identity
// the memory subsystem's caller supplied. A nil-Quadruple is the
// caller's responsibility to reject (callers MUST not pass empty
// identities — the memory subsystem itself enforces).
func (s *Summarizer) Summarize(ctx context.Context, id identity.Quadruple, req memory.SummarizeRequest) (memory.SummarizeResponse, error) {
	if err := ctx.Err(); err != nil {
		return memory.SummarizeResponse{}, fmt.Errorf("summarizer: ctx cancelled: %w", err)
	}
	// Propagate the caller's identity into ctx so the LLM-edge layers
	// (safety, governance, audit) see the same identity the memory
	// subsystem received.
	ctx, err := identity.With(ctx, id.Identity)
	if err != nil {
		return memory.SummarizeResponse{}, fmt.Errorf("summarizer: identity propagation: %w", err)
	}

	userPayload := buildUserPayload(req)
	systemText := systemPromptV1
	userText := userPayload

	messages := []llm.ChatMessage{
		{Role: llm.RoleSystem, Content: llm.Content{Text: &systemText}},
		{Role: llm.RoleUser, Content: llm.Content{Text: &userText}},
	}

	resp, err := s.client.Complete(ctx, llm.CompleteRequest{
		Model:    s.model,
		Messages: messages,
	})
	if err != nil {
		return memory.SummarizeResponse{}, fmt.Errorf("summarizer: llm complete: %w", err)
	}
	return memory.SummarizeResponse{Summary: strings.TrimSpace(resp.Content)}, nil
}

// buildUserPayload assembles the user-side message body from the
// SummarizeRequest. Stable line-prefix scheme:
//
//	Compress the following conversation excerpt into an updated rolling summary.
//
//	[Previous summary]
//	<previous summary verbatim, or "(none)" when empty>
//
//	[New turns]
//	U: <user-message-1>
//	A: <assistant-response-1>
//	U: <user-message-2>
//	A: <assistant-response-2>
//	...
//
// The format is deterministic so test fixtures can pin against it
// and the LLM sees a consistent shape across calls.
func buildUserPayload(req memory.SummarizeRequest) string {
	var b strings.Builder
	b.WriteString(userPromptHeader)
	b.WriteString("\n\n[Previous summary]\n")
	if strings.TrimSpace(req.PreviousSummary) == "" {
		b.WriteString("(none)\n")
	} else {
		b.WriteString(req.PreviousSummary)
		b.WriteString("\n")
	}
	b.WriteString("\n[New turns]\n")
	if len(req.Turns) == 0 {
		b.WriteString("(none)\n")
	} else {
		for _, t := range req.Turns {
			b.WriteString("U: ")
			b.WriteString(t.UserMessage)
			b.WriteString("\nA: ")
			b.WriteString(t.AssistantResponse)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// Compile-time assertion that *Summarizer satisfies memory.Summarizer.
var _ memory.Summarizer = (*Summarizer)(nil)
