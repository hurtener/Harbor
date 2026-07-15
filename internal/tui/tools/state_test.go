package tools

import (
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestDerive_PreservesUnavailableAggregates(t *testing.T) {
	state := Derive(types.ToolListResponse{Tools: []types.Tool{{ID: "future"}}, AggregatesPartial: true}, false)
	if len(state.Rows) != 1 || !state.AggregatesPartial {
		t.Fatalf("state=%#v", state)
	}
}
