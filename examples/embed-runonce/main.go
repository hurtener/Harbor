// Command embed-runonce is the worked example for the embed adopter
// path's one-call runner: assemble a headless Harbor stack, then run a
// goal with a single blocking Stack.RunOnce call and print the answer.
//
// It is the checked-in counterpart to docs/recipes/embed-harbor-headless.md
// step 4a — the embed smoke compiles this real file so the shorthand
// cannot rot. Every import is the public sdk/ facade, so the same program
// builds verbatim from an external module.
//
// Running it for real needs an LLM provider (set OPENROUTER_API_KEY and
// the llm block below to your model); the smoke only compiles it.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	// The production driver aggregator — the single sanctioned
	// blank-import home (§4.4) via its public facade twin. Without it
	// every Open fails loud with "unknown driver".
	_ "github.com/hurtener/Harbor/sdk/drivers/prod"

	"github.com/hurtener/Harbor/sdk/assemble"
	"github.com/hurtener/Harbor/sdk/config"
	"github.com/hurtener/Harbor/sdk/identity"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "embed-runonce:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()

	// 1. Build + validate the config (in-memory drivers by default;
	// point state/events/artifacts at sqlite/postgres for durability).
	cfg := config.Defaults()
	cfg.LLM.Driver = "bifrost"
	cfg.LLM.Provider = "openrouter"
	cfg.LLM.Model = "anthropic/claude-sonnet-4"
	cfg.LLM.APIKey = "env.OPENROUTER_API_KEY" // env-var indirection — never inline a key
	// Every model the run drives needs a profile (its context window) — the
	// run loop fails loud without one. Declare it for the model above.
	cfg.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{
		"anthropic/claude-sonnet-4": {ContextWindowTokens: 200000},
	}
	if err := cfg.ValidateCore(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// 2. Assemble the stack (one fan-out call; reverse-order Close).
	stack, err := assemble.Assemble(ctx, cfg, assemble.Options{Logger: slog.Default()})
	if err != nil {
		if stack != nil {
			_ = stack.Close(ctx)
		}
		return fmt.Errorf("assemble: %w", err)
	}
	defer func() { _ = stack.Close(ctx) }()

	// 3. Run one goal. Identity is mandatory (§6): the (tenant, user,
	// session) triple scopes the run. RunOnce blocks until the run
	// reaches a terminal answer and returns the canonical envelope.
	env, err := stack.RunOnce(ctx, "Summarise the latest deployment status.",
		identity.Identity{TenantID: "acme", UserID: "u-42", SessionID: "s-1"})
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}

	fmt.Printf("answer:        %s\n", env.Answer)
	fmt.Printf("finish_reason: %s\n", env.FinishReason)
	fmt.Printf("tool_calls:    %d\n", env.ToolCallsSeen)

	// 4. Typed output — ask the run for a schema-conforming final answer.
	// WithOutputSchema validates the terminal answer against the schema
	// and delivers the validated raw JSON on env.AnswerPayload (Answer
	// carries its string rendering). A schema-invalid answer after the
	// correction budget returns planner.ErrOutputInvalid — never a silent
	// fallback to unvalidated text. Streaming note: assistant-content
	// token deltas are suppressed for a schema-constrained run (the
	// validated answer arrives once, in the envelope).
	schema := json.RawMessage(`{
		"type": "object",
		"required": ["sentiment"],
		"properties": {
			"sentiment": {"type": "string", "enum": ["positive", "negative", "neutral"]},
			"confidence": {"type": "number"}
		},
		"additionalProperties": false
	}`)
	typedEnv, err := stack.RunOnce(ctx, "Classify the sentiment of the last deployment note.",
		identity.Identity{TenantID: "acme", UserID: "u-42", SessionID: "s-1"},
		assemble.WithOutputSchema(schema))
	if err != nil {
		return fmt.Errorf("typed run: %w", err)
	}
	var out struct {
		Sentiment  string  `json:"sentiment"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal(typedEnv.AnswerPayload, &out); err != nil {
		return fmt.Errorf("decode typed answer: %w", err)
	}
	fmt.Printf("typed answer:  sentiment=%s confidence=%.2f\n", out.Sentiment, out.Confidence)

	// 5. RunTyped — the generic sugar over step 4: derive the schema
	// FROM a Go type instead of hand-authoring the JSON Schema
	// document, and get the validated answer back already unmarshaled.
	// One call replaces WithOutputSchema + json.Unmarshal.
	sentiment, sentimentEnv, err := assemble.RunTyped[SentimentReport](ctx, stack,
		"Classify the sentiment of the last deployment note.",
		identity.Identity{TenantID: "acme", UserID: "u-42", SessionID: "s-1"})
	if err != nil {
		return fmt.Errorf("typed run (RunTyped): %w", err)
	}
	fmt.Printf("RunTyped answer: sentiment=%s confidence=%.2f finish_reason=%s\n",
		sentiment.Sentiment, sentiment.Confidence, sentimentEnv.FinishReason)
	return nil
}

// SentimentReport is the RunTyped target type for step 5 — the same
// shape the hand-authored schema in step 4 describes. RunTyped derives
// its JSON Schema from this Go type via reflection (the shared
// derivation sdk/tools/inproc.RegisterFunc also uses), runs a schema-
// constrained RunOnce, and unmarshals the validated answer into it.
type SentimentReport struct {
	Sentiment  string  `json:"sentiment"`
	Confidence float64 `json:"confidence,omitempty"`
}
