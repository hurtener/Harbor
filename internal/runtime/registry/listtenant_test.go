package registry_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/runtime/registry"
)

// TestListTenant_EnumeratesAcrossSessions — the admin-widened fleet read
// returns every registered agent across ALL (user, session) scopes of one
// tenant, and NEVER an agent from another tenant. The registry persists
// per-identity with no cross-identity index; ListTenant reads the
// StateStore maintenance-scan surface and filters to the named tenant.
func TestListTenant_EnumeratesAcrossSessions(t *testing.T) {
	reg, _, _ := newTestRegistry(t)

	// Tenant A: 2 users × 2 sessions, one agent each = 4 agents.
	aScopes := [][3]string{
		{"tenant-A", "u1", "s1"},
		{"tenant-A", "u1", "s2"},
		{"tenant-A", "u2", "s3"},
		{"tenant-A", "u2", "s4"},
	}
	wantA := map[string]struct{}{}
	for i, sc := range aScopes {
		ctx := identityCtx(t, sc[0], sc[1], sc[2])
		rec, err := reg.Register(ctx, "agent-key", sampleConfig(), registry.RegisterOptions{})
		if err != nil {
			t.Fatalf("Register(%d): %v", i, err)
		}
		wantA[rec.AgentID] = struct{}{}
	}
	// Tenant B: one agent that must NEVER surface in tenant-A's fleet read.
	bCtx := identityCtx(t, "tenant-B", "u9", "s9")
	bRec, err := reg.Register(bCtx, "agent-key", sampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register(tenant-B): %v", err)
	}

	got, err := reg.ListTenant(context.Background(), "tenant-A")
	if err != nil {
		t.Fatalf("ListTenant(tenant-A): %v", err)
	}
	if len(got) != len(wantA) {
		t.Fatalf("ListTenant(tenant-A) count=%d, want %d", len(got), len(wantA))
	}
	for _, rec := range got {
		if rec.Identity.TenantID != "tenant-A" {
			t.Errorf("fleet read leaked tenant %q (agent %q)", rec.Identity.TenantID, rec.AgentID)
		}
		if _, ok := wantA[rec.AgentID]; !ok {
			t.Errorf("unexpected agent %q in tenant-A fleet read", rec.AgentID)
		}
		if rec.AgentID == bRec.AgentID {
			t.Errorf("tenant-B agent %q leaked into tenant-A fleet read", bRec.AgentID)
		}
	}
}

// TestListTenant_EmptyTenant_Rejected — a fleet read with no named tenant
// fails loud rather than dumping the whole store.
func TestListTenant_EmptyTenant_Rejected(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	if _, err := reg.ListTenant(context.Background(), ""); !errors.Is(err, registry.ErrInvalidConfig) {
		t.Errorf("ListTenant(\"\") err=%v, want ErrInvalidConfig", err)
	}
}

// TestListTenant_ClosedRegistry — after Close the fleet read fails loud.
func TestListTenant_ClosedRegistry(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	if err := reg.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := reg.ListTenant(context.Background(), "tenant-A"); !errors.Is(err, registry.ErrRegistryClosed) {
		t.Errorf("ListTenant after Close err=%v, want ErrRegistryClosed", err)
	}
}

// TestListTenant_EmptyStore_NoAgents — a tenant with no registrations
// returns an empty slice, never an error.
func TestListTenant_EmptyStore_NoAgents(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	got, err := reg.ListTenant(context.Background(), "tenant-nobody")
	if err != nil {
		t.Fatalf("ListTenant(empty): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListTenant(empty) = %+v, want empty", got)
	}
}
