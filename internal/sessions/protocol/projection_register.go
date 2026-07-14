package protocol

import (
	"context"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/projectioncheck"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/sessions"
)

// probeSessionLister is the gate-scoped SessionLister double the sessions
// projection probe wires so ListSessions runs over a populated snapshot. It
// is NOT a production default — it exists only for the completeness probe
// (Half A wires its own fake; Half B's prod-wiring test proves mux.go
// installs the real Enricher). the sessions-projection enrichment phase predates the gate, so THIS phase
// adds the sessions registration.
type probeSessionLister struct{}

func (probeSessionLister) ListSnapshots(context.Context, sessions.SessionListFilter) ([]sessions.SessionSnapshot, error) {
	base := time.Unix(1_700_000_000, 0).UTC()
	snap := sessions.SessionSnapshot{Running: true}
	snap.ID = "probe-session"
	snap.Identity = identity.Identity{TenantID: "t", UserID: "u", SessionID: "probe-session"}
	snap.OpenedAt = base
	snap.LastSeen = base.Add(time.Minute)
	return []sessions.SessionSnapshot{snap}, nil
}

// probeCounterEnricher is the gate-scoped Enricher double returning a fully
// populated counter rollup so the projector's enrich overlay is exercised.
type probeCounterEnricher struct{}

func (probeCounterEnricher) Counters(context.Context, identity.Identity, string) SessionCounters {
	return SessionCounters{
		TasksCount:             3,
		EventsCount:            9,
		TotalCostCents:         42,
		TotalTokens:            1024,
		HasPendingIntervention: true,
		HasFailedTask:          true,
	}
}

// init self-registers the sessions projection surface into the
// projection-completeness gate (§4.4). Because the sessions-projection enrichment phase
// predates the gate and cannot register into a package that did not exist,
// this phase adds the sessions ProjectionContract — a probe over the
// production projectRow + enrich seam. Without it the coverage half would
// fail on the class's most-important surface.
func init() {
	projectioncheck.Register(projectioncheck.ProjectionContract{
		Surface: "sessions",
		Probe: func() any {
			p, err := NewListerProjector(probeSessionLister{}, WithEnricher(probeCounterEnricher{}))
			if err != nil {
				return prototypes.SessionRow{}
			}
			rows, err := p.ListSessions(context.Background(),
				identity.Identity{TenantID: "t", UserID: "u", SessionID: "probe-session"},
				prototypes.SessionFilter{}, false)
			if err != nil || len(rows) == 0 {
				return prototypes.SessionRow{}
			}
			return rows[0]
		},
		// The six false-absence counters the sessions facets/sort operate
		// over, plus the two always-assigned sort-key timestamps.
		OperatedFields: []string{
			"tasks_count", "events_count", "total_cost_cents", "total_tokens",
			"has_pending_intervention", "has_failed_task",
			"started_at", "last_activity_at",
		},
		ProdWiringTest: "TestProdWiring_SessionsProjectorInstallsEnricher",
	})
}
