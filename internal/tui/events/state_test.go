package events

import (
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestDerive_PropagatesRetentionAndAggregatePartiality(t *testing.T) {
	state := Derive(types.EventsListResponse{Events: []types.StateEvent{{Type: "future.event"}}, HasMore: true}, types.EventAggregateResponse{Truncated: true})
	if len(state.Rows) != 1 || !state.HasMore || !state.Truncated {
		t.Fatalf("state=%#v", state)
	}
}
