package stream_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	governanceprotocol "github.com/hurtener/Harbor/internal/runtime/governance/protocol"
)

// fakePostureWriteStore is a test double for the set-posture store seam. It
// records the last actor + spec and returns a canned snapshot / error.
type fakePostureWriteStore struct {
	gotActor          identity.Quadruple
	gotSpec           governance.SetPostureSpec
	retSnap           governance.Snapshot
	retErr            error
	enforcementActive bool
}

func (f *fakePostureWriteStore) Set(_ context.Context, actor identity.Quadruple, spec governance.SetPostureSpec) (governance.Snapshot, error) {
	f.gotActor = actor
	f.gotSpec = spec
	if f.retErr != nil {
		return governance.Snapshot{}, f.retErr
	}
	return f.retSnap, nil
}

func (f *fakePostureWriteStore) EnforcementActive() bool { return f.enforcementActive }

func newGovHandlerWithPosture(t *testing.T, store governanceprotocol.PostureWriteStore) http.Handler {
	t.Helper()
	// The tenant-override service is a mandatory dependency of the handler;
	// wire a no-op override store alongside the posture-write service.
	ovSvc, err := governanceprotocol.NewService(&fakeOverrideStore{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	pwSvc, err := governanceprotocol.NewPostureWriteService(store)
	if err != nil {
		t.Fatalf("NewPostureWriteService: %v", err)
	}
	h, err := stream.NewGovernanceHandler(ovSvc, stream.WithGovernancePostureWrite(pwSvc))
	if err != nil {
		t.Fatalf("NewGovernanceHandler: %v", err)
	}
	return h
}

// TestGovernanceHandler_SetPosture_AdminRoundTrip asserts an admin write
// reaches the store with the mapped spec + the verified actor, and the
// response echoes the persisted posture.
func TestGovernanceHandler_SetPosture_AdminRoundTrip(t *testing.T) {
	store := &fakePostureWriteStore{
		retSnap: governance.Snapshot{
			DefaultTier:   "free",
			IdentityTiers: map[string]governance.TierConfig{"free": {BudgetCeilingUSD: 0.5}},
		},
	}
	h := newGovHandlerWithPosture(t, store)
	body := `{"default_tier":"free","identity_tiers":{"free":{"budget_ceiling_usd":0.5,"max_tokens":1000,"rate_limit":{"capacity":0,"refill_tokens":0,"refill_interval_ms":0}}}}`
	code, resp := govReq(t, h, "set_posture", body, adminID(), []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", code, resp)
	}
	if store.gotActor.TenantID != "t1" || store.gotActor.UserID != "admin" {
		t.Errorf("actor = %+v, want verified t1/admin/s1", store.gotActor)
	}
	if store.gotSpec.DefaultTier != "free" || store.gotSpec.IdentityTiers["free"].BudgetCeilingUSD != 0.5 {
		t.Errorf("spec mismatch: %+v", store.gotSpec)
	}
	var out prototypes.GovernanceSetPostureResponse
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if out.DefaultTier != "free" || out.IdentityTiers["free"].BudgetCeilingUSD != 0.5 {
		t.Errorf("echoed posture mismatch: %+v", out)
	}
	if out.ProtocolVersion == "" {
		t.Error("response missing protocol_version")
	}
}

// TestGovernanceHandler_SetPosture_PendingRestartFlag pins W1: a
// fully-latent runtime (no wrapper composed) returns
// enforcement_pending_restart:true — the write persisted but won't enforce
// until restart; a composed runtime returns false.
func TestGovernanceHandler_SetPosture_PendingRestartFlag(t *testing.T) {
	body := `{"default_tier":"free","identity_tiers":{"free":{"budget_ceiling_usd":0.5,"max_tokens":0,"rate_limit":{"capacity":0,"refill_tokens":0,"refill_interval_ms":0}}}}`
	snap := governance.Snapshot{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {BudgetCeilingUSD: 0.5}},
	}

	// Latent runtime (enforcementActive=false) → pending:true.
	latent := &fakePostureWriteStore{retSnap: snap, enforcementActive: false}
	code, resp := govReq(t, newGovHandlerWithPosture(t, latent), "set_posture", body, adminID(), []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusOK {
		t.Fatalf("latent status = %d, body=%s", code, resp)
	}
	var out prototypes.GovernanceSetPostureResponse
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.EnforcementPendingRestart {
		t.Error("latent runtime write must report enforcement_pending_restart:true")
	}

	// Composed runtime (enforcementActive=true) → pending:false.
	active := &fakePostureWriteStore{retSnap: snap, enforcementActive: true}
	code, resp = govReq(t, newGovHandlerWithPosture(t, active), "set_posture", body, adminID(), []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusOK {
		t.Fatalf("active status = %d, body=%s", code, resp)
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.EnforcementPendingRestart {
		t.Error("composed runtime write must report enforcement_pending_restart:false")
	}
}

// TestGovernanceHandler_SetPosture_FleetNotAdminForbidden proves a caller
// bearing the read's second gate (console:fleet) but NOT admin is rejected
// with CodeScopeMismatch (403) — a leaked read-only fleet token cannot widen
// a budget (D-066 / D-079). The store is never touched.
func TestGovernanceHandler_SetPosture_FleetNotAdminForbidden(t *testing.T) {
	store := &fakePostureWriteStore{}
	h := newGovHandlerWithPosture(t, store)
	body := `{"default_tier":"free","identity_tiers":{}}`
	code, resp := govReq(t, h, "set_posture", body, adminID(), []auth.Scope{auth.ScopeConsoleFleet}) // fleet, not admin
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", code, resp)
	}
	if errCode(t, resp) != protoerrors.CodeScopeMismatch {
		t.Errorf("code = %q, want %q", errCode(t, resp), protoerrors.CodeScopeMismatch)
	}
	if store.gotSpec.DefaultTier != "" || store.gotSpec.IdentityTiers != nil {
		t.Error("fleet-not-admin write must not reach the store")
	}
}

// TestGovernanceHandler_SetPosture_WideningRejected asserts a store-level
// ErrPolicyWidening maps to CodeInvalidRequest (400) — the fail-closed path.
func TestGovernanceHandler_SetPosture_WideningRejected(t *testing.T) {
	store := &fakePostureWriteStore{retErr: governance.ErrPolicyWidening}
	h := newGovHandlerWithPosture(t, store)
	body := `{"default_tier":"team","identity_tiers":{"team":{"budget_ceiling_usd":5}}}`
	code, resp := govReq(t, h, "set_posture", body, adminID(), []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", code, resp)
	}
	if errCode(t, resp) != protoerrors.CodeInvalidRequest {
		t.Errorf("code = %q, want %q", errCode(t, resp), protoerrors.CodeInvalidRequest)
	}
}

// TestGovernanceHandler_SetPosture_NoIdentity asserts a missing identity
// triple fails closed with 401.
func TestGovernanceHandler_SetPosture_NoIdentity(t *testing.T) {
	h := newGovHandlerWithPosture(t, &fakePostureWriteStore{})
	code, resp := govReq(t, h, "set_posture", `{"default_tier":"free","identity_tiers":{}}`, nil, []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", code, resp)
	}
	if errCode(t, resp) != protoerrors.CodeIdentityRequired {
		t.Errorf("code = %q, want %q", errCode(t, resp), protoerrors.CodeIdentityRequired)
	}
}

// TestGovernanceHandler_SetPosture_NotWired asserts the route 501s when no
// posture-write service is wired (the partial-build convention).
func TestGovernanceHandler_SetPosture_NotWired(t *testing.T) {
	h := newGovHandler(t, &fakeOverrideStore{}) // no WithGovernancePostureWrite
	code, resp := govReq(t, h, "set_posture", `{"default_tier":"free","identity_tiers":{}}`, adminID(), []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", code, resp)
	}
}
