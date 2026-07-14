package protocol

import (
	"encoding/json"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/protocol/projectioncheck"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// init self-registers the memory projection surface into the
// projection-completeness gate (§4.4). After this phase memory
// operates over only POPULATED facets — scope / driver / strategy — because
// the structurally-dead TTL facet + aggregate were removed and
// `filter.agent_ids` loud-rejects (there is no producer identity on a V1
// turn to populate from). The gate proves the projector assigns every facet
// the surviving filters key on.
func init() {
	projectioncheck.Register(projectioncheck.ProjectionContract{
		Surface: "memory",
		// Probe runs the PRODUCTION snapshotTurns projection over a
		// fully-populated one-turn snapshot and returns the projected
		// MemoryItem the gate reflects.
		Probe: func() any {
			rec := memory.Record{
				Strategy: memory.StrategyTruncation,
				Turns: []memory.ConversationTurn{{
					UserMessage:       "probe user message",
					AssistantResponse: "probe assistant response",
					Timestamp:         time.Unix(1_700_000_000, 0).UTC(),
				}},
			}
			b, err := json.Marshal(rec)
			if err != nil {
				// A marshal failure here is a construction bug — return an
				// empty item so the gate flags the unassigned facets loudly.
				return prototypes.MemoryItem{}
			}
			id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
			rows, err := snapshotTurns(memory.Snapshot{Strategy: memory.StrategyTruncation, Bytes: b}, id, string(prototypes.MemoryDriverInmem), 0)
			if err != nil || len(rows) == 0 {
				return prototypes.MemoryItem{}
			}
			return rows[0].item
		},
		// The three populated facets memory.list keys on after this phase.
		// agent_ids loud-rejects (not an operated post-projection facet);
		// has_ttl_expiring was removed. No honest-omission entry is needed —
		// every surviving operated facet is populated.
		OperatedFields: []string{"scope", "driver", "strategy"},
		ProdWiringTest: "TestProdWiring_MemoryListRejectsAgentFacet",
	})
}
