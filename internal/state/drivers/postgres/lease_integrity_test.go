package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/leases"
	"github.com/hurtener/Harbor/internal/state/drivers/postgres"
)

// TestLeaseIntegrity_PostgresAcceptance runs the execution-edge lease
// accounting invariants against the real PostgreSQL StateStore used by the
// hosted state-postgres job. It is DSN-gated only for local development.
func TestLeaseIntegrity_PostgresAcceptance(t *testing.T) {
	dsn := freshSchema(t, requireDSN(t))
	st, err := postgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	now := time.Now().UTC()
	current := now
	mgr, err := leases.New(st, func() time.Time { return current })
	if err != nil {
		t.Fatal(err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a"}, RunID: "run-a"}
	req := llm.LeaseReservationRequest{
		AttemptID: "pg-attempt", LogicalCallID: "pg-attempt", AttemptNonce: "nonce-pg-attempt",
		GrantID: "pg-grant", LeaseID: "pg-shared-lease", OrganizationID: "org-a", RuntimeID: "runtime-a", AgentID: "agent-a",
		Epoch: 1, Capacity: 10, Units: 5, ExpiresAt: now.Add(time.Minute), Identity: q,
	}
	if _, err := mgr.Reserve(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	collision := req
	collision.AttemptID, collision.LogicalCallID, collision.AttemptNonce = "pg-other", "pg-other", "nonce-pg-other"
	collision.OrganizationID = "org-b"
	if _, err := mgr.Reserve(context.Background(), collision); !errors.Is(err, leases.ErrAttemptConflict) {
		t.Fatalf("cross-organization LeaseID collision = %v, want ErrAttemptConflict", err)
	}
	receipt := postgresLeaseReceipt(req, q, now, "canceled", 7)
	if err := mgr.Settle(context.Background(), llm.LeaseSettlement{AttemptID: req.AttemptID, LogicalCallID: req.LogicalCallID, AttemptNonce: req.AttemptNonce, Receipt: receipt, Units: 7, Now: now}); err != nil {
		t.Fatalf("canceled overshoot settlement: %v", err)
	}
	pending, err := mgr.PendingReceipts(context.Background(), 10)
	if err != nil || len(pending) != 1 || pending[0].TotalTokens != 7 || pending[0].Status != "canceled" {
		t.Fatalf("pending canceled usage = %+v, %v", pending, err)
	}

	crash := req
	crash.AttemptID, crash.LogicalCallID, crash.AttemptNonce = "pg-crash", "pg-crash", "nonce-pg-crash"
	crash.LeaseID, crash.Capacity, crash.Units, crash.ExpiresAt = "pg-crash-lease", 4, 4, now.Add(time.Second)
	if _, err := mgr.Reserve(context.Background(), crash); err != nil {
		t.Fatal(err)
	}
	if err := mgr.TopUp(context.Background(), leases.TopUpRequest{
		GrantID: crash.GrantID, LeaseID: crash.LeaseID, OrganizationID: crash.OrganizationID,
		RuntimeID: crash.RuntimeID, AgentID: crash.AgentID, Identity: crash.Identity,
		Epoch: 2, Capacity: 8, ExpiresAt: now.Add(2 * time.Minute),
	}); err != nil {
		t.Fatalf("top up around in-flight attempt: %v", err)
	}
	current = now.Add(2 * time.Second)
	replayed, err := mgr.Reserve(context.Background(), crash)
	if err != nil || !replayed.Existing || replayed.Status != "expired" {
		t.Fatalf("crash replay = %+v, %v, want existing expired", replayed, err)
	}
	late := postgresLeaseReceipt(crash, q, current, "error", 2)
	settlement := llm.LeaseSettlement{AttemptID: crash.AttemptID, LogicalCallID: crash.LogicalCallID, AttemptNonce: crash.AttemptNonce, Receipt: late, Units: 2, Now: current}
	if err := mgr.Settle(context.Background(), settlement); err != nil {
		t.Fatalf("late provider usage settlement: %v", err)
	}
	if err := mgr.Settle(context.Background(), settlement); err != nil {
		t.Fatalf("late provider usage replay: %v", err)
	}
}

func postgresLeaseReceipt(req llm.LeaseReservationRequest, q identity.Quadruple, now time.Time, status string, total int) llm.AttemptUsageReceipt {
	receipt := llm.AttemptUsageReceipt{
		ReceiptID: req.AttemptID, GrantID: req.GrantID, LogicalCallID: req.LogicalCallID, AttemptNonce: req.AttemptNonce,
		OrganizationID: req.OrganizationID, RuntimeID: req.RuntimeID, AgentID: req.AgentID,
		TenantID: q.TenantID, UserID: q.UserID, SessionID: q.SessionID, LogicalRunID: q.RunID,
		Provider: "openai", ProviderModelID: "model", ProviderConnectionID: "connection", ProviderConnectionGeneration: 1,
		RouteID: "route", CredentialAssetGeneration: 1, PolicyGeneration: 1, AttemptNumber: 1,
		Status: status, StartedAt: now, CompletedAt: now.Add(time.Millisecond), IdempotencyKey: req.AttemptID, TotalTokens: total,
	}
	receipt.CanonicalBodyHash, _ = llm.CanonicalAttemptUsageReceiptBodyHash(receipt)
	return receipt
}
