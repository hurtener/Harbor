package agentcfg

import (
	"context"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// SlotGeneration names one durable record by its kind and the EventID of its
// current write. Every StateStore write mints a fresh EventID, so an
// unchanged (kind, EventID) pair proves the record was not rewritten.
type SlotGeneration struct {
	Kind    string
	EventID state.EventID
}

// CatalogInputs is the durable half of a tool-catalog input stamp: the
// generations of every agent-config record the catalog projection reads for
// one (tenant, user, agent). An empty EventID means the slot is absent.
//
// The values are opaque write identities, never config content. They change
// whenever the projection's durable inputs may have changed: the agent
// active pointer (which is also the lifecycle slot, so retirement moves it),
// the acting user's active pointer, and every signed-capability activation
// fence record (whose phase transitions change Active visibility without a
// pointer write).
type CatalogInputs struct {
	AgentActive state.EventID
	UserActive  state.EventID
	// Fences lists every activation-fence record for the agent, sorted by
	// Kind. Fences are agent-scoped records; user-tier candidates are matched
	// against them at read time, so the same list covers both tiers.
	Fences []SlotGeneration
}

// CatalogInputReader is the optional Registry surface that reports
// CatalogInputs with a bounded number of small reads (two point loads, one
// identity-scoped prefix list, and one more point load for the user tier).
// A Registry that does not implement it cannot back a structural catalog
// cache; callers branch on the type assertion and fall back to building the
// projection on every request.
//
// It performs no authorization. Callers must admit the request before
// reading inputs, exactly as they do before building the projection.
type CatalogInputReader interface {
	CatalogInputs(ctx context.Context, id identity.Quadruple, agentID string) (CatalogInputs, error)
}
