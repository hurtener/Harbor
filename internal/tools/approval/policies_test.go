package approval

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

func TestAlwaysDenyPolicy(t *testing.T) {
	p := AlwaysDenyPolicy{}
	req := &ApprovalRequest{Tool: tools.Tool{Name: "t"}}
	required, reason, err := p.ShouldApprove(context.Background(), req)
	if err != nil {
		t.Fatalf("ShouldApprove: %v", err)
	}
	if !required {
		t.Fatal("Required: want true")
	}
	if reason == "" {
		t.Fatal("default reason empty")
	}
}

func TestAlwaysDenyPolicy_CustomReason(t *testing.T) {
	p := AlwaysDenyPolicy{Reason: "policy: deny-everything"}
	req := &ApprovalRequest{Tool: tools.Tool{Name: "t"}}
	_, reason, err := p.ShouldApprove(context.Background(), req)
	if err != nil {
		t.Fatalf("ShouldApprove: %v", err)
	}
	if reason != "policy: deny-everything" {
		t.Fatalf("Reason: got %q", reason)
	}
}

func TestAlwaysApprovePolicy(t *testing.T) {
	p := AlwaysApprovePolicy{}
	req := &ApprovalRequest{Tool: tools.Tool{Name: "t"}}
	required, reason, err := p.ShouldApprove(context.Background(), req)
	if err != nil {
		t.Fatalf("ShouldApprove: %v", err)
	}
	if required {
		t.Fatal("Required: want false")
	}
	if reason != "" {
		t.Fatalf("Reason: got %q want empty", reason)
	}
}

func TestTaggedPolicy_RequiresOnMatch(t *testing.T) {
	p := TaggedPolicy{RequireTags: []string{"sensitive", "write:prod"}}
	req := &ApprovalRequest{
		Tool:     tools.Tool{Name: "t"},
		Tags:     []string{"sensitive"},
		Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
	}
	required, reason, err := p.ShouldApprove(context.Background(), req)
	if err != nil {
		t.Fatalf("ShouldApprove: %v", err)
	}
	if !required {
		t.Fatal("Required: want true (sensitive tag matched)")
	}
	if reason == "" {
		t.Fatal("default reason empty")
	}
}

func TestTaggedPolicy_NoMatch(t *testing.T) {
	p := TaggedPolicy{RequireTags: []string{"sensitive"}}
	req := &ApprovalRequest{
		Tool: tools.Tool{Name: "t"},
		Tags: []string{"read", "safe"},
	}
	required, _, err := p.ShouldApprove(context.Background(), req)
	if err != nil {
		t.Fatalf("ShouldApprove: %v", err)
	}
	if required {
		t.Fatal("Required: want false (no tag matched)")
	}
}

func TestTaggedPolicy_EmptyRequireTags_NeverApproves(t *testing.T) {
	p := TaggedPolicy{}
	req := &ApprovalRequest{
		Tool: tools.Tool{Name: "t"},
		Tags: []string{"sensitive"},
	}
	required, _, err := p.ShouldApprove(context.Background(), req)
	if err != nil {
		t.Fatalf("ShouldApprove: %v", err)
	}
	if required {
		t.Fatal("Required: want false (RequireTags empty means short-circuit)")
	}
}

func TestTaggedPolicy_NilRequest_DefensiveNoOp(t *testing.T) {
	p := TaggedPolicy{RequireTags: []string{"sensitive"}}
	required, _, err := p.ShouldApprove(context.Background(), nil)
	if err != nil {
		t.Fatalf("ShouldApprove(nil): %v", err)
	}
	if required {
		t.Fatal("Required(nil): want false (defensive)")
	}
}

func TestTaggedPolicy_CustomReason(t *testing.T) {
	p := TaggedPolicy{
		RequireTags: []string{"sensitive"},
		Reason:      "policy: sensitive-write",
	}
	req := &ApprovalRequest{
		Tool: tools.Tool{Name: "t"},
		Tags: []string{"sensitive"},
	}
	_, reason, err := p.ShouldApprove(context.Background(), req)
	if err != nil {
		t.Fatalf("ShouldApprove: %v", err)
	}
	if reason != "policy: sensitive-write" {
		t.Fatalf("Reason: got %q", reason)
	}
}
