package protocol

import (
	"context"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/projectioncheck"
	"github.com/hurtener/Harbor/internal/tools"
)

// init self-registers the tools projection surface into the
// projection-completeness gate (§4.4). The tools catalog reads
// per-tool OAuth / approval / last-used through an OPTIONAL Annotator seam
// that has no production concrete in V1 (the tools annotator follow-up supplies it). The gate
// records the annotator-backed `last_used_at` field on the honest-omission
// allow-list with an annotator-wiring-pending reason; the always-assigned transport /
// scope / reliability facets, and the conservatively-defaulted
// oauth_status / approval_policy fields, are proven assigned. The
// facet-over-unwired hazard (loud-reject) and the fabricated-zero aggregate
// hazard (aggregates_partial marker) are covered by the Service tests, not
// the zero-value reflection.
func init() {
	projectioncheck.Register(projectioncheck.ProjectionContract{
		Surface: "tools",
		// Probe runs the PRODUCTION row projection over a fully-populated
		// tool with NO annotator wired (the V1 production reality), returning
		// the projected Tool row the gate reflects.
		Probe: func() any {
			// projectRow reads only its args + the (nil-safe) overrides map
			// and the nil annotator — no catalog access — so a bare projector
			// is a faithful production run with the annotator UNWIRED.
			p := &CatalogProjector{}
			t := tools.Tool{
				Name:        "probe-tool",
				Description: "gate probe tool",
				Transport:   tools.TransportMCP,    // → scope "agent"
				SideEffects: tools.SideEffectWrite, // → reliability "guarded"
			}
			return p.projectRow(context.Background(), identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, t)
		},
		OperatedFields: []string{"scope", "transport", "reliability_tier", "oauth_status", "approval_policy", "last_used_at"},
		HonestOmissions: map[string]string{
			// last_used_at rides the annotator; the Active aggregate reads it.
			// Zero until the tools annotator follow-up wires the Annotator — gated in the interim
			// by the aggregates_partial marker + CapToolAnnotations, never a
			// silent 0. The facet filters over the sibling oauth_status /
			// approval_policy loud-reject when unwired.
			"last_used_at": "annotator wiring pending — Active aggregate rides it; gated by aggregates_partial + CapToolAnnotations until the annotator lands",
		},
		ProdWiringTest: "TestProdWiring_ToolsProjectorAnnotatorUnwiredIsHonest",
	})
}
