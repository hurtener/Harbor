// phase144_runtyped_test.go — the §13 primitive-with-consumer E2E for
// the typed embed binding (D-273): assemble.RunTyped driven through
// the REAL RunLoop (real inmem drivers + real bus, the full
// safety->corrections->downgrade->retry->governance LLM chain composed
// by assemble.Assemble — the SAME harness phase143_output_schema_test.go
// builds, reused here) across three legs:
//
//   - happy path — the validated payload unmarshals into the target
//     Go type;
//   - corrective-retry + identity propagation — the first response is
//     schema-invalid (missing a required field the type derives), an
//     llm.retry_with_feedback event fires SCOPED TO THIS CALL'S
//     (tenant, user, session) triple, and the second response
//     validates + unmarshals;
//   - exhaustion — every response is schema-invalid; RunTyped surfaces
//     the typed planner.ErrOutputInvalid (never a zero value with a
//     nil error).
//
// Run under -race. No mocks at the seam except the scripted LLM DRIVER
// (registered through the SAME registry path bifrost uses, exactly as
// phase143_output_schema_test.go does), so the whole
// correction/retry chain is exercised for real.
package integration_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
)

// phase144Sentiment is RunTyped's target type for every leg below. Its
// derived schema requires "sentiment" (no omitempty) and leaves
// "confidence" optional — a missing "sentiment" field is what the
// corrective-retry and exhaustion legs exploit as their schema-invalid
// shape (RunTyped derives a schema from Go types alone, so it carries
// no enum constraint the way phase143Schema's hand-authored document
// does; a missing required field is the structural equivalent).
type phase144Sentiment struct {
	Sentiment  string  `json:"sentiment"`
	Confidence float64 `json:"confidence,omitempty"`
}

// TestE2E_Phase144_HappyPath_RunTyped — a conforming scripted response
// unmarshals cleanly into the target Go type through the real chain.
func TestE2E_Phase144_HappyPath_RunTyped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	driver, _ := registerScriptedDriver(`{"sentiment":"positive","confidence":0.9}`)
	stack := phase143Stack(t, driver, assemble.Options{})
	defer func() { _ = stack.Close(ctx) }()

	out, env, err := assemble.RunTyped[phase144Sentiment](ctx, stack, "classify the sentiment", phase143Identity())
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
		t.Error("expected a non-empty AnswerPayload on the envelope RunTyped returns alongside T")
	}
}

// TestE2E_Phase144_CorrectiveRetry_IdentityPropagated — the first
// response is schema-invalid (missing "sentiment"); the retry wrapper
// re-asks within one terminal turn and the second response validates.
// The llm.retry_with_feedback event is observed on a bus subscription
// SCOPED to this call's exact (tenant, user, session) triple — proving
// identity propagates through RunTyped -> RunOnce -> the runctx ->
// react driver -> LLM-edge retry chain, not just through RunOnce
// directly (D-272's own test already covers that half).
func TestE2E_Phase144_CorrectiveRetry_IdentityPropagated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	driver, drv := registerScriptedDriver(
		`{"confidence":0.5}`, // missing required "sentiment" — schema-invalid
		`{"sentiment":"positive","confidence":0.9}`,
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

	out, _, err := assemble.RunTyped[phase144Sentiment](ctx, stack, "classify the sentiment", id)
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}

	sub.Cancel()
	subCancel()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
	}

	if !retrySeen.Load() {
		t.Error("expected an llm.retry_with_feedback event scoped to this call's identity triple — proves identity propagated through the corrective-retry chain")
	}
	if drv.callCount.Load() < 2 {
		t.Errorf("driver call count = %d, want >= 2 (initial + corrective re-ask)", drv.callCount.Load())
	}
	if out.Sentiment != "positive" {
		t.Errorf("out.Sentiment = %q, want positive (the corrected answer)", out.Sentiment)
	}
}

// TestE2E_Phase144_Exhaustion_ErrOutputInvalid — every scripted
// response is schema-invalid (missing "sentiment"); RunTyped surfaces
// the typed planner.ErrOutputInvalid — never a zero value with a nil
// error, and never a silent fallback to unvalidated text (CLAUDE.md
// §13).
func TestE2E_Phase144_Exhaustion_ErrOutputInvalid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	driver, _ := registerScriptedDriver(`{"confidence":0.5}`) // always missing "sentiment"
	stack := phase143Stack(t, driver, assemble.Options{})
	defer func() { _ = stack.Close(ctx) }()

	out, env, err := assemble.RunTyped[phase144Sentiment](ctx, stack, "classify the sentiment", phase143Identity())
	if err == nil {
		t.Fatalf("RunTyped = nil error, want ErrOutputInvalid; out=%+v env=%+v", out, env)
	}
	if !errors.Is(err, planner.ErrOutputInvalid) {
		t.Errorf("error = %v, want errors.Is planner.ErrOutputInvalid", err)
	}
	if out != (phase144Sentiment{}) {
		t.Errorf("out = %+v on error, want the zero value (never a partial/zero-with-nil-error)", out)
	}
	if env.Answer != "" || env.AnswerPayload != nil {
		t.Errorf("expected a zero envelope on failure, got %+v", env)
	}
}
