// phase143_output_schema_test.go — the §13 primitive-with-consumer E2E
// for run-level structured output (D-272). A schema-constrained RunOnce
// drives through the REAL RunLoop (real inmem drivers + real bus, the
// full safety→corrections→downgrade→retry→governance LLM chain composed
// by assemble.Assemble) across four legs:
//
//   - happy path — the validated payload lands on the envelope;
//   - corrective-retry — the first response is schema-invalid, an
//     llm.retry_with_feedback event fires, the second response validates;
//   - exhaustion — every response is invalid; RunOnce returns the typed
//     planner.ErrOutputInvalid (with the llm.ErrRetryExhausted chain);
//   - planner-agnostic — a deterministic planner's Finish payload is
//     validated at the runtime edge (conforming passes, non-conforming
//     fails loud) with NO generation steering.
//
// Run under -race. No mocks at the seam except the scripted LLM DRIVER
// (registered through the SAME registry path bifrost uses, so the whole
// correction/retry chain is exercised for real).
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/hurtener/Harbor/internal/drivers/prod"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
)

// phase143Schema is the run output schema every leg constrains against.
const phase143Schema = `{
	"type": "object",
	"required": ["sentiment"],
	"properties": {
		"sentiment": {"type": "string", "enum": ["positive", "negative", "neutral"]},
		"confidence": {"type": "number"}
	},
	"additionalProperties": false
}`

// phase143ScriptedDriver returns a canned response per Complete call,
// advancing a cursor (repeating the last entry once exhausted). The
// retry wrapper calls it multiple times within one terminal turn; a
// leg scripts [invalid, valid] to exercise the corrective loop.
type phase143ScriptedDriver struct {
	mu        sync.Mutex
	contents  []string
	cursor    int
	callCount atomic.Int64
}

func (d *phase143ScriptedDriver) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	d.callCount.Add(1)
	d.mu.Lock()
	idx := d.cursor
	if idx >= len(d.contents) {
		idx = len(d.contents) - 1
	}
	content := d.contents[idx]
	if d.cursor < len(d.contents) {
		d.cursor++
	}
	d.mu.Unlock()
	if req.Stream && req.OnContent != nil {
		req.OnContent(content, false)
		req.OnContent("", true)
	}
	return llm.CompleteResponse{Content: content}, nil
}

func (d *phase143ScriptedDriver) Close(_ context.Context) error { return nil }

var phase143DriverSeq atomic.Int64

// registerScriptedDriver registers a fresh scripted driver under a
// unique name and returns the name + the instance (so the test can
// assert call counts).
func registerScriptedDriver(contents ...string) (string, *phase143ScriptedDriver) {
	drv := &phase143ScriptedDriver{contents: contents}
	name := fmt.Sprintf("phase143-scripted-%d", phase143DriverSeq.Add(1))
	llm.Register(name, func(_ llm.ConfigSnapshot, _ llm.Deps) (llm.Driver, error) {
		return drv, nil
	})
	return name, drv
}

// phase143FixedFinishPlanner is the deterministic-planner leg: it
// returns a terminal Finish with a fixed payload on the first Next,
// exercising NO generation steering — the runtime edge validates it.
type phase143FixedFinishPlanner struct{ payload any }

func (p phase143FixedFinishPlanner) Next(_ context.Context, _ planner.RunContext) (planner.Decision, error) {
	return planner.Finish{Reason: planner.FinishGoal, Payload: p.payload}, nil
}

// phase143Stack assembles a real stack whose LLM is the named scripted
// driver — injected via Options.LLMSnapshot so the config validator
// (which restricts cfg.LLM.driver to bifrost/mock) is satisfied while
// the assembled LLM client resolves the test driver through the SAME
// registry path bifrost uses (the full safety→corrections→downgrade→
// retry→governance chain composes).
func phase143Stack(t *testing.T, driver string, opts assemble.Options) *assemble.Stack {
	t.Helper()
	cfg, err := config.Load(context.Background(), writeDevConfig(t))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.LLM.Model = "test/model"
	opts.LLMSnapshot = &llm.ConfigSnapshot{
		Driver:               driver,
		Model:                "test/model",
		ContextWindowReserve: cfg.LLM.ContextWindowReserve,
		HeavyOutputThreshold: cfg.Artifacts.HeavyOutputThresholdBytes,
		ModelProfiles: map[string]llm.ModelProfile{
			// OutputMode Native → the terminal completion carries
			// FormatJSONSchema through to the driver (candidate A);
			// MaxRetries 1 bounds the corrective loop at initial + 1 retry.
			"test/model": {
				ContextWindowTokens: 100000,
				TokenEstimator:      "chars_div_4",
				OutputMode:          llm.OutputModeNative,
				MaxRetries:          1,
			},
		},
	}
	stack, err := assemble.Assemble(context.Background(), cfg, opts)
	if err != nil {
		if stack != nil {
			_ = stack.Close(context.Background())
		}
		t.Fatalf("assemble.Assemble: %v", err)
	}
	return stack
}

func phase143Identity() identity.Identity {
	return identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-1"}
}

// TestE2E_Phase143_HappyPath_ValidatedPayload — a schema-constrained
// RunOnce through the real RunLoop returns the validated payload.
func TestE2E_Phase143_HappyPath_ValidatedPayload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	driver, _ := registerScriptedDriver(`{"sentiment":"positive","confidence":0.9}`)
	stack := phase143Stack(t, driver, assemble.Options{})
	defer func() { _ = stack.Close(ctx) }()

	env, err := stack.RunOnce(ctx, "classify the sentiment", phase143Identity(),
		assemble.WithOutputSchema(json.RawMessage(phase143Schema)))
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(env.AnswerPayload) == 0 {
		t.Fatal("expected a validated AnswerPayload")
	}
	// Byte-pin: AnswerPayload carries the scripted content's EXACT bytes
	// — never a lossy string round-trip through a decode/re-encode (map
	// key-order nondeterminism is exactly what capturePayloadJSON's
	// direct-bytes capture is engineered to avoid).
	const wantBytes = `{"sentiment":"positive","confidence":0.9}`
	if string(env.AnswerPayload) != wantBytes {
		t.Errorf("AnswerPayload = %s, want the exact scripted bytes %s", env.AnswerPayload, wantBytes)
	}
	var got struct {
		Sentiment  string  `json:"sentiment"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal(env.AnswerPayload, &got); err != nil {
		t.Fatalf("AnswerPayload not valid JSON: %v", err)
	}
	if got.Sentiment != "positive" || got.Confidence != 0.9 {
		t.Errorf("payload = %+v, want {positive 0.9}", got)
	}
}

// TestE2E_Phase143_CorrectiveRetry_ObservesRetryEvent — the first
// response is schema-invalid; an llm.retry_with_feedback event fires and
// the second response validates. The envelope carries the valid payload.
func TestE2E_Phase143_CorrectiveRetry_ObservesRetryEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// First response: valid JSON but wrong enum (fails schema). Second:
	// conforming. The retry wrapper re-asks within one terminal turn.
	driver, drv := registerScriptedDriver(
		`{"sentiment":"elated"}`,
		`{"sentiment":"positive"}`,
	)
	stack := phase143Stack(t, driver, assemble.Options{})
	defer func() { _ = stack.Close(ctx) }()

	id := phase143Identity()
	subCtx, subCancel := context.WithCancel(ctx)
	defer subCancel()
	sub, err := stack.Bus.Subscribe(subCtx, events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: []events.EventType{llm.EventTypeRetryWithFeedback},
	})
	if err != nil {
		t.Fatalf("Bus.Subscribe: %v", err)
	}
	var retrySeen atomic.Bool
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range sub.Events() {
			retrySeen.Store(true)
		}
	}()

	env, err := stack.RunOnce(ctx, "classify the sentiment", id,
		assemble.WithOutputSchema(json.RawMessage(phase143Schema)))
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	sub.Cancel()
	subCancel()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
	}

	if !retrySeen.Load() {
		t.Error("expected an llm.retry_with_feedback event on the corrective path")
	}
	if drv.callCount.Load() < 2 {
		t.Errorf("driver call count = %d, want >= 2 (initial + corrective re-ask)", drv.callCount.Load())
	}
	var got struct {
		Sentiment string `json:"sentiment"`
	}
	if err := json.Unmarshal(env.AnswerPayload, &got); err != nil {
		t.Fatalf("AnswerPayload not valid JSON: %v (%s)", err, env.AnswerPayload)
	}
	if got.Sentiment != "positive" {
		t.Errorf("payload sentiment = %q, want positive (the corrected answer)", got.Sentiment)
	}
}

// TestE2E_Phase143_Exhaustion_ErrOutputInvalid — every response is
// schema-invalid; RunOnce returns the typed planner.ErrOutputInvalid
// wrapping the llm.ErrRetryExhausted chain, never a silent text
// fallback.
func TestE2E_Phase143_Exhaustion_ErrOutputInvalid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	driver, _ := registerScriptedDriver(`{"sentiment":"elated"}`) // always wrong-enum
	stack := phase143Stack(t, driver, assemble.Options{})
	defer func() { _ = stack.Close(ctx) }()

	env, err := stack.RunOnce(ctx, "classify the sentiment", phase143Identity(),
		assemble.WithOutputSchema(json.RawMessage(phase143Schema)))
	if err == nil {
		t.Fatalf("RunOnce = nil error, want ErrOutputInvalid; env=%+v", env)
	}
	if !errors.Is(err, planner.ErrOutputInvalid) {
		t.Errorf("error = %v, want errors.Is planner.ErrOutputInvalid", err)
	}
	if !errors.Is(err, llm.ErrRetryExhausted) {
		t.Errorf("error = %v, want the llm.ErrRetryExhausted chain", err)
	}
	if env.Answer != "" || env.AnswerPayload != nil {
		t.Errorf("expected zero envelope on failure (no silent text fallback), got %+v", env)
	}
}

// TestE2E_Phase143_PlannerAgnostic_DeterministicLeg — a deterministic
// planner's terminal Finish payload is validated at the runtime edge
// with NO generation steering: a conforming payload passes; a
// non-conforming one fails loud with ErrOutputInvalid.
func TestE2E_Phase143_PlannerAgnostic_DeterministicLeg(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Conforming leg — the planner emits a map payload; the runtime edge
	// validates it and captures the exact bytes onto AnswerPayload.
	t.Run("conforming", func(t *testing.T) {
		driver, _ := registerScriptedDriver(`unused`)
		stack := phase143Stack(t, driver, assemble.Options{
			PlannerOverride: phase143FixedFinishPlanner{
				payload: map[string]any{"sentiment": "neutral", "confidence": 0.5},
			},
		})
		defer func() { _ = stack.Close(ctx) }()

		env, err := stack.RunOnce(ctx, "classify", phase143Identity(),
			assemble.WithOutputSchema(json.RawMessage(phase143Schema)))
		if err != nil {
			t.Fatalf("RunOnce(deterministic conforming): %v", err)
		}
		if !strings.Contains(string(env.AnswerPayload), `"sentiment":"neutral"`) {
			t.Errorf("AnswerPayload = %s, want the conforming flow payload", env.AnswerPayload)
		}
	})

	// Non-conforming leg — a payload violating the schema fails loud at
	// the runtime edge, proving validation is planner-agnostic.
	t.Run("non-conforming", func(t *testing.T) {
		driver, _ := registerScriptedDriver(`unused`)
		stack := phase143Stack(t, driver, assemble.Options{
			PlannerOverride: phase143FixedFinishPlanner{
				payload: map[string]any{"mood": "great"}, // missing required "sentiment"
			},
		})
		defer func() { _ = stack.Close(ctx) }()

		_, err := stack.RunOnce(ctx, "classify", phase143Identity(),
			assemble.WithOutputSchema(json.RawMessage(phase143Schema)))
		if !errors.Is(err, planner.ErrOutputInvalid) {
			t.Errorf("RunOnce(deterministic non-conforming) = %v, want ErrOutputInvalid", err)
		}
	})
}
