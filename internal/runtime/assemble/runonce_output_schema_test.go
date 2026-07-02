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

// schemaToolsEnvelopeDriver always answers a schema-constrained terminal
// turn with the OutputModeTools `{"name":"respond_with","arguments":...}`
// envelope shape — proving the react projector's S1 unwrap (not this
// package's assembled downgrade chain) is what makes AnswerPayload carry
// the caller's schema shape instead of the envelope.
type schemaToolsEnvelopeDriver struct{}

func (schemaToolsEnvelopeDriver) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	content := lastUserText(req)
	if req.ResponseFormat != nil && req.ResponseFormat.Kind == llm.FormatJSONSchema {
		content = fmt.Sprintf(`{"name":"respond_with","arguments":{"schema_title":%q}}`, schemaTitle(req.ResponseFormat.JSONSchema))
	}
	return llm.CompleteResponse{Content: content}, nil
}

func (schemaToolsEnvelopeDriver) Close(_ context.Context) error { return nil }

// schemaAlwaysSchemaErrorDriver always fails with a classified
// schema-class error (llm.ErrInvalidJSONSchema) on every attempt,
// regardless of the downgrade chain's current OutputMode/ResponseFormat
// shaping — used to drive the downgrade chain itself to exhaustion
// (llm.ErrDowngradeExhausted), as opposed to schemaBadDriver (which
// drives the RETRY loop to exhaustion via a Validator rejection).
type schemaAlwaysSchemaErrorDriver struct{}

func (schemaAlwaysSchemaErrorDriver) Complete(_ context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	return llm.CompleteResponse{}, fmt.Errorf("provider rejected request: %w", llm.ErrInvalidJSONSchema)
}

func (schemaAlwaysSchemaErrorDriver) Close(_ context.Context) error { return nil }

func init() {
	llm.Register("schema-echo-143", func(_ llm.ConfigSnapshot, _ llm.Deps) (llm.Driver, error) {
		return schemaEchoDriver{}, nil
	})
	llm.Register("schema-bad-143", func(_ llm.ConfigSnapshot, _ llm.Deps) (llm.Driver, error) {
		return schemaBadDriver{}, nil
	})
	llm.Register("schema-tools-envelope-143", func(_ llm.ConfigSnapshot, _ llm.Deps) (llm.Driver, error) {
		return schemaToolsEnvelopeDriver{}, nil
	})
	llm.Register("schema-always-schema-error-143", func(_ llm.ConfigSnapshot, _ llm.Deps) (llm.Driver, error) {
		return schemaAlwaysSchemaErrorDriver{}, nil
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

// TestRunOnce_WithOutputSchema_ToolsEnvelopeUnwrapped_LandsInAnswerPayload
// — S1 end-to-end: a driver that answers a schema-constrained terminal
// turn with the OutputModeTools respond_with envelope shape ends up with
// the UNWRAPPED arguments in AnswerPayload, never the envelope wrapper.
// Before the fix, the react Validator would validate the raw envelope
// against the caller's schema (guaranteed failure), and even had it
// passed, capturePayloadJSON would have captured the envelope bytes
// verbatim onto AnswerPayload instead of the caller's schema shape.
func TestRunOnce_WithOutputSchema_ToolsEnvelopeUnwrapped_LandsInAnswerPayload(t *testing.T) {
	ctx := context.Background()
	stack := schemaStack(t, "schema-tools-envelope-143")
	defer func() { _ = stack.Close(ctx) }()

	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-1"}
	env, err := stack.RunOnce(ctx, "classify", id, assemble.WithOutputSchema(titledSchema("tools-envelope-schema")))
	if err != nil {
		t.Fatalf("RunOnce(WithOutputSchema): %v", err)
	}
	if len(env.AnswerPayload) == 0 {
		t.Fatal("AnswerPayload is empty on a schema-constrained run")
	}
	if strings.Contains(string(env.AnswerPayload), "respond_with") {
		t.Fatalf("AnswerPayload still carries the respond_with envelope: %s", env.AnswerPayload)
	}
	var payload struct {
		SchemaTitle string `json:"schema_title"`
	}
	if err := json.Unmarshal(env.AnswerPayload, &payload); err != nil {
		t.Fatalf("AnswerPayload is not valid JSON: %v (%s)", err, env.AnswerPayload)
	}
	if payload.SchemaTitle != "tools-envelope-schema" {
		t.Errorf("payload schema_title = %q, want tools-envelope-schema", payload.SchemaTitle)
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

// TestRunOnce_WithOutputSchema_DowngradeExhausted_ErrOutputInvalid — N3:
// a provider that rejects every OutputMode/downgrade attempt surfaces
// llm.ErrDowngradeExhausted from the LLM edge (a DIFFERENT producer site
// than the retry-with-feedback loop's llm.ErrRetryExhausted exercised
// above), and RunOnce maps it to the SAME typed planner.ErrOutputInvalid
// — a provider that can't produce schema-shaped output IS a can't-
// produce-schema-output outcome regardless of which chain ran out.
func TestRunOnce_WithOutputSchema_DowngradeExhausted_ErrOutputInvalid(t *testing.T) {
	ctx := context.Background()
	stack := schemaStack(t, "schema-always-schema-error-143")
	defer func() { _ = stack.Close(ctx) }()

	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-1"}
	env, err := stack.RunOnce(ctx, "classify", id, assemble.WithOutputSchema(titledSchema("downgrade-schema")))
	if err == nil {
		t.Fatalf("RunOnce = nil error, want ErrOutputInvalid; env=%+v", env)
	}
	if !errors.Is(err, planner.ErrOutputInvalid) {
		t.Errorf("error = %v, want errors.Is ErrOutputInvalid", err)
	}
	if !errors.Is(err, llm.ErrDowngradeExhausted) {
		t.Errorf("error = %v, want the llm.ErrDowngradeExhausted chain", err)
	}
	if errors.Is(err, llm.ErrRetryExhausted) {
		t.Errorf("error = %v, want NO llm.ErrRetryExhausted (the downgrade chain exhausted before the retry loop ever saw a response)", err)
	}
	if env.Answer != "" || env.AnswerPayload != nil {
		t.Errorf("expected zero envelope on failure, got %+v", env)
	}
}

// fixedFinishPlanner is a scripted planner that returns a fixed terminal
// Finish decision on the very first Next call — no generation steering,
// no LLM call at all. Used by
// TestRunOnce_WithOutputSchema_NonGoalFinish_NoOutputInvalid to drive a
// non-goal terminal (Cancelled / NoPath) deterministically.
type fixedFinishPlanner struct {
	fin planner.Finish
}

func (p fixedFinishPlanner) Next(_ context.Context, _ planner.RunContext) (planner.Decision, error) {
	return p.fin, nil
}

// TestRunOnce_WithOutputSchema_NonGoalFinish_NoOutputInvalid — M1
// regression. A schema-constrained run whose terminal Finish is NOT
// FinishGoal (a steering CANCEL surfaced as Finish{Cancelled}, or a
// planner NoPath finish) must never engage the runtime-edge output-
// schema validation: that finish was never asked to produce a schema-
// shaped answer, so validating (or failing to capture) its payload as
// if it were a schema-constrained answer mislabels a cancellation /
// no-path outcome as a schema failure. The envelope must match plain
// (no-schema) run behaviour exactly: FinishReason verbatim, empty
// AnswerPayload, Answer via the ordinary ExtractAssistantAnswer
// collapse, and — the headline assertion — errors.Is(err,
// planner.ErrOutputInvalid) is FALSE.
func TestRunOnce_WithOutputSchema_NonGoalFinish_NoOutputInvalid(t *testing.T) {
	ctx := context.Background()
	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-1"}

	cases := []struct {
		name string
		fin  planner.Finish
	}{
		{
			name: "cancelled",
			fin: planner.Finish{
				Reason:   planner.FinishCancelled,
				Metadata: map[string]any{"steering": "cancelled"},
			},
		},
		{
			name: "no_path",
			fin: planner.Finish{
				Reason:   planner.FinishNoPath,
				Metadata: map[string]any{"followup": true},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := minimalCfg(t)
			cfg.LLM.Model = "mock/echo"
			cfg.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{
				"mock/echo": {ContextWindowTokens: 100000, TokenEstimator: "chars_div_4"},
			}
			stack, err := assemble.Assemble(ctx, cfg, assemble.Options{
				PlannerOverride: fixedFinishPlanner{fin: tc.fin},
			})
			if err != nil {
				t.Fatalf("Assemble: %v", err)
			}
			defer func() { _ = stack.Close(ctx) }()

			env, err := stack.RunOnce(ctx, "goal", id, assemble.WithOutputSchema(titledSchema("non-goal-schema")))
			if err != nil {
				t.Fatalf("RunOnce = %v, want nil error (a non-goal finish is not a schema failure)", err)
			}
			if errors.Is(err, planner.ErrOutputInvalid) {
				t.Errorf("errors.Is(err, ErrOutputInvalid) = true, want false for a %s finish", tc.name)
			}
			if env.FinishReason != string(tc.fin.Reason) {
				t.Errorf("FinishReason = %q, want %q", env.FinishReason, tc.fin.Reason)
			}
			if env.AnswerPayload != nil {
				t.Errorf("AnswerPayload = %s, want nil (untouched) on a non-goal finish", env.AnswerPayload)
			}
		})
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
//
// S4b: a subset of runs (both schema-constrained AND plain) additionally
// attach a WithStream sink, each capturing into its OWN per-goroutine
// counters (never a shared map — that would be exactly the kind of
// cross-run bleed this test is designed to catch). Asserts: a streamed
// schema run's sink sees ZERO token events but at least one step event;
// a streamed plain run's sink sees at least one token event carrying its
// OWN goal text (proving no foreign run's tokens landed in this run's
// sink).
func TestRunOnce_WithOutputSchema_ConcurrentMixedTraffic_NoBleed(t *testing.T) {
	baseline := goruntime.NumGoroutine()
	ctx := context.Background()
	stack := schemaStack(t, "schema-echo-143")

	const n = 120
	type result struct {
		idx        int
		schema     bool
		env        planner.AnswerEnvelope
		err        error
		cancelled  bool
		streamed   bool
		tokenCount int
		stepCount  int
		tokenTexts []string
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
			// A third of all runs (both schema and plain) additionally
			// stream — each goroutine owns its own sink + counters.
			withStream := i%3 == 0

			var (
				streamMu   sync.Mutex
				tokenCount int
				stepCount  int
				tokenTexts []string
			)
			var opts []assemble.RunOption
			if withStream {
				sink := func(e assemble.StreamEvent) {
					streamMu.Lock()
					defer streamMu.Unlock()
					switch e.Kind {
					case assemble.StreamToken:
						tokenCount++
						tokenTexts = append(tokenTexts, e.Text)
					case assemble.StreamStep:
						stepCount++
					}
				}
				opts = append(opts, assemble.WithStream(sink))
			}

			var env planner.AnswerEnvelope
			var err error
			if schemaRun {
				title := fmt.Sprintf("schema#%d#", i)
				runOpts := append([]assemble.RunOption{assemble.WithOutputSchema(titledSchema(title))}, opts...)
				env, err = stack.RunOnce(runCtx, "classify", id, runOpts...)
			} else {
				goal := fmt.Sprintf("goal#%d#", i)
				env, err = stack.RunOnce(runCtx, goal, id, opts...)
			}
			results[i] = result{
				idx: i, schema: schemaRun, env: env, err: err, cancelled: cancelled,
				streamed: withStream, tokenCount: tokenCount, stepCount: stepCount, tokenTexts: tokenTexts,
			}
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
			if r.streamed {
				if r.tokenCount != 0 {
					t.Errorf("run %d (schema, streamed): observed %d token events, want 0 (suppressed)", r.idx, r.tokenCount)
				}
				if r.stepCount == 0 {
					t.Errorf("run %d (schema, streamed): observed 0 step events, want >= 1", r.idx)
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
			if r.streamed {
				if r.tokenCount == 0 {
					t.Errorf("run %d (plain, streamed): observed 0 token events, want >= 1", r.idx)
				}
				// No cross-run sink bleed: every token text this run's OWN
				// sink observed must carry this run's own goal, never a
				// foreign run's.
				for _, tt := range r.tokenTexts {
					if !strings.Contains(tt, own) {
						t.Errorf("run %d: sink observed token %q not carrying its own goal %q — cross-run sink bleed", r.idx, tt, own)
					}
					for j := 1; j < n; j += 2 {
						if j == r.idx {
							continue
						}
						foreign := fmt.Sprintf("goal#%d#", j)
						if strings.Contains(tt, foreign) {
							t.Errorf("run %d: sink observed run %d's goal %q — CROSS-RUN SINK BLEED", r.idx, j, foreign)
						}
					}
				}
			}
		}
	}

	if err := stack.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
	settleGoroutines(t, baseline)
}
