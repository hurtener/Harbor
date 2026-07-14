package protocol_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	toolsprotocol "github.com/hurtener/Harbor/internal/tools/protocol"
)

// TestCatalogProjector_AnnotatorUnwiredIsHonest proves the honest-degradation
// path a catalog stack that did NOT wire the Annotator still ships (a headless
// read-only build): AnnotationsAvailable reports false, the dedicated
// annotator-backed facet filters LOUD-REJECT, and the response-riding
// aggregates carry the partial marker instead of a fabricated 0 (D-313). The
// production build wires the annotator (internal/tools/annotate); the
// BuildMux-driven Half-B prod-wiring test
// (TestProdWiring_ToolsAnnotatorThroughBuildMux, in internal/runtime/serve)
// proves a dropped WithAnnotator regresses to exactly this loud-reject, which
// is why THAT test — not this one — is named by the projection contract.
func TestCatalogProjector_AnnotatorUnwiredIsHonest(t *testing.T) {
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

// TestCatalogProjector_InMemoryOverride_IsSessionScoped guards the exact code
// line that carried the shipped 177 cross-session bleed: the in-memory
// approval-policy FALLBACK override (`overrideKey`, used when NO persisting
// annotator is wired). A `tools.set_approval_policy` for session A must NOT be
// observable in session B's projection (same tenant/user, different session).
// The bug this pins was `overrides` keyed by tool ID alone; a regression back
// to tool-ID-only keying makes this test fail. The persisting-annotator path
// is covered separately (the integration test) — it BYPASSES this map, so this
// bare-projector test is the only guard on the fallback line.
func TestCatalogProjector_InMemoryOverride_IsSessionScoped(t *testing.T) {
	// A BARE projector: no annotator wired, so SetApprovalPolicy records the
	// in-memory fallback override (approvalPersists() == false).
	proj, err := toolsprotocol.NewCatalogProjector(newTestCatalog(t))
	if err != nil {
		t.Fatalf("NewCatalogProjector: %v", err)
	}
	if proj.AnnotationsAvailable() {
		t.Fatal("test setup wrong: annotator wired — this test must exercise the in-memory fallback")
	}
	ctx := context.Background()
	idA := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s-A"}
	idB := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s-B"}

	if err := proj.SetApprovalPolicy(ctx, idA, "beta_http", prototypes.ToolApprovalGated); err != nil {
		t.Fatalf("SetApprovalPolicy(session A): %v", err)
	}
	// Session A observes its own override (never a silent no-op).
	rowA, err := proj.GetTool(ctx, idA, "beta_http")
	if err != nil {
		t.Fatalf("GetTool(session A): %v", err)
	}
	if rowA.ApprovalPolicy != prototypes.ToolApprovalGated {
		t.Fatalf("session A ApprovalPolicy = %q, want gated (own override)", rowA.ApprovalPolicy)
	}
	// Session B must NOT see session A's override — it reads the default auto.
	rowB, err := proj.GetTool(ctx, idB, "beta_http")
	if err != nil {
		t.Fatalf("GetTool(session B): %v", err)
	}
	if rowB.ApprovalPolicy != prototypes.ToolApprovalAuto {
		t.Fatalf("session B ApprovalPolicy = %q, want auto — cross-session bleed via the in-memory override (the shipped 177 bug)", rowB.ApprovalPolicy)
	}
}
