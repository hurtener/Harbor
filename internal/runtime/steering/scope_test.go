package steering

import (
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
)

// scopeTestRun is a documented dummy run quadruple — no secrets.
var scopeTestRun = identity.Quadruple{
	Identity: identity.Identity{
		TenantID:  "tenant-scope",
		UserID:    "user-scope",
		SessionID: "session-scope",
	},
	RunID: "run-scope",
}

func TestIsValidScope(t *testing.T) {
	for _, s := range []Scope{ScopeSessionUser, ScopeOwnerUser, ScopeAdmin} {
		if !IsValidScope(s) {
			t.Errorf("IsValidScope(%q) = false, want true", s)
		}
	}
	for _, bad := range []Scope{"", "user", "ADMIN", "root"} {
		if IsValidScope(bad) {
			t.Errorf("IsValidScope(%q) = true, want false", bad)
		}
	}
}

// TestRequiredScope_RFC63Mapping pins the RFC §6.3 per-event scope
// mapping verbatim. This is the master-plan acceptance surface.
func TestRequiredScope_RFC63Mapping(t *testing.T) {
	cases := map[ControlType]Scope{
		// "the session-scoped user"
		ControlInjectContext: ScopeSessionUser,
		ControlUserMessage:   ScopeSessionUser,
		// "the user (the agent's owner)"
		ControlRedirect: ScopeOwnerUser,
		// "the originating user/admin scope"
		ControlCancel:  ScopeOwnerUser,
		ControlPause:   ScopeOwnerUser,
		ControlResume:  ScopeOwnerUser,
		ControlApprove: ScopeOwnerUser,
		ControlReject:  ScopeOwnerUser,
		// "admin"
		ControlPrioritize: ScopeAdmin,
	}
	for tp, want := range cases {
		got, ok := RequiredScope(tp)
		if !ok {
			t.Errorf("RequiredScope(%q) ok=false, want true", tp)
			continue
		}
		if got != want {
			t.Errorf("RequiredScope(%q) = %q, want %q", tp, got, want)
		}
	}
	// Every canonical control type has a mapping.
	for _, tp := range ControlTypes() {
		if _, ok := RequiredScope(tp); !ok {
			t.Errorf("RequiredScope(%q) has no mapping — every control type must", tp)
		}
	}
	// An unknown type has no mapping.
	if _, ok := RequiredScope("STOP"); ok {
		t.Error("RequiredScope(STOP) ok=true, want false for an unknown type")
	}
}

// TestCheckScope_PerEventSufficientScope walks every control type
// with the minimum sufficient scope, the scope above it, and the
// scope below it — proving the per-event gate (RFC §6.3).
func TestCheckScope_PerEventSufficientScope(t *testing.T) {
	for _, tp := range ControlTypes() {
		min, _ := RequiredScope(tp)
		// The exact minimum scope is sufficient.
		if err := CheckScope(tp, min, scopeTestRun.TenantID, scopeTestRun); err != nil {
			t.Errorf("CheckScope(%q, min=%q) = %v, want nil", tp, min, err)
		}
		// Admin is always sufficient.
		if err := CheckScope(tp, ScopeAdmin, scopeTestRun.TenantID, scopeTestRun); err != nil {
			t.Errorf("CheckScope(%q, admin) = %v, want nil", tp, err)
		}
		// A scope below the minimum is rejected.
		switch min {
		case ScopeOwnerUser:
			err := CheckScope(tp, ScopeSessionUser, scopeTestRun.TenantID, scopeTestRun)
			if !errors.Is(err, ErrScopeMismatch) {
				t.Errorf("CheckScope(%q, session_user) = %v, want ErrScopeMismatch (min=%q)", tp, err, min)
			}
		case ScopeAdmin:
			for _, lower := range []Scope{ScopeSessionUser, ScopeOwnerUser} {
				err := CheckScope(tp, lower, scopeTestRun.TenantID, scopeTestRun)
				if !errors.Is(err, ErrScopeMismatch) {
					t.Errorf("CheckScope(%q, %q) = %v, want ErrScopeMismatch (min=admin)", tp, lower, err)
				}
			}
		}
	}
}

func TestCheckScope_UnknownControlType(t *testing.T) {
	err := CheckScope("STOP", ScopeAdmin, scopeTestRun.TenantID, scopeTestRun)
	if !errors.Is(err, ErrUnknownControlType) {
		t.Errorf("CheckScope(unknown type) = %v, want ErrUnknownControlType", err)
	}
}

func TestCheckScope_InvalidScope(t *testing.T) {
	err := CheckScope(ControlCancel, "root", scopeTestRun.TenantID, scopeTestRun)
	if !errors.Is(err, ErrInvalidScope) {
		t.Errorf("CheckScope(invalid scope) = %v, want ErrInvalidScope", err)
	}
}

// TestCheckScope_CrossTenantRequiresAdmin pins the RFC §6.3
// "Cross-tenant steering requires admin" rule.
func TestCheckScope_CrossTenantRequiresAdmin(t *testing.T) {
	const foreignTenant = "tenant-other"

	// A non-admin from a different tenant is rejected even for the
	// weakest control type (INJECT_CONTEXT only needs session_user
	// within-tenant).
	err := CheckScope(ControlInjectContext, ScopeOwnerUser, foreignTenant, scopeTestRun)
	if !errors.Is(err, ErrScopeMismatch) {
		t.Errorf("CheckScope(cross-tenant, owner_user) = %v, want ErrScopeMismatch", err)
	}

	// An admin from a different tenant is allowed (cross-tenant
	// steering requires — and is satisfied by — admin).
	if err := CheckScope(ControlInjectContext, ScopeAdmin, foreignTenant, scopeTestRun); err != nil {
		t.Errorf("CheckScope(cross-tenant, admin) = %v, want nil", err)
	}

	// Same-tenant non-admin is still gated by the per-event minimum
	// only (no cross-tenant penalty).
	if err := CheckScope(ControlInjectContext, ScopeSessionUser, scopeTestRun.TenantID, scopeTestRun); err != nil {
		t.Errorf("CheckScope(same-tenant, session_user, INJECT_CONTEXT) = %v, want nil", err)
	}
}

// TestCheckScope_EmptyCallerTenantFailsClosed asserts a blank caller
// tenant is treated as a cross-tenant mismatch (fail closed) rather
// than matching an empty run tenant.
func TestCheckScope_EmptyCallerTenantFailsClosed(t *testing.T) {
	err := CheckScope(ControlCancel, ScopeOwnerUser, "", scopeTestRun)
	if !errors.Is(err, ErrScopeMismatch) {
		t.Errorf("CheckScope(empty caller tenant) = %v, want ErrScopeMismatch", err)
	}
}
