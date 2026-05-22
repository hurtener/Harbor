package distributed_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/distributed"

	// Pull in the loopback driver to register itself.
	_ "github.com/hurtener/Harbor/internal/distributed/drivers/loopback"
)

func TestRegistry_RegisteredBusDrivers_IncludesLoopback(t *testing.T) {
	names := distributed.RegisteredBusDrivers()
	found := false
	for _, n := range names {
		if n == "loopback" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("loopback not registered; registered=%v", names)
	}
}

func TestRegistry_RegisteredRemoteTransportDrivers_IncludesLoopback(t *testing.T) {
	names := distributed.RegisteredRemoteTransportDrivers()
	found := false
	for _, n := range names {
		if n == "loopback" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("loopback not registered; registered=%v", names)
	}
}

func TestOpenBus_UnknownDriver_Errors(t *testing.T) {
	_, err := distributed.OpenBusDriver("not-a-driver", distributed.Dependencies{
		Cfg: config.DistributedConfig{BusDriver: "not-a-driver", RemoteDriver: "loopback"},
	})
	if !errors.Is(err, distributed.ErrUnknownDriver) {
		t.Errorf("want ErrUnknownDriver, got %v", err)
	}
}

func TestOpenRemoteTransport_UnknownDriver_Errors(t *testing.T) {
	_, err := distributed.OpenRemoteTransportDriver("not-a-driver", distributed.Dependencies{
		Cfg: config.DistributedConfig{BusDriver: "loopback", RemoteDriver: "not-a-driver"},
	})
	if !errors.Is(err, distributed.ErrUnknownDriver) {
		t.Errorf("want ErrUnknownDriver, got %v", err)
	}
}

func TestOpenBus_DefaultDriver_PicksLoopback(t *testing.T) {
	// With empty BusDriver, Open should default to DefaultDriver ("loopback").
	// Loopback bus requires a non-nil EventBus; pass nil and expect the loopback
	// constructor to error, NOT the registry to error.
	_, err := distributed.OpenBus(context.Background(), distributed.Dependencies{
		Cfg: config.DistributedConfig{BusDriver: "", RemoteDriver: "loopback"},
	})
	if err == nil {
		t.Fatalf("expected loopback NewBus to error on nil EventBus")
	}
	if errors.Is(err, distributed.ErrUnknownDriver) {
		t.Errorf("expected default-driver dispatch to succeed; got ErrUnknownDriver")
	}
}

func TestBusEnvelope_Validate_RejectsEmptyIdentity(t *testing.T) {
	env := distributed.BusEnvelope{Edge: "x", EventID: "e"}
	if err := env.Validate(); !errors.Is(err, distributed.ErrIdentityRequired) {
		t.Errorf("want ErrIdentityRequired, got %v", err)
	}
}

func TestOpenRemoteTransport_DefaultDriver_PicksLoopback(t *testing.T) {
	rt, err := distributed.OpenRemoteTransport(context.Background(), distributed.Dependencies{
		Cfg: config.DistributedConfig{BusDriver: "loopback", RemoteDriver: ""},
	})
	if err != nil {
		t.Fatalf("OpenRemoteTransport: %v", err)
	}
	defer rt.Close(context.Background())
}

func TestOpenBus_ExplicitLoopback_NilEventBusErrors(t *testing.T) {
	_, err := distributed.OpenBusDriver("loopback", distributed.Dependencies{})
	if err == nil {
		t.Errorf("expected loopback NewBus to error on nil EventBus, got nil")
	}
	if errors.Is(err, distributed.ErrUnknownDriver) {
		t.Errorf("expected loopback constructor error, got ErrUnknownDriver")
	}
}

func TestRegisterBus_EmptyName_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on empty name")
		}
	}()
	distributed.RegisterBus("", nil)
}

func TestRegisterRemoteTransport_EmptyName_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on empty name")
		}
	}()
	distributed.RegisterRemoteTransport("", nil)
}

func TestRegisterBus_NilFactory_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on nil factory")
		}
	}()
	distributed.RegisterBus("test-nil-factory", nil)
}

func TestRegisterBus_Duplicate_Panics(t *testing.T) {
	// The loopback driver is already registered. Re-registering must panic.
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on duplicate registration")
		}
	}()
	distributed.RegisterBus("loopback", func(d distributed.Dependencies) (distributed.MessageBus, error) {
		return nil, nil
	})
}

func TestRegisterRemoteTransport_Duplicate_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on duplicate registration")
		}
	}()
	distributed.RegisterRemoteTransport("loopback", func(d distributed.Dependencies) (distributed.RemoteTransport, error) {
		return nil, nil
	})
}
