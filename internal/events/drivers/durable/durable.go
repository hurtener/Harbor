// Package durable is Harbor's StateStore-backed durable event log
// driver (Phase 57, RFC §6.13).
//
// Architecture:
//
//   - The driver implements events.EventBus + events.Replayer. It owns
//     its own monotonic, gap-free sequence counter and its own
//     subscriber fan-out (drop-oldest under saturation, bus.dropped
//     notices windowed per DropWindow) — it is a standalone §4.4
//     driver, not a wrapper over the inmem driver.
//   - Every published event is persisted through a state.StateStore
//     before it is fanned out to live subscribers. Persistence is
//     keyed so replay-from-cursor is exact and gap-free across a
//     Runtime restart: see the keying scheme below.
//   - Replay reads from the StateStore — not an in-memory ring — so a
//     late subscriber that connects after the Runtime was rebuilt
//     against the same StateStore sees the full, gap-free history.
//   - When NO StateStore is configured the driver auto-degrades to a
//     best-effort in-memory ring buffer AND emits a loud runtime.warning
//     event plus an slog.Warn (D-074, CLAUDE.md §13 "no silent
//     degradation"). Replay is then NOT durable across restarts.
//
// Keying scheme (within state.StateStore's keyed-slot contract — there
// is no list/scan method, so the durable log is built from one mutable
// "head" record plus one immutable "entry" record per event):
//
//   - The durable log is SESSION-scoped, matching events.Cursor which
//     is (SessionID, Sequence). Both record kinds are stored under the
//     session triple with RunID="" — an event's own RunID is preserved
//     INSIDE the persisted bytes, not in the storage key.
//   - Head record:  Kind = "events.durable.head"        — holds the
//     ordered list of bus-sequences persisted for that session.
//   - Entry record: Kind = "events.durable.entry/<seq>" — holds the
//     JSON-encoded event for bus-sequence <seq>.
//
// On Publish: assign the next bus sequence, write the entry record,
// then read-modify-write the head record's sequence list — all under
// publishMu so the head list and the sequence counter never disagree.
// A torn write (entry persisted, head not yet advanced) never produces
// a GAP in a served replay: Replay only ever returns sequences the
// head record lists, and the next Publish re-derives the head list.
//
// The driver is registered under name "durable" via init(); cmd/harbor
// blank-imports this package so the registration fires at process
// startup. Per CLAUDE.md §4.4.
package durable

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

const (
	// kindHead is the StateStore Kind of the per-session head record.
	kindHead = "events.durable.head"
	// kindEntryPrefix is the StateStore Kind prefix of a per-event
	// entry record; the bus sequence is appended.
	kindEntryPrefix = "events.durable.entry/"
)

// Clock abstracts time so tests do not depend on the wall clock.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Option configures the driver at construction. The exported options
// are test/operator seams; production callers that go through the
// registry use the defaults.
type Option func(*bus)

// WithClock injects a Clock. Tests use a controllable clock; the
// default realClock is correct for production.
func WithClock(c Clock) Option {
	return func(b *bus) { b.clock = c }
}

// WithLogger injects the slog.Logger the loud-degradation path writes
// to. Defaults to slog.Default(). Tests inject a capturing handler so
// the warning is assertable.
func WithLogger(l *slog.Logger) Option {
	return func(b *bus) {
		if l != nil {
			b.logger = l
		}
	}
}

// withOwnStore marks the bus as the owner of the StateStore it was
// handed — Close then closes the store. Used only by the registry
// factory, which opens the store itself; the New-path leaves the
// caller as the store owner.
func withOwnStore() Option {
	return func(b *bus) { b.ownStore = true }
}

// New constructs the durable driver directly. Exposed for tests and
// for cmd/harbor's wiring path (which opens the StateStore and hands
// it in). When store is nil the driver runs in best-effort
// ring-buffer mode and emits a loud warning — see the package doc and
// D-074.
func New(cfg config.EventsConfig, r audit.Redactor, store state.StateStore, opts ...Option) (events.EventBus, error) {
	if r == nil {
		return nil, fmt.Errorf("durable: audit.Redactor required (got nil)")
	}
	if cfg.MaxSubscribersPerSession <= 0 {
		return nil, fmt.Errorf("durable: MaxSubscribersPerSession must be > 0")
	}
	if cfg.SubscriberBufferSize <= 0 {
		return nil, fmt.Errorf("durable: SubscriberBufferSize must be > 0")
	}
	if cfg.DropWindow <= 0 {
		return nil, fmt.Errorf("durable: DropWindow must be > 0")
	}
	if cfg.ReplayBufferSize < 0 {
		return nil, fmt.Errorf("durable: ReplayBufferSize must be >= 0 (best-effort ring size)")
	}
	b := &bus{
		cfg:      cfg,
		redactor: r,
		store:    store,
		clock:    realClock{},
		logger:   slog.Default(),
		ringCap:  cfg.ReplayBufferSize,
		subs:     map[uint64]*subscription{},
	}
	for _, opt := range opts {
		opt(b)
	}
	if b.store == nil {
		// Loud degradation — CLAUDE.md §13 forbids silent degradation.
		b.bestEffort = true
		if b.ringCap > 0 {
			b.ringBuf = make([]events.Event, b.ringCap)
		}
		b.logger.Warn("durable event log: no StateStore configured — degrading to best-effort in-memory ring buffer; replay is NOT durable across restarts",
			slog.String("driver", "durable"),
			slog.Int("ring_buffer_size", b.ringCap))
	}
	return b, nil
}

// init registers the durable factory. Because events.Factory does not
// carry a state.StateStore, the registry-path factory opens the
// StateStore itself from EventsConfig.StateDriver / StateDSN. An empty
// StateDriver routes to best-effort mode (the loud warning fires from
// New).
func init() {
	events.Register("durable", func(cfg config.EventsConfig, r audit.Redactor) (events.EventBus, error) {
		if cfg.StateDriver == "" {
			return New(cfg, r, nil)
		}
		store, err := state.Open(context.Background(), config.StateConfig{
			Driver: cfg.StateDriver,
			DSN:    cfg.StateDSN,
		})
		if err != nil {
			return nil, fmt.Errorf("durable: open StateStore driver %q: %w", cfg.StateDriver, err)
		}
		return New(cfg, r, store, withOwnStore())
	})
}

// bus is the durable driver. It is a compiled artifact: every field is
// set once at construction. Per-publish state lives under publishMu;
// per-subscriber state lives on the subscription. Nothing run-specific
// is stored on the struct (D-025).
type bus struct {
	cfg      config.EventsConfig
	redactor audit.Redactor
	store    state.StateStore // nil ⇒ best-effort mode
	clock    Clock
	logger   *slog.Logger

	bestEffort bool // true when store == nil
	ownStore   bool // true when this bus opened the StateStore (registry path)

	// publishMu serialises sequence assignment + persistence (or ring
	// append). Holding it across the StateStore writes guarantees the
	// head record's sequence list and the sequence counter never
	// disagree, and that the persisted log is in strict sequence
	// order.
	publishMu sync.Mutex
	nextSeq   uint64

	// Best-effort ring (used ONLY when bestEffort is true).
	ringBuf  []events.Event
	ringHead int
	ringFull bool
	ringCap  int

	mu    sync.RWMutex
	subs  map[uint64]*subscription
	subID atomic.Uint64

	closed atomic.Bool
}

// Publish validates, redacts, sequences, persists, and fans out ev.
func (b *bus) Publish(ctx context.Context, ev events.Event) error {
	if b.closed.Load() {
		return events.ErrBusClosed
	}
	if err := events.ValidateEvent(ev); err != nil {
		return err
	}

	// Audit-before-emit boundary (RFC §6.13, D-020). SafePayload
	// bypasses the redactor; everything else is walked.
	payload := ev.Payload
	if _, safe := payload.(events.SafePayload); !safe {
		redacted, err := b.redactor.Redact(ctx, payload)
		if err != nil {
			b.emitRedactionFailure(ctx, ev, err)
			return fmt.Errorf("durable: publish redaction failed: %w", err)
		}
		payload = wrapRedacted(redacted)
	}
	ev.Payload = payload

	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = b.clock.Now()
	}

	if err := b.sequenceAndStore(ctx, &ev); err != nil {
		return err
	}

	b.fanOut(ev)
	return nil
}

// sequenceAndStore assigns the next monotonic sequence to ev and
// persists it (durable mode) or appends it to the best-effort ring.
// Holds publishMu so the sequence counter, the persisted head list,
// and (in best-effort mode) the ring stay mutually consistent.
//
// A persistence failure surfaces loudly — the event is NOT enqueued
// and the error propagates to the Publish caller. Silently dropping a
// persistence failure would foreclose the gap-free guarantee Phase 57
// exists to provide (CLAUDE.md §5 "fail loudly", §13).
func (b *bus) sequenceAndStore(ctx context.Context, ev *events.Event) error {
	b.publishMu.Lock()
	defer b.publishMu.Unlock()

	seq := b.nextSeq + 1
	ev.Sequence = seq

	if b.bestEffort {
		if b.ringCap > 0 {
			b.ringAppendLocked(*ev)
		}
		b.nextSeq = seq
		return nil
	}

	if err := b.persistLocked(ctx, *ev); err != nil {
		// nextSeq is NOT advanced: the failed sequence is retried by
		// the next Publish, keeping the persisted log gap-free.
		return fmt.Errorf("durable: persist event seq=%d: %w", seq, err)
	}
	b.nextSeq = seq
	return nil
}

// persistLocked writes the entry record and advances the per-session
// head record. Caller holds publishMu.
func (b *bus) persistLocked(ctx context.Context, ev events.Event) error {
	entryBytes, err := encodeEvent(ev)
	if err != nil {
		return err
	}
	sessionID := sessionKey(ev.Identity)

	// 1. Write the immutable entry record.
	entryRec := state.StateRecord{
		ID:       state.NewEventID(),
		Identity: sessionID,
		Kind:     kindEntryPrefix + seqToken(ev.Sequence),
		Bytes:    entryBytes,
	}
	if err := b.store.Save(ctx, entryRec); err != nil {
		return fmt.Errorf("save entry record: %w", err)
	}

	// 2. Read-modify-write the head record's sequence list.
	head, err := b.loadHeadLocked(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load head record: %w", err)
	}
	head.Sequences = append(head.Sequences, ev.Sequence)
	headBytes, err := encodeHead(head)
	if err != nil {
		return fmt.Errorf("encode head record: %w", err)
	}
	headRec := state.StateRecord{
		ID:       state.NewEventID(),
		Identity: sessionID,
		Kind:     kindHead,
		Bytes:    headBytes,
	}
	if err := b.store.Save(ctx, headRec); err != nil {
		return fmt.Errorf("save head record: %w", err)
	}
	return nil
}

// loadHeadLocked returns the per-session head record, or a fresh empty
// head when none exists yet. Caller holds publishMu.
func (b *bus) loadHeadLocked(ctx context.Context, sessionID identity.Quadruple) (headRecord, error) {
	rec, err := b.store.Load(ctx, sessionID, kindHead)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return headRecord{}, nil
		}
		return headRecord{}, err
	}
	return decodeHead(rec.Bytes)
}

// ringAppendLocked writes ev to the next best-effort ring slot. Caller
// holds publishMu; called only when bestEffort && ringCap > 0.
func (b *bus) ringAppendLocked(ev events.Event) {
	b.ringBuf[b.ringHead] = ev
	b.ringHead++
	if b.ringHead >= b.ringCap {
		b.ringHead = 0
		b.ringFull = true
	}
}

// ringSnapshotLocked returns the best-effort ring contents in sequence
// order (oldest first). Caller holds publishMu.
func (b *bus) ringSnapshotLocked() []events.Event {
	if b.ringCap == 0 {
		return nil
	}
	if !b.ringFull {
		out := make([]events.Event, b.ringHead)
		copy(out, b.ringBuf[:b.ringHead])
		return out
	}
	out := make([]events.Event, b.ringCap)
	copy(out, b.ringBuf[b.ringHead:])
	copy(out[b.ringCap-b.ringHead:], b.ringBuf[:b.ringHead])
	return out
}

// Replay implements events.Replayer. Returns events whose Sequence is
// strictly greater than from.Sequence and that match f, in Sequence
// order.
//
// Durable mode: reads from the StateStore — exact and gap-free across
// restarts. Best-effort mode: reads from the in-memory ring and
// applies the same ErrCursorTooOld semantics as the inmem driver.
func (b *bus) Replay(ctx context.Context, from events.Cursor, f events.Filter) ([]events.Event, error) {
	if b.closed.Load() {
		return nil, events.ErrBusClosed
	}
	if !f.Admin && !f.HasFullTriple() {
		return nil, events.ErrIdentityScopeRequired
	}

	if f.Admin {
		// Mirror the inmem driver: surface admin-scope use on the bus
		// so abuse is retroactively detectable. Phase 61 adds the
		// cryptographic check.
		b.emitAdminScopeUsed(f)
	}

	if b.bestEffort {
		return b.replayBestEffort(from, f)
	}
	return b.replayDurable(ctx, from, f)
}

// replayDurable serves a replay from the StateStore.
//
// The cursor's SessionID selects which session's head record to read;
// when f is admin and from.SessionID is empty there is no single
// session to scan, so admin replay requires a SessionID on the cursor.
func (b *bus) replayDurable(ctx context.Context, from events.Cursor, f events.Filter) ([]events.Event, error) {
	session := from.SessionID
	if session == "" {
		session = f.Session
	}
	if session == "" {
		return nil, fmt.Errorf("%w: durable replay requires a SessionID on the cursor or filter",
			events.ErrIdentityScopeRequired)
	}

	// Resolve the session triple. Non-admin filters carry the full
	// triple; admin filters may only carry the session, so fall back
	// to the filter's tenant/user when present.
	sessionID := identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  f.Tenant,
			UserID:    f.User,
			SessionID: session,
		},
	}
	if sessionID.TenantID == "" || sessionID.UserID == "" {
		// Admin replay without a full triple cannot resolve the
		// storage key — the head record is keyed by the triple.
		return nil, fmt.Errorf("%w: durable replay requires the full identity triple on the filter",
			events.ErrIdentityScopeRequired)
	}

	head, err := b.loadHead(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("durable: replay load head: %w", err)
	}
	if len(head.Sequences) == 0 {
		return nil, nil
	}

	seqs := append([]uint64(nil), head.Sequences...)
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })

	out := make([]events.Event, 0, len(seqs))
	for _, seq := range seqs {
		if seq <= from.Sequence {
			continue
		}
		rec, err := b.store.Load(ctx, sessionID, kindEntryPrefix+seqToken(seq))
		if err != nil {
			if errors.Is(err, state.ErrNotFound) {
				// The head lists a sequence whose entry record is
				// missing — a torn write or a storage bug. Fail
				// loudly rather than serving a gap.
				return nil, fmt.Errorf("durable: replay gap — head lists seq=%d but entry record is missing: %w",
					seq, err)
			}
			return nil, fmt.Errorf("durable: replay load entry seq=%d: %w", seq, err)
		}
		ev, err := decodeEvent(rec.Bytes)
		if err != nil {
			return nil, fmt.Errorf("durable: replay decode entry seq=%d: %w", seq, err)
		}
		if !f.Matches(ev) {
			continue
		}
		out = append(out, ev)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// replayBestEffort serves a replay from the in-memory ring (no
// StateStore). Mirrors the inmem driver's cursor semantics.
func (b *bus) replayBestEffort(from events.Cursor, f events.Filter) ([]events.Event, error) {
	if b.ringCap == 0 {
		return nil, events.ErrReplayUnavailable
	}
	b.publishMu.Lock()
	snapshot := b.ringSnapshotLocked()
	headSeq := b.nextSeq
	b.publishMu.Unlock()

	if len(snapshot) == 0 {
		return nil, nil
	}
	oldestSeq := snapshot[0].Sequence
	if from.Sequence >= headSeq {
		return nil, nil
	}
	if from.Sequence > 0 && from.Sequence+1 < oldestSeq {
		return nil, fmt.Errorf("%w: oldest=%d requested=%d",
			events.ErrCursorTooOld, oldestSeq, from.Sequence)
	}
	out := make([]events.Event, 0, len(snapshot))
	for _, ev := range snapshot {
		if ev.Sequence <= from.Sequence {
			continue
		}
		if !f.Matches(ev) {
			continue
		}
		out = append(out, ev)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// loadHead reads a session's head record outside publishMu (used by
// Replay). Returns an empty head when none exists.
func (b *bus) loadHead(ctx context.Context, sessionID identity.Quadruple) (headRecord, error) {
	rec, err := b.store.Load(ctx, sessionID, kindHead)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return headRecord{}, nil
		}
		return headRecord{}, err
	}
	return decodeHead(rec.Bytes)
}

// Subscribe validates the filter, audits Admin scope, enforces the
// per-session subscriber cap, and returns a live Subscription.
func (b *bus) Subscribe(_ context.Context, f events.Filter) (events.Subscription, error) {
	if b.closed.Load() {
		return nil, events.ErrBusClosed
	}
	if !f.Admin && !f.HasFullTriple() {
		return nil, events.ErrIdentityScopeRequired
	}

	if !f.Admin {
		b.mu.RLock()
		count := 0
		for _, s := range b.subs {
			if s.cancelled.Load() {
				continue
			}
			if !s.filter.Admin &&
				s.filter.Tenant == f.Tenant &&
				s.filter.User == f.User &&
				s.filter.Session == f.Session {
				count++
			}
		}
		b.mu.RUnlock()
		if count >= b.cfg.MaxSubscribersPerSession {
			return nil, events.ErrSubscriberLimitReached
		}
	}

	id := b.subID.Add(1)
	bound := identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  f.Tenant,
			UserID:    f.User,
			SessionID: f.Session,
		},
	}
	s := &subscription{
		id:     id,
		filter: f,
		bound:  bound,
		ch:     make(chan events.Event, b.cfg.SubscriberBufferSize),
	}
	b.mu.Lock()
	b.subs[id] = s
	b.mu.Unlock()

	if f.Admin {
		b.emitAdminScopeUsed(f)
	}
	return s, nil
}

// fanOut walks subscribers and enqueues ev to each whose filter
// matches.
func (b *bus) fanOut(ev events.Event) {
	b.mu.RLock()
	matched := make([]*subscription, 0, len(b.subs))
	for _, s := range b.subs {
		if s.cancelled.Load() {
			continue
		}
		if s.filter.Matches(ev) {
			matched = append(matched, s)
		}
	}
	b.mu.RUnlock()
	for _, s := range matched {
		s.enqueue(ev, b)
	}
}

// emitAdminScopeUsed publishes the audit.admin_scope_used sibling
// event. The event is sequenced + persisted exactly like any other
// (best-effort persistence error is logged, not returned — the caller
// asked for a replay/subscribe, not a publish).
func (b *bus) emitAdminScopeUsed(f events.Filter) {
	ev := events.Event{
		Type: events.EventTypeAdminScopeUsed,
		Identity: identity.Quadruple{Identity: identity.Identity{
			TenantID:  f.Tenant,
			UserID:    f.User,
			SessionID: f.Session,
		}},
		OccurredAt: b.clock.Now(),
		Payload: events.AdminScopeUsedPayload{
			Tenant:  f.Tenant,
			User:    f.User,
			Session: f.Session,
		},
	}
	b.publishInternal(ev)
}

// emitRedactionFailure publishes the audit.redaction_failed sibling
// event with NO original payload bytes.
func (b *bus) emitRedactionFailure(_ context.Context, original events.Event, cause error) {
	ev := events.Event{
		Type:       events.EventTypeAuditRedactionFailed,
		Identity:   original.Identity,
		OccurredAt: b.clock.Now(),
		Payload: events.AuditRedactionFailedPayload{
			OriginalType: original.Type,
			Reason:       cause.Error(),
		},
	}
	b.publishInternal(ev)
}

// publishInternal sequences and fans out a bus-internal SafePayload
// notice (admin-scope-used, redaction-failed). These notices are
// per-call observability, NOT session event history — an
// admin_scope_used event for a fully-admin filter does not even carry
// a complete identity triple, so it cannot be a StateStore record.
// They are therefore assigned a bus sequence (for live ordering) and
// fanned out, but NOT persisted to the durable log. The durable log
// is the gap-free session history; transient notices are not part of
// it.
func (b *bus) publishInternal(ev events.Event) {
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = b.clock.Now()
	}
	b.publishMu.Lock()
	b.nextSeq++
	ev.Sequence = b.nextSeq
	b.publishMu.Unlock()
	b.fanOut(ev)
}

// Close idempotently shuts the bus down. After Close, Publish /
// Subscribe / Replay return ErrBusClosed and every live subscriber's
// channel is closed. Whether the StateStore is closed depends on
// ownership: the registry-path factory opens the store and marks the
// bus as its owner (Close then closes it); a caller that passes a
// store into New owns the store's lifecycle and Close leaves it open
// (D-074).
func (b *bus) Close(ctx context.Context) error {
	if b.closed.Swap(true) {
		return nil
	}
	b.mu.Lock()
	subs := make([]*subscription, 0, len(b.subs))
	for _, s := range b.subs {
		subs = append(subs, s)
	}
	b.subs = map[uint64]*subscription{}
	b.mu.Unlock()
	for _, s := range subs {
		s.cancel()
	}
	if b.ownStore && b.store != nil {
		if err := b.store.Close(ctx); err != nil {
			return fmt.Errorf("durable: close StateStore: %w", err)
		}
	}
	return nil
}

// wrapRedacted converts the audit redactor's output into a value
// satisfying events.EventPayload — mirrors the inmem driver.
func wrapRedacted(v any) events.EventPayload {
	if p, ok := v.(events.EventPayload); ok {
		return p
	}
	if m, ok := v.(map[string]any); ok {
		return events.RedactedMap{Data: m}
	}
	return events.RedactedMap{Data: map[string]any{"value": v}}
}

// sessionKey projects an event's Quadruple onto the session triple
// (RunID dropped) used as the StateStore key for the durable log. The
// event's own RunID is preserved inside the persisted bytes.
func sessionKey(q identity.Quadruple) identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  q.TenantID,
			UserID:    q.UserID,
			SessionID: q.SessionID,
		},
	}
}

// seqToken renders a bus sequence as a zero-padded fixed-width token
// so entry Kinds sort lexicographically in the same order as the
// numeric sequence (useful for any future scan-capable StateStore).
func seqToken(seq uint64) string {
	return fmt.Sprintf("%020d", seq)
}

func unixNanoToTime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// Compile-time assertions: bus implements both interfaces.
var (
	_ events.EventBus = (*bus)(nil)
	_ events.Replayer = (*bus)(nil)
)
