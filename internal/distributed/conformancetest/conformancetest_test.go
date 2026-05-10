package conformancetest_test

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/distributed"
	"github.com/hurtener/Harbor/internal/distributed/conformancetest"
	"github.com/hurtener/Harbor/internal/distributed/drivers/loopback"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
)

// freshDeps constructs a fresh EventBus + Dependencies wrapper for a
// single test invocation. The EventBus is closed by the cleanup.
func freshDeps(t *testing.T) (events.EventBus, audit.Redactor, distributed.Dependencies, func()) {
	t.Helper()
	red := auditpatterns.New()
	eb, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 32,
		SubscriberBufferSize:     2048,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
		ReplayBufferSize:         128,
	}, red)
	if err != nil {
		t.Fatalf("events bus: %v", err)
	}
	deps := distributed.Dependencies{
		EventBus: eb,
		Cfg:      config.DistributedConfig{BusDriver: "loopback", RemoteDriver: "loopback"},
	}
	cleanup := func() {
		_ = eb.Close(context.Background())
	}
	return eb, red, deps, cleanup
}

func TestConformance_Bus_Loopback(t *testing.T) {
	conformancetest.RunBus(t, func(t *testing.T) (distributed.MessageBus, events.EventBus, func()) {
		eb, _, deps, evCleanup := freshDeps(t)
		bus, err := loopback.NewBus(deps)
		if err != nil {
			evCleanup()
			t.Fatalf("loopback NewBus: %v", err)
		}
		cleanup := func() {
			_ = bus.Close(context.Background())
			evCleanup()
		}
		return bus, eb, cleanup
	})
}

func TestConformance_RemoteTransport_Loopback(t *testing.T) {
	conformancetest.RunRemoteTransport(t, func(t *testing.T) (distributed.RemoteTransport, conformancetest.AgentBinding, func()) {
		_, _, deps, evCleanup := freshDeps(t)
		rt, err := loopback.NewRemoteTransport(deps)
		if err != nil {
			evCleanup()
			t.Fatalf("loopback NewRemoteTransport: %v", err)
		}
		lt, ok := rt.(loopback.LoopbackTransport)
		if !ok {
			evCleanup()
			t.Fatalf("RemoteTransport %T does not implement LoopbackTransport", rt)
		}
		binding := conformancetest.AgentBinding(func(url string, agent loopback.Agent) {
			lt.RegisterAgent(url, agent)
		})
		cleanup := func() {
			_ = rt.Close(context.Background())
			evCleanup()
		}
		return rt, binding, cleanup
	})
}
