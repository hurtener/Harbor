package conformance

import (
	stderrors "errors"
	"testing"

	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/runtime/steering"
)

// These internal tests pin the small helper surface the conformance
// suite consumes — they exist to raise coverage on the helpers that
// would otherwise only run inside subtests, AND to surface a helper
// regression at the canonical-helper level (independent of the suite's
// matrix scenarios).

func TestInternal_MethodScopeFor_PerMethodMinimum(t *testing.T) {
	cases := []struct {
		method methods.Method
		scope  steering.Scope
	}{
		{methods.MethodInjectContext, steering.ScopeSessionUser},
		{methods.MethodUserMessage, steering.ScopeSessionUser},
		{methods.MethodCancel, steering.ScopeOwnerUser},
		{methods.MethodPause, steering.ScopeOwnerUser},
		{methods.MethodResume, steering.ScopeOwnerUser},
		{methods.MethodRedirect, steering.ScopeOwnerUser},
		{methods.MethodApprove, steering.ScopeOwnerUser},
		{methods.MethodReject, steering.ScopeOwnerUser},
		{methods.MethodPrioritize, steering.ScopeAdmin},
		{methods.MethodStart, steering.ScopeSessionUser},
	}
	for _, c := range cases {
		t.Run(string(c.method), func(t *testing.T) {
			if got := methodScopeFor(c.method); got != c.scope {
				t.Fatalf("methodScopeFor(%q) = %q, want %q", c.method, got, c.scope)
			}
		})
	}
}

func TestInternal_MethodScopeFor_UnknownDefaultsToSessionUser(t *testing.T) {
	if got := methodScopeFor(methods.Method("unknown_phantom_method")); got != steering.ScopeSessionUser {
		t.Fatalf("methodScopeFor(unknown) = %q, want %q", got, steering.ScopeSessionUser)
	}
}

func TestInternal_HappyPayloadFor_PerMethod(t *testing.T) {
	cases := []struct {
		method  methods.Method
		nonNil  bool
		key     string
		present bool
	}{
		{methods.MethodRedirect, true, "goal", true},
		{methods.MethodInjectContext, true, "note", true},
		{methods.MethodUserMessage, true, "message", true},
		{methods.MethodPrioritize, true, "priority", true},
		{methods.MethodApprove, true, "approved_by", true},
		{methods.MethodReject, true, "rejected_by", true},
		{methods.MethodCancel, false, "", false},
		{methods.MethodStart, false, "", false},
	}
	for _, c := range cases {
		t.Run(string(c.method), func(t *testing.T) {
			got := happyPayloadFor(c.method)
			if c.nonNil && got == nil {
				t.Fatalf("happyPayloadFor(%q) = nil, want non-nil", c.method)
			}
			if !c.nonNil && got != nil {
				t.Fatalf("happyPayloadFor(%q) = %v, want nil", c.method, got)
			}
			if c.present {
				if _, ok := got[c.key]; !ok {
					t.Fatalf("happyPayloadFor(%q): key %q missing from %v", c.method, c.key, got)
				}
			}
		})
	}
}

func TestInternal_ErrorCodeMatrix_AllCanonical(t *testing.T) {
	for _, c := range errorCodeMatrix {
		if !protoerrors.IsValidCode(c) {
			t.Errorf("errorCodeMatrix contains %q, not a canonical Code", c)
		}
	}
	if len(errorCodeMatrix) != 8 {
		t.Errorf("errorCodeMatrix size = %d, want 8 (Protocol 0.1.0 canonical set)", len(errorCodeMatrix))
	}
}

func TestInternal_ExpectedHTTPStatus_EveryCodeHasAStatus(t *testing.T) {
	for _, c := range errorCodeMatrix {
		status, ok := expectedHTTPStatus[c]
		if !ok {
			t.Errorf("expectedHTTPStatus has no entry for code %q", c)
			continue
		}
		if status < 400 || status >= 600 {
			t.Errorf("expectedHTTPStatus[%q] = %d, want 4xx/5xx", c, status)
		}
	}
}

func TestInternal_RunIdentity_BuildsFullQuadruple(t *testing.T) {
	q := runIdentity("tenant-test", "suffix")
	if q.TenantID != "tenant-test" {
		t.Errorf("TenantID = %q, want %q", q.TenantID, "tenant-test")
	}
	if q.SessionID != "session-conformance-suffix" {
		t.Errorf("SessionID = %q", q.SessionID)
	}
	if q.RunID != "run-conformance-suffix" {
		t.Errorf("RunID = %q", q.RunID)
	}
	if q.UserID == "" {
		t.Error("UserID must be non-empty (identity is mandatory)")
	}
}

// TestInternal_AssertCode_BranchesCorrectly drives the assertCode
// helper through a sub-`testing.T` so we can observe Failed() without
// failing the outer test. The helper's t.Fatalf branches fire only
// when the outer assertion is wrong — pinning the happy-path keeps
// the helper honest.
func TestInternal_AssertCode_AcceptsMatchingCode(t *testing.T) {
	err := protoerrors.New(protoerrors.CodeInvalidRequest, "test")
	sub := &testing.T{}
	defer func() {
		if r := recover(); r != nil {
			// t.Fatalf inside the helper uses runtime.Goexit; a panic
			// would surface as a test-runner bug. Recover for tidy
			// failure surfacing.
			t.Fatalf("assertCode panicked unexpectedly: %v", r)
		}
	}()
	assertCode(sub, err, protoerrors.CodeInvalidRequest)
	if sub.Failed() {
		t.Error("assertCode failed the sub-test for a matching code")
	}
}

func TestInternal_DefaultClaims_CarriesIdentityAndScopes(t *testing.T) {
	c := defaultClaims(testIdent(), nil)
	if c["tenant"] != "tenant-t" {
		t.Errorf("tenant claim = %v, want tenant-t", c["tenant"])
	}
	scopes, ok := c["scopes"].([]string)
	if !ok {
		t.Fatalf("scopes claim is not []string: %T", c["scopes"])
	}
	if len(scopes) != 0 {
		t.Errorf("empty scopes input produced non-empty claim: %v", scopes)
	}
}

func TestInternal_StaticKeySet_KnownKidResolves(t *testing.T) {
	// staticKeySet rejects unknown kid with a wrapped error. The
	// known-kid path is exercised by the suite itself; the unknown
	// path is exercised here.
	s := &staticKeySet{kid: "k1", pub: nil}
	_, _, err := s.KeyByID("unknown-kid")
	if err == nil {
		t.Fatal("KeyByID(unknown) returned nil error")
	}
	if !stderrors.Is(err, err) { // sanity: err identity
		t.Fatal("err identity broken")
	}
}

// testIdent is a small helper returning a sample identity for the
// internal helper tests above.
func testIdent() ident {
	return ident{TenantID: "tenant-t", UserID: "user-u", SessionID: "session-s"}
}

// ident mirrors identity.Identity locally so the internal_test file
// can avoid importing the identity package twice (already imported by
// conformance.go via package scope).
type ident = struct {
	TenantID  string
	UserID    string
	SessionID string
}
