package llm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// envelopeValidatingBus is the 280-rejected-chunks regression gate
// the phase plan mandates: a bus double that REJECTS any event whose
// envelope identity is incomplete — the exact validation the real
// inmem bus applies before fan-out (CLAUDE.md §6 rule 5). Accepted
// events are recorded for shape assertions.
type envelopeValidatingBus struct {
	mu           sync.Mutex
	events       []events.Event
	rejected     int
	publishCalls int
	liveCalls    int
	fail         error
}

func (b *envelopeValidatingBus) Publish(_ context.Context, ev events.Event) error {
	b.mu.Lock()
	b.publishCalls++
	b.mu.Unlock()
	return b.record(ev)
}

func (b *envelopeValidatingBus) PublishLive(_ context.Context, ev events.Event) error {
	b.mu.Lock()
	b.liveCalls++
	b.mu.Unlock()
	return b.record(ev)
}

func (b *envelopeValidatingBus) record(ev events.Event) error {
	if b.fail != nil {
		return b.fail
	}
	id := ev.Identity
	if id.TenantID == "" || id.UserID == "" || id.SessionID == "" {
		b.mu.Lock()
		b.rejected++
		b.mu.Unlock()
		return fmt.Errorf("events: event identity missing one or more components: type=%s", ev.Type)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, ev)
	return nil
}

func (b *envelopeValidatingBus) Subscribe(context.Context, events.Filter) (events.Subscription, error) {
	return nil, errors.New("envelopeValidatingBus: Subscribe unsupported")
}

func (b *envelopeValidatingBus) Close(context.Context) error { return nil }

func (b *envelopeValidatingBus) captured() []events.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]events.Event, len(b.events))
	copy(out, b.events)
	return out
}

func (b *envelopeValidatingBus) rejectedCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rejected
}

func (b *envelopeValidatingBus) laneCounts() (publish, live int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.publishCalls, b.liveCalls
}

// legacyEventBus intentionally implements only the durable EventBus core.
// It pins source compatibility for custom SDK buses that predate the
// additive LivePublisher capability.
type legacyEventBus struct {
	mu           sync.Mutex
	events       []events.Event
	publishCalls int
}

func (b *legacyEventBus) Publish(_ context.Context, ev events.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.publishCalls++
	b.events = append(b.events, ev)
	return nil
}

func (b *legacyEventBus) Subscribe(context.Context, events.Filter) (events.Subscription, error) {
	return nil, errors.New("legacyEventBus: Subscribe unsupported")
}

func (b *legacyEventBus) Close(context.Context) error { return nil }

func (b *legacyEventBus) counts() (publish, captured int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.publishCalls, len(b.events)
}

func chunkPublisherTestQuad(run string) identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-a"},
		RunID:    run,
	}
}

// TestNewChunkPublisher_IdentityLandsOnEnvelope — the trap the
// constructor encodes: identity must reach the Event ENVELOPE (the
// bus validates it before fan-out), not just the payload. A publisher
// that stamps the payload only would see every chunk rejected here.
func TestNewChunkPublisher_IdentityLandsOnEnvelope(t *testing.T) {
	bus := &envelopeValidatingBus{}
	q := chunkPublisherTestQuad("run-1")
	pub := NewChunkPublisher(bus, q, "task-1", slog.Default())

	pub("hello", false, "content")
	pub("", true, "content")

	if rejected := bus.rejectedCount(); rejected != 0 {
		t.Fatalf("%d chunks rejected for missing envelope identity — the 280-rejected-chunks regression", rejected)
	}
	got := bus.captured()
	if len(got) != 2 {
		t.Fatalf("published %d events, want 2", len(got))
	}
	if publish, live := bus.laneCounts(); publish != 0 || live != 2 {
		t.Fatalf("chunk publisher lanes = Publish:%d PublishLive:%d, want Publish:0 PublishLive:2", publish, live)
	}
	for _, ev := range got {
		if ev.Type != EventTypeCompletionChunk {
			t.Errorf("event type = %q, want %q", ev.Type, EventTypeCompletionChunk)
		}
		if ev.Identity != q {
			t.Errorf("envelope identity = %+v, want %+v", ev.Identity, q)
		}
	}
}

// TestNewChunkPublisher_PayloadCarriesTaskAndRunIDs — the typed
// payload carries task/run IDs + delta/done/kind verbatim.
func TestNewChunkPublisher_PayloadCarriesTaskAndRunIDs(t *testing.T) {
	bus := &envelopeValidatingBus{}
	q := chunkPublisherTestQuad("run-7")
	pub := NewChunkPublisher(bus, q, "task-7", slog.Default())

	pub("delta-text", true, "reasoning")

	got := bus.captured()
	if len(got) != 1 {
		t.Fatalf("published %d events, want 1", len(got))
	}
	payload, ok := got[0].Payload.(CompletionChunkPayload)
	if !ok {
		t.Fatalf("payload is %T, want CompletionChunkPayload", got[0].Payload)
	}
	if payload.TaskID != "task-7" {
		t.Errorf("payload.TaskID = %q, want task-7", payload.TaskID)
	}
	if payload.RunID != "run-7" {
		t.Errorf("payload.RunID = %q, want run-7", payload.RunID)
	}
	if payload.Identity != q {
		t.Errorf("payload.Identity = %+v, want %+v", payload.Identity, q)
	}
	if payload.Delta != "delta-text" || !payload.Done || payload.Kind != "reasoning" {
		t.Errorf("payload fields = %+v, want delta-text/true/reasoning", payload)
	}
	if payload.OccurredAt.IsZero() {
		t.Error("payload.OccurredAt is zero")
	}
}

func TestNewChunkPublisher_LegacyBusDisablesAnimationWithoutDurableFallback(t *testing.T) {
	bus := &legacyEventBus{}
	var log bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&log, nil))
	pub := NewChunkPublisher(bus, chunkPublisherTestQuad("run-legacy"), "task-legacy", logger)

	pub("first", false, "content")
	pub("", true, "content")

	if live, ok := any(bus).(events.LivePublisher); ok {
		t.Fatalf("legacy bus unexpectedly implements LivePublisher: %T", live)
	}
	publish, captured := bus.counts()
	if publish != 0 || captured != 0 {
		t.Fatalf("legacy chunk degradation counts = Publish:%d captured:%d, want 0/0", publish, captured)
	}
	if warnings := strings.Count(log.String(), "completion animation disabled"); warnings != 1 {
		t.Fatalf("legacy chunk degradation warnings = %d, want exactly one: %s", warnings, log.String())
	}
}

// TestNewChunkPublisher_WarnsOnPublishFailure — brief 06 §5: publish
// failures are surfaced loudly (a closed bus mid-stream logs, never
// silently drops).
func TestNewChunkPublisher_WarnsOnPublishFailure(t *testing.T) {
	bus := &envelopeValidatingBus{fail: errors.New("bus closed")}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	pub := NewChunkPublisher(bus, chunkPublisherTestQuad("run-1"), "task-1", logger)

	pub("hello", false, "content")

	if !strings.Contains(buf.String(), "completion-chunk publish failed") {
		t.Errorf("expected loud Warn on publish failure; log: %s", buf.String())
	}
}

// TestNewChunkPublisher_NilLoggerDefaults — a nil logger must not
// panic the failure path.
func TestNewChunkPublisher_NilLoggerDefaults(t *testing.T) {
	bus := &envelopeValidatingBus{fail: errors.New("boom")}
	pub := NewChunkPublisher(bus, chunkPublisherTestQuad("run-1"), "task-1", nil)
	pub("hello", false, "content") // must not panic
}

// TestNewChunkPublisher_ConcurrentRuns_NoIdentityBleed — D-025 stress
// gate: N≥100 per-run publisher closures share ONE bus; every
// delivered chunk carries exactly its own run's quadruple on both the
// envelope and the payload.
func TestNewChunkPublisher_ConcurrentRuns_NoIdentityBleed(t *testing.T) {
	bus := &envelopeValidatingBus{}
	const runs = 128
	const perRun = 4

	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			q := identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  fmt.Sprintf("tenant-%d", n),
					UserID:    fmt.Sprintf("user-%d", n),
					SessionID: fmt.Sprintf("sess-%d", n),
				},
				RunID: fmt.Sprintf("run-%d", n),
			}
			pub := NewChunkPublisher(bus, q, fmt.Sprintf("task-%d", n), slog.Default())
			for j := range perRun {
				pub(fmt.Sprintf("delta-%d-%d", n, j), j == perRun-1, "content")
			}
		}(i)
	}
	wg.Wait()

	if rejected := bus.rejectedCount(); rejected != 0 {
		t.Fatalf("%d chunks rejected under concurrency", rejected)
	}
	got := bus.captured()
	if len(got) != runs*perRun {
		t.Fatalf("delivered %d events, want %d", len(got), runs*perRun)
	}
	for _, ev := range got {
		payload, ok := ev.Payload.(CompletionChunkPayload)
		if !ok {
			t.Fatalf("payload is %T, want CompletionChunkPayload", ev.Payload)
		}
		if ev.Identity != payload.Identity {
			t.Fatalf("envelope/payload identity mismatch: %+v vs %+v", ev.Identity, payload.Identity)
		}
		wantRun := "run-" + strings.TrimPrefix(payload.TaskID, "task-")
		if ev.Identity.RunID != wantRun {
			t.Fatalf("cross-run identity bleed: task %q chunk carries RunID %q", payload.TaskID, ev.Identity.RunID)
		}
	}
}
