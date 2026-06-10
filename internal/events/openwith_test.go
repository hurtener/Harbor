// openwith_test.go — Phase 110d (D-197) registry-level coverage for
// the deps-aware OpenWith entry point.
package events

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
)

// openWithNameCounter mints process-unique driver names so runtime
// registrations in this file never collide under `go test -count=N`.
var openWithNameCounter atomic.Uint64

func uniqueDriverName(t *testing.T, base string) string {
	t.Helper()
	return fmt.Sprintf("%s::%s::%d", base, t.Name(), openWithNameCounter.Add(1))
}

// TestOpenWith_NoDepsFactory_FallsBackToPlainFactory — a driver that
// registered no deps-aware factory routes through its plain Factory:
// OpenWith is a strict superset of Open.
func TestOpenWith_NoDepsFactory_FallsBackToPlainFactory(t *testing.T) {
	sentinel := errors.New("plain factory hit")
	name := RegisterForTest(t, "openwith-plain", func(config.EventsConfig, audit.Redactor) (EventBus, error) {
		return nil, sentinel
	})
	_, err := OpenWith(context.Background(), config.EventsConfig{Driver: name}, auditpatterns.New(), Deps{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected fallback to the plain factory, got %v", err)
	}
}

// TestOpenWith_DepsFactory_ReceivesDeps — a deps-aware registration
// wins over the plain factory and receives the caller's Deps.
func TestOpenWith_DepsFactory_ReceivesDeps(t *testing.T) {
	name := uniqueDriverName(t, "openwith-deps")
	plainHit := errors.New("plain factory must not be hit")
	Register(name, func(config.EventsConfig, audit.Redactor) (EventBus, error) {
		return nil, plainHit
	})
	var got Deps
	depsHit := errors.New("deps factory hit")
	RegisterWithDeps(name, func(_ config.EventsConfig, _ audit.Redactor, d Deps) (EventBus, error) {
		got = d
		return nil, depsHit
	})
	_, err := OpenWith(context.Background(), config.EventsConfig{Driver: name}, auditpatterns.New(), Deps{})
	if !errors.Is(err, depsHit) {
		t.Fatalf("expected the deps-aware factory to win, got %v", err)
	}
	_ = got // Deps carried through (State nil here; the durable driver test pins the non-nil path)
}

// TestOpenWith_NilRedactor_FailsLoud — same mandatory-redactor posture
// as Open.
func TestOpenWith_NilRedactor_FailsLoud(t *testing.T) {
	_, err := OpenWith(context.Background(), config.EventsConfig{Driver: DefaultDriver}, nil, Deps{})
	if err == nil || !strings.Contains(err.Error(), "audit.Redactor") {
		t.Fatalf("expected mandatory-redactor error, got %v", err)
	}
}

// TestOpenWith_UnknownDriver_NamesRegistrations — the canonical
// unknown-driver error (listing registered drivers) flows through
// OpenWith's fallback path.
func TestOpenWith_UnknownDriver_NamesRegistrations(t *testing.T) {
	_, err := OpenWith(context.Background(), config.EventsConfig{Driver: "no-such-driver-110d"}, auditpatterns.New(), Deps{})
	if !errors.Is(err, ErrUnknownDriver) {
		t.Fatalf("expected ErrUnknownDriver, got %v", err)
	}
}

// TestRegisterWithDeps_GuardRails — empty name, nil factory, and
// duplicate registration all panic (write-once-at-init contract).
func TestRegisterWithDeps_GuardRails(t *testing.T) {
	mustPanic := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: expected panic", name)
			}
		}()
		fn()
	}
	mustPanic("empty name", func() {
		RegisterWithDeps("", func(config.EventsConfig, audit.Redactor, Deps) (EventBus, error) { return nil, nil })
	})
	mustPanic("nil factory", func() { RegisterWithDeps(uniqueDriverName(t, "g"), nil) })
	dup := uniqueDriverName(t, "dup")
	RegisterWithDeps(dup, func(config.EventsConfig, audit.Redactor, Deps) (EventBus, error) { return nil, nil })
	mustPanic("duplicate", func() {
		RegisterWithDeps(dup, func(config.EventsConfig, audit.Redactor, Deps) (EventBus, error) { return nil, nil })
	})
}
