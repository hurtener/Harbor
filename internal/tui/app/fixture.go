package app

import (
	"time"

	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tui/projection"
)

// FixtureProjection returns the non-operational in-phase consumer projection.
func FixtureProjection() projection.Projection {
	return projection.Projection{Identity: types.IdentityScope{Tenant: "acme", User: "operator", Session: "01K0HARBORFIXTURE0000000000"}, Generation: 1, SessionStatus: "running", Blocks: []projection.Block{
		{ID: "task:fixture", Kind: "task", RunID: "fixture-run", Status: "running", Text: "Inspecting Runtime posture", At: time.Unix(1, 0).UTC()},
		{ID: "tool:fixture", Kind: "tool", RunID: "fixture-run", Status: "succeeded", Tool: "runtime.health", Text: "Runtime health snapshot received", At: time.Unix(2, 0).UTC()},
		{ID: "intervention:fixture", Kind: "intervention", RunID: "fixture-run", Status: "pending", Text: "Operator approval required", At: time.Unix(3, 0).UTC()},
	}}
}
