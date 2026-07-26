package artifacts_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/config"

	// Side-effect: register the inmem driver for Open tests.
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
)

func TestValidate_RejectsMissingIdentity(t *testing.T) {
	cases := []artifacts.ArtifactScope{
		{},
		{UserID: "U", SessionID: "S"},
		{TenantID: "T", SessionID: "S"},
		{TenantID: "T", UserID: "U"},
	}
	for i, sc := range cases {
		err := artifacts.Validate(sc)
		if !errors.Is(err, artifacts.ErrIdentityRequired) {
			t.Errorf("case %d (%+v): err=%v, want ErrIdentityRequired", i, sc, err)
		}
		methodErr := sc.Validate()
		if !errors.Is(methodErr, artifacts.ErrIdentityRequired) {
			t.Errorf("case %d (%+v) method: err=%v, want ErrIdentityRequired", i, sc, methodErr)
		}
	}
}

func TestValidate_AcceptsCompleteIdentity(t *testing.T) {
	// Complete with task.
	sc := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S", TaskID: "K"}
	if err := artifacts.Validate(sc); err != nil {
		t.Errorf("complete scope rejected: %v", err)
	}
	// Empty TaskID is acceptable (session-scoped).
	sc.TaskID = ""
	if err := artifacts.Validate(sc); err != nil {
		t.Errorf("session-scoped (empty TaskID) rejected: %v", err)
	}
}

func TestArtifactScope_Equal(t *testing.T) {
	a := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S", TaskID: "K"}
	b := a
	if !a.Equal(b) {
		t.Errorf("identical scopes: Equal=false")
	}
	b.TaskID = "K2"
	if a.Equal(b) {
		t.Errorf("scopes differ in TaskID but Equal=true")
	}
}

// TestArtifactScope_Triple_ClearsTheProvenanceAnnotation pins the read
// key's shape: `Triple` strips `TaskID` and nothing else, so a caller
// that stamped a task and one that did not address the same artifact.
func TestArtifactScope_Triple_ClearsTheProvenanceAnnotation(t *testing.T) {
	stamped := artifacts.ArtifactScope{
		TenantID: "T", UserID: "U", SessionID: "S", TaskID: "run-1",
	}
	key := stamped.Triple()
	if key.TaskID != "" {
		t.Errorf("Triple().TaskID=%q, want empty", key.TaskID)
	}
	if key.TenantID != "T" || key.UserID != "U" || key.SessionID != "S" {
		t.Errorf("Triple() disturbed the isolation components: %+v", key)
	}
	// The receiver is a value; the caller's own scope must be untouched.
	if stamped.TaskID != "run-1" {
		t.Errorf("Triple() mutated the receiver: %+v", stamped)
	}
	if key.Triple() != key {
		t.Errorf("Triple() is not idempotent")
	}
}

// TestArtifactScope_EqualTriple_IgnoresTaskID pins the comparison the
// ScopedArtifacts facade uses. `Equal` and `EqualTriple` must disagree
// on exactly one input class — scopes differing only in the stamp —
// because that is the class the reconciled read key exists to admit.
func TestArtifactScope_EqualTriple_IgnoresTaskID(t *testing.T) {
	base := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"}
	stamped := base
	stamped.TaskID = "run-1"
	other := base
	other.TaskID = "run-2"

	if !stamped.EqualTriple(other) {
		t.Errorf("EqualTriple false for scopes differing only in TaskID")
	}
	if stamped.Equal(other) {
		t.Errorf("Equal true for scopes differing in TaskID — Equal must stay exact")
	}
	if !stamped.EqualTriple(base) || !base.EqualTriple(stamped) {
		t.Errorf("EqualTriple is not symmetric across a stamped/unstamped pair")
	}

	for _, differing := range []artifacts.ArtifactScope{
		{TenantID: "OTHER", UserID: "U", SessionID: "S", TaskID: "run-1"},
		{TenantID: "T", UserID: "OTHER", SessionID: "S", TaskID: "run-1"},
		{TenantID: "T", UserID: "U", SessionID: "OTHER", TaskID: "run-1"},
	} {
		if stamped.EqualTriple(differing) {
			t.Errorf("EqualTriple true across an isolation boundary: %+v", differing)
		}
	}
}

// TestValidateFilter_RequiresTenantOnly pins `List`'s precondition: the
// zero-value scope stops being a legal all-tenants filter, while every
// component below the tenant stays a wildcard.
func TestValidateFilter_RequiresTenantOnly(t *testing.T) {
	rejected := []artifacts.ArtifactScope{
		{},
		{UserID: "U"},
		{SessionID: "S"},
		{UserID: "U", SessionID: "S", TaskID: "K"},
	}
	for _, filter := range rejected {
		if err := artifacts.ValidateFilter(filter); !errors.Is(err, artifacts.ErrIdentityRequired) {
			t.Errorf("ValidateFilter(%+v)=%v, want ErrIdentityRequired", filter, err)
		}
		if err := filter.ValidateFilter(); !errors.Is(err, artifacts.ErrIdentityRequired) {
			t.Errorf("method form ValidateFilter(%+v)=%v, want ErrIdentityRequired", filter, err)
		}
	}
	accepted := []artifacts.ArtifactScope{
		{TenantID: "T"},
		{TenantID: "T", UserID: "U"},
		{TenantID: "T", UserID: "U", SessionID: "S"},
		{TenantID: "T", UserID: "U", SessionID: "S", TaskID: "K"},
	}
	for _, filter := range accepted {
		if err := artifacts.ValidateFilter(filter); err != nil {
			t.Errorf("ValidateFilter(%+v)=%v, want nil", filter, err)
		}
	}
	// A filter is NOT a key: the identity precondition on a read is
	// stricter, and must stay so.
	tenantOnly := artifacts.ArtifactScope{TenantID: "T"}
	if err := artifacts.Validate(tenantOnly); !errors.Is(err, artifacts.ErrIdentityRequired) {
		t.Errorf("Validate(tenant-only)=%v, want ErrIdentityRequired — a read still "+
			"needs the whole triple", err)
	}
}

func TestOpen_RoutesByDriverName(t *testing.T) {
	cfg := config.ArtifactsConfig{Driver: "inmem"}
	s, err := artifacts.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	if s == nil {
		t.Fatal("Open returned nil store")
	}
}

func TestOpen_UsesDefaultDriverWhenEmpty(t *testing.T) {
	cfg := config.ArtifactsConfig{} // empty driver name
	s, err := artifacts.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
}

func TestOpen_RejectsUnknownDriver(t *testing.T) {
	cfg := config.ArtifactsConfig{Driver: "no-such-driver"}
	_, err := artifacts.Open(context.Background(), cfg)
	if !errors.Is(err, artifacts.ErrUnknownDriver) {
		t.Fatalf("err=%v, want errors.Is ErrUnknownDriver", err)
	}
	// Error message should list registered drivers so misconfig is
	// obvious.
	if !strings.Contains(err.Error(), "registered:") {
		t.Errorf("err=%q, want substring 'registered:'", err)
	}
}

func TestRegisteredDrivers_IncludesInMem(t *testing.T) {
	got := artifacts.RegisteredDrivers()
	found := false
	for _, n := range got {
		if n == "inmem" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("inmem not in registered drivers: %v", got)
	}
}

func TestRegister_PanicsOnEmptyName(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on empty name")
		}
	}()
	artifacts.Register("", func(config.ArtifactsConfig) (artifacts.ArtifactStore, error) {
		return nil, nil
	})
}

func TestRegister_PanicsOnNilFactory(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil factory")
		}
	}()
	artifacts.Register("nil-factory-test", nil)
}

func TestRegister_PanicsOnDuplicate(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on duplicate registration")
		}
	}()
	artifacts.Register("inmem", func(config.ArtifactsConfig) (artifacts.ArtifactStore, error) {
		return nil, nil
	})
}

func TestSentinels_AreDistinct(t *testing.T) {
	// Sanity: each sentinel is its own error value (so errors.Is on
	// one doesn't accidentally match another).
	all := []error{
		artifacts.ErrNotFound,
		artifacts.ErrScopeMismatch,
		artifacts.ErrIdentityRequired,
		artifacts.ErrInvalidScope,
		artifacts.ErrUnknownDriver,
		artifacts.ErrStoreClosed,
	}
	for i, a := range all {
		for j, b := range all {
			if i == j {
				continue
			}
			if errors.Is(a, b) {
				t.Errorf("sentinel collision: %v Is %v", a, b)
			}
		}
	}
}
