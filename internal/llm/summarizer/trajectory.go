// trajectory.go — the production LLM-backed planner.Summariser
// (Phase 111e, D-202). Distinct from the memory-subsystem Summarizer
// in summarizer.go: see the package godoc's two-interface
// disambiguation.
package summarizer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
)

// TrajectoryPromptVersion is the stable identifier for the trajectory
// compaction prompt template. Tests pin this; a later prompt-craft
// improvement bumps the constant. Format: `vN`, monotonically
// increasing — independent of the memory summarizer's PromptVersion.
const TrajectoryPromptVersion = "v1"

// trajectorySystemPromptV1 anchors the compaction persona. The five
// output fields mirror planner.TrajectorySummary (RFC §6.2, D-055).
// Kept declarative and provider-neutral; the JSON-schema response
// format (Phase 35) constrains the shape, this prompt constrains the
// content.
const trajectorySystemPromptV1 = `You are Harbor's trajectory compaction summariser. You receive an agent run's trajectory: the user's query, the current goal, and the sequence of steps taken so far (each step's action and its observation). Compress it into a JSON object with exactly these fields:
- "goals": the goals the agent is tracking (array of short strings).
- "facts": every load-bearing fact extracted from prior observations that the agent still needs — identifiers, codes, values, decisions. Never drop a concrete value the final answer could depend on (array of strings).
- "pending": the open subgoals still to be done (array of strings).
- "last_output_digest": a one-line digest of the most recent observation (string).
- "note": a one-line rationale for the compaction (string).
Be factual and concise. Never speculate beyond the trajectory provided. Respond with the JSON object only.`

// trajectorySummarySchemaV1 is the JSON Schema handed to the LLM via
// the Phase 35 structured-output path (FormatJSONSchema; the
// per-provider downgrade ladder json_schema → json_object → text
// applies unchanged). Field names mirror trajectory.Summary's JSON
// tags so the response unmarshals directly.
var trajectorySummarySchemaV1 = json.RawMessage(`{
	"type": "object",
	"additionalProperties": false,
	"properties": {
		"goals":   {"type": "array", "items": {"type": "string"}},
		"facts":   {"type": "array", "items": {"type": "string"}},
		"pending": {"type": "array", "items": {"type": "string"}},
		"last_output_digest": {"type": "string"},
		"note": {"type": "string"}
	},
	"required": ["goals", "facts", "pending", "last_output_digest", "note"]
}`)

// trajectoryFragmentCap is the per-fragment byte cap the payload
// builder applies to each step's rendered observation / reasoning
// trace. It keeps the compaction prompt bounded (and safely under the
// D-026 heavy-output threshold the LLM-edge safety pass enforces per
// message) even when a single step carried a large observation.
const trajectoryFragmentCap = 4096

// TrajectorySummariser is the production planner.Summariser Phase
// 111e ships — the producer half of trajectory compression (RFC §6.2,
// brief 02 §4). It composes a compaction prompt over the trajectory's
// planner-facing projection, calls llm.LLMClient.Complete in
// structured-output JSON-schema mode (Phase 35), and parses the
// response into the five-field planner.TrajectorySummary.
//
// Construct via NewTrajectorySummariser; do not construct directly.
//
// TrajectorySummariser is a compiled artifact (D-025): every field is
// set once at construction and never mutated. One instance is safe to
// share across N concurrent Summarise goroutines; per-call state
// lives on the function stack and in ctx. trajectory_test.go pins
// N≥100 under -race.
//
// Fail-loud contract (the planner.Summariser seam's): an LLM error
// propagates wrapped, never swallowed; a structurally valid but
// vacuous summary surfaces planner.ErrEmptySummary; a response that
// does not parse as the five-field object is a loud parse error.
// There is no silent fall-through to raw history.
type TrajectorySummariser struct {
	client           llm.LLMClient
	model            string
	systemPrompt     string
	maxSummaryTokens int
}

// TrajectoryOption configures a TrajectorySummariser at construction.
type TrajectoryOption func(*TrajectorySummariser)

// WithTrajectoryModel pins the model the summariser requests against
// the LLMClient — operators can route compaction to a cheaper (or
// stronger) model than the planner's. Empty falls back to the
// client's resolved default model at request time.
func WithTrajectoryModel(model string) TrajectoryOption {
	return func(s *TrajectorySummariser) {
		if model != "" {
			s.model = model
		}
	}
}

// WithTrajectorySystemPrompt overrides the versioned compaction
// system prompt. The override is rendered verbatim; the caller owns
// keeping it aligned with the five-field output schema. Empty is a
// no-op (the versioned default stays).
func WithTrajectorySystemPrompt(prompt string) TrajectoryOption {
	return func(s *TrajectorySummariser) {
		if prompt != "" {
			s.systemPrompt = prompt
		}
	}
}

// WithTrajectoryMaxSummaryTokens caps the completion's MaxTokens —
// a guard against a runaway summary. Non-positive is a no-op (the
// provider default applies).
func WithTrajectoryMaxSummaryTokens(n int) TrajectoryOption {
	return func(s *TrajectorySummariser) {
		if n > 0 {
			s.maxSummaryTokens = n
		}
	}
}

// NewTrajectorySummariser constructs the production trajectory
// summariser. The client is mandatory; a nil client returns an error
// rather than building a summariser that would nil-panic on the
// first Summarise call.
//
// The returned summariser satisfies planner.Summariser. Wire it via
// planner.NewCompressionRunner(s) and set the runner on
// steering.RunSpec.Compression (plus Base.Budget.TokenBudget > 0) —
// or let the assembly do it from the `planner.token_budget` config
// knob (Phase 111e, D-202).
func NewTrajectorySummariser(client llm.LLMClient, opts ...TrajectoryOption) (*TrajectorySummariser, error) {
	if client == nil {
		return nil, fmt.Errorf("summarizer: NewTrajectorySummariser requires a non-nil llm.LLMClient")
	}
	s := &TrajectorySummariser{
		client:       client,
		systemPrompt: trajectorySystemPromptV1,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Summarise implements planner.Summariser. It renders the
// trajectory's planner-facing projection (query, goal, per-step
// action + LLM-visible observation), sends it through the LLM client
// (which runs the Phase 32 safety pass + Phase 34 corrections +
// Phase 35 structured-output downgrade + Phase 36 retry + governance
// chain), and parses the assistant's JSON reply into the five-field
// planner.TrajectorySummary.
//
// The payload deliberately renders Step.LLMObservation (the D-026
// heavy-content-disciplined projection — what the planner itself saw)
// over the raw Step.Observation: raw observations may carry heavy
// content that must never reach the LLM edge (CLAUDE.md §13 /
// ErrContextLeak). When a step predates the projection split,
// the raw observation is used, truncated at trajectoryFragmentCap.
//
// Identity is mandatory: rc.Quadruple's identity is propagated into
// ctx so the LLM-edge layers (safety, governance, audit) see the same
// identity the compression runner validated.
func (s *TrajectorySummariser) Summarise(ctx context.Context, rc planner.RunContext, tr *planner.Trajectory) (*planner.TrajectorySummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("trajectory summariser: ctx cancelled: %w", err)
	}
	if tr == nil {
		return nil, fmt.Errorf("trajectory summariser: %w", planner.ErrNilTrajectory)
	}
	ctx, err := identity.With(ctx, rc.Quadruple.Identity)
	if err != nil {
		return nil, fmt.Errorf("trajectory summariser: identity propagation: %w", err)
	}

	systemText := s.systemPrompt
	userText := buildTrajectoryPayload(rc, tr)

	req := llm.CompleteRequest{
		Model: s.model,
		Messages: []llm.ChatMessage{
			{Role: llm.RoleSystem, Content: llm.Content{Text: &systemText}},
			{Role: llm.RoleUser, Content: llm.Content{Text: &userText}},
		},
		ResponseFormat: &llm.ResponseFormat{
			Kind:       llm.FormatJSONSchema,
			JSONSchema: trajectorySummarySchemaV1,
		},
	}
	if s.maxSummaryTokens > 0 {
		maxTok := s.maxSummaryTokens
		req.MaxTokens = &maxTok
	}

	resp, err := s.client.Complete(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("trajectory summariser: llm complete: %w", err)
	}

	summary, err := parseTrajectorySummary(resp.Content)
	if err != nil {
		return nil, fmt.Errorf("trajectory summariser: %w", err)
	}
	return summary, nil
}

// parseTrajectorySummary parses the assistant content into the
// five-field summary. Tolerates a markdown code fence around the JSON
// (providers on the json_object / text downgrade rungs emit them);
// everything else that does not parse is a loud error. A structurally
// valid but vacuous object (all five fields empty) surfaces
// planner.ErrEmptySummary — the seam's fail-loud contract.
func parseTrajectorySummary(content string) (*planner.TrajectorySummary, error) {
	trimmed := stripJSONFence(strings.TrimSpace(content))
	if trimmed == "" {
		return nil, fmt.Errorf("parse summary: %w (LLM returned empty content)", planner.ErrEmptySummary)
	}
	var sum planner.TrajectorySummary
	if err := json.Unmarshal([]byte(trimmed), &sum); err != nil {
		return nil, fmt.Errorf("parse summary: response is not the five-field JSON object: %w (content prefix: %.160q)", err, trimmed)
	}
	if len(sum.Goals) == 0 && len(sum.Facts) == 0 && len(sum.Pending) == 0 &&
		sum.LastOutputDigest == "" && sum.Note == "" {
		return nil, fmt.Errorf("parse summary: %w (LLM returned a structurally valid but empty summary)", planner.ErrEmptySummary)
	}
	return &sum, nil
}

// stripJSONFence removes a single surrounding markdown code fence
// (``` or ```json) when present. The fence is the only tolerated
// decoration; anything else fails the parse loudly.
func stripJSONFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	body := strings.TrimPrefix(s, "```")
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		// Drop the fence-info line (e.g. "json").
		body = body[i+1:]
	}
	body = strings.TrimSpace(body)
	body = strings.TrimSuffix(body, "```")
	return strings.TrimSpace(body)
}

// buildTrajectoryPayload assembles the user-side message body: the
// run's query + current goal + the per-step action/observation
// sequence, each fragment capped at trajectoryFragmentCap. Stable
// line-prefix scheme so the LLM sees a consistent shape across calls
// and test fixtures can pin against it.
func buildTrajectoryPayload(rc planner.RunContext, tr *planner.Trajectory) string {
	var b strings.Builder
	b.WriteString("Compress the following agent trajectory into the five-field JSON summary.\n")
	b.WriteString("\n[User query]\n")
	if tr.Query != "" {
		b.WriteString(tr.Query)
	} else {
		b.WriteString(rc.Query)
	}
	b.WriteString("\n\n[Current goal]\n")
	if rc.Goal != "" {
		b.WriteString(rc.Goal)
	} else {
		b.WriteString("(same as query)")
	}
	b.WriteString("\n\n[Steps]\n")
	if len(tr.Steps) == 0 {
		b.WriteString("(none)\n")
	}
	for i, step := range tr.Steps {
		fmt.Fprintf(&b, "Step %d action: %s\n", i+1, capFragment(renderJSONFragment(step.Action)))
		obs := step.LLMObservation
		if obs == nil {
			// Pre-projection-split steps: fall back to the raw
			// observation, capped — the cap keeps the payload under the
			// D-026 heavy threshold even on the legacy shape.
			obs = step.Observation
		}
		if obs != nil {
			fmt.Fprintf(&b, "Step %d observation: %s\n", i+1, capFragment(renderJSONFragment(obs)))
		}
		if step.Error != "" {
			fmt.Fprintf(&b, "Step %d error: %s\n", i+1, capFragment(step.Error))
		}
		if step.ReasoningTrace != "" {
			fmt.Fprintf(&b, "Step %d reasoning: %s\n", i+1, capFragment(step.ReasoningTrace))
		}
	}
	return b.String()
}

// renderJSONFragment JSON-encodes v for the payload; a value that does
// not encode (the trajectory contract requires JSON-encodable leaves)
// renders as a loud inline marker rather than panicking — the
// summariser still produces a usable payload and the unserialisable
// leaf is visible in the prompt + any pinned fixture.
func renderJSONFragment(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("(unserialisable: %v)", err)
	}
	return string(raw)
}

// capFragment truncates s to trajectoryFragmentCap bytes, appending a
// truncation marker when truncation happened.
func capFragment(s string) string {
	if len(s) <= trajectoryFragmentCap {
		return s
	}
	return s[:trajectoryFragmentCap] + "…[truncated]"
}

// Compile-time assertion that *TrajectorySummariser satisfies
// planner.Summariser.
var _ planner.Summariser = (*TrajectorySummariser)(nil)
