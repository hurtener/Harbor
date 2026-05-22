package repair_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/repair"
)

// stubClient is a programmable llm.LLMClient: each Complete call
// returns the next scripted response in `responses` (with errors).
// Concurrent-safe: a mutex serialises the cursor.
type stubClient struct {
	responses []llm.CompleteResponse
	errs      []error
	seen      []seenCall
	cursor    int
	calls     atomic.Int64
	mu        sync.Mutex
}

type seenCall struct {
	id  identity.Quadruple
	req llm.CompleteRequest
}

func (s *stubClient) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := identity.QuadrupleFrom(ctx)
	s.seen = append(s.seen, seenCall{id: id, req: req})
	if s.cursor >= len(s.responses) {
		// Repeat last response forever.
		idx := len(s.responses) - 1
		var lastResp llm.CompleteResponse
		var lastErr error
		if idx >= 0 {
			lastResp = s.responses[idx]
		}
		if idx >= 0 && idx < len(s.errs) {
			lastErr = s.errs[idx]
		}
		return lastResp, lastErr
	}
	resp := s.responses[s.cursor]
	var err error
	if s.cursor < len(s.errs) {
		err = s.errs[s.cursor]
	}
	s.cursor++
	return resp, err
}

func (s *stubClient) Close(_ context.Context) error { return nil }

func (s *stubClient) callCount() int64 { return s.calls.Load() }

func (s *stubClient) snapshot() []seenCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]seenCall, len(s.seen))
	copy(out, s.seen)
	return out
}

// rcWithIdentity builds a planner.RunContext with a valid quadruple
// and an Emit closure that records events.
func rcWithIdentity(emit func(events.Event)) planner.RunContext {
	return planner.RunContext{
		Quadruple: identity.Quadruple{
			Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
			RunID:    "r-1",
		},
		Emit: emit,
	}
}

// recordingEmit collects events into a slice (mutex-guarded).
type recordingEmit struct {
	events []events.Event
	mu     sync.Mutex
}

func (r *recordingEmit) emit(ev events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recordingEmit) snapshot() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]events.Event, len(r.events))
	copy(out, r.events)
	return out
}

func sampleRequest() llm.CompleteRequest {
	t := "do something"
	return llm.CompleteRequest{
		Model: "test-model",
		Messages: []llm.ChatMessage{
			{Role: llm.RoleUser, Content: llm.Content{Text: &t}},
		},
	}
}

func ctxWithIdentity(t *testing.T) context.Context {
	t.Helper()
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, err := identity.WithRun(t.Context(), id, "r-1")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

// passValidator returns nil for every tool/args pair. Used to test
// pure-salvage paths.
func passValidator(_ string, _ json.RawMessage) error { return nil }

// rejectingValidator returns a synthetic validation error for the
// first `failN` calls, then nil. Goroutine-safe.
type rejectingValidator struct {
	count atomic.Int64
	failN int64
}

func (r *rejectingValidator) Validate(toolName string, _ json.RawMessage) error {
	c := r.count.Add(1)
	if c <= r.failN {
		return fmt.Errorf("missing required field `%s_field`", toolName)
	}
	return nil
}

// TestRun_Salvage_SingleAction is the Step 1 ladder gate: a clean
// LLM response with one valid CallTool is returned verbatim.
func TestRun_Salvage_SingleAction(t *testing.T) {
	t.Parallel()
	client := &stubClient{
		responses: []llm.CompleteResponse{
			{Content: `{"tool":"search","args":{"q":"hi"},"reasoning":"r"}`},
		},
	}
	rec := &recordingEmit{}
	loop := repair.New(repair.Config{ArgFillEnabled: true})

	dec, err := loop.Run(ctxWithIdentity(t), rcWithIdentity(rec.emit),
		client, sampleRequest(), passValidator)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	call, ok := dec.(planner.CallTool)
	if !ok {
		t.Fatalf("decision = %T, want planner.CallTool", dec)
	}
	if call.Tool != "search" {
		t.Errorf("Tool = %q, want %q", call.Tool, "search")
	}
	if client.callCount() != 1 {
		t.Errorf("client.calls = %d, want 1", client.callCount())
	}
	if len(rec.snapshot()) != 0 {
		t.Errorf("emitted events on success: %d", len(rec.snapshot()))
	}
}

// TestRun_SchemaRepair_SucceedsAfterOneRetry is the Step 2 ladder
// gate: first response has invalid args; the loop builds a corrective
// sub-prompt and re-asks; the second response validates.
func TestRun_SchemaRepair_SucceedsAfterOneRetry(t *testing.T) {
	t.Parallel()
	client := &stubClient{
		responses: []llm.CompleteResponse{
			{Content: `{"tool":"search","args":{"q":"hi"}}`}, // attempt 1: rejected by validator
			{Content: `{"tool":"search","args":{"q":"hi"}}`}, // attempt 2: accepted by validator (count moves past failN)
		},
	}
	v := &rejectingValidator{failN: 1}
	rec := &recordingEmit{}
	loop := repair.New(repair.Config{
		ArgFillEnabled:            true,
		RepairAttempts:            3,
		MaxConsecutiveArgFailures: 5, // high cap; storm-guard doesn't trip
	})

	dec, err := loop.Run(ctxWithIdentity(t), rcWithIdentity(rec.emit),
		client, sampleRequest(), v.Validate)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := dec.(planner.CallTool); !ok {
		t.Fatalf("decision = %T, want planner.CallTool after repair", dec)
	}
	if got := client.callCount(); got != 2 {
		t.Errorf("client.calls = %d, want 2 (initial + 1 repair)", got)
	}
	// Inspect the second LLM call's request — it MUST include the
	// corrective turn.
	seen := client.snapshot()
	if len(seen) != 2 {
		t.Fatalf("seen %d calls, want 2", len(seen))
	}
	second := seen[1].req
	if len(second.Messages) <= len(seen[0].req.Messages) {
		t.Errorf("second request's Messages should be longer; got %d <= %d",
			len(second.Messages), len(seen[0].req.Messages))
	}
	last := second.Messages[len(second.Messages)-1]
	if last.Role != llm.RoleUser || last.Content.Text == nil ||
		!strings.Contains(*last.Content.Text, "failed validation") {
		t.Errorf("corrective turn missing/malformed: %+v", last)
	}
	if len(rec.snapshot()) != 0 {
		t.Errorf("emitted events on success-after-retry: %d", len(rec.snapshot()))
	}
}

// TestRun_GracefulFailure_StormGuard is the Step 3 ladder gate: the
// validator rejects every attempt; after MaxConsecutiveArgFailures
// the loop returns Finish{NoPath, Followup=true} AND emits
// planner.repair_exhausted.
func TestRun_GracefulFailure_StormGuard(t *testing.T) {
	t.Parallel()
	client := &stubClient{
		responses: []llm.CompleteResponse{
			{Content: `{"tool":"search","args":{"q":"hi"}}`},
		},
	}
	v := &rejectingValidator{failN: 100} // reject everything
	rec := &recordingEmit{}
	loop := repair.New(repair.Config{
		ArgFillEnabled:            true,
		RepairAttempts:            5, // higher than storm guard so guard fires first
		MaxConsecutiveArgFailures: 2,
	})

	dec, err := loop.Run(ctxWithIdentity(t), rcWithIdentity(rec.emit),
		client, sampleRequest(), v.Validate)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	fin, ok := dec.(planner.Finish)
	if !ok {
		t.Fatalf("decision = %T, want planner.Finish", dec)
	}
	if fin.Reason != planner.FinishNoPath {
		t.Errorf("Reason = %q, want %q", fin.Reason, planner.FinishNoPath)
	}
	if fin.Metadata == nil {
		t.Fatalf("Metadata is nil")
	}
	if got, _ := fin.Metadata["followup"].(bool); !got {
		t.Errorf("Metadata[followup] = %v, want true", fin.Metadata["followup"])
	}
	if got, _ := fin.Metadata["repair_consecutive_arg_failures"].(int); got != 2 {
		t.Errorf("Metadata[repair_consecutive_arg_failures] = %v, want 2", fin.Metadata["repair_consecutive_arg_failures"])
	}
	// Storm guard fires when consecutiveArgFails == MaxConsecutiveArgFailures (2),
	// which means attempts == 2 also. Verify.
	if got, _ := fin.Metadata["repair_attempts"].(int); got != 2 {
		t.Errorf("Metadata[repair_attempts] = %v, want 2 (storm guard fires at consecutive=2)", got)
	}
	chain, _ := fin.Metadata["repair_chain"].(string)
	if !strings.Contains(chain, "arg-validation") {
		t.Errorf("repair_chain should contain `arg-validation`: %q", chain)
	}

	// Event emit.
	evs := rec.snapshot()
	if len(evs) != 1 {
		t.Fatalf("emitted %d events, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Type != planner.EventTypePlannerRepairExhausted {
		t.Errorf("event.Type = %q, want %q", ev.Type, planner.EventTypePlannerRepairExhausted)
	}
	if ev.Identity.TenantID != "t" || ev.Identity.RunID != "r-1" {
		t.Errorf("event.Identity not preserved: %+v", ev.Identity)
	}
	payload, ok := ev.Payload.(planner.RepairExhaustedPayload)
	if !ok {
		t.Fatalf("event.Payload = %T, want RepairExhaustedPayload", ev.Payload)
	}
	if payload.ConsecutiveArgFailures != 2 {
		t.Errorf("payload.ConsecutiveArgFailures = %d, want 2", payload.ConsecutiveArgFailures)
	}
	if len(payload.Reasons) != 2 {
		t.Errorf("payload.Reasons = %d, want 2", len(payload.Reasons))
	}
}

// TestRun_GracefulFailure_AttemptsBudget is the Step 3 ladder gate
// (companion): when the storm guard is set high but
// RepairAttempts is the binding bound, the loop also returns
// Finish{NoPath} cleanly. Verifies the attempts-budget path fires
// the same emit.
func TestRun_GracefulFailure_AttemptsBudget(t *testing.T) {
	t.Parallel()
	client := &stubClient{
		responses: []llm.CompleteResponse{
			// 3 responses, each rejected. We use a non-zero validator
			// that ALSO rejects each turn so the storm guard would
			// fire normally — set MaxConsecutiveArgFailures very high
			// so the attempts budget exhausts first.
			{Content: `{"tool":"a","args":{}}`},
			{Content: `{"tool":"a","args":{}}`},
			{Content: `{"tool":"a","args":{}}`},
		},
	}
	v := &rejectingValidator{failN: 100}
	rec := &recordingEmit{}
	loop := repair.New(repair.Config{
		ArgFillEnabled:            true,
		RepairAttempts:            3,
		MaxConsecutiveArgFailures: 1000, // very high
	})

	dec, err := loop.Run(ctxWithIdentity(t), rcWithIdentity(rec.emit),
		client, sampleRequest(), v.Validate)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	fin, ok := dec.(planner.Finish)
	if !ok {
		t.Fatalf("decision = %T, want planner.Finish", dec)
	}
	if fin.Reason != planner.FinishNoPath {
		t.Errorf("Reason = %q, want %q", fin.Reason, planner.FinishNoPath)
	}
	if got, _ := fin.Metadata["repair_attempts"].(int); got != 3 {
		t.Errorf("repair_attempts = %v, want 3", got)
	}
	if len(rec.snapshot()) != 1 {
		t.Errorf("emitted %d events, want 1", len(rec.snapshot()))
	}
}

// TestRun_MultiActionSalvage is the Step 4 ladder gate: a JSON array
// of well-shaped envelopes promotes to CallParallel.
func TestRun_MultiActionSalvage(t *testing.T) {
	t.Parallel()
	client := &stubClient{
		responses: []llm.CompleteResponse{
			{Content: `[{"tool":"a","args":{}}, {"tool":"b","args":{}}]`},
		},
	}
	rec := &recordingEmit{}
	loop := repair.New(repair.Config{ArgFillEnabled: true})

	dec, err := loop.Run(ctxWithIdentity(t), rcWithIdentity(rec.emit),
		client, sampleRequest(), passValidator)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	par, ok := dec.(planner.CallParallel)
	if !ok {
		t.Fatalf("decision = %T, want planner.CallParallel", dec)
	}
	if len(par.Branches) != 2 {
		t.Fatalf("Branches = %d, want 2", len(par.Branches))
	}
	if par.Branches[0].Tool != "a" || par.Branches[1].Tool != "b" {
		t.Errorf("order not preserved: %q %q", par.Branches[0].Tool, par.Branches[1].Tool)
	}
	if par.Join == nil || par.Join.Kind != planner.JoinAll {
		t.Errorf("Join = %+v, want JoinAll", par.Join)
	}
}

// TestRun_NilValidator_ShortCircuits verifies that a nil validator
// bypasses the schema-repair path — the parser's first valid action(s)
// surface verbatim. This is the dispatcher-validates fallback that
// concretes can opt into.
func TestRun_NilValidator_ShortCircuits(t *testing.T) {
	t.Parallel()
	client := &stubClient{
		responses: []llm.CompleteResponse{
			{Content: `{"tool":"x","args":{"k":"v"}}`},
		},
	}
	loop := repair.New(repair.Config{ArgFillEnabled: true})

	dec, err := loop.Run(ctxWithIdentity(t), rcWithIdentity(nil),
		client, sampleRequest(), nil) // nil validator
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := dec.(planner.CallTool); !ok {
		t.Fatalf("decision = %T, want planner.CallTool", dec)
	}
	if client.callCount() != 1 {
		t.Errorf("client.calls = %d, want 1", client.callCount())
	}
}

// TestRun_ArgFillDisabled_ShortCircuits verifies that
// ArgFillEnabled=false short-circuits the schema-repair path even
// when a validator is supplied. The parser's first valid action(s)
// surface verbatim.
func TestRun_ArgFillDisabled_ShortCircuits(t *testing.T) {
	t.Parallel()
	client := &stubClient{
		responses: []llm.CompleteResponse{
			{Content: `{"tool":"x","args":{"k":"v"}}`},
		},
	}
	v := &rejectingValidator{failN: 100}
	loop := repair.New(repair.Config{
		ArgFillEnabled:            false,
		RepairAttempts:            3,
		MaxConsecutiveArgFailures: 2,
	})

	dec, err := loop.Run(ctxWithIdentity(t), rcWithIdentity(nil),
		client, sampleRequest(), v.Validate)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := dec.(planner.CallTool); !ok {
		t.Fatalf("decision = %T, want planner.CallTool (ArgFillEnabled=false short-circuits)", dec)
	}
	if client.callCount() != 1 {
		t.Errorf("client.calls = %d, want 1 (no repair attempted)", client.callCount())
	}
	if v.count.Load() != 0 {
		t.Errorf("validator called %d times, want 0 (ArgFillEnabled=false)", v.count.Load())
	}
}

// TestRun_ParserFails_BuildsParserCorrection verifies that a
// parser-level failure (no actions found) builds the parser-shape
// corrective sub-prompt for the next attempt — distinct from the
// arg-validation correction.
func TestRun_ParserFails_BuildsParserCorrection(t *testing.T) {
	t.Parallel()
	client := &stubClient{
		responses: []llm.CompleteResponse{
			{Content: `I dunno, can't help with that.`},                  // parser fails
			{Content: `{"tool":"search","args":{"q":"now structured"}}`}, // parser succeeds
		},
	}
	rec := &recordingEmit{}
	loop := repair.New(repair.Config{
		ArgFillEnabled:            true,
		RepairAttempts:            3,
		MaxConsecutiveArgFailures: 5,
	})

	dec, err := loop.Run(ctxWithIdentity(t), rcWithIdentity(rec.emit),
		client, sampleRequest(), passValidator)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := dec.(planner.CallTool); !ok {
		t.Fatalf("decision = %T, want planner.CallTool after parser-correction", dec)
	}
	if client.callCount() != 2 {
		t.Errorf("client.calls = %d, want 2 (initial + 1 parser-correction)", client.callCount())
	}
	// The corrective turn for parser failure mentions "parse".
	seen := client.snapshot()
	last := seen[1].req.Messages[len(seen[1].req.Messages)-1]
	if last.Content.Text == nil || !strings.Contains(*last.Content.Text, "parse") {
		t.Errorf("parser-correction missing/malformed: %+v", last)
	}
}

// TestRun_MissingIdentity_FailsLoudly verifies the identity-mandatory
// pre-check rejects calls without a full quadruple.
func TestRun_MissingIdentity_FailsLoudly(t *testing.T) {
	t.Parallel()
	client := &stubClient{}
	loop := repair.New(repair.Config{})

	rc := planner.RunContext{
		Quadruple: identity.Quadruple{
			Identity: identity.Identity{TenantID: "t"}, // user + session missing
			RunID:    "r",
		},
	}
	_, err := loop.Run(context.Background(), rc, client, sampleRequest(), nil)
	if !errors.Is(err, llm.ErrIdentityMissing) {
		t.Fatalf("want llm.ErrIdentityMissing, got %v", err)
	}
	if client.callCount() != 0 {
		t.Errorf("client called %d times despite missing identity", client.callCount())
	}
}

// TestRun_NilClient_FailsLoudly verifies the nil-client guard.
func TestRun_NilClient_FailsLoudly(t *testing.T) {
	t.Parallel()
	loop := repair.New(repair.Config{})
	_, err := loop.Run(ctxWithIdentity(t), rcWithIdentity(nil), nil, sampleRequest(), nil)
	if err == nil {
		t.Fatalf("expected error on nil client")
	}
	if !strings.Contains(err.Error(), "nil") {
		t.Errorf("error should mention nil: %v", err)
	}
}

// TestRun_CtxCancel_AbortsLoop verifies that a cancelled ctx stops
// the loop without burning a Complete call.
func TestRun_CtxCancel_AbortsLoop(t *testing.T) {
	t.Parallel()
	client := &stubClient{
		responses: []llm.CompleteResponse{
			{Content: `{"tool":"x","args":{}}`},
		},
	}
	v := &rejectingValidator{failN: 100}
	loop := repair.New(repair.Config{
		ArgFillEnabled:            true,
		RepairAttempts:            5,
		MaxConsecutiveArgFailures: 5,
	})

	ctx, cancel := context.WithCancel(ctxWithIdentity(t))
	cancel()
	_, err := loop.Run(ctx, rcWithIdentity(nil), client, sampleRequest(), v.Validate)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// TestRun_LLMError_BubbleVerbatim verifies the loop does NOT
// swallow LLM-call errors (Phase 36 retry composes inside the
// client; repair is OUTSIDE the LLM call — silent retry here would
// be a §13 two-parallel-implementations violation).
func TestRun_LLMError_BubbleVerbatim(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("transient: connection reset")
	client := &stubClient{
		responses: []llm.CompleteResponse{{}},
		errs:      []error{sentinel},
	}
	loop := repair.New(repair.Config{ArgFillEnabled: true})

	_, err := loop.Run(ctxWithIdentity(t), rcWithIdentity(nil),
		client, sampleRequest(), passValidator)
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("want LLM error to bubble, got %v", err)
	}
	if client.callCount() != 1 {
		t.Errorf("client.calls = %d, want 1 (no repair on LLM-call error)", client.callCount())
	}
}

// TestNew_AppliesDefaults verifies a zero-value Config picks up the
// documented defaults.
func TestNew_AppliesDefaults(t *testing.T) {
	t.Parallel()
	loop := repair.New(repair.Config{})
	cfg := loop.Config()
	if cfg.RepairAttempts != repair.DefaultRepairAttempts {
		t.Errorf("RepairAttempts = %d, want %d", cfg.RepairAttempts, repair.DefaultRepairAttempts)
	}
	if cfg.MaxConsecutiveArgFailures != repair.DefaultMaxConsecutiveArgFailures {
		t.Errorf("MaxConsecutiveArgFailures = %d, want %d",
			cfg.MaxConsecutiveArgFailures, repair.DefaultMaxConsecutiveArgFailures)
	}
}

// TestNew_PreservesExplicitConfig verifies an explicit (non-zero)
// Config is used as-is.
func TestNew_PreservesExplicitConfig(t *testing.T) {
	t.Parallel()
	loop := repair.New(repair.Config{
		ArgFillEnabled:            true,
		RepairAttempts:            7,
		MaxConsecutiveArgFailures: 5,
	})
	cfg := loop.Config()
	if cfg.RepairAttempts != 7 || cfg.MaxConsecutiveArgFailures != 5 || !cfg.ArgFillEnabled {
		t.Errorf("Config not preserved: %+v", cfg)
	}
}
