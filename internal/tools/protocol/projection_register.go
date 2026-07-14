package protocol

import (
	"context"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/projectioncheck"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tools"
)

// probeAnnotator is the Annotator double the tools projection probe wires
// so the annotator-backed assignment path (OAuth status / approval policy /
// last-used) runs over a populated source. It is NOT a production default —
// it exists only for the projection-completeness probe (Half A wires its own
// populated double; Half B's prod-wiring test proves the REAL production
// annotator is installed by mux.go). The production concrete lives in
// internal/tools/annotate.
type probeAnnotator struct{}

func (probeAnnotator) OAuthStatus(context.Context, identity.Identity, string) prototypes.ToolOAuthStatus {
	return prototypes.ToolOAuthRequired
}
func (probeAnnotator) ApprovalPolicy(context.Context, identity.Identity, string) prototypes.ToolApprovalPolicy {
	return prototypes.ToolApprovalGated
}
func (probeAnnotator) LastUsedAt(context.Context, identity.Identity, string) time.Time {
	return time.Unix(1_700_000_000, 0).UTC()
}
func (probeAnnotator) Metrics(_ context.Context, _ identity.Identity, _ string, w prototypes.ToolMetricsWindow) prototypes.ToolMetrics {
	return prototypes.ToolMetrics{Window: w, Status: prototypes.ToolStatusHealthy}
}
func (probeAnnotator) ContentStats(context.Context, identity.Identity, string) prototypes.ToolContentStats {
	return prototypes.ToolContentStats{Histogram: []prototypes.ToolContentBucket{}}
}
func (probeAnnotator) DisplayModes(context.Context, identity.Identity, string) map[string]string {
	return map[string]string{}
}

// init self-registers the tools projection surface into the
// projection-completeness gate (§4.4). With the production Annotator wired
// (internal/tools/annotate, installed by mux.go's WithAnnotator), the
// annotator-backed OAuth-status / approval-policy / last-used facets carry
// REAL data. The probe runs the production row projection WITH the annotator
// wired so `last_used_at` is truly assigned — a regression dropping the
// assignment (or the wiring) fails the gate (Half A here; Half B in the
// BuildMux prod-wiring test).
func init() {
	projectioncheck.Register(projectioncheck.ProjectionContract{
		Surface: "tools",
		// Probe runs the PRODUCTION row projection over a fully-populated tool
		// WITH the annotator wired (the production reality after the annotator
		// landed), returning the projected Tool row the gate reflects. The
		// probe annotator populates the annotator-backed fields so the gate
		// asserts they are assigned — never left at their zero value.
		Probe: func() any {
			p, err := NewCatalogProjector(tools.NewCatalog(), WithAnnotator(probeAnnotator{}))
			if err != nil {
				panic("projectioncheck tools probe: " + err.Error())
			}
			t := tools.Tool{
				Name:        "probe-tool",
				Description: "gate probe tool",
				Transport:   tools.TransportMCP,    // → scope "agent"
				SideEffects: tools.SideEffectWrite, // → reliability "guarded"
			}
			return p.projectRow(context.Background(), identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, t)
		},
		// Every axis tools.list filter/search/aggregate operates over
		// (filter.go). `name` + `version` are the free-text search axis;
		// scope / transport / reliability_tier are always-assigned facets;
		// oauth_status / approval_policy / last_used_at are the annotator-backed
		// fields the wired annotator populates (the OAuth / approval facets and
		// the Active aggregate ride them).
		OperatedFields: []string{"name", "version", "scope", "transport", "reliability_tier", "oauth_status", "approval_policy", "last_used_at"},
		HonestOmissions: map[string]string{
			// version is part of the Name+Version free-text search axis but no
			// V1 transport carries a tool version: the runtime tools.Tool
			// descriptor has no version field and the Annotator seam surfaces
			// none. The Name half of the search axis works; the version half is
			// honestly name-only (representable absence, never a fabricated
			// version). Over-rejecting the combined search would break
			// working name search, so the search axis is not loud-rejected
			// (unlike the dedicated oauth/approval facet filters).
			"version": "no V1 transport carries a tool version (tools.Tool has no version field); the Name half of the search axis works, the version half is honestly name-only",
		},
		ProdWiringTest: "TestProdWiring_ToolsAnnotatorThroughBuildMux",
	})
}
