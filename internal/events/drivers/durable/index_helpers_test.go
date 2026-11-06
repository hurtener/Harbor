package durable

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// sessionIndexSeqs returns a session's indexed sequences in order, whatever
// the persisted format (legacy head or v2 header + chunks). It walks the
// verified chain, so a corrupt index fails the test.
func sessionIndexSeqs(t testing.TB, store state.StateStore, id identity.Quadruple) []uint64 {
	t.Helper()
	rows := sessionIndexRows(t, store, id)
	out := make([]uint64, len(rows))
	for i, r := range rows {
		out[i] = r.Seq
	}
	return out
}

func sessionIndexRows(t testing.TB, store state.StateStore, id identity.Quadruple) []indexRow {
	t.Helper()
	rows, err := readIndexRows(context.Background(), store, id)
	if err != nil {
		t.Fatalf("read session index: %v", err)
	}
	return rows
}

// readIndexRows reads every retained row oldest-first through a throwaway
// reader bus that shares no state with the caller's bus.
func readIndexRows(ctx context.Context, store state.StateStore, id identity.Quadruple) ([]indexRow, error) {
	b := &bus{store: store, clock: realClock{}, logger: discardTestLogger()}
	b.maint.init(0)
	view, err := b.loadIndexView(ctx, id)
	if err != nil {
		return nil, err
	}
	rows, _, err := b.collectBelow(ctx, &view, 0, 0, nil)
	if err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Seq < rows[j].Seq })
	return rows, nil
}

// loadHeaderV2 loads and verifies a session's v2 header.
func loadHeaderV2(t testing.TB, store state.StateStore, id identity.Quadruple) (indexHeader, state.StateRecord) {
	t.Helper()
	rec, err := store.Load(context.Background(), id, kindHead)
	if err != nil {
		t.Fatalf("load header: %v", err)
	}
	if !isIndexV2(rec.Bytes) {
		t.Fatalf("head is not v2: %.40q", rec.Bytes)
	}
	hdr, err := decodeHeader(rec.Bytes)
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	return hdr, rec
}

func isV2Head(t testing.TB, store state.StateStore, id identity.Quadruple) bool {
	t.Helper()
	rec, err := store.Load(context.Background(), id, kindHead)
	if errors.Is(err, state.ErrNotFound) {
		return false
	}
	if err != nil {
		t.Fatalf("load head: %v", err)
	}
	return isIndexV2(rec.Bytes)
}

// openIndexBus opens a durable bus with the background loop disabled and
// fast, deterministic retention pacing. Extra options apply after.
func openIndexBus(t testing.TB, store state.StateStore, auditMaxAge time.Duration, opts ...Option) *bus {
	t.Helper()
	cfg := metadataCfg()
	cfg.Retention.AuditMaxAge = auditMaxAge
	all := append([]Option{withMaintenance(func(m *maintenance) {
		m.disabled = true
		m.deleteInterval = 0
	}), WithLogger(discardTestLogger())}, opts...)
	opened, err := New(context.Background(), cfg, auditpatterns.New(), store, all...)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	b := opened.(*bus)
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	return b
}

// withLegacyIndexWrites makes the bus write the pre-v2 head format.
func withLegacyIndexWrites() Option {
	return func(b *bus) { b.legacyIndexWrites = true }
}

func discardTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testEvent builds a publishable event of type typ for id at occurredAt.
func testEvent(id identity.Quadruple, typ events.EventType, occurredAt time.Time) events.Event {
	return events.Event{
		Type:       typ,
		Identity:   id,
		OccurredAt: occurredAt,
		Payload:    events.BusDroppedPayload{FromSeq: 1, ToSeq: 1},
	}
}

// settableClock is a concurrency-safe controllable clock.
type settableClock struct {
	now   time.Time
	guard chan struct{}
}

func newSettableClock(at time.Time) *settableClock {
	c := &settableClock{now: at, guard: make(chan struct{}, 1)}
	c.guard <- struct{}{}
	return c
}

func (c *settableClock) Now() time.Time {
	<-c.guard
	defer func() { c.guard <- struct{}{} }()
	return c.now
}

func (c *settableClock) Set(at time.Time) {
	<-c.guard
	c.now = at
	c.guard <- struct{}{}
}
