package stream_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	governanceprotocol "github.com/hurtener/Harbor/internal/runtime/governance/protocol"
)

// fakeOverrideStore is a test double for the governance tenant-override
// store seam. It records the last Set and returns canned Get / errors.
type fakeOverrideStore struct {
	setTenant string
	setSpec   governance.TenantOverrideSpec
	setErr    error
	getSpec   governance.TenantOverrideSpec
	getSet    bool
	getErr    error
}

func (f *fakeOverrideStore) Set(_ context.Context, _ identity.Quadruple, tenant string, spec governance.TenantOverrideSpec) error {
	f.setTenant = tenant
	f.setSpec = spec
	return f.setErr
}

func (f *fakeOverrideStore) Get(_ context.Context, _ string) (governance.TenantOverrideSpec, bool, error) {
	return f.getSpec, f.getSet, f.getErr
}

func newGovHandler(t *testing.T, store governanceprotocol.TenantOverrideStore) http.Handler {
	t.Helper()
	svc, err := governanceprotocol.NewService(store)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	h, err := stream.NewGovernanceHandler(svc)
	if err != nil {
		t.Fatalf("NewGovernanceHandler: %v", err)
	}
	return h
}

func govReq(t *testing.T, h http.Handler, route, body string, id *identity.Identity, scopes []auth.Scope) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/governance/"+route, strings.NewReader(body))
	if id != nil {
		req.Header.Set(stream.HeaderTenant, id.TenantID)
		req.Header.Set(stream.HeaderUser, id.UserID)
		req.Header.Set(stream.HeaderSession, id.SessionID)
	}
	if scopes != nil {
		req = req.WithContext(auth.WithScopes(req.Context(), scopes))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func adminID() *identity.Identity {
	return &identity.Identity{TenantID: "t1", UserID: "admin", SessionID: "s1"}
}

func errCode(t *testing.T, body []byte) protoerrors.Code {
	t.Helper()
	var e protoerrors.Error
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return e.Code
}

// TestGovernanceHandler_Set_AdminRoundTrip asserts an admin set reaches the
// store with the mapped spec and returns 200.
func TestGovernanceHandler_Set_AdminRoundTrip(t *testing.T) {
	store := &fakeOverrideStore{}
	h := newGovHandler(t, store)
	body := `{"overrides":{"model":"model-x","temperature":0.4,"extra_instructions":"be terse"}}`
	code, resp := govReq(t, h, "set_tenant_overrides", body, adminID(), []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", code, resp)
	}
	if store.setTenant != "t1" {
		t.Errorf("store.setTenant = %q, want t1", store.setTenant)
	}
	if store.setSpec.Model == nil || *store.setSpec.Model != "model-x" {
		t.Errorf("spec.Model = %v", store.setSpec.Model)
	}
	if store.setSpec.ExtraInstructions == nil || *store.setSpec.ExtraInstructions != "be terse" {
		t.Errorf("spec.ExtraInstructions = %v", store.setSpec.ExtraInstructions)
	}
	var out prototypes.GovernanceSetTenantOverridesResponse
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if out.ProtocolVersion == "" {
		t.Error("response missing protocol_version")
	}
}

// TestGovernanceHandler_Set_NonAdminForbidden asserts a caller without the
// admin scope is rejected with CodeScopeMismatch (403) and the store is
// never touched.
func TestGovernanceHandler_Set_NonAdminForbidden(t *testing.T) {
	store := &fakeOverrideStore{}
	h := newGovHandler(t, store)
	body := `{"overrides":{"model":"model-x"}}`
	code, resp := govReq(t, h, "set_tenant_overrides", body, adminID(), []auth.Scope{}) // no admin scope
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", code, resp)
	}
	if errCode(t, resp) != protoerrors.CodeScopeMismatch {
		t.Errorf("code = %q, want %q", errCode(t, resp), protoerrors.CodeScopeMismatch)
	}
	if store.setTenant != "" {
		t.Error("non-admin set must not reach the store")
	}
}

// TestGovernanceHandler_Set_NoIdentity asserts a missing identity triple
// fails closed with 401.
func TestGovernanceHandler_Set_NoIdentity(t *testing.T) {
	h := newGovHandler(t, &fakeOverrideStore{})
	code, resp := govReq(t, h, "set_tenant_overrides", `{}`, nil, []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", code, resp)
	}
	if errCode(t, resp) != protoerrors.CodeIdentityRequired {
		t.Errorf("code = %q, want %q", errCode(t, resp), protoerrors.CodeIdentityRequired)
	}
}

// TestGovernanceHandler_Set_UnknownModel asserts a store-level
// ErrUnknownModel maps to CodeInvalidRequest (400).
func TestGovernanceHandler_Set_UnknownModel(t *testing.T) {
	store := &fakeOverrideStore{setErr: governance.ErrUnknownModel}
	h := newGovHandler(t, store)
	code, resp := govReq(t, h, "set_tenant_overrides", `{"overrides":{"model":"ghost"}}`, adminID(), []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", code, resp)
	}
	if errCode(t, resp) != protoerrors.CodeInvalidRequest {
		t.Errorf("code = %q, want %q", errCode(t, resp), protoerrors.CodeInvalidRequest)
	}
}

// TestGovernanceHandler_Get_AdminRoundTrip asserts an admin get returns the
// store's record.
func TestGovernanceHandler_Get_AdminRoundTrip(t *testing.T) {
	model := "model-y"
	store := &fakeOverrideStore{getSet: true, getSpec: governance.TenantOverrideSpec{Model: &model}}
	h := newGovHandler(t, store)
	code, resp := govReq(t, h, "get_tenant_overrides", `{}`, adminID(), []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", code, resp)
	}
	var out prototypes.GovernanceGetTenantOverridesResponse
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Set || out.Overrides.Model == nil || *out.Overrides.Model != "model-y" {
		t.Errorf("get response = %+v, want model-y set=true", out)
	}
}

// TestGovernanceHandler_Get_NonAdminForbidden asserts get is admin-gated.
func TestGovernanceHandler_Get_NonAdminForbidden(t *testing.T) {
	h := newGovHandler(t, &fakeOverrideStore{})
	code, _ := govReq(t, h, "get_tenant_overrides", `{}`, adminID(), []auth.Scope{})
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", code)
	}
}

// TestGovernanceHandler_Set_BodyIdentityMismatch asserts a body whose
// identity component differs from the verified (header) identity is
// rejected (401 CodeIdentityRequired) and the store is never touched —
// the defence-in-depth check.
func TestGovernanceHandler_Set_BodyIdentityMismatch(t *testing.T) {
	store := &fakeOverrideStore{}
	h := newGovHandler(t, store)
	// Verified identity is t1/admin/s1; the body claims a different tenant.
	body := `{"identity":{"tenant":"other-tenant","user":"admin","session":"s1"},"overrides":{"model":"model-x"}}`
	code, resp := govReq(t, h, "set_tenant_overrides", body, adminID(), []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", code, resp)
	}
	if errCode(t, resp) != protoerrors.CodeIdentityRequired {
		t.Errorf("code = %q, want %q", errCode(t, resp), protoerrors.CodeIdentityRequired)
	}
	if store.setTenant != "" {
		t.Error("body/verified mismatch must not reach the store")
	}
}

// TestGovernanceHandler_Set_ClearAllNil asserts an all-nil overrides body
// (the wire-level clear) is accepted and reaches the store as an empty
// spec.
func TestGovernanceHandler_Set_ClearAllNil(t *testing.T) {
	store := &fakeOverrideStore{}
	h := newGovHandler(t, store)
	code, resp := govReq(t, h, "set_tenant_overrides", `{"overrides":{}}`, adminID(), []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", code, resp)
	}
	if store.setTenant != "t1" {
		t.Errorf("store.setTenant = %q, want t1", store.setTenant)
	}
	if !store.setSpec.IsEmpty() {
		t.Errorf("clear should reach the store as an empty spec, got %+v", store.setSpec)
	}
}

// TestGovernanceHandler_UnknownRoute asserts an unknown trailing segment is
// a 404.
func TestGovernanceHandler_UnknownRoute(t *testing.T) {
	h := newGovHandler(t, &fakeOverrideStore{})
	code, _ := govReq(t, h, "bogus", `{}`, adminID(), []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

// TestGovernanceHandler_MethodNotAllowed asserts a non-POST is rejected.
func TestGovernanceHandler_MethodNotAllowed(t *testing.T) {
	svc, err := governanceprotocol.NewService(&fakeOverrideStore{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	h, err := stream.NewGovernanceHandler(svc)
	if err != nil {
		t.Fatalf("NewGovernanceHandler: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/governance/get_tenant_overrides", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
