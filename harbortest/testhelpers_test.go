package harbortest_test

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
)

// stubRedactor is a test-only no-op audit.Redactor used to keep the
// kit's self-tests deterministic — the patterns driver is exercised
// by its own package's tests, and the harbortest kit's tests focus
// on event capture + assertions, not on redaction patterns.
//
// Per CLAUDE.md §13 ("Test stubs as production defaults on
// operator-facing seams"): this stub lives in *_test.go (not in a
// production file), is not registered as a driver, and is not the
// default the binary resolves at boot. The §13 trip-wire is clear.
type stubRedactor struct{}

func (stubRedactor) Redact(_ context.Context, v any) (any, error) {
	return v, nil
}

// openInmemBus opens a fresh in-mem EventBus with kit-typical
// settings + the kit's stub redactor; the bus closes automatically
// when the test ends via t.Cleanup. Returned to the test as a real
// events.EventBus — production driver, no shortcuts.
func openInmemBus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     512,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         256,
	}, stubRedactor{})
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = bus.Close(context.Background())
	})
	return bus
}
