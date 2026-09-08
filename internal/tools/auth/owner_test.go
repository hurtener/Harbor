package auth

import "testing"

func TestOwnerScopeExhaustive(t *testing.T) {
	for _, tc := range []struct {
		o Owner
		s SourceScope
	}{
		{Owner{}, ScopeBootGlobal},
		{Owner{Tenant: "t", Agent: "a"}, ScopeTenantAgent},
		{Owner{Tenant: "t", Agent: "a", User: "u"}, ScopeTenantUser},
		{Owner{Tenant: "t"}, ScopeInvalid}, {Owner{Agent: "a"}, ScopeInvalid},
		{Owner{User: "u"}, ScopeInvalid}, {Owner{Tenant: "t", User: "u"}, ScopeInvalid},
		{Owner{Agent: "a", User: "u"}, ScopeInvalid},
	} {
		if got := tc.o.Scope(); got != tc.s {
			t.Errorf("%+v: %s want %s", tc.o, got, tc.s)
		}
	}
}
