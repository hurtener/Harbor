package protocol_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/memory"
	memprotocol "github.com/hurtener/Harbor/internal/memory/protocol"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// TestProdWiring_MemoryListRejectsAgentFacet is the memory prod-wiring test
// named by the projection-completeness contract (Half B). It proves the
// PRODUCTION memory.List loud-rejects a `filter.agent_ids` facet over the
// unpopulated producer identity rather than returning a false-empty page,
// and that the surviving populated facets (scope / driver / strategy) still
// list real rows (D-313).
func TestProdWiring_MemoryListRejectsAgentFacet(t *testing.T) {
	h := newMemHarness(t, memory.StrategyTruncation, 100000)
	id := testIdentity()
	seedTurns(t, h, id, 3)

	// agent_ids loud-rejects.
	_, err := memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
		prototypes.MemoryListRequest{Filter: prototypes.MemoryFilter{AgentIDs: []string{"a"}}}, id)
	if !errors.Is(err, memprotocol.ErrInvalidFilter) {
		t.Fatalf("agent_ids: err = %v, want ErrInvalidFilter", err)
	}

	// A populated facet still lists rows (no over-rejection).
	resp, err := memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
		prototypes.MemoryListRequest{Filter: prototypes.MemoryFilter{Scopes: []string{string(prototypes.MemoryScopeSession)}}}, id)
	if err != nil {
		t.Fatalf("scope facet: unexpected err %v", err)
	}
	if len(resp.Items) == 0 {
		t.Fatal("scope facet returned 0 rows — the surviving facet must still list real records")
	}
}
