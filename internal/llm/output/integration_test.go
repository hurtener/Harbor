package output_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	_ "github.com/hurtener/Harbor/internal/llm/corrections"
	_ "github.com/hurtener/Harbor/internal/llm/output"
	_ "github.com/hurtener/Harbor/internal/llm/retry"
)

// TestE2E_OutputChain_ComposesWithCorrectionsAndSafety wires the full
// chain — `retry(downgrade(corrections(safety(mock))))` — and asserts
// the compose order from D-043 by sending a request that exercises:
//
//   - safety pass (identity check + materialize/leak/budget)
//   - corrections pass (model-prefix profile applies, e.g. nim/*
//     message reordering)
//   - downgrade chain (Native → Prompted forced by a stub mock that
//     fails on json_schema)
//   - retry wrapper (validator forces one corrective re-ask)
func TestE2E_OutputChain_ComposesWithCorrectionsAndSafety(t *testing.T) {
	t.Parallel()

	bus := testBus(t)
	store, err := artifacts.Open(t.Context(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	// Register a unique driver name for this test that exercises the
	// downgrade + retry path. The registry is write-once so each test
	// instance picks a unique name to avoid colliding with other
	// test files.
	const driverName = "stage-d-staged-driver"
	var callCount atomic.Int64
	llm.Register(driverName, func(cfg llm.ConfigSnapshot, deps llm.Deps) (llm.Driver, error) {
		return &stagedDriver{counter: &callCount}, nil
	})

	cfg := llm.ConfigSnapshot{
		Driver:               driverName,
		ContextWindowReserve: 0.05,
		HeavyOutputThreshold: 32_768,
		ModelProfiles: map[string]llm.ModelProfile{
			"openai/gpt-4o": {
				ContextWindowTokens: 1000,
				OutputMode:          llm.OutputModeNative,
				MaxRetries:          2,
			},
		},
	}
	client, err := llm.Open(context.Background(), cfg, llm.Deps{
		Artifacts: store,
		Bus:       bus,
	})
	if err != nil {
		t.Fatalf("llm.Open: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })

	// First validator call rejects "downgraded-bad"; second call
	// accepts "downgraded-good".
	var validatorCalls atomic.Int64
	validator := func(r llm.CompleteResponse) error {
		validatorCalls.Add(1)
		if r.Content == "downgraded-good" {
			return nil
		}
		return errors.New("not good enough")
	}

	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, err := identity.WithRun(t.Context(), id, "r")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	text := "give me JSON"
	resp, err := client.Complete(ctx, llm.CompleteRequest{
		Model: "openai/gpt-4o",
		Messages: []llm.ChatMessage{
			{Role: llm.RoleUser, Content: llm.Content{Text: &text}},
		},
		ResponseFormat: &llm.ResponseFormat{
			Kind:       llm.FormatJSONSchema,
			JSONSchema: sampleSchema(),
		},
		Validator: validator,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "downgraded-good" {
		t.Errorf("Content = %q, want \"downgraded-good\"", resp.Content)
	}
	// Total inner calls: 1 (native fails) + 1 (prompted -- first
	// attempt of retry, validator rejects "downgraded-bad") + 1
	// (prompted -- second attempt of retry, returns
	// "downgraded-good"). Total = 3.
	if got := callCount.Load(); got != 3 {
		t.Errorf("inner driver was called %d times; expected 3", got)
	}
	if validatorCalls.Load() != 2 {
		t.Errorf("validator called %d times; expected 2", validatorCalls.Load())
	}

	// Multi-isolation cross-check: missing identity → fail-closed.
	_, err = client.Complete(context.Background(), llm.CompleteRequest{
		Model:    "openai/gpt-4o",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
	})
	if !errors.Is(err, llm.ErrIdentityMissing) {
		t.Errorf("expected ErrIdentityMissing; got %v", err)
	}
}

// stagedDriver is a tiny Driver that:
//
//	attempt 1 → return invalid-json-schema error  (Native fails)
//	attempt 2 → return content "downgraded-bad"   (Prompted; validator rejects)
//	attempt 3 → return content "downgraded-good"  (Prompted; validator accepts)
type stagedDriver struct {
	counter *atomic.Int64
	closed  atomic.Bool
}

func (s *stagedDriver) Complete(_ context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	n := s.counter.Add(1)
	switch n {
	case 1:
		return llm.CompleteResponse{}, errors.New("provider: invalid json_schema response")
	case 2:
		return llm.CompleteResponse{Content: "downgraded-bad"}, nil
	default:
		return llm.CompleteResponse{Content: "downgraded-good"}, nil
	}
}

func (s *stagedDriver) Close(_ context.Context) error {
	s.closed.CompareAndSwap(false, true)
	return nil
}
