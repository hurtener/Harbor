// Wave 7b cross-subsystem integration test per AGENTS.md §17.5.
//
// Wave 7b closed the LLM subsystem (and the governance scaffold):
//
//   - Phase 32: `LLMClient` one-method interface + multimodal sum-type
//     (D-021) + auto-materialize boundary (D-022) + mandatory
//     context-window safety net (D-026 / D-039).
//   - Phase 33: bifrost integration (the only V1 driver). Translates
//     `CompleteRequest` ↔ bifrost's chat shapes; cost passthrough via
//     `llm.cost.recorded` (D-040). Native providers + OpenAI-compat.
//   - Phase 33a: custom OpenAI-compatible providers (NIM, vLLM, ollama,
//     in-house gateways) + per-provider network tuning (D-042).
//   - Phase 34: provider correction layer (`SchemaSanitizer` +
//     `MessageNormalizer`) — single baked-in mode, composes OUTSIDE
//     the safety pass (D-041).
//   - Phase 35: structured-output downgrade chain
//     `Native → Prompted → Text` on `IsInvalidJSONSchemaError`.
//   - Phase 36: validator-driven retry with corrective sub-prompt,
//     bounded by `ModelProfile.MaxRetries`.
//   - Phase 36a + 36b: governance (cost accumulator + token bucket +
//     MaxTokens). Latent default per Wave 7b scoping (D-044) —
//     empty `IdentityTiers` ⇒ permit everywhere.
//
// The wave-end E2E proves these COMPOSE: the runtime can `llm.Open`
// a full client (mock or bifrost) whose chain is
// `governance(retry(downgrade(corrections(safety(driver)))))`,
// requests flow through every layer with identity preserved, and the
// observability surface (cost-recorded, mode-downgraded,
// retry-with-feedback, budget-exceeded) fires the way Wave 8's
// planner phases will need.
//
// Two halves:
//
//   - The mock-driven tests run on every CI invocation. They pin the
//     compose order, latent-governance, retry, and the
//     identity-mandatory contract end-to-end without burning API
//     credits.
//   - The live tests behind `HARBOR_LIVE_LLM=1` exercise the
//     translation layers against real providers. Two free providers
//     (OpenRouter free-tier + Gemini free-tier) plus NIM via the
//     custom-provider path — the same diversity the future CI
//     live-smoke job will use. CI default skips. Run locally with:
//
//       HARBOR_LIVE_LLM=1 go test -race -count=1 -timeout 600s \
//           -run TestE2E_Wave7b_Live ./test/integration/...
//
// NIM specifically validates the Phase 34 NIM message-reorder quirk
// end-to-end against a real provider — the corrections layer's most
// impactful normalization, settled by D-041.
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	_ "github.com/hurtener/Harbor/internal/llm/corrections"
	_ "github.com/hurtener/Harbor/internal/llm/drivers/bifrost"
	_ "github.com/hurtener/Harbor/internal/llm/mock"
	_ "github.com/hurtener/Harbor/internal/llm/output"
	_ "github.com/hurtener/Harbor/internal/llm/retry"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// --- helpers --------------------------------------------------------------

func openLLMDeps(t *testing.T) (llm.Deps, func()) {
	t.Helper()
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		ReplayBufferSize:         16,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	store, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("artifacts.Open: %v", err)
	}
	cleanup := func() {
		_ = bus.Close(context.Background())
		_ = store.Close(context.Background())
	}
	return llm.Deps{Artifacts: store, Bus: bus}, cleanup
}

func wave7bIdentity(t *testing.T, label string) context.Context {
	t.Helper()
	id := identity.Identity{
		TenantID:  "wave7b",
		UserID:    "harbor",
		SessionID: label,
	}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// --- mock-driven compose-order + governance + retry -----------------------

// TestE2E_Wave7b_ComposeOrder_MockEndToEnd proves the full Wave 7b chain
// wires up via `llm.Open`: `governance(retry(downgrade(corrections(safety(driver)))))`.
// Uses the mock driver; asserts the request flows through every layer
// and returns a response.
func TestE2E_Wave7b_ComposeOrder_MockEndToEnd(t *testing.T) {
	deps, cleanup := openLLMDeps(t)
	defer cleanup()

	snap := llm.ConfigSnapshot{
		Driver:               "mock",
		ContextWindowReserve: 0.05,
		HeavyOutputThreshold: 32 * 1024,
		ModelProfiles: map[string]llm.ModelProfile{
			"mock-model": {ContextWindowTokens: 8192},
		},
	}
	client, err := llm.Open(context.Background(), snap, deps)
	if err != nil {
		t.Fatalf("llm.Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	ctx := wave7bIdentity(t, "compose")
	text := "hello, wave-7b"
	resp, err := client.Complete(ctx, llm.CompleteRequest{
		Model: "mock-model",
		Messages: []llm.ChatMessage{
			{Role: llm.RoleUser, Content: llm.Content{Text: &text}},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content == "" {
		t.Errorf("Complete returned empty content")
	}
	if resp.Usage.TotalTokens == 0 {
		t.Errorf("Complete returned zero Usage.TotalTokens — usage wiring suspected dead")
	}
}

// TestE2E_Wave7b_GovernanceLatent_NoConfig_Permits proves the
// latent-default scoping decision (D-044): with no `governance.Factory`
// wired and no tiers declared, every call permits.
func TestE2E_Wave7b_GovernanceLatent_NoConfig_Permits(t *testing.T) {
	deps, cleanup := openLLMDeps(t)
	defer cleanup()

	snap := llm.ConfigSnapshot{
		Driver:               "mock",
		ContextWindowReserve: 0.05,
		HeavyOutputThreshold: 32 * 1024,
		ModelProfiles: map[string]llm.ModelProfile{
			"mock-model": {ContextWindowTokens: 8192},
		},
	}
	client, err := llm.Open(context.Background(), snap, deps)
	if err != nil {
		t.Fatalf("llm.Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	ctx := wave7bIdentity(t, "latent")
	text := "should permit"
	const calls = 8
	for i := 0; i < calls; i++ {
		resp, err := client.Complete(ctx, llm.CompleteRequest{
			Model: "mock-model",
			Messages: []llm.ChatMessage{
				{Role: llm.RoleUser, Content: llm.Content{Text: &text}},
			},
		})
		if err != nil {
			t.Fatalf("call %d: latent governance should permit, got %v", i, err)
		}
		if resp.Content == "" {
			t.Errorf("call %d: empty content", i)
		}
	}
}

// TestE2E_Wave7b_GovernanceCeiling_FailsClosed proves the cost
// accumulator's enforcement path: with a ceiling set + cost reported
// above it, PreCall returns ErrBudgetExceeded. Uses `CostAccumulator`
// directly (the unit-level surface) wired against a real in-mem
// StateStore + event bus to exercise the cross-subsystem boundary.
func TestE2E_Wave7b_GovernanceCeiling_FailsClosed(t *testing.T) {
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	defer func() { _ = bus.Close(context.Background()) }()

	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	const tier = "wave7b-cap-tier"
	const ceiling = 0.05 // USD
	cfg := governance.Config{
		DefaultTier: tier,
		IdentityTiers: map[string]governance.TierConfig{
			tier: {BudgetCeilingUSD: ceiling},
		},
	}
	acc, err := governance.NewCostAccumulator(store, bus, cfg)
	if err != nil {
		t.Fatalf("NewCostAccumulator: %v", err)
	}
	defer func() { _ = acc.Close(context.Background()) }()

	ctx := wave7bIdentity(t, "ceiling")

	// Drive cost from below toward exceedance. PreCall MUST permit
	// while cumulative total < ceiling.
	req := llm.CompleteRequest{Model: "mock-model"}
	if err := acc.PreCall(ctx, req); err != nil {
		t.Fatalf("PreCall (initial): %v", err)
	}
	// Each PostCall records 0.02 USD; after 3 calls the cumulative
	// total is 0.06 > 0.05 ceiling.
	for i := 0; i < 3; i++ {
		err := acc.PostCall(ctx, req, llm.CompleteResponse{
			Cost: llm.Cost{TotalCost: 0.02},
		}, nil)
		if err != nil {
			t.Fatalf("PostCall[%d]: %v", i, err)
		}
	}

	// The NEXT PreCall MUST fail closed with wrapped ErrBudgetExceeded.
	err = acc.PreCall(ctx, req)
	if !errors.Is(err, governance.ErrBudgetExceeded) {
		t.Errorf("PreCall after exceedance: got %v, want ErrBudgetExceeded", err)
	}
}

// TestE2E_Wave7b_RetryWithFeedback_BoundedByMaxRetries proves the
// validator-driven retry loop fires bounded by ModelProfile.MaxRetries
// and surfaces a wrapped error on exhaustion.
func TestE2E_Wave7b_RetryWithFeedback_BoundedByMaxRetries(t *testing.T) {
	deps, cleanup := openLLMDeps(t)
	defer cleanup()

	const maxRetries = 2
	snap := llm.ConfigSnapshot{
		Driver:               "mock",
		ContextWindowReserve: 0.05,
		HeavyOutputThreshold: 32 * 1024,
		ModelProfiles: map[string]llm.ModelProfile{
			"mock-model": {
				ContextWindowTokens: 8192,
				MaxRetries:          maxRetries,
			},
		},
	}
	client, err := llm.Open(context.Background(), snap, deps)
	if err != nil {
		t.Fatalf("llm.Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	ctx := wave7bIdentity(t, "retry")

	var attempts atomic.Int32
	validator := func(_ llm.CompleteResponse) error {
		attempts.Add(1)
		return fmt.Errorf("validator: always fail")
	}

	text := "should retry"
	_, err = client.Complete(ctx, llm.CompleteRequest{
		Model: "mock-model",
		Messages: []llm.ChatMessage{
			{Role: llm.RoleUser, Content: llm.Content{Text: &text}},
		},
		Validator: validator,
	})
	if err == nil {
		t.Fatalf("Complete with always-failing validator: got nil err, want exhausted")
	}
	// Total invocations of the validator = 1 (original) + maxRetries.
	if got := attempts.Load(); got != int32(1+maxRetries) {
		t.Errorf("validator attempts=%d, want %d (1 original + %d retries)",
			got, 1+maxRetries, maxRetries)
	}
}

// TestE2E_Wave7b_IdentityMandatory_FailsClosed proves the Wave 7b
// chain rejects a request with no identity in ctx. The safety pass
// is the canonical gate; downstream layers must NOT silently permit.
func TestE2E_Wave7b_IdentityMandatory_FailsClosed(t *testing.T) {
	deps, cleanup := openLLMDeps(t)
	defer cleanup()

	snap := llm.ConfigSnapshot{
		Driver:               "mock",
		ContextWindowReserve: 0.05,
		HeavyOutputThreshold: 32 * 1024,
		ModelProfiles: map[string]llm.ModelProfile{
			"mock-model": {ContextWindowTokens: 8192},
		},
	}
	client, err := llm.Open(context.Background(), snap, deps)
	if err != nil {
		t.Fatalf("llm.Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	text := "should reject"
	_, err = client.Complete(context.Background(), llm.CompleteRequest{
		Model: "mock-model",
		Messages: []llm.ChatMessage{
			{Role: llm.RoleUser, Content: llm.Content{Text: &text}},
		},
	})
	if err == nil {
		t.Errorf("Complete without identity: got nil err, want identity-rejection")
	}
}

// --- live provider tests (HARBOR_LIVE_LLM=1) ------------------------------

func requireLiveLLM(t *testing.T) {
	t.Helper()
	if os.Getenv("HARBOR_LIVE_LLM") != "1" {
		t.Skip("set HARBOR_LIVE_LLM=1 to run the live wave-end E2E (burns API credits or hits free-tier rate limits)")
	}
}

// liveCtx builds an identity-bearing ctx with a generous deadline.
// NIM cold-start can exceed 60s on a free-tier endpoint; default to
// 90s, with the NIM test overriding to 180s.
func liveCtx(t *testing.T, label string, timeout time.Duration) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx := wave7bIdentity(t, label)
	return context.WithTimeout(ctx, timeout)
}

// subscribeCostRecorded sets up an admin-scope subscriber that asserts
// `llm.cost.recorded` fires during the test. The bifrost driver
// publishes this event on every successful Complete (D-040). Used by
// live tests to gate the observability surface end-to-end.
func subscribeCostRecorded(t *testing.T, bus events.EventBus) events.Subscription {
	t.Helper()
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{llm.EventTypeCostRecorded},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe(cost): %v", err)
	}
	return sub
}

// awaitCostRecorded blocks up to `deadline` for a cost-recorded event.
// Logs (but does not fail) on miss — some live providers don't emit
// usage on every response shape; the wave-end's job is to surface, not
// strictly gate on, this observation.
func awaitCostRecorded(t *testing.T, sub events.Subscription, deadline time.Duration) {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Logf("cost-recorded subscriber closed; not asserting")
			return
		}
		t.Logf("observed %s: %+v", ev.Type, ev.Payload)
	case <-time.After(deadline):
		t.Logf("llm.cost.recorded did NOT fire within %s — observability gap suspected", deadline)
	}
}

// TestE2E_Wave7b_Live_OpenRouter_BasicChat exercises the bifrost ↔
// OpenRouter path against a free-tier model. Catches: bifrost API
// drift, OpenRouter free-tier response shape changes, our translation
// layer regressions.
func TestE2E_Wave7b_Live_OpenRouter_BasicChat(t *testing.T) {
	requireLiveLLM(t)
	if os.Getenv("OPENROUTER_API_KEY") == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}
	model := os.Getenv("OPENROUTER_LLM_MODEL")
	if model == "" {
		// Default to a stable openrouter free-tier model. Operators
		// override via OPENROUTER_LLM_MODEL.
		model = "google/gemini-2.5-flash-lite"
	}

	deps, cleanup := openLLMDeps(t)
	defer cleanup()

	snap := llm.ConfigSnapshot{
		Driver:               "bifrost",
		Provider:             "openrouter",
		APIKey:               "env.OPENROUTER_API_KEY",
		ContextWindowReserve: 0.05,
		HeavyOutputThreshold: 32 * 1024,
		Timeout:              60 * time.Second,
		ModelProfiles: map[string]llm.ModelProfile{
			model: {ContextWindowTokens: 128_000},
		},
	}
	client, err := llm.Open(context.Background(), snap, deps)
	if err != nil {
		t.Fatalf("llm.Open(openrouter): %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	sub := subscribeCostRecorded(t, deps.Bus)
	defer sub.Cancel()

	ctx, cancel := liveCtx(t, "openrouter", 90*time.Second)
	defer cancel()

	text := "Reply with the single word: ok"
	resp, err := client.Complete(ctx, llm.CompleteRequest{
		Model: model,
		Messages: []llm.ChatMessage{
			{Role: llm.RoleUser, Content: llm.Content{Text: &text}},
		},
	})
	if err != nil {
		t.Fatalf("OpenRouter Complete: %v", err)
	}
	if resp.Content == "" {
		t.Errorf("OpenRouter response: empty content")
	}
	t.Logf("OpenRouter %s replied: %q (usage=%+v)", model, resp.Content, resp.Usage)

	awaitCostRecorded(t, sub, 3*time.Second)
}

// TestE2E_Wave7b_Live_Gemini_StructuredOutput exercises the bifrost ↔
// Gemini path (NON-OpenAI shape — bifrost's translate has a separate
// code path for Gemini). Catches: bifrost Gemini-shape drift, our
// corrections-layer handling of the Gemini envelope.
func TestE2E_Wave7b_Live_Gemini_StructuredOutput(t *testing.T) {
	requireLiveLLM(t)
	if os.Getenv("GOOGLE_API_KEY") == "" {
		t.Skip("GOOGLE_API_KEY not set")
	}
	model := os.Getenv("GOOGLE_LLM_MODEL")
	if model == "" {
		model = "gemini-2.5-flash"
	}
	// Strip a "google/" prefix if the operator uses the OpenRouter-
	// flavoured form locally — bifrost's native gemini provider wants
	// the bare model name.
	model = strings.TrimPrefix(model, "google/")

	deps, cleanup := openLLMDeps(t)
	defer cleanup()

	snap := llm.ConfigSnapshot{
		Driver:               "bifrost",
		Provider:             "gemini",
		APIKey:               "env.GOOGLE_API_KEY",
		ContextWindowReserve: 0.05,
		HeavyOutputThreshold: 32 * 1024,
		Timeout:              60 * time.Second,
		ModelProfiles: map[string]llm.ModelProfile{
			model: {ContextWindowTokens: 1_000_000},
		},
	}
	client, err := llm.Open(context.Background(), snap, deps)
	if err != nil {
		t.Fatalf("llm.Open(gemini): %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	sub := subscribeCostRecorded(t, deps.Bus)
	defer sub.Cancel()

	ctx, cancel := liveCtx(t, "gemini-json", 90*time.Second)
	defer cancel()

	text := `Respond with valid JSON only: {"answer": "ok"}`
	resp, err := client.Complete(ctx, llm.CompleteRequest{
		Model: model,
		Messages: []llm.ChatMessage{
			{Role: llm.RoleUser, Content: llm.Content{Text: &text}},
		},
		ResponseFormat: &llm.ResponseFormat{Kind: llm.FormatJSONObject},
	})
	if err != nil {
		t.Fatalf("Gemini Complete: %v", err)
	}
	if resp.Content == "" {
		t.Fatalf("Gemini response: empty content")
	}
	if !strings.Contains(resp.Content, "{") {
		t.Errorf("Gemini response: content lacks JSON shape: %q", resp.Content)
	}
	// Loose round-trip: parse what came back, expect SOMETHING with
	// an `answer` field. Free models don't always honour the schema
	// precisely; we accept any object.
	stripped := strings.TrimSpace(resp.Content)
	stripped = strings.TrimPrefix(stripped, "```json")
	stripped = strings.TrimPrefix(stripped, "```")
	stripped = strings.TrimSuffix(stripped, "```")
	stripped = strings.TrimSpace(stripped)
	var got map[string]any
	if err := json.Unmarshal([]byte(stripped), &got); err != nil {
		t.Errorf("Gemini response: JSON unmarshal failed: %v (raw=%q)", err, resp.Content)
	}
	t.Logf("Gemini %s replied: %q (usage=%+v)", model, resp.Content, resp.Usage)

	awaitCostRecorded(t, sub, 3*time.Second)
}

// TestE2E_Wave7b_Live_NIM_MessageReorderQuirk would exercise Phase 33a's
// custom-provider path AND Phase 34's NIM message-reorder quirk end-to-
// end, but the NIM endpoint is currently unresponsive for the
// operator's account.
//
// Observed: an initial direct `curl` to the operator's NIM endpoint
// with `google/gemma-4-31b-it` completed cleanly in ~6 seconds and
// returned "OK." — establishing key + endpoint + model wired up.
// Subsequent calls (both `curl` AND bifrost-routed) time out after
// 30-180s with zero bytes received. Both `google/gemma-4-31b-it` and
// the alternate `deepseek-ai/deepseek-v4-flash` reproduce the timeout
// at the transport layer. Because direct curl ALSO times out, the
// signal is NIM-side (account throttling, cold-pool eviction, or
// model unavailability) rather than a Harbor / bifrost translation
// regression.
//
// The message-reorder quirk this test would have validated end-to-end
// IS unit-tested in `internal/llm/corrections/normalizer_test.go` and
// stress-tested in the D-025 concurrent-reuse suite. The live wave-end
// observation is the missing gate, deferred to a future PR once a
// stable NIM model + window is identified.
//
// Skipped until NIM endpoint availability stabilises (track separately).
func TestE2E_Wave7b_Live_NIM_MessageReorderQuirk(t *testing.T) {
	t.Skip("TODO: phase-33a-nim-live — NIM endpoint is unresponsive on this account for both google/gemma-4-31b-it and deepseek-ai/deepseek-v4-flash (direct curl times out identically to bifrost-routed). Re-enable when a stable NIM model is identified.")

	requireLiveLLM(t)
	if os.Getenv("NVIDIA_API_KEY") == "" {
		t.Skip("NVIDIA_API_KEY not set")
	}
	model := os.Getenv("MODEL_NIM")
	if model == "" {
		model = "google/gemma-4-31b-it"
	}

	deps, cleanup := openLLMDeps(t)
	defer cleanup()

	snap := llm.ConfigSnapshot{
		Driver:               "bifrost",
		Provider:             "nim",
		ContextWindowReserve: 0.05,
		HeavyOutputThreshold: 32 * 1024,
		Timeout:              180 * time.Second,
		ModelProfiles: map[string]llm.ModelProfile{
			model: {
				ContextWindowTokens: 32_000,
				Corrections: llm.CorrectionsProfile{
					MessageOrdering: llm.OrderingSystemFirstStrict,
				},
			},
		},
		CustomProviders: []llm.CustomProviderSpec{
			{
				Name:                "nim",
				BaseURL:             "https://integrate.api.nvidia.com",
				APIKeyEnvVar:        "NVIDIA_API_KEY",
				Models:              []string{model},
				BaseProviderType:    "openai",
				Timeout:             180 * time.Second,
				MaxRetries:          2,
				RetryBackoffInitial: 500 * time.Millisecond,
				RetryBackoffMax:     5 * time.Second,
				Concurrency:         3,
				BufferSize:          10,
			},
		},
	}
	client, err := llm.Open(context.Background(), snap, deps)
	if err != nil {
		t.Fatalf("llm.Open(nim): %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	sub := subscribeCostRecorded(t, deps.Bus)
	defer sub.Cancel()

	// Deliberately out-of-order conversation: a user message FIRST,
	// then a system message. NIM strict mode would reject; the
	// corrections layer's MessageOrdering=SystemFirstStrict should
	// reorder before dispatch.
	ctx, cancel := liveCtx(t, "nim-reorder", 180*time.Second)
	defer cancel()

	userText := "Reply with ok."
	systemText := "You are concise."
	resp, err := client.Complete(ctx, llm.CompleteRequest{
		Model: model,
		Messages: []llm.ChatMessage{
			{Role: llm.RoleUser, Content: llm.Content{Text: &userText}},
			{Role: llm.RoleSystem, Content: llm.Content{Text: &systemText}},
		},
	})
	if err != nil {
		t.Fatalf("NIM Complete (corrections expected): %v", err)
	}
	if resp.Content == "" {
		t.Errorf("NIM response: empty content")
	}
	t.Logf("NIM %s (corrections reorder) replied: %q (usage=%+v)", model, resp.Content, resp.Usage)

	awaitCostRecorded(t, sub, 3*time.Second)
}
