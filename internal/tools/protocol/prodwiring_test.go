package protocol_test

import (
	"context"
	"errors"
	"testing"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	toolsprotocol "github.com/hurtener/Harbor/internal/tools/protocol"
)

// TestProdWiring_ToolsProjectorAnnotatorUnwiredIsHonest is the tools
// prod-wiring test named by the projection-completeness contract (Half B).
// It proves the V1 PRODUCTION build — a CatalogProjector with NO Annotator
// wired (the concrete lands in Phase 178) — is honest: AnnotationsAvailable
// reports false, the dedicated annotator-backed facet filters LOUD-REJECT,
// and the response-riding aggregates carry the partial marker instead of a
// fabricated 0 (D-313).
func TestProdWiring_ToolsProjectorAnnotatorUnwiredIsHonest(t *testing.T) {
	proj, err := toolsprotocol.NewCatalogProjector(newTestCatalog(t))
	if err != nil {
		t.Fatalf("NewCatalogProjector: %v", err)
	}
	if proj.AnnotationsAvailable() {
		t.Fatal("AnnotationsAvailable() = true on a build with no annotator wired")
	}
	svc, err := toolsprotocol.NewService(proj)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// (a) The dedicated annotator-backed facet filters loud-reject.
	_, err = svc.List(context.Background(), prototypes.ToolListRequest{
		Identity: validID(),
		Filter:   prototypes.ToolFilter{OAuthStatuses: []prototypes.ToolOAuthStatus{prototypes.ToolOAuthRequired}},
	})
	if !errors.Is(err, toolsprotocol.ErrInvalidRequest) {
		t.Fatalf("oauth_statuses facet unwired: err = %v, want ErrInvalidRequest (loud-reject)", err)
	}
	_, err = svc.List(context.Background(), prototypes.ToolListRequest{
		Identity: validID(),
		Filter:   prototypes.ToolFilter{ApprovalPolicies: []prototypes.ToolApprovalPolicy{prototypes.ToolApprovalGated}},
	})
	if !errors.Is(err, toolsprotocol.ErrInvalidRequest) {
		t.Fatalf("approval_policies facet unwired: err = %v, want ErrInvalidRequest (loud-reject)", err)
	}

	// (b) The response-riding aggregates carry the partial marker and NEVER a
	// real-looking non-total zero — Total is authoritative.
	resp, err := svc.List(context.Background(), prototypes.ToolListRequest{Identity: validID()})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !resp.AggregatesPartial {
		t.Fatal("aggregates_partial = false on an unwired build — the Console would render a fabricated 0")
	}
	if resp.Aggregates.Total != 3 {
		t.Fatalf("Aggregates.Total = %d, want 3 (Total stays authoritative)", resp.Aggregates.Total)
	}
	if resp.Aggregates.Active != 0 || resp.Aggregates.PendingApproval != 0 || resp.Aggregates.AwaitingOAuth != 0 {
		t.Fatal("annotator-backed aggregates should be zeroed AND marked partial (Console renders 'unavailable'), never real-looking values")
	}
}
