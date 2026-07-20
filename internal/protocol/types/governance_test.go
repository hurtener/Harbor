package types_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

// TestGovernanceSetPostureRequest_CarriesNoIdentityField pins D-219: the
// `governance.set_posture` request carries the full tier table + default tier
// but NO identity / scope field — authority is derived server-side from the
// verified session, never the request body. A body-supplied scope cannot
// elevate because there is no such field to supply.
func TestGovernanceSetPostureRequest_CarriesNoIdentityField(t *testing.T) {
	rt := reflect.TypeOf(types.GovernanceSetPostureRequest{})
	// An identity/scope AUTHORITY field is one of these exact names / json
	// tags — NOT the `IdentityTiers` policy table (whose name merely shares
	// the "identity" prefix). Authority must never be a body field (D-219).
	forbiddenNames := map[string]struct{}{
		"Identity": {}, "IdentityScope": {}, "Scope": {}, "Scopes": {},
		"Tenant": {}, "TenantID": {}, "User": {}, "UserID": {}, "Session": {}, "SessionID": {},
	}
	forbiddenTags := map[string]struct{}{
		"identity": {}, "scope": {}, "scopes": {}, "tenant": {}, "tenant_id": {},
		"user": {}, "user_id": {}, "session": {}, "session_id": {},
	}
	for i := range rt.NumField() {
		f := rt.Field(i)
		if _, bad := forbiddenNames[f.Name]; bad {
			t.Errorf("GovernanceSetPostureRequest must carry no identity/scope field (authority is server-side, D-219); found %q", f.Name)
		}
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if _, bad := forbiddenTags[tag]; bad {
			t.Errorf("GovernanceSetPostureRequest json tag leaks an identity/scope field: %q", tag)
		}
	}
	// Sanity: the shape it MUST carry is present.
	if _, ok := rt.FieldByName("DefaultTier"); !ok {
		t.Error("GovernanceSetPostureRequest missing DefaultTier")
	}
	if _, ok := rt.FieldByName("IdentityTiers"); !ok {
		t.Error("GovernanceSetPostureRequest missing IdentityTiers")
	}
}

// TestGovernanceSetPostureResponse_ReusesReadShapes pins that the write
// response echoes the same IdentityTierView + DefaultTier the read projects,
// so a caller sees byte-faithful what the next `governance.posture` returns.
func TestGovernanceSetPostureResponse_ReusesReadShapes(t *testing.T) {
	resp := types.GovernanceSetPostureResponse{
		DefaultTier: "free",
		IdentityTiers: map[string]types.IdentityTierView{
			"free": {BudgetCeilingUSD: 0.5, MaxTokens: 1000},
		},
		ProtocolVersion: types.ProtocolVersion,
	}
	if resp.IdentityTiers["free"].BudgetCeilingUSD != 0.5 {
		t.Errorf("IdentityTierView not reused faithfully: %+v", resp.IdentityTiers["free"])
	}
	if resp.DefaultTier != "free" {
		t.Errorf("DefaultTier not carried: %q", resp.DefaultTier)
	}
	if resp.ProtocolVersion != types.ProtocolVersion {
		t.Errorf("ProtocolVersion not carried: %q", resp.ProtocolVersion)
	}
}
