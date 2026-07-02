// example_test.go — runnable, copy-pasteable godoc examples for the
// sdk/assemble facade: the one-call Assemble → Stack.RunOnce golden
// path, and the WithStream variant that observes a run's StreamEvents.
//
// These examples are mock-LLM-backed so they are deterministic and
// need no network. Every line an operator would copy out of an example
// BODY imports only the public sdk/ facade — that is the whole point of
// the facade. The production driver aggregator is blank-imported through
// its sdk facade (sdk/drivers/prod), so even the file-level imports stay
// inside sdk/. The ONE internal/ reference is the dev-only mock LLM
// (D-089): it has no sdk facade by design and is blank-imported HERE, in
// a _test file, only to keep the examples deterministic and offline — a
// real deployment configures a real provider in harbor.yaml and drops
// it. This mirrors internal/runtime/assemble/runonce_test.go.
package assemble_test

import (
	"context"
	"fmt"
	"log"
	"strings"

	// Production driver aggregator — the SAME sdk-facade blank import a
	// headless embedder adds (see docs/recipes/embed-harbor-headless.md)
	// so every Open the assembly performs resolves the production driver
	// set. Copyable as-is: it stays inside the sdk facade.
	_ "github.com/hurtener/Harbor/sdk/drivers/prod"
	// Dev-only mock LLM (D-089): explicit opt-in, never in the aggregator,
	// and deliberately has no sdk facade. It is imported here ONLY to keep
	// the example deterministic and offline; a real deployment configures a
	// real provider in harbor.yaml and drops this import.
	_ "github.com/hurtener/Harbor/internal/llm/mock"

	"github.com/hurtener/Harbor/sdk/assemble"
	"github.com/hurtener/Harbor/sdk/config"
	"github.com/hurtener/Harbor/sdk/identity"
	"github.com/hurtener/Harbor/sdk/planner"
)

// mockRunnableConfig builds the canonical baseline configuration and
// points the LLM seam at the deterministic, offline mock driver. It
// keeps the example bodies focused on the one-call run surface.
func mockRunnableConfig() *config.Config {
	cfg := config.Defaults()
	cfg.Telemetry.LogLevel = "error" // quiet lifecycle logs in examples
	cfg.LLM.Driver = "mock"
	cfg.LLM.Model = "mock/echo"
	cfg.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{
		"mock/echo": {ContextWindowTokens: 100000, TokenEstimator: "chars_div_4"},
	}
	return cfg
}

// Example shows the golden one-call path: Assemble composes a headless
// runtime from a validated config, and Stack.RunOnce turns a goal plus
// the mandatory (tenant, user, session) identity triple into a terminal
// answer envelope in a single blocking call.
func Example() {
	ctx := context.Background()

	cfg := mockRunnableConfig()
	stack, err := assemble.Assemble(ctx, cfg, assemble.Options{})
	if err != nil {
		log.Fatalf("assemble: %v", err)
	}
	defer func() { _ = stack.Close(ctx) }()

	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-1"}
	env, err := stack.RunOnce(ctx, "Summarise the deployment status.", id)
	if err != nil {
		fmt.Println("run:", err)
		return
	}

	fmt.Println(env.FinishReason)
	// Output: goal
}

// Example_streaming shows RunOnce driving the same run while a WithStream
// sink observes its StreamEvents — token deltas, planner-step
// boundaries, and tool dispatches — as they occur. RunOnce still blocks
// and returns the same terminal envelope; the sink fires synchronously
// on the run goroutine, so every StreamEvent arrives before RunOnce
// returns. Here the sink reassembles the streamed content tokens, which
// reproduce the final answer exactly.
func Example_streaming() {
	ctx := context.Background()

	cfg := mockRunnableConfig()
	stack, err := assemble.Assemble(ctx, cfg, assemble.Options{})
	if err != nil {
		log.Fatalf("assemble: %v", err)
	}
	defer func() { _ = stack.Close(ctx) }()

	var streamed strings.Builder
	sink := func(e assemble.StreamEvent) {
		// Content deltas only — skip reasoning tokens and non-token events.
		if e.Kind == assemble.StreamToken && !e.Reasoning {
			streamed.WriteString(e.Text)
		}
	}

	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-1"}
	env, err := stack.RunOnce(ctx, "Ping.", id, assemble.WithStream(sink))
	if err != nil {
		fmt.Println("run:", err)
		return
	}

	fmt.Println(streamed.String() == env.Answer)
	// Output: true
}

// sentimentReport is the RunTyped target type for Example_runTyped — a
// plain Go struct, no hand-authored JSON Schema required.
type sentimentReport struct {
	Sentiment  string  `json:"sentiment"`
	Confidence float64 `json:"confidence,omitempty"`
}

// fixedFinishPlanner is a deterministic planner.Planner concrete used
// ONLY to keep this example offline and reproducible: it returns a
// fixed terminal Finish payload with no LLM call at all, so
// Example_runTyped needs no network and no scripted-provider ceremony.
// A production RunTyped call rides the SAME schema-derivation +
// validation path against a real generation-steering planner — see
// docs/recipes/embed-harbor-headless.md and examples/embed-runonce/.
type fixedFinishPlanner struct{ payload any }

func (p fixedFinishPlanner) Next(_ context.Context, _ planner.RunContext) (planner.Decision, error) {
	return planner.Finish{Reason: planner.FinishGoal, Payload: p.payload}, nil
}

// Example_runTyped shows the generic typed embed binding: RunTyped
// derives a JSON Schema from a Go type (the same reflection-based
// derivation sdk/tools/inproc.RegisterFunc uses for tool registration),
// drives a schema-constrained run, and returns the validated answer
// already unmarshaled into that type — replacing the WithOutputSchema
// + json.Unmarshal two-liner with one generic call.
func Example_runTyped() {
	ctx := context.Background()

	cfg := mockRunnableConfig()
	stack, err := assemble.Assemble(ctx, cfg, assemble.Options{
		PlannerOverride: fixedFinishPlanner{
			payload: map[string]any{"sentiment": "positive", "confidence": 0.9},
		},
	})
	if err != nil {
		log.Fatalf("assemble: %v", err)
	}
	defer func() { _ = stack.Close(ctx) }()

	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-1"}
	report, env, err := assemble.RunTyped[sentimentReport](ctx, stack, "Classify the sentiment.", id)
	if err != nil {
		fmt.Println("run:", err)
		return
	}

	fmt.Println(report.Sentiment, env.FinishReason)
	// Output: positive goal
}
