// Command portable-context exercises a persistent, version-checked editing agent
// through the public Harbor SDK. Separate invocations restore the same session.
// Only explicit live runs call the configured Bifrost provider; inspect does not.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	_ "github.com/hurtener/Harbor/sdk/drivers/prod" // Seat the production drivers, including Bifrost and SQLite.

	"github.com/hurtener/Harbor/sdk/assemble"
	"github.com/hurtener/Harbor/sdk/config"
	"github.com/hurtener/Harbor/sdk/events"
	"github.com/hurtener/Harbor/sdk/identity"
	"github.com/hurtener/Harbor/sdk/llm"
	"github.com/hurtener/Harbor/sdk/planner"
	"github.com/hurtener/Harbor/sdk/state"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := command(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		// Provider errors may contain sensitive response text. This command
		// reports no arbitrary failure details to ordinary terminal logs.
		fmt.Fprintln(os.Stderr, "portable-context: operation did not complete; check -help and configuration, and inspect the document before retrying writes")
		os.Exit(1)
	}
}

func command(ctx context.Context, args []string, out, diagnostic io.Writer) (retErr error) {
	flags := flag.NewFlagSet("portable-context", flag.ContinueOnError)
	flags.SetOutput(diagnostic)
	provider := flags.String("provider", "", "Bifrost provider name; required for a live run")
	model := flags.String("model", "", "model ID authorized for this provider; required for a live run")
	window := flags.Int("context-window", 0, "verified model context capacity in tokens; required for a live run")
	outputLimit := flags.Int("max-output", 2048, "maximum output tokens per completion")
	target := flags.Int("token-budget", 12000, "working-input target before compaction")
	turns := flags.Int("retained-turns", 8, "retained recent turns, 1..32")
	baseURL := flags.String("base-url", "", "optional trusted provider base URL; never supply an untrusted credential destination")
	dataDir := flags.String("data-dir", ".portable-context", "private local test data directory")
	tenant := flags.String("tenant", "context-lab", "test tenant")
	user := flags.String("user", "tester", "test user")
	session := flags.String("session", "editing", "retained conversation; change this for an isolated document")
	inspect := flags.Bool("inspect", false, "print actual document JSON without inference, seeding or editing")
	prompt := flags.String("prompt", "", "one user turn; the selected provider may charge for inference")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || (!*inspect && (*provider == "" || *model == "" || *window <= 0 || *prompt == "" || *outputLimit <= 0 || *outputLimit >= *window || *target <= 0 || *turns < 1 || *turns > 32)) || (*inspect && *prompt != "") {
		return errors.New("supply either -inspect or explicit provider, model, capacity and prompt; use bounded positive output, budget and retained turns")
	}
	id := identity.Identity{TenantID: *tenant, UserID: *user, SessionID: *session}
	ctx, err := identity.With(ctx, id)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(*dataDir, 0700); err != nil {
		return err
	}
	stateConfig := config.StateConfig{Driver: "sqlite", DSN: filepath.Join(*dataDir, "state.db")}
	if *inspect {
		store, err := state.Open(ctx, stateConfig)
		if err != nil {
			return err
		}
		defer func() { retErr = errors.Join(retErr, store.Close(context.Background())) }()
		doc, _, err := loadDocument(ctx, store)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(doc)
	}
	// Headless embedding uses ValidateCore, not a Protocol JWT configuration.
	// No HARBOR_* environment override layer is claimed by this small example.
	cfg := config.Defaults()
	cfg.State = stateConfig
	cfg.Artifacts.Driver, cfg.Artifacts.FSRoot = "fs", filepath.Join(*dataDir, "artifacts")
	cfg.Memory.Strategy = "none"
	cfg.Sessions.RetainedContextTurns = *turns
	cfg.Planner.TokenBudget, cfg.Planner.MaxSteps = *target, 12
	cfg.LLM.Driver, cfg.LLM.Provider, cfg.LLM.Model = "bifrost", *provider, *model
	cfg.LLM.APIKey, cfg.LLM.BaseURL = "env.PORTABLE_CONTEXT_API_KEY", *baseURL
	cfg.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{
		*model: {ContextWindowTokens: *window, DefaultMaxTokens: outputLimit},
	}
	if err = cfg.ValidateCore(); err != nil {
		return err
	}
	// No raw provider-error logging in this synthetic corpus example. Inspect
	// counts through the returned envelope; full diagnostics use Harbor Protocol.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	stack, err := assemble.Assemble(ctx, cfg, assemble.Options{Logger: logger})
	if stack != nil {
		defer func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			retErr = errors.Join(retErr, stack.Close(closeCtx))
		}()
	}
	if err != nil {
		return err
	}
	if err = seedDocument(ctx, stack.State); err != nil {
		return err
	}
	if err = registerDocuments(stack.State, stack.Catalog); err != nil {
		return err
	}
	runID := "context-lab-" + string(state.NewEventID())
	envelope, err := stack.RunOnce(ctx, *prompt, id, assemble.WithRunID(runID))
	if err != nil {
		return err
	}
	result := sampleResult{AnswerEnvelope: envelope, RunID: runID}
	if replay, ok := stack.Bus.(events.Replayer); ok {
		entries, replayErr := replay.Replay(ctx, events.Cursor{}, events.Filter{
			Tenant: id.TenantID, User: id.UserID, Session: id.SessionID, Run: runID,
			Types: []events.EventType{llm.EventTypeContextPrepared},
		})
		if replayErr == nil {
			result.DiagnosticsAvailable = true
			if len(entries) > 64 {
				entries = entries[len(entries)-64:]
				result.DiagnosticsTruncated = true
			}
			for _, entry := range entries {
				if payload, ok := entry.Payload.(llm.ContextPreparedPayload); ok {
					result.Context = append(result.Context, payload)
				}
			}
		}
	}
	// Diagnostic replay is best effort, never a reason to repeat a successful
	// edit. Only fixed-shape capacity events are copied, not arbitrary payloads.
	return json.NewEncoder(out).Encode(result)
}

// This is example output, not an additional Harbor Protocol contract.
type sampleResult struct {
	planner.AnswerEnvelope
	RunID                string                       `json:"run_id"`
	DiagnosticsAvailable bool                         `json:"diagnostics_available"`
	DiagnosticsTruncated bool                         `json:"diagnostics_truncated"`
	Context              []llm.ContextPreparedPayload `json:"context"`
}
