// Package durable is Harbor's StateStore-backed durable event log
// driver (RFC §6.13).
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
//   - At construction (durable mode) the monotonic sequence counter is
//     rehydrated from the persisted head records (recoverNextSeq, via the
//     StateStore.ListKind maintenance scan) so a Runtime rebuilt against
//     the same StateStore issues sequences strictly greater than any
//     pre-restart token — a client reconnecting at a high Last-Event-ID
//     is never silently skipped.
//   - When NO StateStore is configured the driver auto-degrades to a
//     best-effort in-memory ring buffer AND emits a loud runtime.warning
//     event plus an slog.Warn (CLAUDE.md §13 "no silent
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

// errEventFenced is the internal sentinel sequenceAndStore returns when
// an event's session triple is fenced (erased) — Publish maps it to a
// logged, successful no-op rather than persisting or fanning the event
// out (see events.Fencer).
var errEventFenced = errors.New("durable: event dropped — session is fenced (erased)")

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

// optWithOwnedStore marks the bus as the owner of the StateStore it
// was handed — Close then closes the store. Registry-internal: the
// registry-path factory below opens the store itself and uses this
// option to tell Close to dispose of it; the public `New(...)` path
// leaves the caller as the store owner. Name disambiguates from the
// public `Option`-typed accessors (WithClock / WithLogger) which are
// exported for tests + operator wiring.
func optWithOwnedStore() Option {
	return func(b *bus) { b.ownStore = true }
}

// New constructs the durable driver directly. Exposed for tests and
// for cmd/harbor's wiring path (which opens the StateStore and hands
// it in). When store is nil the driver runs in best-effort
// ring-buffer mode and emits a loud warning — see the package doc.
//
// In durable mode New does construction-time I/O: it rehydrates the
// monotonic sequence counter from the persisted head records (see
// recoverNextSeq) so post-restart sequences stay strictly greater than
// any pre-restart token. ctx is the first parameter because of that I/O
// (CLAUDE.md §5) and bounds the recovery scan. A scan or decode failure
// fails the construction loudly — the driver never silently starts the
// counter at 0 (CLAUDE.md §13).
func New(ctx context.Context, cfg config.EventsConfig, r audit.Redactor, store state.StateStore, opts ...Option) (events.EventBus, error) {
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
		// Best-effort mode persists nothing, so there is nothing to
		// rehydrate — recovery is a durable-mode-only step (Non-goals).
		return b, nil
	}
	if err := b.recoverNextSeq(ctx); err != nil {
		return nil, err
	}
	return b, nil
}

// recoverNextSeq rehydrates the monotonic sequence counter from the
// persisted log at construction (durable mode only). Without it, a
// Runtime rebuilt against the same StateStore would re-issue
// Sequence=1,2,3… and collide with pre-restart tokens, so a Protocol
// client reconnecting with a high Last-Event-ID would have every
// post-restart event silently skipped by Replay (RFC §6.13 gap-free,
// resumable-across-restart contract).
//
// It enumerates the per-session head records via the explicitly-elevated
// maintenance scan (StateStore.ListKind with MaintenanceScoped set — RFC
// §6.11), decodes each, and floors nextSeq at the GLOBAL maximum sequence
// across every record's Sequences list (0 for an empty log). It is
// another maintenance-scan consumer alongside the pause sweeper's
// crash-orphan rescan: it reads sequence numbers only, mutates nothing,
// and records the cross-identity scan via structured slog rather than a
// dedicated audit event (the sweeper's posture).
//
// Fail-loud (CLAUDE.md §13): a scan error or an undecodable head record
// returns a wrapped error so New fails the boot; the counter is never
// silently started at 0.
func (b *bus) recoverNextSeq(ctx context.Context) error {
	recs, err := b.store.ListKind(ctx, state.ListScope{MaintenanceScoped: true}, kindHead)
	if err != nil {
		return fmt.Errorf("durable: recover sequence counter: scan head records: %w", err)
	}
	var maxSeq uint64
	var minSeq uint64
	var minSeqOwner identity.Quadruple
	for _, rec := range recs {
		// ListKind matches kindHead as a literal PREFIX. Today only the
		// exact head kind starts with it (entry records use the disjoint
		// "events.durable.entry/" prefix), but a future sibling kind under
		// the same dotted stem (e.g. a "events.durable.head.checkpoint"
		// global-max record — the noted scaling follow-up) would be swept
		// in and fail decodeHead. Guard on the exact kind so the scan stays
		// correct if such a sibling lands.
		if rec.Kind != kindHead {
			continue
		}
		head, err := decodeHead(rec.Bytes)
		if err != nil {
			return fmt.Errorf("durable: recover sequence counter: decode head record (id=%s): %w", rec.ID, err)
		}
		for _, seq := range head.Sequences {
			if seq > maxSeq {
				maxSeq = seq
			}
			if minSeq == 0 || seq < minSeq {
				minSeq = seq
				minSeqOwner = rec.Identity
			}
		}
	}
	b.nextSeq = maxSeq
	// Seed the observed retention horizon from the persisted head — the
	// oldest event's OccurredAt. The log is gap-free and untrimmed, so the
	// global-minimum sequence IS the head; loading its one entry record
	// (not a table scan) is the cheap min-index read.
	if minSeq > 0 {
		if err := b.recoverOldestRetained(ctx, minSeqOwner, minSeq); err != nil {
			return err
		}
	}
	b.logger.Info("durable event log: rehydrated sequence counter from persisted head records",
		slog.String("driver", "durable"),
		slog.Uint64("recovered_max_sequence", maxSeq),
		slog.Int("session_count", len(recs)))
	return nil
}

// recoverOldestRetained loads the persisted entry for the global-minimum
// sequence and seeds oldestRetainedAt from its OccurredAt. A decode /
// load failure fails the boot loudly (CLAUDE.md §13) — the horizon is
// never silently started at zero when the log is non-empty. A
// not-found entry for a sequence its head record claims is a corrupt log
// and also fails loud.
//
// Best-effort, under-claims-safe: the seed reads the oldest-by-SEQUENCE
// entry, while the live floor (sequenceAndStore) tracks oldest-by-
// OccurredAt. These coincide when events were persisted in occurrence
// order (the normal case); if a caller published an out-of-order
// OccurredAt, the two can differ. The horizon is a coarse operational
// signal, so this errs safe — the seed is the first persisted event's
// stamp and any later-published-but-earlier-stamped event only pulls the
// floor further back — so the reported horizon never claims MORE
// retention than the log actually holds.
func (b *bus) recoverOldestRetained(ctx context.Context, owner identity.Quadruple, seq uint64) error {
	rec, err := b.store.Load(ctx, owner, kindEntryPrefix+seqToken(seq))
	if err != nil {
		return fmt.Errorf("durable: recover retention horizon: load oldest entry seq=%d: %w", seq, err)
	}
	ev, err := decodeEvent(rec.Bytes)
	if err != nil {
		return fmt.Errorf("durable: recover retention horizon: decode oldest entry seq=%d: %w", seq, err)
	}
	b.oldestRetainedAt = ev.OccurredAt
	return nil
}

// init registers the durable factory. Because events.Factory does not
// carry a state.StateStore, the registry-path factory opens the
// StateStore itself from EventsConfig.StateDriver / StateDSN.
//
// Per the CLAUDE.md §13 "Test stubs as production defaults on
// operator-facing seams" rule, the
// registry-path factory fails LOUD AT BOOT when StateDriver is
// empty rather than auto-degrading to the in-memory ring. An
// operator who explicitly selected `events.driver = "durable"` has
// signalled they want durability; silently producing a non-durable
// bus is exactly the operator-confusion failure mode §13 forbids.
// Configurations that want the in-memory ring select
// `events.driver = "inmem"`; tests that exercise the degraded mode
// keep the in-process `New(..., store=nil, ...)` constructor below.
func init() {
	events.Register("durable", func(cfg config.EventsConfig, r audit.Redactor) (events.EventBus, error) {
		if cfg.StateDriver == "" {
			return nil, fmt.Errorf("durable: events.state_driver is required when driver=durable; configure a state driver (e.g. inmem, sqlite, postgres) or pick events.driver=inmem")
		}
		return newWithOwnedStore(cfg, r)
	})
	// the deps-aware factory: when the runtime
	// hands its already-open StateStore through `events.OpenWith`, the
	// durable log persists into the SAME store the rest of the runtime
	// uses, and the bus's Close leaves the shared store open (the
	// caller owns it). Precedence:
	//
	//  1. An explicit `events.state_driver` wins — the operator asked
	//     for a dedicated event-log store; the factory opens (and owns)
	//     it exactly like the plain registry path.
	//  2. Otherwise a non-nil deps.State is shared (not owned).
	//  3. Otherwise fail loud — same posture as the plain factory; the
	//     error names both ways out (the §13 fail-loud-at-boot posture
	//     carried forward: an operator who selected `durable` signalled
	//     they want durability; silently degrading is forbidden).
	events.RegisterWithDeps("durable", func(cfg config.EventsConfig, r audit.Redactor, deps events.Deps) (events.EventBus, error) {
		if cfg.StateDriver != "" {
			return newWithOwnedStore(cfg, r)
		}
		if deps.State != nil {
			// The events.RegisterWithDeps factory contract is ctx-free, so
			// the recovery ctx bridges with context.Background() — the §5
			// unmanaged-async-boundary case, matching newWithOwnedStore's
			// state.Open precedent. Threading a real boot ctx through the
			// factory contract is the tracked follow-up.
			return New(context.Background(), cfg, r, deps.State)
		}
		return nil, fmt.Errorf("durable: no StateStore available — set events.state_driver (dedicated event-log store) or call events.OpenWith with Deps.State (share the runtime's store), or pick events.driver=inmem")
	})
}

// newWithOwnedStore opens a dedicated StateStore from
// cfg.StateDriver/cfg.StateDSN and constructs the bus as its owner —
// Close disposes of the store. Shared by the plain and deps-aware
// factory paths when the operator configured an explicit driver.
func newWithOwnedStore(cfg config.EventsConfig, r audit.Redactor) (events.EventBus, error) {
	// One boot ctx for both the store open and the sequence-recovery scan
	// (the §5 unmanaged-async-boundary bridge — the events.Register factory
	// contract is ctx-free).
	ctx := context.Background()
	store, err := state.Open(ctx, config.StateConfig{
		Driver: cfg.StateDriver,
		DSN:    cfg.StateDSN,
	})
	if err != nil {
		return nil, fmt.Errorf("durable: open StateStore driver %q: %w", cfg.StateDriver, err)
	}
	return New(ctx, cfg, r, store, optWithOwnedStore())
}

// bus is the durable driver. It is a compiled artifact: every field is
// set once at construction. Per-publish state lives under publishMu;
// per-subscriber state lives on the subscription. Nothing run-specific
// is stored on the struct.
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
	// oldestRetainedAt is the observed retention horizon for durable
	// mode — the OccurredAt of the oldest persisted event, seeded at boot
	// from the persisted log (recoverOldestRetained) and floored on every
	// persist. Because the durable log is gap-free and untrimmed in V1 it
	// only ever moves earlier (the boot seed IS the head), but the floor
	// keeps it correct if a boot seed was skipped. Guarded by publishMu —
	// the same lock the persist path holds — so the horizon, the sequence
	// counter, and the persisted log never disagree. Zero when the log is
	// empty. Unused in best-effort mode (the ring is the source there).
	oldestRetainedAt time.Time

	// Best-effort ring (used ONLY when bestEffort is true).
	ringBuf  []events.Event
	ringHead int
	ringFull bool
	// evicted is the precise "history became lossy" signal for the
	// best-effort ring: true once an append has overwritten a
	// previously-occupied slot (distinct from ringFull, which is true at
	// exactly-capacity before any eviction). Flows to
	// HistoryReplayer.Bounds' truncated return.
	evicted bool
	ringCap int

	mu    sync.RWMutex
	subs  map[uint64]*subscription
	subID atomic.Uint64

	// fenceMu guards fenced — the set of erased session triples (see
	// events.Fencer). A fenced triple drops new events on the persist path
	// and reads as empty history. Held briefly; never nested under publishMu
	// EXCEPT in the documented publishMu→fenceMu order (Fence + the persist
	// fenced-check both take publishMu first, so the order is consistent and
	// deadlock-free).
	fenceMu sync.RWMutex
	fenced  map[string]struct{}

	closed atomic.Bool
}

// fenceKey renders a session triple as the fenced-set key. The NUL
// separator can never appear in a tenant/user/session id, so distinct
// triples never collide.
func fenceKey(id identity.Identity) string {
	return id.TenantID + "\x00" + id.UserID + "\x00" + id.SessionID
}

// isFenced reports whether the event's session triple is fenced (erased).
func (b *bus) isFenced(id identity.Quadruple) bool {
	b.fenceMu.RLock()
	defer b.fenceMu.RUnlock()
	if b.fenced == nil {
		return false
	}
	_, ok := b.fenced[fenceKey(id.Identity)]
	return ok
}

// Fence implements events.Fencer. It marks the triple erased so the
// persist path drops its future events and its history reads empty. It
// acquires publishMu first so any in-flight Publish for the triple has
// finished persisting before Fence returns — the cascade's DeleteScope
// then sweeps that just-persisted event; nothing is retained past the
// fence.
func (b *bus) Fence(_ context.Context, id identity.Identity) error {
	if b.closed.Load() {
		return events.ErrBusClosed
	}
	b.publishMu.Lock()
	defer b.publishMu.Unlock()
	b.fenceMu.Lock()
	defer b.fenceMu.Unlock()
	if b.fenced == nil {
		b.fenced = map[string]struct{}{}
	}
	b.fenced[fenceKey(id)] = struct{}{}
	return nil
}

// Unfence implements events.Fencer. It lifts a fence so a reused session
// id opened afresh retains events normally. Idempotent.
func (b *bus) Unfence(_ context.Context, id identity.Identity) error {
	if b.closed.Load() {
		return events.ErrBusClosed
	}
	b.fenceMu.Lock()
	defer b.fenceMu.Unlock()
	delete(b.fenced, fenceKey(id))
	return nil
}

// OldestRetainedAt implements events.RetentionReporter. In durable mode
// it returns the seeded/floored oldestRetainedAt — the OccurredAt of the
// oldest persisted event; present is false when the log is empty. In
// best-effort mode it reads the in-memory ring (like the inmem driver),
// so the horizon advances as the ring evicts. Bus-internal notices are
// excluded so the horizon matches a session read's history shape.
func (b *bus) OldestRetainedAt(_ context.Context) (time.Time, bool, error) {
	if b.closed.Load() {
		return time.Time{}, false, events.ErrBusClosed
	}
	b.publishMu.Lock()
	defer b.publishMu.Unlock()
	if b.bestEffort {
		snapshot := b.ringSnapshotLocked()
		for _, ev := range snapshot {
			if events.IsBusInternalNotice(ev.Type) {
				continue
			}
			return ev.OccurredAt, true, nil
		}
		return time.Time{}, false, nil
	}
	if b.oldestRetainedAt.IsZero() {
		return time.Time{}, false, nil
	}
	return b.oldestRetainedAt, true, nil
}

// Publish validates, redacts, sequences, persists, and fans out ev.
//
// Publish honours ctx (CLAUDE.md §5): a cancelled caller context is
// rejected up front, BEFORE anything is persisted — the publish ctx
// drives store.Save, and the caller's ctx is the lifetime bound for
// the persistence (the run-loop drivers' emit closures publish
// under their driver-lifetime ctx, so events stop persisting at driver
// teardown regardless of whether the configured StateStore driver
// itself reads ctx — the inmem store does no I/O and ignores it).
func (b *bus) Publish(ctx context.Context, ev events.Event) error {
	if b.closed.Load() {
		return events.ErrBusClosed
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("durable: publish cancelled: %w", err)
	}
	if err := events.ValidateEvent(ev); err != nil {
		return err
	}

	// Audit-before-emit boundary (RFC §6.13). SafePayload
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
		if errors.Is(err, errEventFenced) {
			// The session was erased while this event was in flight. Dropping
			// it is the CORRECT outcome (the erased session has no history to
			// land in), not silent degradation — it is logged loudly so the
			// drop is observable (CLAUDE.md §13).
			b.logger.InfoContext(ctx, "durable: dropped event for erased (fenced) session",
				slog.String("driver", "durable"),
				slog.String("event_type", string(ev.Type)),
				slog.String("tenant_id", ev.Identity.TenantID),
				slog.String("user_id", ev.Identity.UserID),
				slog.String("session_id", ev.Identity.SessionID))
			return nil
		}
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
// persistence failure would foreclose the gap-free guarantee this driver
// exists to provide (CLAUDE.md §5 "fail loudly", §13).
func (b *bus) sequenceAndStore(ctx context.Context, ev *events.Event) error {
	b.publishMu.Lock()
	defer b.publishMu.Unlock()

	// Fenced-session drop. Checked under publishMu (the same lock Fence
	// acquires) so the fence and the persist never disagree: an event that
	// reaches here after Fence took hold is dropped and never retained.
	if b.isFenced(ev.Identity) {
		return errEventFenced
	}

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
	// Floor the observed retention horizon at the oldest persisted event.
	// The untrimmed log means the first persisted event is the head, so
	// this only sets the horizon on the empty-log edge (or an earlier
	// out-of-order OccurredAt); later events are newer and leave it.
	if b.oldestRetainedAt.IsZero() || ev.OccurredAt.Before(b.oldestRetainedAt) {
		b.oldestRetainedAt = ev.OccurredAt
	}
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
	if b.ringFull {
		// The ring is already at capacity, so this append overwrites the
		// oldest retained event — the first actual eviction.
		b.evicted = true
	}
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
		// so abuse is retroactively detectable. Harbor adds the
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
	if b.isFenced(sessionID) {
		// Erased session — no history to replay (events.Fencer).
		return nil, nil
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

// Bounds implements events.HistoryReplayer. It returns the lowest (head)
// and highest (tail) retained sequence for the filter's session, or
// events.ErrNoHistory when the session has no retained events. It is
// identity-scoped exactly like Replay.
func (b *bus) Bounds(ctx context.Context, f events.Filter) (head, tail uint64, truncated bool, err error) {
	if b.closed.Load() {
		return 0, 0, false, events.ErrBusClosed
	}
	if !f.Admin && !f.HasFullTriple() {
		return 0, 0, false, events.ErrIdentityScopeRequired
	}
	if f.Admin {
		b.emitAdminScopeUsed(f)
	}
	if b.bestEffort {
		return b.boundsBestEffort(f)
	}
	sessionID, err := resolveWindowSessionKey(f)
	if err != nil {
		return 0, 0, false, err
	}
	if b.isFenced(sessionID) {
		// Erased session — reads as no retained history (events.Fencer).
		return 0, 0, false, events.ErrNoHistory
	}
	hd, err := b.loadHead(ctx, sessionID)
	if err != nil {
		return 0, 0, false, fmt.Errorf("durable: bounds load head: %w", err)
	}
	lo, hi, ok := minMaxSeq(hd.Sequences)
	if !ok {
		return 0, 0, false, events.ErrNoHistory
	}
	// The durable (persisted) log keeps the per-session sequence list
	// gap-free and untrimmed in V1, so the retained head IS the session's
	// first sequence — never truncated.
	return lo, hi, false, nil
}

// Window implements events.HistoryReplayer. It returns at most limit
// events whose Sequence < before (before==0 ⇒ from the tail), the
// most-recent K selected, returned oldest-first within the window,
// matching f. It is a by-id read scoped to the named session
// (MatchesScoped) even under Admin.
//
// Window does NOT emit audit.admin_scope_used: the handler always calls
// Bounds first, which emits it once per state.history request (a paired
// Bounds+Window must not double-audit a single read).
func (b *bus) Window(ctx context.Context, before uint64, limit int, f events.Filter) ([]events.Event, error) {
	if b.closed.Load() {
		return nil, events.ErrBusClosed
	}
	if !f.Admin && !f.HasFullTriple() {
		return nil, events.ErrIdentityScopeRequired
	}
	if limit <= 0 {
		return nil, nil
	}
	if b.bestEffort {
		return b.windowBestEffort(before, limit, f)
	}
	sessionID, err := resolveWindowSessionKey(f)
	if err != nil {
		return nil, err
	}
	if b.isFenced(sessionID) {
		// Erased session — no retained history window (events.Fencer).
		return nil, nil
	}
	head, err := b.loadHead(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("durable: window load head: %w", err)
	}
	if len(head.Sequences) == 0 {
		return nil, nil
	}
	seqs := append([]uint64(nil), head.Sequences...)
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] > seqs[j] }) // descending: newest first

	// Walk newest-first, skip sequences at/above `before`, collect up to
	// `limit` MATCHING events, then reverse to oldest-first.
	out := make([]events.Event, 0, limit)
	for _, seq := range seqs {
		if before != 0 && seq >= before {
			continue
		}
		rec, err := b.store.Load(ctx, sessionID, kindEntryPrefix+seqToken(seq))
		if err != nil {
			if errors.Is(err, state.ErrNotFound) {
				// The head lists a sequence whose entry record is missing —
				// a torn write or storage bug. Fail loudly rather than
				// serving a gap (RFC §6.13 / CLAUDE.md §13).
				return nil, fmt.Errorf("durable: window gap — head lists seq=%d but entry record is missing: %w",
					seq, err)
			}
			return nil, fmt.Errorf("durable: window load entry seq=%d: %w", seq, err)
		}
		ev, err := decodeEvent(rec.Bytes)
		if err != nil {
			return nil, fmt.Errorf("durable: window decode entry seq=%d: %w", seq, err)
		}
		if events.IsBusInternalNotice(ev.Type) || !f.MatchesScoped(ev) {
			continue
		}
		out = append(out, ev)
		if len(out) >= limit {
			break
		}
	}
	reverseEvents(out)
	return out, nil
}

// ListWindow implements events.HistoryReplayer — the cross-session,
// time-ranged, cursor-paged `events.list` read. Unlike Bounds/Window
// (by-id, single session via MatchesScoped), it honours the wire filter's
// multi-valued identity sets + since/until bounds (MatchWire) and, when
// q.Admin is set, fans IN across sessions in GLOBAL-SEQUENCE order. A
// non-admin query whose filter elides an identity axis is rejected with
// ErrIdentityScopeRequired (fail closed). A widened (Admin) query emits one
// audit.admin_scope_used before returning — the per-request audit that
// makes the sanctioned fan-in observable.
//
// The durable persisted log never trims the per-session sequence list in
// V1, so a durable-mode page reports truncated=false. In best-effort ring
// mode (no StateStore) it reports truncated=evicted, exactly like
// boundsBestEffort.
func (b *bus) ListWindow(ctx context.Context, q events.EventListQuery) (events.EventListPage, error) {
	if b.closed.Load() {
		return events.EventListPage{}, events.ErrBusClosed
	}
	if !q.Admin && !events.WireFilterHasFullTriple(q.Filter) {
		return events.EventListPage{}, events.ErrIdentityScopeRequired
	}
	if q.Admin {
		b.emitAdminScopeUsed(events.Filter{
			Tenant:  events.WireFilterFirst(q.Filter.TenantIDs),
			User:    events.WireFilterFirst(q.Filter.UserIDs),
			Session: events.WireFilterFirst(q.Filter.SessionIDs),
		})
	}
	if q.Limit <= 0 {
		return events.EventListPage{}, nil
	}
	if b.bestEffort {
		// Best-effort mode with the ring disabled (ReplayBufferSize 0) has
		// no windowed-read substrate — fail loud, never a silent empty page
		// (mirrors windowBestEffort / the inmem driver).
		if b.ringCap == 0 {
			return events.EventListPage{}, events.ErrReplayUnavailable
		}
		b.publishMu.Lock()
		snapshot := b.ringSnapshotLocked()
		evicted := b.evicted
		b.publishMu.Unlock()
		page := events.ListWindowFromSnapshot(snapshot, q.Before, q.Limit, q.Filter)
		page.Truncated = evicted
		return page, nil
	}
	return b.listWindowDurable(ctx, q)
}

// listWindowDurable serves the events.list page from the persisted log. It
// gathers the candidate session head records (the single named session for
// a non-admin read; every session head via the maintenance ListKind scan
// for a fleet read), merges their sequences in global-descending order,
// then loads entries lazily — MatchWire filtering per event — collecting up
// to limit+1 matches so HasMore is exact. A pathological fleet window is
// bounded by limit per page (the load loop stops at limit+1 matches); no
// unbounded entry scan runs.
func (b *bus) listWindowDurable(ctx context.Context, q events.EventListQuery) (events.EventListPage, error) {
	type sessionHead struct {
		id   identity.Quadruple
		seqs []uint64
	}
	var heads []sessionHead

	if !q.Admin {
		// The handler folded the caller's triple; the filter names exactly
		// one session. Resolve it and load just that head (no maintenance
		// scan for the common own-session read).
		quad := identity.Quadruple{Identity: identity.Identity{
			TenantID:  events.WireFilterFirst(q.Filter.TenantIDs),
			UserID:    events.WireFilterFirst(q.Filter.UserIDs),
			SessionID: events.WireFilterFirst(q.Filter.SessionIDs),
		}}
		if b.isFenced(quad) {
			return events.EventListPage{}, nil
		}
		hd, err := b.loadHead(ctx, quad)
		if err != nil {
			return events.EventListPage{}, fmt.Errorf("durable: events.list load head: %w", err)
		}
		heads = append(heads, sessionHead{id: quad, seqs: hd.Sequences})
	} else {
		recs, err := b.store.ListKind(ctx, state.ListScope{MaintenanceScoped: true}, kindHead)
		if err != nil {
			return events.EventListPage{}, fmt.Errorf("durable: events.list scan head records: %w", err)
		}
		for _, rec := range recs {
			// ListKind matches kindHead as a literal PREFIX — guard on the
			// exact kind so a future sibling kind under the same stem is not
			// mis-decoded (mirrors recoverNextSeq).
			if rec.Kind != kindHead {
				continue
			}
			// Cheap identity pre-filter on the head's session triple: skip
			// sessions the filter's identity sets exclude before loading any
			// entry. Run + event-type + time predicates run per event below.
			if !events.WireFilterMatchesTriple(q.Filter, rec.Identity.Identity) {
				continue
			}
			if b.isFenced(rec.Identity) {
				continue
			}
			hd, err := decodeHead(rec.Bytes)
			if err != nil {
				return events.EventListPage{}, fmt.Errorf("durable: events.list decode head (id=%s): %w", rec.ID, err)
			}
			heads = append(heads, sessionHead{id: rec.Identity, seqs: hd.Sequences})
		}
	}

	// Merge candidate (session, seq) pairs below the cursor, global-
	// descending by sequence.
	type candidate struct {
		id  identity.Quadruple
		seq uint64
	}
	var cands []candidate
	for _, h := range heads {
		for _, seq := range h.seqs {
			if q.Before != 0 && seq >= q.Before {
				continue
			}
			cands = append(cands, candidate{id: h.id, seq: seq})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].seq > cands[j].seq })

	matches := make([]events.Event, 0, q.Limit+1)
	for _, c := range cands {
		if err := ctx.Err(); err != nil {
			return events.EventListPage{}, err
		}
		rec, err := b.store.Load(ctx, c.id, kindEntryPrefix+seqToken(c.seq))
		if err != nil {
			if errors.Is(err, state.ErrNotFound) {
				// The head lists a sequence whose entry record is missing —
				// a torn write or storage bug. Fail loudly rather than
				// serving a gap (RFC §6.13 / CLAUDE.md §13).
				return events.EventListPage{}, fmt.Errorf("durable: events.list gap — head lists seq=%d but entry record is missing: %w",
					c.seq, err)
			}
			return events.EventListPage{}, fmt.Errorf("durable: events.list load entry seq=%d: %w", c.seq, err)
		}
		ev, err := decodeEvent(rec.Bytes)
		if err != nil {
			return events.EventListPage{}, fmt.Errorf("durable: events.list decode entry seq=%d: %w", c.seq, err)
		}
		if events.IsBusInternalNotice(ev.Type) || !events.MatchWire(ev, q.Filter) {
			continue
		}
		matches = append(matches, ev)
		if len(matches) > q.Limit {
			break
		}
	}

	var page events.EventListPage
	if len(matches) > q.Limit {
		page.HasMore = true
		matches = matches[:q.Limit]
		page.NextCursor = matches[len(matches)-1].Sequence // lowest in page
	}
	reverseEvents(matches)
	page.Events = matches
	// The durable persisted log never trims — truncated is always false.
	return page, nil
}

// boundsBestEffort reports the head/tail from the in-memory ring. The
// best-effort ring EVICTS on overflow, so it reports truncated=evicted:
// once an append has overwritten an occupied slot, older events were
// dropped and the retained head is NOT the session's first sequence (the
// honest never-silently-lossy signal — CLAUDE.md §13).
func (b *bus) boundsBestEffort(f events.Filter) (uint64, uint64, bool, error) {
	if b.ringCap == 0 {
		return 0, 0, false, events.ErrReplayUnavailable
	}
	b.publishMu.Lock()
	snapshot := b.ringSnapshotLocked()
	evicted := b.evicted
	b.publishMu.Unlock()
	var lo, hi uint64
	found := false
	for _, ev := range snapshot {
		if events.IsBusInternalNotice(ev.Type) || !f.MatchesScoped(ev) {
			continue
		}
		if !found || ev.Sequence < lo {
			lo = ev.Sequence
		}
		if ev.Sequence > hi {
			hi = ev.Sequence
		}
		found = true
	}
	if !found {
		return 0, 0, false, events.ErrNoHistory
	}
	return lo, hi, evicted, nil
}

// windowBestEffort serves a backward window from the in-memory ring.
func (b *bus) windowBestEffort(before uint64, limit int, f events.Filter) ([]events.Event, error) {
	if b.ringCap == 0 {
		return nil, events.ErrReplayUnavailable
	}
	b.publishMu.Lock()
	snapshot := b.ringSnapshotLocked()
	b.publishMu.Unlock()
	return windowFromSnapshot(snapshot, before, limit, f), nil
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

// publishInternal fans out a bus-internal SafePayload notice
// (admin-scope-used, redaction-failed). These notices are per-call
// observability, NOT session event history — an admin_scope_used event
// for a fully-admin filter does not even carry a complete identity
// triple, so it cannot be a StateStore record. They are fanned out but
// NOT persisted to the durable log.
//
// A transient notice carries NO replay position: it is assigned the
// non-replayable sentinel Sequence == 0 and does NOT advance nextSeq (the
// shared persisted-replay counter). The SSE transport omits the id: line
// for Sequence == 0, so a reconnecting client can never anchor
// Last-Event-ID on a transient tick — which the post-restart recovery
// floor (max persisted) would not exceed, leaving the next persisted
// event silently skipped by Replay. The highest sequence ever surfaced
// with an id: line is therefore exactly the max persisted sequence.
// Live fan-out order is unchanged: notices still arrive in call order.
func (b *bus) publishInternal(ev events.Event) {
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = b.clock.Now()
	}
	ev.Sequence = 0
	b.fanOut(ev)
}

// Close idempotently shuts the bus down. After Close, Publish /
// Subscribe / Replay return ErrBusClosed and every live subscriber's
// channel is closed. Whether the StateStore is closed depends on
// ownership: the registry-path factory opens the store and marks the
// bus as its owner (Close then closes it); a caller that passes a
// store into New owns the store's lifecycle and Close leaves it open.
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

// resolveWindowSessionKey resolves the StateStore key (the session
// triple) the windowed read scans. The durable log is keyed by the full
// triple, so a non-admin filter carries it directly and an admin filter
// must still name a tenant/user/session to resolve the storage key —
// mirrors replayDurable's resolution.
func resolveWindowSessionKey(f events.Filter) (identity.Quadruple, error) {
	if f.Session == "" {
		return identity.Quadruple{}, fmt.Errorf("%w: windowed read requires a SessionID on the filter",
			events.ErrIdentityScopeRequired)
	}
	if f.Tenant == "" || f.User == "" {
		return identity.Quadruple{}, fmt.Errorf("%w: windowed read requires the full identity triple on the filter",
			events.ErrIdentityScopeRequired)
	}
	return identity.Quadruple{Identity: identity.Identity{
		TenantID:  f.Tenant,
		UserID:    f.User,
		SessionID: f.Session,
	}}, nil
}

// minMaxSeq returns the lowest and highest values in seqs, or ok=false
// when seqs is empty.
func minMaxSeq(seqs []uint64) (lo, hi uint64, ok bool) {
	for i, s := range seqs {
		if i == 0 {
			lo, hi = s, s
			continue
		}
		if s < lo {
			lo = s
		}
		if s > hi {
			hi = s
		}
	}
	return lo, hi, len(seqs) > 0
}

// reverseEvents reverses out in place (newest-first → oldest-first).
func reverseEvents(out []events.Event) {
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
}

// windowFromSnapshot selects, from a sequence-ordered snapshot
// (oldest-first), at most limit MATCHING events with Sequence < before
// (before==0 ⇒ from the tail), the most-recent K, returned oldest-first.
func windowFromSnapshot(snapshot []events.Event, before uint64, limit int, f events.Filter) []events.Event {
	if limit <= 0 || len(snapshot) == 0 {
		return nil
	}
	out := make([]events.Event, 0, limit)
	for i := len(snapshot) - 1; i >= 0; i-- {
		ev := snapshot[i]
		if before != 0 && ev.Sequence >= before {
			continue
		}
		if events.IsBusInternalNotice(ev.Type) || !f.MatchesScoped(ev) {
			continue
		}
		out = append(out, ev)
		if len(out) >= limit {
			break
		}
	}
	reverseEvents(out)
	return out
}

// Compile-time assertions: bus implements all three interfaces.
var (
	_ events.EventBus        = (*bus)(nil)
	_ events.Replayer        = (*bus)(nil)
	_ events.HistoryReplayer = (*bus)(nil)
	_ events.Fencer          = (*bus)(nil)
)
