package state_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"

	// Side-effect: register the inmem driver under "inmem".
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

func TestNewEventID_NonEmpty(t *testing.T) {
	id := state.NewEventID()
	if id == "" {
		t.Fatal("NewEventID returned empty")
	}
	id2 := state.NewEventID()
	if id == id2 {
		t.Errorf("NewEventID returned identical IDs: %q", id)
	}
}

func TestValidateIdentity_Cases(t *testing.T) {
	good := identity.Quadruple{
		Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"},
	}
	if err := state.ValidateIdentity(good); err != nil {
		t.Errorf("good identity rejected: %v", err)
	}
	cases := []identity.Quadruple{
		{},
		{Identity: identity.Identity{UserID: "U", SessionID: "S"}},
		{Identity: identity.Identity{TenantID: "T", SessionID: "S"}},
		{Identity: identity.Identity{TenantID: "T", UserID: "U"}},
	}
	for i, q := range cases {
		err := state.ValidateIdentity(q)
		if !errors.Is(err, state.ErrIdentityRequired) {
			t.Errorf("case %d (%+v): err=%v, want ErrIdentityRequired", i, q, err)
		}
	}
	// Empty RunID must NOT be rejected — session-scoped state is fine.
	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"},
	}
	if err := state.ValidateIdentity(q); err != nil {
		t.Errorf("empty RunID rejected: %v", err)
	}
}

func TestValidateRecord_Cases(t *testing.T) {
	good := state.StateRecord{
		ID:       "01HABXXX",
		Identity: identity.Quadruple{Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}},
		Kind:     "k",
		Bytes:    []byte("x"),
	}
	if err := state.ValidateRecord(good); err != nil {
		t.Errorf("good record rejected: %v", err)
	}
	noID := good
	noID.ID = ""
	if err := state.ValidateRecord(noID); !errors.Is(err, state.ErrInvalidRecord) {
		t.Errorf("empty ID: err=%v, want ErrInvalidRecord", err)
	}
	noKind := good
	noKind.Kind = ""
	if err := state.ValidateRecord(noKind); !errors.Is(err, state.ErrInvalidRecord) {
		t.Errorf("empty Kind: err=%v, want ErrInvalidRecord", err)
	}
}

func TestRegister_DuplicatePanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Register did not panic on duplicate")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "inmem") {
			t.Errorf("panic value %v missing duplicate driver", r)
		}
	}()
	state.Register("inmem", func(_ config.StateConfig) (state.StateStore, error) {
		return nil, nil
	})
}

func TestRegister_EmptyNamePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Register did not panic on empty name")
		}
	}()
	state.Register("", func(_ config.StateConfig) (state.StateStore, error) { return nil, nil })
}

func TestRegister_NilFactoryPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Register did not panic on nil factory")
		}
	}()
	state.Register("nil-factory-test", nil)
}

func TestRegisteredDrivers_ContainsInmem(t *testing.T) {
	got := state.RegisteredDrivers()
	found := false
	for _, n := range got {
		if n == "inmem" {
			found = true
		}
	}
	if !found {
		t.Errorf("inmem driver not in registered list: %v", got)
	}
}

func TestOpen_DefaultDriver(t *testing.T) {
	cfg := config.StateConfig{Driver: "inmem"}
	s, err := state.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	if s == nil {
		t.Fatal("Open returned nil")
	}
}

func TestOpen_DefaultsToInmemWhenDriverEmpty(t *testing.T) {
	cfg := config.StateConfig{Driver: ""}
	s, err := state.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
}

func TestOpenDriver_UnknownNameWrapsSentinel(t *testing.T) {
	_, err := state.OpenDriver("does-not-exist", config.StateConfig{})
	if err == nil {
		t.Fatal("OpenDriver returned nil for unknown driver")
	}
	if !errors.Is(err, state.ErrUnknownDriver) {
		t.Fatalf("err=%v, want errors.Is ErrUnknownDriver", err)
	}
	if !strings.Contains(err.Error(), "inmem") {
		t.Errorf("err=%q does not list registered drivers", err.Error())
	}
}

func TestWithStore_RoundTrip(t *testing.T) {
	s, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	ctx := state.WithStore(context.Background(), s)
	got, ok := state.From(ctx)
	if !ok {
		t.Fatal("From returned ok=false after WithStore")
	}
	if got != s {
		t.Errorf("From returned different store")
	}
}

func TestFrom_AbsentReturnsZeroAndFalse(t *testing.T) {
	got, ok := state.From(context.Background())
	if ok {
		t.Errorf("From on bare ctx returned ok=true")
	}
	if got != nil {
		t.Errorf("From on bare ctx returned non-nil: %v", got)
	}
}

func TestMustFrom_PanicsOnAbsence(t *testing.T) {
	defer func() {
		v := recover()
		if v == nil {
			t.Fatal("MustFrom did not panic on bare ctx")
		}
		err, ok := v.(error)
		if !ok || !errors.Is(err, state.ErrStoreClosed) {
			t.Fatalf("panic value %v is not ErrStoreClosed", v)
		}
	}()
	_ = state.MustFrom(context.Background())
}
