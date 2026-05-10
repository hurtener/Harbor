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
