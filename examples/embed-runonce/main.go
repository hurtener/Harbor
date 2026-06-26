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
	return nil
}
