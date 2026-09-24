package memory_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/memory"
)

func TestOpen_CumulativeDefaultAndRemovedStrategy(t *testing.T) {
	deps := newTestDeps(t)
	store, err := memory.Open(t.Context(), memory.ConfigSnapshot{}, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	view, err := store.Inspect(t.Context(), validQuadruple())
	if err != nil || view.Strategy != memory.StrategyRollingSummary {
		t.Fatalf("default does not use cumulative owner: %+v %v", view, err)
	}
	for _, strategy := range []memory.Strategy{"truncation", "unknown"} {
		if _, err := memory.Open(t.Context(), memory.ConfigSnapshot{Strategy: strategy}, deps); !errors.Is(err, memory.ErrStrategyNotImplemented) {
			t.Fatalf("removed strategy %q accepted: %v", strategy, err)
		}
	}
}
