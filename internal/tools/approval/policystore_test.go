package approval_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/state"
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

func TestStatePolicyStore_Policies_IsolatedAndDetached(t *testing.T) {
	ps := mkPolicyStore(t)
	ctx := context.Background()
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	want := map[string]prototypes.ToolApprovalPolicy{"alpha": prototypes.ToolApprovalGated, "beta": prototypes.ToolApprovalDenied}
	for tool, policy := range want {
		if err := ps.SetPolicy(ctx, id, tool, policy); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ps.Policies(ctx, id)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Policies = %v, %v; want %v", got, err, want)
	}
	got["alpha"] = prototypes.ToolApprovalDenied
	if policy, err := ps.Policy(ctx, id, "alpha"); err != nil || policy != prototypes.ToolApprovalGated {
		t.Fatalf("mutating policies changed storage: %s, %v", policy, err)
	}
	for _, foreign := range []identity.Identity{
		{TenantID: "other", UserID: "u", SessionID: "s"},
		{TenantID: "t", UserID: "other", SessionID: "s"},
		{TenantID: "t", UserID: "u", SessionID: "other"},
	} {
		if got, err := ps.Policies(ctx, foreign); err != nil || len(got) != 0 {
			t.Fatalf("foreign policies = %v, %v", got, err)
		}
	}
}

func TestStatePolicyStore_Policies_InvalidAndCancelled_FailClosed(t *testing.T) {
	ps := mkPolicyStore(t)
	if _, err := ps.Policies(context.Background(), identity.Identity{TenantID: "t"}); err == nil {
		t.Fatal("incomplete identity accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ps.Policies(ctx, identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled policies error = %v", err)
	}
}

func TestStatePolicyStore_Policies_CorruptOverrideSkipped(t *testing.T) {
	for _, data := range []string{`{`, `{"policy":"invalid"}`} {
		t.Run(data, func(t *testing.T) {
			st, err := stateinmem.New(config.StateConfig{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close(context.Background()) })
			ps, err := approval.NewStatePolicyStore(st)
			if err != nil {
				t.Fatal(err)
			}
			id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
			if err := st.Save(context.Background(), state.StateRecord{
				ID: state.NewEventID(), Identity: identity.Quadruple{Identity: id},
				Kind: "tools.approval.policy.alpha", Bytes: []byte(data),
			}); err != nil {
				t.Fatal(err)
			}
			if err := ps.SetPolicy(context.Background(), id, "beta", prototypes.ToolApprovalGated); err != nil {
				t.Fatal(err)
			}
			policies, err := ps.Policies(context.Background(), id)
			if err != nil {
				t.Fatalf("one corrupt override failed the whole read: %v", err)
			}
			if _, ok := policies["alpha"]; ok || policies["beta"] != prototypes.ToolApprovalGated || len(policies) != 1 {
				t.Fatalf("policies = %v, want only beta=gated (corrupt alpha skipped)", policies)
			}
			if single, err := ps.Policy(context.Background(), id, "alpha"); err == nil {
				t.Fatalf("single-tool read of the corrupt record = %v, want an error the annotator maps to auto", single)
			}
		})
	}
}
