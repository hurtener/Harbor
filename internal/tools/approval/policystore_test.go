package approval_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools/approval"
)

func mkPolicyStore(t *testing.T) approval.PolicyStore {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{})
	if err != nil {
		t.Fatalf("state inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	ps, err := approval.NewStatePolicyStore(st)
	if err != nil {
		t.Fatalf("NewStatePolicyStore: %v", err)
	}
	return ps
}

func TestStatePolicyStore_NilStore_FailLoud(t *testing.T) {
	if _, err := approval.NewStatePolicyStore(nil); err == nil {
		t.Fatal("nil StateStore should fail loud")
	}
}

func TestStatePolicyStore_DefaultIsAuto(t *testing.T) {
	ps := mkPolicyStore(t)
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	got, err := ps.Policy(context.Background(), id, "never-set")
	if err != nil {
		t.Fatalf("Policy: %v", err)
	}
	if got != prototypes.ToolApprovalAuto {
		t.Errorf("unset policy = %q, want auto (the honest default)", got)
	}
}

func TestStatePolicyStore_SetGet_RoundTrip(t *testing.T) {
	ps := mkPolicyStore(t)
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	for _, p := range []prototypes.ToolApprovalPolicy{
		prototypes.ToolApprovalGated, prototypes.ToolApprovalDenied, prototypes.ToolApprovalAuto,
	} {
		if err := ps.SetPolicy(context.Background(), id, "alpha", p); err != nil {
			t.Fatalf("SetPolicy %q: %v", p, err)
		}
		got, err := ps.Policy(context.Background(), id, "alpha")
		if err != nil {
			t.Fatalf("Policy: %v", err)
		}
		if got != p {
			t.Errorf("policy = %q, want %q", got, p)
		}
	}
}

func TestStatePolicyStore_Isolation(t *testing.T) {
	ps := mkPolicyStore(t)
	sessA := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s-A"}
	sessB := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s-B"}
	if err := ps.SetPolicy(context.Background(), sessA, "alpha", prototypes.ToolApprovalGated); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	// Session B must not observe session A's pinned posture.
	got, err := ps.Policy(context.Background(), sessB, "alpha")
	if err != nil {
		t.Fatalf("Policy: %v", err)
	}
	if got != prototypes.ToolApprovalAuto {
		t.Errorf("session B policy = %q, want auto — cross-session bleed", got)
	}
}

func TestStatePolicyStore_Validation(t *testing.T) {
	ps := mkPolicyStore(t)
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	if err := ps.SetPolicy(context.Background(), id, "alpha", prototypes.ToolApprovalPolicy("bogus")); err == nil {
		t.Fatal("invalid policy should fail loud")
	}
	if err := ps.SetPolicy(context.Background(), id, "", prototypes.ToolApprovalGated); err == nil {
		t.Fatal("empty tool id should fail loud")
	}
	bad := identity.Identity{TenantID: "t"} // incomplete triple
	if err := ps.SetPolicy(context.Background(), bad, "alpha", prototypes.ToolApprovalGated); err == nil {
		t.Fatal("incomplete identity should fail closed")
	}
	if _, err := ps.Policy(context.Background(), bad, "alpha"); err == nil {
		t.Fatal("incomplete identity should fail closed on read")
	}
}
