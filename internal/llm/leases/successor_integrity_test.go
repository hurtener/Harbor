package leases

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

func TestResolveSuccessor_RejectsCorruptCanonicalBytesAndFingerprint(t *testing.T) {
	for _, corrupt := range []func(*leaseState){
		func(lease *leaseState) { lease.GrantCanonical = json.RawMessage(`{"unknown":true}`) },
		func(lease *leaseState) { lease.GrantFingerprint = "corrupt-fingerprint" },
	} {
		st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
		if err != nil {
			t.Fatal(err)
		}
		mgr, err := New(st, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		root, successor := integrityGrantPair()
		if err := mgr.ApplySuccessor(context.Background(), root, successor); err != nil {
			t.Fatal(err)
		}
		q := identity.InternalCoordinationQuadruple()
		kind := leasePrefix + root.Lease.LeaseID
		rec, lease, err := mgr.loadLease(context.Background(), q, kind)
		if err != nil {
			t.Fatal(err)
		}
		corrupt(&lease)
		body, err := json.Marshal(lease)
		if err != nil {
			t.Fatal(err)
		}
		next := state.NewInternalRecord(state.NewEventID(), q, kind, body)
		if err := st.SaveIf(context.Background(), []state.SlotExpectation{state.InternalSlotExpectation(q, kind, rec.ID)}, next); err != nil {
			t.Fatal(err)
		}
		if _, _, err := mgr.ResolveSuccessor(context.Background(), root); !errors.Is(err, ErrAttemptConflict) {
			t.Fatalf("corrupt successor resolve=%v", err)
		}
		_ = st.Close(context.Background())
	}
}

func integrityGrantPair() (llm.ExternalGrant, llm.ExternalGrant) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	root := llm.ExternalGrant{
		Version: llm.ExternalGrantVersionAgentBound, KeyID: "key-a", Audience: "harbor-runtime", GrantID: "grant-integrity",
		RouteMode: llm.ExternalGrantRouteRuntimeDefault, OrganizationID: "org-a", RuntimeID: "runtime-a", AgentID: "agent-a",
		TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a", LogicalRunID: "run-a", LogicalCallID: "call-a", AttemptNonce: "nonce-a",
		PolicyGeneration: 1, MaxReasoning: llm.ReasoningMedium, MaxOutputTokens: 10,
		Lease:    llm.ComputeLease{LeaseID: "lease-integrity", Epoch: 1, TokenUnits: 10, ExpiresAt: now.Add(time.Minute)},
		IssuedAt: now, ExpiresAt: now.Add(30 * time.Second), Signature: "signature-a",
	}
	successor := root
	successor.KeyID, successor.Signature, successor.Lease.Epoch = "key-b", "signature-b", 2
	successor.IssuedAt, successor.ExpiresAt, successor.Lease.ExpiresAt = now.Add(time.Second), now.Add(31*time.Second), now.Add(61*time.Second)
	return root, successor
}
