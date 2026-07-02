// runtyped_test.go — coverage for the typed embed binding (D-273):
// the happy path (validated payload unmarshaled into T), the
// call-time unsupported-T rejection (before any run starts), the
// WithOutputSchema conflict, the validated-but-unmarshalable payload
// (schema-valid, Go-unmarshal-invalid), and the mandatory N>=100
// concurrent-reuse stress mixing >=3 distinct T types plus plain
// RunOnce calls against ONE shared Stack (D-025 / CLAUDE.md §5 / §11).
package assemble_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	goruntime "runtime"
	"sync"
	"testing"

	// Production driver aggregator + dev-only mock LLM — same pattern
	// as runonce_test.go.
	_ "github.com/hurtener/Harbor/internal/drivers/prod"
	_ "github.com/hurtener/Harbor/internal/llm/mock"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/tools/schema"
)

// runTypedFinish is a one-line helper building the fixedFinishPlanner
// (defined in runonce_output_schema_test.go, same package) with a
// goal-satisfying Finish carrying the given payload — RunTyped's
// schema-validation + unmarshal path exercised with NO generation
// steering (mirrors
// test/integration/phase143_output_schema_test.go's
// phase143FixedFinishPlanner).
func runTypedFinish(payload any) fixedFinishPlanner {
	return fixedFinishPlanner{fin: planner.Finish{Reason: planner.FinishGoal, Payload: payload}}
}

// runTypedEchoPlanner returns the run's OWN goal text (rc.Query)
// verbatim as the terminal Finish payload. The mixed-type concurrent
// test encodes each call's EXPECTED answer as its own goal, so a bug
// that let one run's schema/payload bleed into another's surfaces as a
// decode/field mismatch on the wrong run.
type runTypedEchoPlanner struct{}

func (runTypedEchoPlanner) Next(_ context.Context, rc planner.RunContext) (planner.Decision, error) {
	return planner.Finish{Reason: planner.FinishGoal, Payload: rc.Query}, nil
}

func runTypedStack(t *testing.T, override planner.Planner) *assemble.Stack {
	t.Helper()
	cfg := minimalCfg(t)
	cfg.LLM.Model = "mock/echo"
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{PlannerOverride: override})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	return stack
}

type runTypedSentiment struct {
	Sentiment  string  `json:"sentiment"`
	Confidence float64 `json:"confidence"`
}

type runTypedTicket struct {
	ID       string `json:"id"`
	Priority int    `json:"priority"`
}

type runTypedProfile struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// TestRunTyped_HappyPath_UnmarshalsValidatedPayload — the golden path:
// the fixed-finish planner's conforming payload validates and
// unmarshals cleanly into T.
func TestRunTyped_HappyPath_UnmarshalsValidatedPayload(t *testing.T) {
	ctx := context.Background()
	stack := runTypedStack(t, runTypedFinish(map[string]any{"sentiment": "positive", "confidence": 0.9}))
	defer func() { _ = stack.Close(ctx) }()

	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-1"}
	out, env, err := assemble.RunTyped[runTypedSentiment](ctx, stack, "classify the sentiment", id)
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if out.Sentiment != "positive" || out.Confidence != 0.9 {
		t.Errorf("out = %+v, want {positive 0.9}", out)
	}
	if env.FinishReason != string(planner.FinishGoal) {
		t.Errorf("FinishReason = %q, want %q", env.FinishReason, planner.FinishGoal)
	}
	if len(env.AnswerPayload) == 0 {
		t.Error("expected a non-empty AnswerPayload")
	}
}

// TestRunTyped_UnsupportedType_FailsLoudBeforeRun — an unsupported T
// (a channel field) is rejected at call time via the derivation's own
// typed error, before the run loop ever starts.
func TestRunTyped_UnsupportedType_FailsLoudBeforeRun(t *testing.T) {
	ctx := context.Background()
	stack := runTypedStack(t, runTypedFinish(map[string]any{}))
	defer func() { _ = stack.Close(ctx) }()

	type badType struct {
		Ch chan int `json:"ch"`
	}
	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-1"}
	_, _, err := assemble.RunTyped[badType](ctx, stack, "goal", id)
	if !errors.Is(err, schema.ErrUnsupportedType) {
		t.Fatalf("RunTyped[badType] = %v, want errors.Is schema.ErrUnsupportedType", err)
	}
}

// TestRunTyped_WithOutputSchemaConflict_FailsLoud — a caller-supplied
// WithOutputSchema alongside RunTyped is a loud conflict, never a
// silent override.
func TestRunTyped_WithOutputSchemaConflict_FailsLoud(t *testing.T) {
	ctx := context.Background()
	stack := runTypedStack(t, runTypedFinish(map[string]any{"sentiment": "positive"}))
	defer func() { _ = stack.Close(ctx) }()

	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-1"}
	_, _, err := assemble.RunTyped[runTypedSentiment](ctx, stack, "goal", id,
		assemble.WithOutputSchema(json.RawMessage(`{"type":"object"}`)))
	if !errors.Is(err, assemble.ErrRunTypedSchemaConflict) {
		t.Fatalf("RunTyped with a conflicting WithOutputSchema = %v, want ErrRunTypedSchemaConflict", err)
	}
}

// TestRunTyped_ValidatedButUnmarshalable_FailsLoud — a payload that
// VALIDATES against T's derived schema ({"type":"integer"} carries no
// bounds) but overflows a narrower Go numeric type fails loud with
// ErrRunTypedUnmarshal, never a zero-value-with-nil-error return.
func TestRunTyped_ValidatedButUnmarshalable_FailsLoud(t *testing.T) {
	type narrowType struct {
		Age int8 `json:"age"`
	}
	ctx := context.Background()
	// 200000 is a valid JSON integer (schema: {"type":"integer"}, no
	// bounds) but overflows int8 (max 127) on unmarshal.
	stack := runTypedStack(t, runTypedFinish(map[string]any{"age": 200000}))
	defer func() { _ = stack.Close(ctx) }()

	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-1"}
	out, _, err := assemble.RunTyped[narrowType](ctx, stack, "goal", id)
	if !errors.Is(err, assemble.ErrRunTypedUnmarshal) {
		t.Fatalf("RunTyped[narrowType] = %v, want errors.Is ErrRunTypedUnmarshal", err)
	}
	if out != (narrowType{}) {
		t.Errorf("out = %+v on error, want the zero value", out)
	}
}

// TestRunTyped_SchemaInvalid_ReturnsErrOutputInvalid — a non-conforming
// payload surfaces the SAME planner.ErrOutputInvalid RunOnce itself
// returns; RunTyped adds no second error shape for this case.
func TestRunTyped_SchemaInvalid_ReturnsErrOutputInvalid(t *testing.T) {
	ctx := context.Background()
	// Missing the required "sentiment" field.
	stack := runTypedStack(t, runTypedFinish(map[string]any{"confidence": 0.1}))
	defer func() { _ = stack.Close(ctx) }()

	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-1"}
	_, _, err := assemble.RunTyped[runTypedSentiment](ctx, stack, "goal", id)
	if !errors.Is(err, planner.ErrOutputInvalid) {
		t.Fatalf("RunTyped(non-conforming payload) = %v, want errors.Is planner.ErrOutputInvalid", err)
	}
}

// TestRunTyped_ConcurrentReuse_MixedTypes_NoBleedNoLeak — the
// mandatory D-025 concurrent-reuse stress: N>=100 concurrent calls
// against ONE shared Stack, mixing THREE distinct T types plus plain
// RunOnce calls, asserting no data races (-race), no schema/payload
// bleed across runs (each call's goal encodes its OWN expected
// answer), and no goroutine leak (baseline restored after Close).
func TestRunTyped_ConcurrentReuse_MixedTypes_NoBleedNoLeak(t *testing.T) {
	baseline := goruntime.NumGoroutine()
	ctx := context.Background()
	stack := runTypedStack(t, runTypedEchoPlanner{})

	const n = 160
	type result struct {
		idx int
		err error
		ok  bool
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  fmt.Sprintf("t-%d", i),
				UserID:    fmt.Sprintf("u-%d", i),
				SessionID: fmt.Sprintf("s-%d", i),
			}
			switch i % 4 {
			case 0:
				want := runTypedSentiment{Sentiment: fmt.Sprintf("s-%d", i), Confidence: float64(i) / 1000}
				goalJSON, err := json.Marshal(want)
				if err != nil {
					results[i] = result{idx: i, err: err}
					return
				}
				out, env, err := assemble.RunTyped[runTypedSentiment](ctx, stack, string(goalJSON), id)
				ok := err == nil && out == want && env.FinishReason == string(planner.FinishGoal)
				results[i] = result{idx: i, err: err, ok: ok}
			case 1:
				want := runTypedTicket{ID: fmt.Sprintf("tix-%d", i), Priority: i}
				goalJSON, err := json.Marshal(want)
				if err != nil {
					results[i] = result{idx: i, err: err}
					return
				}
				out, env, err := assemble.RunTyped[runTypedTicket](ctx, stack, string(goalJSON), id)
				ok := err == nil && out == want && env.FinishReason == string(planner.FinishGoal)
				results[i] = result{idx: i, err: err, ok: ok}
			case 2:
				want := runTypedProfile{Name: fmt.Sprintf("p-%d", i), Tags: []string{fmt.Sprintf("tag-%d", i)}}
				goalJSON, err := json.Marshal(want)
				if err != nil {
					results[i] = result{idx: i, err: err}
					return
				}
				out, env, err := assemble.RunTyped[runTypedProfile](ctx, stack, string(goalJSON), id)
				ok := err == nil && out.Name == want.Name && len(out.Tags) == 1 && out.Tags[0] == want.Tags[0] &&
					env.FinishReason == string(planner.FinishGoal)
				results[i] = result{idx: i, err: err, ok: ok}
			default:
				// Plain RunOnce mixed in — proves RunTyped's per-call
				// WithOutputSchema append never leaks onto a sibling
				// plain call against the SAME shared stack.
				goal := fmt.Sprintf("plain-goal-%d", i)
				env, err := stack.RunOnce(ctx, goal, id)
				ok := err == nil && env.FinishReason == string(planner.FinishGoal) && env.Answer == goal && env.AnswerPayload == nil
				results[i] = result{idx: i, err: err, ok: ok}
			}
		}(i)
	}
	wg.Wait()

	for _, r := range results {
		if r.err != nil {
			t.Errorf("run %d: %v", r.idx, r.err)
			continue
		}
		if !r.ok {
			t.Errorf("run %d: result mismatch (context/schema/payload bleed?)", r.idx)
		}
	}

	if err := stack.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
	settleGoroutines(t, baseline)
}
