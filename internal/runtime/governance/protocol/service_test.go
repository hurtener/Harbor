package protocol_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	governanceprotocol "github.com/hurtener/Harbor/internal/runtime/governance/protocol"
)

type recordingStore struct {
	tenant string
	spec   governance.TenantOverrideSpec
}

func (r *recordingStore) Set(_ context.Context, _ identity.Quadruple, tenant string, spec governance.TenantOverrideSpec) error {
	r.tenant = tenant
	r.spec = spec
	return nil
}

func (r *recordingStore) Get(_ context.Context, _ string) (governance.TenantOverrideSpec, bool, error) {
	return r.spec, r.spec.Model != nil, nil
}

func TestService_NilStore_FailLoud(t *testing.T) {
	if _, err := governanceprotocol.NewService(nil); !errors.Is(err, governanceprotocol.ErrMisconfigured) {
		t.Errorf("nil store: want ErrMisconfigured, got %v", err)
	}
}

func TestService_Set_IdentityRequired(t *testing.T) {
	svc, err := governanceprotocol.NewService(&recordingStore{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	// Empty identity triple → fail closed.
	_, err = svc.SetTenantOverrides(context.Background(), prototypes.GovernanceSetTenantOverridesRequest{})
	if !errors.Is(err, governanceprotocol.ErrIdentityRequired) {
		t.Errorf("want ErrIdentityRequired, got %v", err)
	}
}

func TestService_Set_MapsWireToSpec(t *testing.T) {
	store := &recordingStore{}
	svc, err := governanceprotocol.NewService(store)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	model := "m"
	temp := 0.9
	_, err = svc.SetTenantOverrides(context.Background(), prototypes.GovernanceSetTenantOverridesRequest{
		Identity:  prototypes.IdentityScope{Tenant: "t1", User: "u", Session: "s"},
		Overrides: prototypes.GovernanceTenantOverrides{Model: &model, Temperature: &temp},
	})
	if err != nil {
		t.Fatalf("SetTenantOverrides: %v", err)
	}
	if store.tenant != "t1" {
		t.Errorf("tenant = %q, want t1", store.tenant)
	}
	if store.spec.Model == nil || *store.spec.Model != "m" {
		t.Errorf("spec.Model = %v", store.spec.Model)
	}
	if store.spec.Temperature == nil || *store.spec.Temperature != 0.9 {
		t.Errorf("spec.Temperature = %v", store.spec.Temperature)
	}
}
