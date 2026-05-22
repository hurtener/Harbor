package corrections_test

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/corrections"
)

// recordingDriver is a minimal `llm.LLMClient` test stub that records
// the last request it received. Used to verify the corrections layer
// rewrote the request before delegating.
//
// Concurrent-reuse safe (D-025): the seen-request slice is guarded by
// a mutex and the SeenIdentity channel is buffered.
type recordingDriver struct {
	mu      sync.Mutex
	seen    []llm.CompleteRequest
	respCB  func(req llm.CompleteRequest) llm.CompleteResponse
	closed  atomic.Bool
	identCh chan identity.Quadruple
}

func newRecorder() *recordingDriver {
	return &recordingDriver{
		respCB: func(req llm.CompleteRequest) llm.CompleteResponse {
			// Default response: echo the model name + a token count.
			return llm.CompleteResponse{
				Content: "mock:" + req.Model,
				Usage: llm.Usage{
					PromptTokens:     8,
					CompletionTokens: 4,
					TotalTokens:      12,
				},
			}
		},
	}
}

func (r *recordingDriver) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	if r.closed.Load() {
		return llm.CompleteResponse{}, llm.ErrClientClosed
	}
	r.mu.Lock()
	r.seen = append(r.seen, req)
	r.mu.Unlock()
	if r.identCh != nil {
		if id, ok := identity.QuadrupleFrom(ctx); ok {
			select {
			case r.identCh <- id:
			default:
			}
		} else if id, ok := identity.From(ctx); ok {
			select {
			case r.identCh <- identity.Quadruple{Identity: id}:
			default:
			}
		}
	}
	return r.respCB(req), nil
}

func (r *recordingDriver) Close(_ context.Context) error {
	r.closed.CompareAndSwap(false, true)
	return nil
}

func (r *recordingDriver) lastRequest() (llm.CompleteRequest, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seen) == 0 {
		return llm.CompleteRequest{}, false
	}
	return r.seen[len(r.seen)-1], true
}

// helper — wrap a recorder with the corrections layer using the
// supplied snapshot. The wrapper bypasses the safety pass (this test
// exercises the corrections layer in isolation; the safety pass is
// Phase 32's responsibility).
func wrapRecorder(t *testing.T, rec *recordingDriver, cfg llm.ConfigSnapshot) llm.LLMClient {
	t.Helper()
	return corrections.Wrap(rec, cfg)
}

// snapshotWithProfile builds a `ConfigSnapshot` containing one model
// profile keyed by `model`. The defaults are sane for the safety
// pass shape (which isn't exercised here) — the corrections layer
// only reads `cfg.ModelProfiles`.
func snapshotWithProfile(model string, profile llm.ModelProfile) llm.ConfigSnapshot {
	return llm.ConfigSnapshot{
		Driver:               "mock",
		ContextWindowReserve: 0.05,
		HeavyOutputThreshold: 32_768,
		ModelProfiles: map[string]llm.ModelProfile{
			model: profile,
		},
	}
}

// strPtr returns &s. Used to construct `Content.Text` test data.
func strPtr(s string) *string { return &s }

// makeRequest builds a CompleteRequest with text-only content. The
// supplied messages slice is copied so tests can verify the
// corrections layer does not mutate input.
func makeRequest(model string, messages []llm.ChatMessage) llm.CompleteRequest {
	return llm.CompleteRequest{
		Model:    model,
		Messages: messages,
	}
}

// testCtx attaches a synthetic identity to context.Background. The
// corrections layer does not enforce identity (the safety pass does);
// we still attach one so the recorder's identity channel populates.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// ---------------------------------------------------------------------------
// Quirk 1: Message reordering (NIM)
// ---------------------------------------------------------------------------

func TestCorrections_MessageReordering_NIM(t *testing.T) {
	const model = "nim/llama-3.1-70b"
	profile := llm.ModelProfile{
		ContextWindowTokens: 128_000,
		Corrections: llm.CorrectionsProfile{
			MessageOrdering: llm.OrderingSystemFirstStrict,
		},
	}
	cfg := snapshotWithProfile(model, profile)
	rec := newRecorder()
	c := wrapRecorder(t, rec, cfg)

	// Build a request where `system` is INTERLEAVED with user/assistant.
	// NIM rejects this; the corrections layer must collapse all
	// system messages to the front.
	in := []llm.ChatMessage{
		{Role: llm.RoleUser, Content: llm.Content{Text: strPtr("hi")}},
		{Role: llm.RoleSystem, Content: llm.Content{Text: strPtr("you are helpful")}},
		{Role: llm.RoleAssistant, Content: llm.Content{Text: strPtr("hello")}},
		{Role: llm.RoleSystem, Content: llm.Content{Text: strPtr("respond briefly")}},
		{Role: llm.RoleUser, Content: llm.Content{Text: strPtr("how are you")}},
	}
	_, err := c.Complete(testCtx(t), makeRequest(model, in))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	got, ok := rec.lastRequest()
	if !ok {
		t.Fatalf("recorder did not receive a request")
	}
	if len(got.Messages) != 5 {
		t.Fatalf("Messages: got %d entries, want 5", len(got.Messages))
	}
	// Expected order: system, system, user, assistant, user.
	wantRoles := []llm.Role{
		llm.RoleSystem, llm.RoleSystem,
		llm.RoleUser, llm.RoleAssistant, llm.RoleUser,
	}
	for i, w := range wantRoles {
		if got.Messages[i].Role != w {
			t.Errorf("Messages[%d].Role: got %q want %q",
				i, got.Messages[i].Role, w)
		}
	}
	// The corrections layer must NOT mutate the caller's input
	// slice; the original is still interleaved.
	if in[1].Role != llm.RoleSystem || in[3].Role != llm.RoleSystem {
		t.Fatalf("corrections layer mutated caller's input slice")
	}
}

// ---------------------------------------------------------------------------
// Quirk 3: Reasoning-effort routing (thinking-class models)
// ---------------------------------------------------------------------------

func TestCorrections_ReasoningEffort_ThinkingRouting(t *testing.T) {
	const model = "openai/o1-preview"
	profile := llm.ModelProfile{
		ContextWindowTokens: 128_000,
		Corrections: llm.CorrectionsProfile{
			ReasoningEffortRouting: llm.ReasoningRouteThinking,
		},
	}
	cfg := snapshotWithProfile(model, profile)
	rec := newRecorder()
	c := wrapRecorder(t, rec, cfg)

	req := makeRequest(model, []llm.ChatMessage{
		{Role: llm.RoleUser, Content: llm.Content{Text: strPtr("solve")}},
	})
	req.ReasoningEffort = llm.ReasoningHigh

	_, err := c.Complete(testCtx(t), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	got, _ := rec.lastRequest()
	if got.ReasoningEffort != "" {
		t.Errorf("top-level ReasoningEffort should be cleared, got %q", got.ReasoningEffort)
	}
	gotHint, ok := got.Extra["reasoning_effort"]
	if !ok {
		t.Fatalf("Extra[reasoning_effort] not set")
	}
	if gotHint != "high" {
		t.Errorf("Extra[reasoning_effort]: got %v want %q", gotHint, "high")
	}
}

// ---------------------------------------------------------------------------
// Quirk 4: Response-format envelope translation (Anthropic + JSONOnly)
// ---------------------------------------------------------------------------

func TestCorrections_ResponseFormat_AnthropicEnvelope(t *testing.T) {
	const model = "anthropic/claude-sonnet-4"
	profile := llm.ModelProfile{
		ContextWindowTokens: 200_000,
		Corrections: llm.CorrectionsProfile{
			ResponseFormatShape: llm.ResponseFormatAnthropic,
		},
	}
	cfg := snapshotWithProfile(model, profile)
	rec := newRecorder()
	c := wrapRecorder(t, rec, cfg)

	schema := json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)
	req := makeRequest(model, []llm.ChatMessage{
		{Role: llm.RoleUser, Content: llm.Content{Text: strPtr("weather?")}},
	})
	req.ResponseFormat = &llm.ResponseFormat{
		Kind:       llm.FormatJSONSchema,
		JSONSchema: schema,
	}

	_, err := c.Complete(testCtx(t), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	got, _ := rec.lastRequest()
	if got.ResponseFormat != nil {
		t.Errorf("top-level ResponseFormat should be cleared for Anthropic envelope, got %+v", got.ResponseFormat)
	}
	stash, ok := got.Extra["anthropic_tool_schema"]
	if !ok {
		t.Fatalf("Extra[anthropic_tool_schema] not set")
	}
	// Stash must be the decoded schema (map), not raw bytes — the
	// downstream consumer reads structurally.
	stashMap, ok := stash.(map[string]any)
	if !ok {
		t.Fatalf("Extra[anthropic_tool_schema]: got %T, want map[string]any", stash)
	}
	if stashMap["type"] != "object" {
		t.Errorf("stashed schema type: got %v want %q", stashMap["type"], "object")
	}
}

func TestCorrections_ResponseFormat_JSONOnly_DowngradesSchemaToObject(t *testing.T) {
	const model = "openrouter/some/no-schema-route"
	profile := llm.ModelProfile{
		ContextWindowTokens: 64_000,
		Corrections: llm.CorrectionsProfile{
			ResponseFormatShape: llm.ResponseFormatJSONOnly,
		},
	}
	cfg := snapshotWithProfile(model, profile)
	rec := newRecorder()
	c := wrapRecorder(t, rec, cfg)

	schema := json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`)
	req := makeRequest(model, []llm.ChatMessage{
		{Role: llm.RoleUser, Content: llm.Content{Text: strPtr("hi")}},
	})
	req.ResponseFormat = &llm.ResponseFormat{
		Kind:       llm.FormatJSONSchema,
		JSONSchema: schema,
	}

	_, err := c.Complete(testCtx(t), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	got, _ := rec.lastRequest()
	if got.ResponseFormat == nil {
		t.Fatalf("ResponseFormat should still be set (downgraded), got nil")
	}
	if got.ResponseFormat.Kind != llm.FormatJSONObject {
		t.Errorf("ResponseFormat.Kind: got %q want %q",
			got.ResponseFormat.Kind, llm.FormatJSONObject)
	}
	if _, ok := got.Extra["schema_hint"]; !ok {
		t.Errorf("Extra[schema_hint] not set — operator-supplied schema lost")
	}
}

// ---------------------------------------------------------------------------
// Quirk 5: Usage backfill
// ---------------------------------------------------------------------------

func TestCorrections_UsageBackfill_ZeroUsage(t *testing.T) {
	const model = "openrouter/proxy/silent"
	profile := llm.ModelProfile{
		ContextWindowTokens: 32_000,
		Corrections: llm.CorrectionsProfile{
			UsageBackfillEnabled: true,
		},
		CostOverrides: &llm.CostTable{
			InputPer1M:  3.0,
			OutputPer1M: 15.0,
			Currency:    "USD",
		},
	}
	cfg := snapshotWithProfile(model, profile)
	rec := newRecorder()
	rec.respCB = func(req llm.CompleteRequest) llm.CompleteResponse {
		return llm.CompleteResponse{
			Content: "a non-empty response that has some bytes",
			Usage:   llm.Usage{}, // all zeros — backfill kicks in
		}
	}
	c := wrapRecorder(t, rec, cfg)

	req := makeRequest(model, []llm.ChatMessage{
		{Role: llm.RoleUser, Content: llm.Content{Text: strPtr(strings.Repeat("x", 800))}},
	})

	resp, err := c.Complete(testCtx(t), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Usage.PromptTokens == 0 {
		t.Errorf("PromptTokens not backfilled, got 0")
	}
	if resp.Usage.CompletionTokens == 0 {
		t.Errorf("CompletionTokens not backfilled, got 0")
	}
	if resp.Usage.TotalTokens == 0 {
		t.Errorf("TotalTokens not backfilled, got 0")
	}
	if resp.Cost.TotalCost == 0 {
		t.Errorf("TotalCost not backfilled (CostOverrides set), got 0")
	}
	if resp.Cost.Currency != "USD" {
		t.Errorf("Cost.Currency: got %q want USD", resp.Cost.Currency)
	}
}

func TestCorrections_UsageBackfill_NoOpWhenUsagePresent(t *testing.T) {
	const model = "openrouter/proxy/reports-correctly"
	profile := llm.ModelProfile{
		ContextWindowTokens: 32_000,
		Corrections: llm.CorrectionsProfile{
			UsageBackfillEnabled: true,
		},
	}
	cfg := snapshotWithProfile(model, profile)
	rec := newRecorder()
	rec.respCB = func(req llm.CompleteRequest) llm.CompleteResponse {
		return llm.CompleteResponse{
			Content: "ok",
			Usage:   llm.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
		}
	}
	c := wrapRecorder(t, rec, cfg)

	req := makeRequest(model, []llm.ChatMessage{
		{Role: llm.RoleUser, Content: llm.Content{Text: strPtr("hi")}},
	})
	resp, err := c.Complete(testCtx(t), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// Existing usage must NOT be overwritten.
	if resp.Usage.PromptTokens != 100 {
		t.Errorf("PromptTokens overwritten: got %d want 100", resp.Usage.PromptTokens)
	}
}

// ---------------------------------------------------------------------------
// Composition: default profile (no quirks) is a no-op pass-through.
// ---------------------------------------------------------------------------

func TestCorrections_DefaultProfile_NoOp(t *testing.T) {
	const model = "anthropic/claude-sonnet-4"
	profile := llm.ModelProfile{
		ContextWindowTokens: 200_000,
		// Corrections is the zero value — no quirks declared.
	}
	cfg := snapshotWithProfile(model, profile)
	rec := newRecorder()
	c := wrapRecorder(t, rec, cfg)

	in := []llm.ChatMessage{
		{Role: llm.RoleSystem, Content: llm.Content{Text: strPtr("be brief")}},
		{Role: llm.RoleUser, Content: llm.Content{Text: strPtr("hi")}},
	}
	_, err := c.Complete(testCtx(t), makeRequest(model, in))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got, _ := rec.lastRequest()
	if len(got.Messages) != 2 ||
		got.Messages[0].Role != llm.RoleSystem ||
		got.Messages[1].Role != llm.RoleUser {
		t.Errorf("default profile mutated messages: got %+v", got.Messages)
	}
}

// ---------------------------------------------------------------------------
// Composition: unknown model bypasses corrections (the inner safety
// pass would fail with ErrUnsupportedModel; corrections does not pre-
// empt that error path).
// ---------------------------------------------------------------------------

func TestCorrections_UnknownModel_BypassesCorrections(t *testing.T) {
	const model = "unmapped/some/model"
	cfg := llm.ConfigSnapshot{
		Driver:        "mock",
		ModelProfiles: map[string]llm.ModelProfile{}, // empty — unknown model
	}
	rec := newRecorder()
	c := wrapRecorder(t, rec, cfg)

	_, err := c.Complete(testCtx(t), makeRequest(model, []llm.ChatMessage{
		{Role: llm.RoleUser, Content: llm.Content{Text: strPtr("hi")}},
	}))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// The recorder received the request verbatim. Production
	// composition through `llm.Open()` would have failed with
	// ErrUnsupportedModel at the safety pass; the corrections layer
	// does not enforce.
	if _, ok := rec.lastRequest(); !ok {
		t.Errorf("recorder did not see the request — corrections should pass through")
	}
}

// ---------------------------------------------------------------------------
// Identity propagation — the corrections layer does NOT consume identity
// but must not break it. The recorder's SeenIdentity channel verifies.
// ---------------------------------------------------------------------------

func TestCorrections_IdentityPropagation(t *testing.T) {
	const model = "anthropic/claude-sonnet-4"
	profile := llm.ModelProfile{
		ContextWindowTokens: 200_000,
		Corrections: llm.CorrectionsProfile{
			MessageOrdering: llm.OrderingSystemFirstStrict,
		},
	}
	cfg := snapshotWithProfile(model, profile)
	rec := newRecorder()
	rec.identCh = make(chan identity.Quadruple, 1)
	c := wrapRecorder(t, rec, cfg)

	id := identity.Identity{TenantID: "T1", UserID: "U1", SessionID: "S1"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	_, err = c.Complete(ctx, makeRequest(model, []llm.ChatMessage{
		{Role: llm.RoleUser, Content: llm.Content{Text: strPtr("hi")}},
	}))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	select {
	case seen := <-rec.identCh:
		if seen.TenantID != id.TenantID {
			t.Errorf("identity TenantID: got %q want %q", seen.TenantID, id.TenantID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recorder did not observe identity within 2s")
	}
}

// ---------------------------------------------------------------------------
// Concurrent-reuse contract (D-025).
// N≥100 concurrent goroutines invoking Complete against ONE shared
// corrections wrapper. Asserts: no race; baseline goroutine count
// restored; per-goroutine identity bleed-free; each call's Extra map
// is independent (no shared-map mutation).
// ---------------------------------------------------------------------------

func TestCorrections_ConcurrentReuse_D025(t *testing.T) {
	const (
		model     = "anthropic/claude-sonnet-4"
		nWorkers  = 128
		nRequests = 2
	)
	profile := llm.ModelProfile{
		ContextWindowTokens: 200_000,
		Corrections: llm.CorrectionsProfile{
			MessageOrdering:        llm.OrderingSystemFirstStrict,
			ReasoningEffortRouting: llm.ReasoningRouteThinking,
		},
	}
	cfg := snapshotWithProfile(model, profile)
	rec := newRecorder()
	rec.identCh = make(chan identity.Quadruple, nWorkers*nRequests*2)
	c := wrapRecorder(t, rec, cfg)

	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	wg.Add(nWorkers)
	errs := make(chan error, nWorkers*nRequests)
	for i := range nWorkers {
		go func(i int) {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  "T",
				UserID:    fmt.Sprintf("U-%d", i),
				SessionID: fmt.Sprintf("S-%d", i),
			}
			ctx, err := identity.With(context.Background(), id)
			if err != nil {
				errs <- err
				return
			}
			for j := range nRequests {
				req := makeRequest(model, []llm.ChatMessage{
					{Role: llm.RoleUser, Content: llm.Content{Text: strPtr(fmt.Sprintf("hi-%d-%d", i, j))}},
				})
				req.ReasoningEffort = llm.ReasoningHigh
				_, err := c.Complete(ctx, req)
				if err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("worker error: %v", err)
	}

	// Drain the identity channel and verify each goroutine's identity
	// reached the recorder. Cross-goroutine bleed would show up as a
	// duplicate or a wrong-tenant tag.
	seenUsers := make(map[string]int)
	for done := false; !done; {
		select {
		case q := <-rec.identCh:
			seenUsers[q.UserID]++
		case <-time.After(50 * time.Millisecond):
			done = true
		}
	}
	if got := len(seenUsers); got != nWorkers {
		t.Errorf("seen distinct users: got %d want %d", got, nWorkers)
	}

	// Per-call Extra map independence — sample one recorded request
	// and verify its Extra map contains only its own reasoning hint
	// (not, say, two hints stacked from the corrections layer
	// mutating a shared map).
	last, _ := rec.lastRequest()
	if last.Extra == nil {
		t.Errorf("recorded request missing Extra map")
	} else if _, ok := last.Extra["reasoning_effort"]; !ok {
		t.Errorf("recorded request missing reasoning_effort in Extra")
	}

	// Wait for the goroutine count to settle. The corrections
	// wrapper itself spawns no goroutines, but the test's own
	// goroutines may take a tick to retire. Bounded by a real-time
	// deadline; per AGENTS.md §11 we don't use time.Sleep for
	// synchronisation — Gosched yields cooperatively while the
	// deadline caps wall-clock cost.
	deadline := time.Now().Add(1 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if got := runtime.NumGoroutine(); got > baseline+5 {
		t.Errorf("goroutine count: got %d, baseline %d (corrections wrapper leaked)", got, baseline)
	}
}

// ---------------------------------------------------------------------------
// Wrap panics on nil inner — composition error caught at boot.
// ---------------------------------------------------------------------------

func TestCorrections_Wrap_NilInnerPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("Wrap(nil) did not panic")
		}
	}()
	_ = corrections.Wrap(nil, llm.ConfigSnapshot{})
}

// ---------------------------------------------------------------------------
// Close is idempotent.
// ---------------------------------------------------------------------------

func TestCorrections_Close_Idempotent(t *testing.T) {
	rec := newRecorder()
	c := corrections.Wrap(rec, llm.ConfigSnapshot{})
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Errorf("second Close should be no-op, got %v", err)
	}
}
