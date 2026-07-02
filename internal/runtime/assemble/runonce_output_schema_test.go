// runonce_output_schema_test.go — coverage for run-level structured
// output (D-272): WithOutputSchema option validation, the runtime-edge
// final validation (validated answer_payload on the envelope, typed
// ErrOutputInvalid on a schema-invalid answer after the correction
// budget), the token-suppression streaming posture, and the mandatory
// D-025 mixed-traffic concurrent-reuse stress (schema-constrained and
// plain runs interleaved against ONE shared Stack, distinct schemas, no
// schema/payload bleed).
package assemble_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"

	_ "github.com/hurtener/Harbor/internal/drivers/prod"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
)

// schemaEchoDriver is a stateless test LLM driver: on a schema-
// constrained turn it emits a JSON object echoing the schema's `title`
// (so a test can prove the correct per-run schema reached the driver);
// on a plain turn it echoes the last user message. Stateless → safe for
// concurrent Complete (the mixed-traffic D-025 stress shares one).
type schemaEchoDriver struct{}

func (schemaEchoDriver) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	content := lastUserText(req)
	if req.ResponseFormat != nil && req.ResponseFormat.Kind == llm.FormatJSONSchema {
		content = fmt.Sprintf(`{"schema_title":%q}`, schemaTitle(req.ResponseFormat.JSONSchema))
	}
	// Stream when asked so a step boundary (done=true) fires — the
	// suppression test asserts token deltas are dropped but step events
	// survive under a schema-constrained run.
	if req.Stream && req.OnContent != nil {
		req.OnContent(content, false)
		req.OnContent("", true)
	}
	return llm.CompleteResponse{Content: content}, nil
}

func (schemaEchoDriver) Close(_ context.Context) error { return nil }

// schemaBadDriver always emits a non-conforming payload — used to drive
// the retry loop to exhaustion → ErrOutputInvalid.
type schemaBadDriver struct{}

func (schemaBadDriver) Complete(_ context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	return llm.CompleteResponse{Content: `{"wrong":"shape"}`}, nil
}

func (schemaBadDriver) Close(_ context.Context) error { return nil }

func init() {
	llm.Register("schema-echo-143", func(_ llm.ConfigSnapshot, _ llm.Deps) (llm.Driver, error) {
		return schemaEchoDriver{}, nil
	})
	llm.Register("schema-bad-143", func(_ llm.ConfigSnapshot, _ llm.Deps) (llm.Driver, error) {
		return schemaBadDriver{}, nil
	})
}

func schemaTitle(raw json.RawMessage) string {
	var doc struct {
		Title string `json:"title"`
	}
	_ = json.Unmarshal(raw, &doc) // test helper; empty title is acceptable
	return doc.Title
}

func lastUserText(req llm.CompleteRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role == llm.RoleUser && m.Content.Text != nil {
			return *m.Content.Text
		}
	}
	return "no user message"
}

// schemaStack builds a runnable stack whose LLM is the named test driver.
func schemaStack(t *testing.T, driver string) *assemble.Stack {
	t.Helper()
	cfg := minimalCfg(t)
	cfg.LLM.Driver = driver
	cfg.LLM.Model = "test/model"
	cfg.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{
		// JSONSchemaMode "native" pins OutputMode=Native so the terminal
		// completion carries FormatJSONSchema through to the driver
		// unchanged (candidate A — the profile's OutputMode decides).
		"test/model": {ContextWindowTokens: 100000, TokenEstimator: "chars_div_4", JSONSchemaMode: "native", MaxRetries: 1},
	}
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{})
	if err != nil {
		t.Fatalf("Assemble(%s): %v", driver, err)
	}
	if stack.RunLoop == nil || stack.Planner == nil {
		t.Fatalf("runnable stack must have RunLoop + Planner")
	}
	return stack
}

// titledSchema returns a distinct-per-title object schema satisfied by
// {"schema_title":"<title>"}.
func titledSchema(title string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"type":"object","title":%q,"required":["schema_title"],"properties":{"schema_title":{"type":"string"}},"additionalProperties":true}`,
		title,
	))
}

// TestRunOnce_WithOutputSchema_EmptySchema_FailsLoud — a nil/empty
// schema is a loud config error at call time, never a silent no-op.
func TestRunOnce_WithOutputSchema_EmptySchema_FailsLoud(t *testing.T) {
	ctx := context.Background()
	stack := schemaStack(t, "schema-echo-143")
	defer func() { _ = stack.Close(ctx) }()

	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-1"}
	for _, raw := range []json.RawMessage{nil, json.RawMessage(""), json.RawMessage("  \n ")} {
		_, err := stack.RunOnce(ctx, "goal", id, assemble.WithOutputSchema(raw))
		if err == nil {
			t.Errorf("RunOnce(WithOutputSchema(%q)) = nil error, want a loud error", string(raw))
		}
	}
}

// TestRunOnce_WithOutputSchema_HappyPath_ValidatedPayload — a schema-
// constrained run returns the validated raw JSON as AnswerPayload, with
// Answer carrying its string rendering.
func TestRunOnce_WithOutputSchema_HappyPath_ValidatedPayload(t *testing.T) {
	ctx := context.Background()
	stack := schemaStack(t, "schema-echo-143")
	defer func() { _ = stack.Close(ctx) }()

	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-1"}
	env, err := stack.RunOnce(ctx, "classify", id, assemble.WithOutputSchema(titledSchema("happy-schema")))
	if err != nil {
		t.Fatalf("RunOnce(WithOutputSchema): %v", err)
	}
	if env.FinishReason != string(planner.FinishGoal) {
		t.Errorf("FinishReason = %q, want %q", env.FinishReason, planner.FinishGoal)
	}
	if len(env.AnswerPayload) == 0 {
		t.Fatal("AnswerPayload is empty on a schema-constrained run")
	}
	// The payload validates against the schema and carries the schema's
	// title (proving the correct schema reached the driver).
	var payload struct {
		SchemaTitle string `json:"schema_title"`
	}
	if err := json.Unmarshal(env.AnswerPayload, &payload); err != nil {
		t.Fatalf("AnswerPayload is not valid JSON: %v (%s)", err, env.AnswerPayload)
	}
	if payload.SchemaTitle != "happy-schema" {
		t.Errorf("payload schema_title = %q, want happy-schema", payload.SchemaTitle)
	}
	if env.Answer != string(env.AnswerPayload) {
		t.Errorf("Answer = %q, want the payload string rendering %q", env.Answer, env.AnswerPayload)
	}
}

// TestRunOnce_WithOutputSchema_InvalidOutput_ErrOutputInvalid — a run
// whose model never conforms exhausts the correction budget and returns
// the typed planner.ErrOutputInvalid (never a silent text fallback), and
// the chain carries llm.ErrRetryExhausted.
func TestRunOnce_WithOutputSchema_InvalidOutput_ErrOutputInvalid(t *testing.T) {
	ctx := context.Background()
	stack := schemaStack(t, "schema-bad-143")
	defer func() { _ = stack.Close(ctx) }()

	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-1"}
	env, err := stack.RunOnce(ctx, "classify", id, assemble.WithOutputSchema(titledSchema("strict-schema")))
	if err == nil {
		t.Fatalf("RunOnce = nil error, want ErrOutputInvalid; env=%+v", env)
	}
	if !errors.Is(err, planner.ErrOutputInvalid) {
		t.Errorf("error = %v, want errors.Is ErrOutputInvalid", err)
	}
	if !errors.Is(err, llm.ErrRetryExhausted) {
		t.Errorf("error = %v, want the llm.ErrRetryExhausted chain", err)
	}
	// No silent text fallback: the envelope is the zero value.
	if env.Answer != "" || env.AnswerPayload != nil {
		t.Errorf("expected zero envelope on failure, got %+v", env)
	}
}

// TestRunOnce_WithOutputSchema_StreamSuppressesTokens — WithStream +
// WithOutputSchema compose: no token events reach the sink, step events
// still do, and every event precedes the envelope.
func TestRunOnce_WithOutputSchema_StreamSuppressesTokens(t *testing.T) {
	ctx := context.Background()
	stack := schemaStack(t, "schema-echo-143")
	defer func() { _ = stack.Close(ctx) }()

	var (
		mu     sync.Mutex
		kinds  []assemble.StreamEventKind
		tokens int
	)
	sink := func(e assemble.StreamEvent) {
		mu.Lock()
		defer mu.Unlock()
		kinds = append(kinds, e.Kind)
		if e.Kind == assemble.StreamToken {
			tokens++
		}
	}

	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-1"}
	env, err := stack.RunOnce(ctx, "classify", id,
		assemble.WithOutputSchema(titledSchema("stream-schema")),
		assemble.WithStream(sink))
	if err != nil {
		t.Fatalf("RunOnce(schema+stream): %v", err)
	}
	if len(env.AnswerPayload) == 0 {
		t.Fatal("expected a validated payload")
	}
	mu.Lock()
	defer mu.Unlock()
	if tokens != 0 {
		t.Errorf("observed %d token events under WithOutputSchema, want 0 (suppressed)", tokens)
	}
	// step events still flow (the terminal step boundary is not a token).
	hasStep := false
	for _, k := range kinds {
		if k == assemble.StreamStep {
			hasStep = true
		}
	}
	if !hasStep {
		t.Error("expected at least one step event under a schema-constrained streamed run")
	}
}

// TestRunOnce_WithOutputSchema_ConcurrentMixedTraffic_NoBleed — the
// mandatory D-025 mixed-traffic stress: N≥100 concurrent RunOnce calls
// against ONE shared Stack, interleaving schema-constrained runs (each
// with a DISTINCT schema) and plain runs. Asserts no schema/payload
// bleed, no cross-cancellation, goroutine baseline restored, under -race.
func TestRunOnce_WithOutputSchema_ConcurrentMixedTraffic_NoBleed(t *testing.T) {
	baseline := goruntime.NumGoroutine()
	ctx := context.Background()
	stack := schemaStack(t, "schema-echo-143")

	const n = 120
	type result struct {
		idx       int
		schema    bool
		env       planner.AnswerEnvelope
		err       error
		cancelled bool
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			runCtx := ctx
			cancelled := false
			if i%7 == 0 {
				c, cancel := context.WithCancel(ctx)
				cancel()
				runCtx = c
				cancelled = true
			}
			id := identity.Identity{
				TenantID:  "t-" + fmt.Sprint(i),
				UserID:    "u-" + fmt.Sprint(i),
				SessionID: "s-" + fmt.Sprint(i),
			}
			// Even runs are schema-constrained (distinct title per run);
			// odd runs are plain (goal echo). Interleaved on one Stack.
			schemaRun := i%2 == 0
			var env planner.AnswerEnvelope
			var err error
			if schemaRun {
				title := fmt.Sprintf("schema#%d#", i)
				env, err = stack.RunOnce(runCtx, "classify", id, assemble.WithOutputSchema(titledSchema(title)))
			} else {
				goal := fmt.Sprintf("goal#%d#", i)
				env, err = stack.RunOnce(runCtx, goal, id)
			}
			results[i] = result{idx: i, schema: schemaRun, env: env, err: err, cancelled: cancelled}
		}(i)
	}
	wg.Wait()

	for _, r := range results {
		if r.cancelled {
			continue // cancellation is allowed to fail; the point is it did not corrupt others
		}
		if r.err != nil {
			t.Errorf("run %d (uncancelled) failed: %v", r.idx, r.err)
			continue
		}
		if r.schema {
			own := fmt.Sprintf("schema#%d#", r.idx)
			if !strings.Contains(string(r.env.AnswerPayload), own) {
				t.Errorf("run %d: payload %s does not carry its own schema %q — schema/payload bleed", r.idx, r.env.AnswerPayload, own)
			}
			// No foreign schema's title leaked into this run's payload.
			for j := 0; j < n; j += 2 {
				if j == r.idx {
					continue
				}
				foreign := fmt.Sprintf("schema#%d#", j)
				if strings.Contains(string(r.env.AnswerPayload), foreign) {
					t.Errorf("run %d: payload carries run %d's schema %q — CROSS-RUN SCHEMA BLEED", r.idx, j, foreign)
				}
			}
		} else {
			// Plain run: no schema, no payload; answer echoes its own goal.
			if r.env.AnswerPayload != nil {
				t.Errorf("run %d: plain run carried an AnswerPayload %s", r.idx, r.env.AnswerPayload)
			}
			own := fmt.Sprintf("goal#%d#", r.idx)
			if !strings.Contains(r.env.Answer, own) {
				t.Errorf("run %d: plain answer %q does not carry its own goal %q — context bleed", r.idx, r.env.Answer, own)
			}
		}
	}

	if err := stack.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
	settleGoroutines(t, baseline)
}
