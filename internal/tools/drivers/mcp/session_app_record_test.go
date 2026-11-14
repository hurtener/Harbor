package mcp

import (
	"context"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// ledgerRecorder is a capturer + SessionAppRecorder.
type ledgerRecorder struct {
	recordingCapturer
	mu       sync.Mutex
	recorded []string
}

func (r *ledgerRecorder) RecordSessionApp(ctx context.Context, agentID, serverID, resourceURI string) error {
	if _, ok := identity.From(ctx); !ok {
		return ErrIdentityMissing
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recorded = append(r.recorded, serverID+" "+resourceURI)
	return nil
}

func (r *ledgerRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.recorded)
}

// orderCheckingBus fails the test if an App render event is published before
// the App is on the session ledger.
type orderCheckingBus struct {
	events.EventBus
	t        *testing.T
	ledger   *ledgerRecorder
	appSeen  int
	appEarly bool
	mu       sync.Mutex
}

func (b *orderCheckingBus) Publish(ctx context.Context, ev events.Event) error {
	if ev.Type == EventTypeMCPAppAvailable {
		b.mu.Lock()
		b.appSeen++
		if b.ledger.count() == 0 {
			b.appEarly = true
		}
		b.mu.Unlock()
	}
	return b.EventBus.Publish(ctx, ev)
}

// TestProvider_RecordsSessionAppBeforePublishing pins the D-510 fast-press
// fix at its source: the provider writes the session App ledger before the
// render event is published, so a person who presses a button the moment the
// App appears never races the record.
func TestProvider_RecordsSessionAppBeforePublishing(t *testing.T) {
	inner := newTestBus(t)
	ledger := &ledgerRecorder{recordingCapturer: recordingCapturer{calls: map[string]capturedCall{}}}
	bus := &orderCheckingBus{EventBus: inner, t: t, ledger: ledger}
	p := newAppToolProvider(t, bus, "ui://weather/main.html")
	p.cfg.ToolContext = ledger
	invokeAppTool(t, p, "run-ledger-1")
	bus.mu.Lock()
	defer bus.mu.Unlock()
	if bus.appSeen != 1 || bus.appEarly {
		t.Fatalf("app events = %d, published before the ledger write = %v", bus.appSeen, bus.appEarly)
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.recorded) != 1 || ledger.recorded[0] != "weather-server ui://weather/main.html" {
		t.Fatalf("recorded = %v", ledger.recorded)
	}
}
