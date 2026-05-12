package governance_test

import (
	"context"
	"errors"
	"testing"
	"time"

	artifactsinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/llm"
	_ "github.com/hurtener/Harbor/internal/llm/mock"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// TestLLMOpen_GovernanceLatent verifies the latent default: with no
// factory installed, llm.Open's chain does not engage governance — every
// call passes through unchanged. The wrapper hook is seated (via the
// governance package's init()) but returns inner verbatim.
func TestLLMOpen_GovernanceLatent(t *testing.T) {
	governance.ClearFactory()
	defer governance.ClearFactory()

	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	defer bus.Close(context.Background())
	art, err := artifactsinmem.New(config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.New: %v", err)
	}

	cfg := llm.ConfigSnapshot{
		Driver: "mock",
		ModelProfiles: map[string]llm.ModelProfile{
			"m": {ContextWindowTokens: 10000},
		},
		// Disable corrections/downgrade/retry to keep the chain
		// minimal — we're isolating governance behaviour.
		DisableCorrections: true,
		DisableDowngrade:   true,
		DisableRetry:       true,
	}
	client, err := llm.Open(context.Background(), cfg, llm.Deps{Artifacts: art, Bus: bus})
	if err != nil {
		t.Fatalf("llm.Open: %v", err)
	}
	defer client.Close(context.Background())

	ctx := ctxWith(t, "T", "U", "S", "R")
	resp, err := client.Complete(ctx, llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{
			{Role: llm.RoleUser, Content: llm.Content{Text: ptr("hi")}},
		},
	})
	if err != nil {
		t.Errorf("Complete under latent governance: %v", err)
	}
	if resp.Content == "" {
		t.Errorf("empty response")
	}
}

// TestLLMOpen_GovernanceFactoryShortCircuits verifies that with a
// registered factory, governance PreCall fires and short-circuits the
// chain. The mock LLM driver should NOT be invoked when governance
// blocks.
func TestLLMOpen_GovernanceFactoryShortCircuits(t *testing.T) {
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	defer bus.Close(context.Background())
	art, err := artifactsinmem.New(config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.New: %v", err)
	}

	governance.SetFactory(func(_ llm.ConfigSnapshot, _ llm.Deps) (governance.Subsystem, error) {
		return &alwaysReject{}, nil
	})
	defer governance.ClearFactory()

	cfg := llm.ConfigSnapshot{
		Driver:             "mock",
		ModelProfiles:      map[string]llm.ModelProfile{"m": {ContextWindowTokens: 10000}},
		DisableCorrections: true,
		DisableDowngrade:   true,
		DisableRetry:       true,
	}
	client, err := llm.Open(context.Background(), cfg, llm.Deps{Artifacts: art, Bus: bus})
	if err != nil {
		t.Fatalf("llm.Open: %v", err)
	}
	defer client.Close(context.Background())

	ctx := ctxWith(t, "T", "U", "S", "R")
	_, err = client.Complete(ctx, llm.CompleteRequest{
		Model:    "m",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: ptr("hi")}}},
	})
	if !errors.Is(err, governance.ErrBudgetExceeded) {
		t.Errorf("expected ErrBudgetExceeded short-circuit, got %v", err)
	}
}

type alwaysReject struct{}

func (alwaysReject) PreCall(_ context.Context, _ llm.CompleteRequest) error {
	return governance.ErrBudgetExceeded
}
func (alwaysReject) PostCall(_ context.Context, _ llm.CompleteRequest, _ llm.CompleteResponse, _ error) error {
	return nil
}

func ptr[T any](v T) *T { return &v }
