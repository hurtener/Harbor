package retry_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	artifactsinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/llm"
)

// costCadenceDriver returns a caller-scripted response per attempt.
type costCadenceDriver struct {
	attempt atomic.Int64
	fn      func(attempt int) (llm.CompleteResponse, error)
}

func (d *costCadenceDriver) Complete(_ context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	return d.fn(int(d.attempt.Add(1)))
}
func (d *costCadenceDriver) Close(context.Context) error { return nil }

var costCadenceSeq atomic.Int64

// TestCostEmit_PerAttemptUnderRetry pins the R1 cadence (D-293): the
// mandatory LLM-edge safety wrapper emits llm.cost.recorded ONCE PER
// DRIVER-LEVEL completion. Because the retry-with-feedback wrapper composes
// OUTSIDE the safety band, a single Complete that retries once routes through
// the safety band twice — so exactly TWO cost events fire, matching the
// per-call attempt-cost governance cadence (no double emission). This lives
// in the retry test binary (where the retry wrapper is seated via import),
// NOT in internal/llm's test binary — seating it there would break the
// unseated-wrapper warning test.
func TestCostEmit_PerAttemptUnderRetry(t *testing.T) {
	t.Parallel()
	bus := testBus(t)

	store, err := artifactsinmem.New(config.ArtifactsConfig{
		Driver:                    "inmem",
		HeavyOutputThresholdBytes: 32 * 1024,
	})
	if err != nil {
		t.Fatalf("artifacts inmem: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{llm.EventTypeCostRecorded},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	drv := &costCadenceDriver{fn: func(attempt int) (llm.CompleteResponse, error) {
		// Both attempts succeed at the DRIVER level (so the safety band emits
		// each time); the request Validator rejects attempt 1, driving one
		// retry.
		content := "good"
		if attempt == 1 {
			content = "bad"
		}
		return llm.CompleteResponse{
			Content: content,
			Cost:    llm.Cost{TotalCost: 0.01 * float64(attempt)},
			Usage:   llm.Usage{TotalTokens: 5},
		}, nil
	}}
	name := fmt.Sprintf("cost-cadence-retry-%d", costCadenceSeq.Add(1))
	llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return drv, nil })

	cfg := snapshotWithProfile(llm.ModelProfile{ContextWindowTokens: 1000, MaxRetries: 1})
	cfg.Driver = name
	client, err := llm.Open(context.Background(), cfg, llm.Deps{Artifacts: store, Bus: bus})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	validator := func(r llm.CompleteResponse) error {
		if r.Content != "good" {
			return errors.New("content not good")
		}
		return nil
	}
	if _, err := client.Complete(ctxWithIdentity(t), sampleRequest(validator)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := drv.attempt.Load(); got != 2 {
		t.Fatalf("driver attempts = %d, want 2 (one retry)", got)
	}

	// Collect cost events over a bounded window.
	got := 0
	deadline := time.After(1 * time.Second)
loop:
	for {
		select {
		case ev := <-sub.Events():
			if _, ok := ev.Payload.(llm.CostRecordedPayload); ok {
				got++
			}
		case <-deadline:
			break loop
		}
	}
	if got != 2 {
		t.Fatalf("observed %d llm.cost.recorded events, want exactly 2 (one per driver-level attempt)", got)
	}
}
