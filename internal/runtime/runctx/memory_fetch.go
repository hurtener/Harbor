package runctx

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/planner"
)

// FetchMemoryBlocks projects the pair-store path while its remaining consumers
// are retired. Cumulative session execution uses memory/session instead.
// No semantic index or external-memory injection is owned by this helper.
func FetchMemoryBlocks(ctx context.Context, store memory.MemoryStore, id identity.Quadruple) (*planner.MemoryBlocks, error) {
	patch, err := store.GetLLMContext(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("memory.GetLLMContext: %w", err)
	}
	return ProjectMemoryBlocks(patch), nil
}
