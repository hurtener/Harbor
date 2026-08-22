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
//   - In durable mode a reserved StateStore authority record allocates the
//     global sequence across independent Runtime processes. Construction
//     adopts/floors that authority from legacy head records, so restart and
//     rolling overlap never reuse a token.
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
//     ordered list of bus-sequences persisted for that session plus the
//     typed routing metadata projection for each sequence.
//   - Entry record: Kind = "events.durable.entry/<seq>" — holds the
//     JSON-encoded event for bus-sequence <seq>.
//
// On Publish, one mandatory StateStore SaveBatchIf transaction conditionally
// advances the global authority and commits the opaque body plus conditional
// session head. No partial body/head/authority state is visible. An ambiguous
// transaction acknowledgement poisons the process until restart; restart reads
// the authority and backfills legacy metadata with generation checks.
//
// The driver is registered under name "durable" via init(); cmd/harbor
// blank-imports this package so the registration fires at process
// startup. Per CLAUDE.md §4.4.
package durable

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
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
	// kindSequenceAuthority is a reserved internal StateStore slot containing
	// the global durable event sequence. Its identity is deliberately outside
	// normal session identities and its kind cannot collide with event heads or
	// bodies.
	kindSequenceAuthority = state.InternalKindPrefix + "events/global-sequence-authority"
	kindFencePrefix       = state.InternalKindPrefix + "events/session-fence/"
	// metadataValidationBatchSize bounds canonical payload validation before
	// a durable checkpoint is written. A cancelled/restarted runtime resumes
	// after the validated prefix instead of repeating the whole head scan.
	metadataValidationBatchSize = 128
	// recoveryStableViewMaxAttempts bounds boot catch-up under a continuously
	// changing head set. Two identical consecutive views are required before
	// nextSeq is admitted; persistent churn fails boot loudly rather than
	// risking sequence reuse.
	recoveryStableViewMaxAttempts = 8
	// recoveryRetryDelay prevents a conflicting writer from turning boot
	// catch-up into a tight retry loop while remaining negligible normally.
	recoveryRetryDelay = time.Millisecond
	// publishCASMaxAttempts bounds cross-runtime contention. The ceiling admits
	// the required N=100 concurrent publishers while failing loudly under
	// sustained churn instead of spinning forever.
	publishCASMaxAttempts = 256
)

var sequenceAuthorityIdentity = identity.InternalCoordinationQuadruple()

type sequenceAuthorityRecord struct {
	Sequence uint64 `json:"sequence"`
}

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
// §6.11), decodes each, and requires two identical consecutive generations
// before flooring nextSeq at the GLOBAL maximum sequence (0 for an empty
// log). Conditional adoption conflicts restart the bounded scan. It is
// another maintenance-scan consumer alongside the pause sweeper's
// crash-orphan rescan. In addition to the sequence floor it adopts legacy
// metadata heads through bounded, conditional checkpoints; the cross-
// identity scan is recorded via structured slog rather than a dedicated
// audit event (the sweeper's posture).
//
// Fail-loud (CLAUDE.md §13): a scan error or an undecodable head record
// returns a wrapped error so New fails the boot; the counter is never
// silently started at 0.
func (b *bus) recoverNextSeq(ctx context.Context) error {
	var previous recoveryHeadView
	for attempt := 1; attempt <= recoveryStableViewMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("durable: recover sequence counter: catch-up cancelled: %w", err)
		}
		current, err := b.scanRecoveryHeadView(ctx)
		if err != nil {
			if errors.Is(err, state.ErrConditionFailed) {
				previous = recoveryHeadView{}
			} else {
				return err
			}
		} else if previous.generations != nil && recoveryViewsEqual(previous.generations, current.generations) {
			_, authorityExpectation, authorityErr := b.loadSequenceAuthority(ctx)
			if authorityErr != nil {
				return fmt.Errorf("durable: inspect sequence authority: %w", authorityErr)
			}
			if authorityExpectation.ExpectedEventID == "" && !b.cfg.LegacyWritersDrained {
				return fmt.Errorf("durable: sequence authority is absent; events.legacy_writers_drained=true is required after every v1.29.0 event writer sharing this StateStore has stopped, even when the head scan is empty; rolling writer overlap is unsafe")
			}
			authoritySeq, err := b.adoptSequenceAuthority(ctx, current.maxSeq)
			if err != nil {
				return fmt.Errorf("durable: recover sequence authority: %w", err)
			}
			b.nextSeq = authoritySeq
			if current.minSeq > 0 {
				if err := b.recoverOldestRetained(ctx, current.minSeqOwner, current.minSeq); err != nil {
					return err
				}
			}
			b.logger.Info("durable event log: rehydrated sequence counter from stable persisted head view",
				slog.String("driver", "durable"),
				slog.Uint64("recovered_max_sequence", current.maxSeq),
				slog.Int("session_count", current.sessionCount),
				slog.Int("recovery_attempts", attempt))
			return nil
		} else {
			previous = current
		}
		if attempt < recoveryStableViewMaxAttempts {
			if err := waitRecoveryRetry(ctx); err != nil {
				return fmt.Errorf("durable: recover sequence counter: catch-up cancelled: %w", err)
			}
		}
	}
	return fmt.Errorf("durable: recover sequence counter: head view did not stabilize after %d bounded attempts", recoveryStableViewMaxAttempts)
}

// adoptSequenceAuthority initializes or floors the shared authority from the
// canonical legacy heads. It never lowers an existing value and is safe when
// several new runtimes adopt the same database concurrently.
func (b *bus) adoptSequenceAuthority(ctx context.Context, floor uint64) (uint64, error) {
	for attempt := 1; attempt <= publishCASMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		current, expected, err := b.loadSequenceAuthority(ctx)
		if err != nil {
			return 0, err
		}
		if expected.ExpectedEventID != "" && current >= floor {
			return current, nil
		}
		payload, err := json.Marshal(sequenceAuthorityRecord{Sequence: floor})
		if err != nil {
			return 0, err
		}
		next := state.NewInternalRecord(state.NewEventID(), sequenceAuthorityIdentity, kindSequenceAuthority, payload)
		if err := b.store.SaveBatchIf(ctx, []state.SlotExpectation{expected}, []state.StateRecord{next}); err != nil {
			if errors.Is(err, state.ErrConditionFailed) {
				continue
			}
			return 0, err
		}
		return floor, nil
	}
	return 0, fmt.Errorf("authority adoption exceeded %d CAS attempts", publishCASMaxAttempts)
}

func (b *bus) loadSequenceAuthority(ctx context.Context) (uint64, state.SlotExpectation, error) {
	expect := state.InternalSlotExpectation(sequenceAuthorityIdentity, kindSequenceAuthority, "")
	rec, err := b.store.Load(ctx, sequenceAuthorityIdentity, kindSequenceAuthority)
	if errors.Is(err, state.ErrNotFound) {
		return 0, expect, nil
	}
	if err != nil {
		return 0, expect, err
	}
	var authority sequenceAuthorityRecord
	if err := json.Unmarshal(rec.Bytes, &authority); err != nil {
		return 0, expect, fmt.Errorf("decode authority event_id=%s: %w", rec.ID, err)
	}
	expect.ExpectedEventID = rec.ID
	return authority.Sequence, expect, nil
}

type recoveryHeadView struct {
	generations  map[string]string
	maxSeq       uint64
	minSeq       uint64
	minSeqOwner  identity.Quadruple
	sessionCount int
}

func (b *bus) scanRecoveryHeadView(ctx context.Context) (recoveryHeadView, error) {
	if err := ctx.Err(); err != nil {
		return recoveryHeadView{}, err
	}
	recs, err := b.store.ListKind(ctx, state.ListScope{MaintenanceScoped: true}, kindHead)
	if err != nil {
		return recoveryHeadView{}, fmt.Errorf("durable: recover sequence counter: scan head records: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return recoveryHeadView{}, err
	}
	view := recoveryHeadView{generations: make(map[string]string, len(recs))}
	for _, rec := range recs {
		if err := ctx.Err(); err != nil {
			return recoveryHeadView{}, err
		}
		if rec.Kind != kindHead {
			continue
		}
		head, err := decodeHead(rec.Bytes)
		if err != nil {
			return recoveryHeadView{}, fmt.Errorf("durable: recover sequence counter: decode head record (id=%s): %w", rec.ID, err)
		}
		head, err = b.ensureHeadMetadata(ctx, rec.Identity, head)
		if err != nil {
			return recoveryHeadView{}, fmt.Errorf("durable: recover sequence counter: index head record (id=%s): %w", rec.ID, err)
		}
		headBytes, err := encodeHead(head)
		if err != nil {
			return recoveryHeadView{}, fmt.Errorf("durable: recover sequence counter: encode adopted head record (id=%s): %w", rec.ID, err)
		}
		key := rec.Identity.TenantID + "\x00" + rec.Identity.UserID + "\x00" + rec.Identity.SessionID + "\x00" + rec.Identity.RunID
		if _, duplicate := view.generations[key]; duplicate {
			return recoveryHeadView{}, fmt.Errorf("durable: recover sequence counter: duplicate head identity (%s,%s,%s)", rec.Identity.TenantID, rec.Identity.UserID, rec.Identity.SessionID)
		}
		view.generations[key] = string(headBytes)
		view.sessionCount++
		for _, seq := range head.Sequences {
			if seq > view.maxSeq {
				view.maxSeq = seq
			}
			if view.minSeq == 0 || seq < view.minSeq {
				view.minSeq = seq
				view.minSeqOwner = rec.Identity
			}
		}
	}
	return view, nil
}

func recoveryViewsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, generation := range a {
		if b[key] != generation {
			return false
		}
	}
	return true
}

func waitRecoveryRetry(ctx context.Context) error {
	timer := time.NewTimer(recoveryRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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
			ctx := deps.StartupContext
			if ctx == nil {
				// Direct callers of a deps factory cannot reach this branch
				// through events.OpenWith, which always supplies its ctx. Keep
				// the defensive fallback for an explicitly hand-built Deps.
				ctx = context.Background()
			}
			return New(ctx, cfg, r, deps.State)
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

	// publishMu serialises this process's publication and best-effort ring. In
	// durable mode the StateStore authority CAS, not this mutex, is the
	// cross-process sequence source of truth.
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
	// persistenceBroken is set after an ambiguous StateStore write failure.
	// The entry may have reached the store even when Save returned an error;
	// refusing subsequent publishes prevents sequence reuse from overwriting
	// a committed payload that has not yet been indexed. A process restart
	// rehydrates the head and backfills safely.
	persistenceBroken bool

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

	// wake is the bounded best-effort watermark notification hub backing
	// the events.ProjectionSource seam (see projection.go). Publish
	// notifies it after each successfully persisted canonical event; the
	// hub's non-blocking sends never couple the publish path to a
	// projector. Zero value is ready to use.
	wake events.ProjectionWakeHub

	closed atomic.Bool
}

// fenceKey renders a session triple as the fenced-set key. The NUL
// separator can never appear in a tenant/user/session id, so distinct
// triples never collide.
func fenceKey(id identity.Identity) string {
	return id.TenantID + "\x00" + id.UserID + "\x00" + id.SessionID
}

func durableFenceKind(id identity.Identity) string {
	sum := sha256.Sum256([]byte(fenceKey(id)))
	return fmt.Sprintf("%s%x", kindFencePrefix, sum[:])
}

func (b *bus) isDurablyFenced(ctx context.Context, id identity.Quadruple) (bool, error) {
	_, err := b.store.Load(ctx, sequenceAuthorityIdentity, durableFenceKind(id.Identity))
	if err == nil {
		b.setFenceCache(id.Identity, true)
		return true, nil
	}
	if errors.Is(err, state.ErrNotFound) {
		b.setFenceCache(id.Identity, false)
		return false, nil
	}
	return false, fmt.Errorf("load durable session fence: %w", err)
}

func (b *bus) setFenceCache(id identity.Identity, fenced bool) {
	b.fenceMu.Lock()
	defer b.fenceMu.Unlock()
	if fenced {
		if b.fenced == nil {
			b.fenced = map[string]struct{}{}
		}
		b.fenced[fenceKey(id)] = struct{}{}
		return
	}
	delete(b.fenced, fenceKey(id))
}

type durableFenceSnapshot map[string]struct{}

func (s durableFenceSnapshot) contains(id identity.Quadruple) bool {
	_, ok := s[durableFenceKind(id.Identity)]
	return ok
}

func (b *bus) loadDurableFenceSnapshot(ctx context.Context) (durableFenceSnapshot, error) {
	recs, err := b.store.ListKind(ctx, state.ListScope{MaintenanceScoped: true}, kindFencePrefix)
	if err != nil {
		return nil, fmt.Errorf("scan durable session fences: %w", err)
	}
	out := make(durableFenceSnapshot, len(recs))
	for _, rec := range recs {
		if rec.Kind != "" && state.IsInternalKind(rec.Kind) && len(rec.Kind) >= len(kindFencePrefix) && rec.Kind[:len(kindFencePrefix)] == kindFencePrefix {
			out[rec.Kind] = struct{}{}
		}
	}
	return out, nil
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
func (b *bus) Fence(ctx context.Context, id identity.Identity) error {
	if b.closed.Load() {
		return events.ErrBusClosed
	}
	b.publishMu.Lock()
	defer b.publishMu.Unlock()
	if !b.bestEffort {
		kind := durableFenceKind(id)
		existing, err := b.store.Load(ctx, sequenceAuthorityIdentity, kind)
		if err != nil && !errors.Is(err, state.ErrNotFound) {
			return fmt.Errorf("durable: load session fence: %w", err)
		}
		if errors.Is(err, state.ErrNotFound) {
			payload, marshalErr := json.Marshal(id)
			if marshalErr != nil {
				return fmt.Errorf("durable: encode session fence: %w", marshalErr)
			}
			next := state.NewInternalRecord(state.NewEventID(), sequenceAuthorityIdentity, kind, payload)
			if saveErr := b.store.SaveIf(ctx, []state.SlotExpectation{state.InternalSlotExpectation(sequenceAuthorityIdentity, kind, "")}, next); saveErr != nil {
				if !errors.Is(saveErr, state.ErrConditionFailed) {
					return fmt.Errorf("durable: persist session fence: %w", saveErr)
				}
				// A concurrent fencer won; the persisted outcome is equivalent.
				if _, loadErr := b.store.Load(ctx, sequenceAuthorityIdentity, kind); loadErr != nil {
					return fmt.Errorf("durable: confirm concurrent session fence: %w", loadErr)
				}
			}
		} else if existing.ID == "" {
			return fmt.Errorf("durable: invalid persisted session fence")
		}
	}
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
func (b *bus) Unfence(ctx context.Context, id identity.Identity) error {
	if b.closed.Load() {
		return events.ErrBusClosed
	}
	if !b.bestEffort {
		kind := durableFenceKind(id)
		rec, err := b.store.Load(ctx, sequenceAuthorityIdentity, kind)
		if err != nil && !errors.Is(err, state.ErrNotFound) {
			return fmt.Errorf("durable: load session fence for removal: %w", err)
		}
		if err == nil {
			deleted, deleteErr := b.store.DeleteIf(ctx, state.InternalSlotExpectation(sequenceAuthorityIdentity, kind, rec.ID))
			if deleteErr != nil {
				return fmt.Errorf("durable: remove session fence: %w", deleteErr)
			}
			if !deleted {
				return fmt.Errorf("durable: session fence changed during removal: %w", state.ErrConditionFailed)
			}
		}
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

	// Wake projection watchers (best-effort, non-blocking — a projector
	// can never delay or fail the publish path). Only after the event was
	// persisted; a persistence failure never reaches here.
	b.notifyProjectionWatermark(ev)

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
	if b.persistenceBroken {
		return fmt.Errorf("durable: persistence/index state is uncertain; restart required before publishing")
	}

	// Fenced-session drop. Checked under publishMu (the same lock Fence
	// acquires) so the fence and the persist never disagree: an event that
	// reaches here after Fence took hold is dropped and never retained.
	if b.bestEffort {
		if b.isFenced(ev.Identity) {
			return errEventFenced
		}
		seq := b.nextSeq + 1
		ev.Sequence = seq
		if b.ringCap > 0 {
			b.ringAppendLocked(*ev)
		}
		b.nextSeq = seq
		return nil
	}
	fenced, err := b.isDurablyFenced(ctx, ev.Identity)
	if err != nil {
		return err
	}
	if fenced {
		return errEventFenced
	}

	seq, err := b.persistAtomic(ctx, ev)
	if err != nil {
		if errors.Is(err, errEventFenced) {
			return err
		}
		// Only an explicit commit-acknowledgement ambiguity poisons the bus.
		// Validation, reads, cancellation, CAS exhaustion, and known rollback
		// errors definitely did not commit and leave the next Publish usable.
		if errors.Is(err, state.ErrCommitOutcomeUnknown) {
			b.persistenceBroken = true
		}
		// nextSeq is NOT advanced. Definite no-commit outcomes retry from shared
		// authority on the next Publish. An ambiguous outcome stays poisoned:
		// reusing its sequence could overwrite a commit not yet represented by
		// the head/index, so restart must perform recovery.
		return fmt.Errorf("durable: persist event: %w", err)
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

// persistAtomic allocates through the shared authority and atomically commits
// authority, immutable body, and conditional session head. Caller holds this
// bus's publishMu; SaveBatchIf provides cross-process serialization.
func (b *bus) persistAtomic(ctx context.Context, ev *events.Event) (uint64, error) {
	for attempt := 1; attempt <= publishCASMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		currentSeq, authorityExpectation, err := b.loadSequenceAuthority(ctx)
		if err != nil {
			return 0, fmt.Errorf("load sequence authority: %w", err)
		}
		seq := currentSeq + 1
		if seq == 0 {
			return 0, fmt.Errorf("global sequence exhausted")
		}
		ev.Sequence = seq
		if err := b.persistAtomicAttempt(ctx, *ev, authorityExpectation); err != nil {
			if errors.Is(err, state.ErrConditionFailed) {
				continue
			}
			return 0, err
		}
		return seq, nil
	}
	return 0, fmt.Errorf("global sequence allocation exceeded %d CAS attempts: %w", publishCASMaxAttempts, state.ErrConditionFailed)
}

func (b *bus) persistAtomicAttempt(ctx context.Context, ev events.Event, authorityExpectation state.SlotExpectation) error {
	entryBytes, err := encodeEvent(ev)
	if err != nil {
		return err
	}
	metadata, err := metadataRecordFromEvent(ev)
	if err != nil {
		return fmt.Errorf("build event metadata: %w", err)
	}
	sessionID := sessionKey(ev.Identity)
	fenceExpectation := state.InternalSlotExpectation(sequenceAuthorityIdentity, durableFenceKind(ev.Identity.Identity), "")
	if _, err := b.store.Load(ctx, fenceExpectation.Identity, fenceExpectation.Kind); err == nil {
		return errEventFenced
	} else if !errors.Is(err, state.ErrNotFound) {
		return fmt.Errorf("load durable session fence: %w", err)
	}

	entryRec := state.StateRecord{
		ID:       state.NewEventID(),
		Identity: sessionID,
		Kind:     kindEntryPrefix + seqToken(ev.Sequence),
		Bytes:    entryBytes,
	}
	head, headExpectation, err := b.loadHeadForBatch(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load head record: %w", err)
	}
	// The steady-state path already has one integrity-authenticated metadata
	// row per sequence. Avoid rebuilding a map over the entire session head on
	// every publish (which would turn a long-lived session into O(n²) write
	// work), but force legacy/unchecked heads through bounded canonical
	// validation before appending a new row.
	if !headMetadataReady(head) {
		head, err = b.ensureHeadMetadata(ctx, sessionID, head)
		if err != nil {
			return fmt.Errorf("upgrade head index: %w", err)
		}
		// Adoption may have replaced the head generation.
		head, headExpectation, err = b.loadHeadForBatch(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("reload adopted head record: %w", err)
		}
		if len(head.Sequences) > 0 && !headMetadataReady(head) {
			return fmt.Errorf("adopted head changed before publication: %w", state.ErrConditionFailed)
		}
	}
	head.Sequences = append(head.Sequences, ev.Sequence)
	head.Metadata = append(head.Metadata, metadata)
	head.MetadataValidatedCount = len(head.Sequences)
	head.MetadataReady = true
	head.MetadataIntegrityChecksum = metadataIntegrityChecksum(head.Sequences, head.Metadata)
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
	authorityBytes, err := json.Marshal(sequenceAuthorityRecord{Sequence: ev.Sequence})
	if err != nil {
		return fmt.Errorf("encode sequence authority: %w", err)
	}
	authorityRec := state.NewInternalRecord(state.NewEventID(), sequenceAuthorityIdentity, kindSequenceAuthority, authorityBytes)
	entryExpectation := state.SlotExpectation{Identity: sessionID, Kind: entryRec.Kind}
	return b.store.SaveBatchIf(ctx,
		[]state.SlotExpectation{authorityExpectation, entryExpectation, headExpectation, fenceExpectation},
		[]state.StateRecord{authorityRec, entryRec, headRec})
}

func (b *bus) loadHeadForBatch(ctx context.Context, sessionID identity.Quadruple) (headRecord, state.SlotExpectation, error) {
	expect := state.SlotExpectation{Identity: sessionID, Kind: kindHead}
	rec, err := b.store.Load(ctx, sessionID, kindHead)
	if errors.Is(err, state.ErrNotFound) {
		return headRecord{}, expect, nil
	}
	if err != nil {
		return headRecord{}, expect, err
	}
	head, err := decodeHead(rec.Bytes)
	if err != nil {
		return headRecord{}, expect, err
	}
	expect.ExpectedEventID = rec.ID
	return head, expect, nil
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

// headMetadataReady reports whether the projection has been validated against
// canonical event bodies and its persisted digest still authenticates the
// ordered sequence/metadata pair. A length match alone is intentionally not
// sufficient: a valid event type/cost/identity can still belong to another
// payload.
func headMetadataReady(head headRecord) bool {
	return len(head.Sequences) > 0 &&
		head.MetadataReady &&
		head.MetadataValidatedCount == len(head.Sequences) &&
		len(head.Metadata) == len(head.Sequences) &&
		head.MetadataIntegrityChecksum == metadataIntegrityChecksum(head.Sequences, head.Metadata)
}

// saveHeadCheckpoint conditionally persists a bounded metadata adoption
// checkpoint. The expected bytes are advanced after each successful save, so
// another runtime cannot be overwritten silently while a large legacy head is
// being adopted.
func (b *bus) saveHeadCheckpoint(ctx context.Context, id identity.Quadruple, expectedBytes []byte, head headRecord) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	headBytes, err := encodeHead(head)
	if err != nil {
		return nil, err
	}
	current, err := b.store.Load(ctx, id, kindHead)
	if err != nil {
		return nil, fmt.Errorf("reload head before metadata checkpoint: %w", err)
	}
	if !bytes.Equal(current.Bytes, expectedBytes) {
		return nil, fmt.Errorf("metadata checkpoint raced with a newer head: %w", state.ErrConditionFailed)
	}
	if err := b.store.SaveIf(ctx, []state.SlotExpectation{{
		Identity: id, Kind: kindHead, ExpectedEventID: current.ID,
	}}, state.StateRecord{
		ID: state.NewEventID(), Identity: id, Kind: kindHead, Bytes: headBytes,
	}); err != nil {
		return nil, fmt.Errorf("save metadata checkpoint: %w", err)
	}
	return headBytes, nil
}

// ensureHeadMetadata upgrades a v1.29 head (which only carried sequence
// numbers) to the typed metadata projection. It also authenticates heads
// whose metadata already has the right length: the event body is canonical,
// while the persisted ready+checksum marker permits metadata-only consumers
// to remain O(1) after one successful validation. Legacy/adoption work is
// bounded by metadataValidationBatchSize and checkpoints a validated prefix,
// making cancellation and restart resumable rather than a repeated whole-head
// scan. A stale but structurally-valid row is repaired from its body.
func (b *bus) ensureHeadMetadata(ctx context.Context, id identity.Quadruple, head headRecord) (headRecord, error) {
	if err := ctx.Err(); err != nil {
		return headRecord{}, err
	}
	if len(head.Sequences) == 0 {
		if len(head.Metadata) != 0 || head.MetadataReady || head.MetadataValidatedCount != 0 || head.MetadataIntegrityChecksum != "" {
			return headRecord{}, fmt.Errorf("metadata exists for empty head")
		}
		return head, nil
	}

	seqSet := make(map[uint64]struct{}, len(head.Sequences))
	for _, seq := range head.Sequences {
		if seq == 0 {
			return headRecord{}, fmt.Errorf("invalid zero sequence in head")
		}
		if _, duplicate := seqSet[seq]; duplicate {
			return headRecord{}, fmt.Errorf("duplicate sequence=%d in head", seq)
		}
		seqSet[seq] = struct{}{}
	}

	bySeq := make(map[uint64]eventMetadataRecord, len(head.Metadata))
	for _, m := range head.Metadata {
		if m.Sequence == 0 || m.Type == "" {
			return headRecord{}, fmt.Errorf("invalid metadata sequence=%d type=%q", m.Sequence, m.Type)
		}
		if !events.IsValidEventType(m.Type) {
			return headRecord{}, fmt.Errorf("metadata sequence=%d has unknown event type %q", m.Sequence, m.Type)
		}
		if m.Internal != events.IsBusInternalNotice(m.Type) {
			return headRecord{}, fmt.Errorf("metadata sequence=%d internal marker disagrees with type %q", m.Sequence, m.Type)
		}
		if math.IsNaN(m.CostDollars) || math.IsInf(m.CostDollars, 0) {
			return headRecord{}, fmt.Errorf("metadata sequence=%d has non-finite cost", m.Sequence)
		}
		if _, ok := seqSet[m.Sequence]; !ok {
			return headRecord{}, fmt.Errorf("metadata sequence=%d is not present in head", m.Sequence)
		}
		if _, duplicate := bySeq[m.Sequence]; duplicate {
			return headRecord{}, fmt.Errorf("duplicate metadata sequence=%d", m.Sequence)
		}
		if m.TenantID != id.TenantID || m.UserID != id.UserID || m.SessionID != id.SessionID {
			return headRecord{}, fmt.Errorf("metadata sequence=%d has identity (%s,%s,%s), want (%s,%s,%s)", m.Sequence, m.TenantID, m.UserID, m.SessionID, id.TenantID, id.UserID, id.SessionID)
		}
		bySeq[m.Sequence] = m
	}
	if len(head.Metadata) > len(head.Sequences) {
		return headRecord{}, fmt.Errorf("metadata rows=%d exceed head sequences=%d", len(head.Metadata), len(head.Sequences))
	}

	// A complete, authenticated head is the normal steady-state path. This is
	// the only path that avoids loading payload bodies, and the checksum makes
	// an externally mutated stale-valid row fall back to canonical validation.
	if headMetadataReady(head) {
		return head, nil
	}

	originalHeadBytes, err := encodeHead(head)
	if err != nil {
		return headRecord{}, fmt.Errorf("encode head before metadata adoption: %w", err)
	}

	// A checkpoint contains only the validated sequence prefix. Its checksum
	// authenticates that prefix so an interrupted adoption never resumes from
	// a structurally-valid but untrusted row.
	resumeCount := 0
	if head.MetadataValidatedCount > 0 && head.MetadataValidatedCount <= len(head.Sequences) &&
		head.MetadataValidatedCount == len(head.Metadata) &&
		head.MetadataIntegrityChecksum == metadataIntegrityChecksum(
			head.Sequences[:head.MetadataValidatedCount], head.Metadata) {
		resumeCount = head.MetadataValidatedCount
		for i, m := range head.Metadata {
			if m.Sequence != head.Sequences[i] {
				resumeCount = 0
				break
			}
		}
	}
	metadata := make([]eventMetadataRecord, resumeCount, len(head.Sequences))
	copy(metadata, head.Metadata[:resumeCount])
	expectedBytes := originalHeadBytes

	checkpoint := func(final bool) error {
		checkpointHead := head
		checkpointHead.Metadata = append([]eventMetadataRecord(nil), metadata...)
		checkpointHead.MetadataValidatedCount = len(metadata)
		checkpointHead.MetadataReady = final
		if final {
			checkpointHead.MetadataIntegrityChecksum = metadataIntegrityChecksum(checkpointHead.Sequences, checkpointHead.Metadata)
		} else {
			checkpointHead.MetadataIntegrityChecksum = metadataIntegrityChecksum(checkpointHead.Sequences[:len(metadata)], checkpointHead.Metadata)
		}
		var saveErr error
		expectedBytes, saveErr = b.saveHeadCheckpoint(ctx, id, expectedBytes, checkpointHead)
		if saveErr != nil {
			return saveErr
		}
		head = checkpointHead
		return nil
	}

	for i := resumeCount; i < len(head.Sequences); i++ {
		if err := ctx.Err(); err != nil {
			return headRecord{}, fmt.Errorf("metadata adoption cancelled after %d/%d rows: %w", len(metadata), len(head.Sequences), err)
		}
		seq := head.Sequences[i]
		rec, err := b.store.Load(ctx, id, kindEntryPrefix+seqToken(seq))
		if err != nil {
			return headRecord{}, fmt.Errorf("backfill sequence=%d: load payload: %w", seq, err)
		}
		ev, err := decodeEvent(rec.Bytes)
		if err != nil {
			return headRecord{}, fmt.Errorf("backfill sequence=%d: decode payload: %w", seq, err)
		}
		if ev.Sequence != seq || ev.Identity.Identity != id.Identity || (ev.Identity.RunID != id.RunID && ev.Identity.RunID != "") {
			return headRecord{}, fmt.Errorf("backfill sequence=%d: payload identity/sequence mismatch", seq)
		}
		canonical, err := metadataRecordFromEvent(ev)
		if err != nil {
			return headRecord{}, fmt.Errorf("backfill sequence=%d: %w", seq, err)
		}
		// The body is authoritative. A stale but valid existing row is
		// replaced, while malformed rows were rejected by the structural
		// checks above.
		metadata = append(metadata, canonical)

		if len(metadata)%metadataValidationBatchSize == 0 || i == len(head.Sequences)-1 {
			if err := checkpoint(i == len(head.Sequences)-1); err != nil {
				return headRecord{}, fmt.Errorf("metadata adoption checkpoint after %d/%d rows: %w", len(metadata), len(head.Sequences), err)
			}
		}
	}
	if resumeCount == len(head.Sequences) {
		if err := checkpoint(true); err != nil {
			return headRecord{}, fmt.Errorf("finalize metadata adoption: %w", err)
		}
	}
	return head, nil
}

func metadataFromHead(head headRecord) []events.EventMetadata {
	out := make([]events.EventMetadata, 0, len(head.Metadata))
	for _, m := range head.Metadata {
		out = append(out, m.metadata())
	}
	return out
}

func validateMetadataEvent(meta events.EventMetadata, ev events.Event) error {
	if meta.Sequence != ev.Sequence || meta.Type != ev.Type || meta.Identity != ev.Identity || !meta.OccurredAt.Equal(ev.OccurredAt) || meta.Internal != events.IsBusInternalNotice(ev.Type) {
		return fmt.Errorf("projection differs from payload (metadata=%+v payload seq=%d type=%q)", meta, ev.Sequence, ev.Type)
	}
	rebuilt, err := events.NewEventMetadata(ev)
	if err != nil {
		return fmt.Errorf("rebuild metadata: %w", err)
	}
	if meta.CostSummary != rebuilt.CostSummary || meta.TotalTokens != rebuilt.TotalTokens || meta.CostDollars != rebuilt.CostDollars {
		return fmt.Errorf("cost summary differs from payload")
	}
	return nil
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
func (b *bus) replayDurable(ctx context.Context, from events.Cursor, f events.Filter) (out []events.Event, retErr error) {
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
	fenced, err := b.isDurablyFenced(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if fenced {
		// Erased session — no history to replay (events.Fencer).
		return nil, nil
	}
	defer func() {
		fenced, err := b.isDurablyFenced(ctx, sessionID)
		if err != nil {
			out, retErr = nil, err
		} else if fenced {
			out, retErr = nil, nil
		}
	}()

	head, err := b.loadHead(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("durable: replay load head: %w", err)
	}
	if len(head.Sequences) == 0 {
		return nil, nil
	}

	seqs := append([]uint64(nil), head.Sequences...)
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })

	out = make([]events.Event, 0, len(seqs))
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
	fenced, err := b.isDurablyFenced(ctx, sessionID)
	if err != nil {
		return 0, 0, false, err
	}
	if fenced {
		// Erased session — reads as no retained history (events.Fencer).
		return 0, 0, false, events.ErrNoHistory
	}
	defer func() {
		fenced, fenceErr := b.isDurablyFenced(ctx, sessionID)
		if fenceErr != nil {
			head, tail, truncated, err = 0, 0, false, fenceErr
		} else if fenced {
			head, tail, truncated, err = 0, 0, false, events.ErrNoHistory
		}
	}()
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
func (b *bus) Window(ctx context.Context, before uint64, limit int, f events.Filter) (out []events.Event, retErr error) {
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
	fenced, err := b.isDurablyFenced(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if fenced {
		// Erased session — no retained history window (events.Fencer).
		return nil, nil
	}
	defer func() {
		fenced, err := b.isDurablyFenced(ctx, sessionID)
		if err != nil {
			out, retErr = nil, err
		} else if fenced {
			out, retErr = nil, nil
		}
	}()
	head, err := b.loadHead(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("durable: window load head: %w", err)
	}
	head, err = b.ensureHeadMetadata(ctx, sessionID, head)
	if err != nil {
		return nil, fmt.Errorf("durable: window index head: %w", err)
	}
	if len(head.Sequences) == 0 {
		return nil, nil
	}
	metadata := metadataFromHead(head)
	sort.Slice(metadata, func(i, j int) bool { return metadata[i].Sequence > metadata[j].Sequence }) // descending: newest first

	// Walk newest-first, skip sequences at/above `before`, collect up to
	// `limit` MATCHING events, then reverse to oldest-first.
	out = make([]events.Event, 0, limit)
	for _, meta := range metadata {
		if before != 0 && meta.Sequence >= before {
			continue
		}
		if meta.Internal || !events.MatchMetadataScoped(meta, f) {
			continue
		}
		rec, err := b.store.Load(ctx, sessionID, kindEntryPrefix+seqToken(meta.Sequence))
		if err != nil {
			if errors.Is(err, state.ErrNotFound) {
				// The head lists a sequence whose entry record is missing —
				// a torn write or storage bug. Fail loudly rather than
				// serving a gap (RFC §6.13 / CLAUDE.md §13).
				return nil, fmt.Errorf("durable: window gap — index lists seq=%d but entry record is missing: %w",
					meta.Sequence, err)
			}
			return nil, fmt.Errorf("durable: window load entry seq=%d: %w", meta.Sequence, err)
		}
		ev, err := decodeEvent(rec.Bytes)
		if err != nil {
			return nil, fmt.Errorf("durable: window decode entry seq=%d: %w", meta.Sequence, err)
		}
		if err := validateMetadataEvent(meta, ev); err != nil {
			return nil, fmt.Errorf("durable: window metadata mismatch seq=%d: %w", meta.Sequence, err)
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
	if !q.Admin {
		id := identity.Quadruple{Identity: identity.Identity{
			TenantID: events.WireFilterFirst(q.Filter.TenantIDs), UserID: events.WireFilterFirst(q.Filter.UserIDs), SessionID: events.WireFilterFirst(q.Filter.SessionIDs),
		}}
		fenced, err := b.isDurablyFenced(ctx, id)
		if err != nil {
			return events.EventListPage{}, err
		}
		if fenced {
			return events.EventListPage{}, nil
		}
		page, readErr := b.listWindowDurable(ctx, q)
		fenced, fenceErr := b.isDurablyFenced(ctx, id)
		if fenceErr != nil {
			return events.EventListPage{}, fenceErr
		}
		if fenced {
			return events.EventListPage{}, nil
		}
		return page, readErr
	}
	return b.listWindowDurable(ctx, q)
}

// ListWindowMetadata serves the same page as ListWindow using only the
// durable typed metadata projection. It is the read path for aggregate and
// session-counter consumers: payload bodies are never loaded.
func (b *bus) ListWindowMetadata(ctx context.Context, q events.EventListQuery) (page events.MetadataListPage, retErr error) {
	if b.closed.Load() {
		return events.MetadataListPage{}, events.ErrBusClosed
	}
	if err := ctx.Err(); err != nil {
		return events.MetadataListPage{}, err
	}
	if !q.Admin && !events.WireFilterHasFullTriple(q.Filter) {
		return events.MetadataListPage{}, events.ErrIdentityScopeRequired
	}
	if q.Admin {
		b.emitAdminScopeUsed(events.Filter{
			Tenant:  events.WireFilterFirst(q.Filter.TenantIDs),
			User:    events.WireFilterFirst(q.Filter.UserIDs),
			Session: events.WireFilterFirst(q.Filter.SessionIDs),
		})
	}
	if q.Limit <= 0 {
		return events.MetadataListPage{}, nil
	}
	if b.bestEffort {
		if b.ringCap == 0 {
			return events.MetadataListPage{}, events.ErrReplayUnavailable
		}
		b.publishMu.Lock()
		snapshot := b.ringSnapshotLocked()
		evicted := b.evicted
		b.publishMu.Unlock()
		page, err := events.MetadataListWindowFromSnapshot(snapshot, q.Before, q.Limit, q.Filter)
		if err != nil {
			return events.MetadataListPage{}, err
		}
		page.Truncated = evicted
		return page, nil
	}
	if !q.Admin {
		id := identity.Quadruple{Identity: identity.Identity{
			TenantID: events.WireFilterFirst(q.Filter.TenantIDs), UserID: events.WireFilterFirst(q.Filter.UserIDs), SessionID: events.WireFilterFirst(q.Filter.SessionIDs),
		}}
		fenced, err := b.isDurablyFenced(ctx, id)
		if err != nil {
			return events.MetadataListPage{}, err
		}
		if fenced {
			return events.MetadataListPage{}, nil
		}
		defer func() {
			fenced, err := b.isDurablyFenced(ctx, id)
			if err != nil {
				page, retErr = events.MetadataListPage{}, err
			} else if fenced {
				page, retErr = events.MetadataListPage{}, nil
			}
		}()
	}

	type sessionHead struct {
		id       identity.Quadruple
		metadata []events.EventMetadata
	}
	var heads []sessionHead
	var adminFences durableFenceSnapshot
	if q.Admin {
		var err error
		adminFences, err = b.loadDurableFenceSnapshot(ctx)
		if err != nil {
			return events.MetadataListPage{}, err
		}
	}
	if !q.Admin {
		id := identity.Quadruple{Identity: identity.Identity{
			TenantID:  events.WireFilterFirst(q.Filter.TenantIDs),
			UserID:    events.WireFilterFirst(q.Filter.UserIDs),
			SessionID: events.WireFilterFirst(q.Filter.SessionIDs),
		}}
		hd, err := b.loadHead(ctx, id)
		if err != nil {
			return events.MetadataListPage{}, fmt.Errorf("durable: events.list metadata load head: %w", err)
		}
		hd, err = b.ensureHeadMetadata(ctx, id, hd)
		if err != nil {
			return events.MetadataListPage{}, fmt.Errorf("durable: events.list metadata index head: %w", err)
		}
		heads = append(heads, sessionHead{id: id, metadata: metadataFromHead(hd)})
	} else {
		recs, err := b.store.ListKind(ctx, state.ListScope{MaintenanceScoped: true}, kindHead)
		if err != nil {
			return events.MetadataListPage{}, fmt.Errorf("durable: events.list metadata scan heads: %w", err)
		}
		for _, rec := range recs {
			if err := ctx.Err(); err != nil {
				return events.MetadataListPage{}, err
			}
			if rec.Kind != kindHead || !events.WireFilterMatchesTriple(q.Filter, rec.Identity.Identity) || adminFences.contains(rec.Identity) {
				continue
			}
			hd, err := decodeHead(rec.Bytes)
			if err != nil {
				return events.MetadataListPage{}, fmt.Errorf("durable: events.list metadata decode head (id=%s): %w", rec.ID, err)
			}
			hd, err = b.ensureHeadMetadata(ctx, rec.Identity, hd)
			if err != nil {
				return events.MetadataListPage{}, fmt.Errorf("durable: events.list metadata index head (id=%s): %w", rec.ID, err)
			}
			heads = append(heads, sessionHead{id: rec.Identity, metadata: metadataFromHead(hd)})
		}
	}

	matches := make([]events.EventMetadata, 0, q.Limit+1)
	for _, head := range heads {
		if err := ctx.Err(); err != nil {
			return events.MetadataListPage{}, err
		}
		for _, m := range head.metadata {
			if err := ctx.Err(); err != nil {
				return events.MetadataListPage{}, err
			}
			if q.Before != 0 && m.Sequence >= q.Before {
				continue
			}
			if m.Internal || !events.MatchMetadataWire(m, q.Filter) {
				continue
			}
			matches = append(matches, m)
		}
	}
	if q.Admin {
		currentFences, err := b.loadDurableFenceSnapshot(ctx)
		if err != nil {
			return events.MetadataListPage{}, err
		}
		kept := matches[:0]
		for _, meta := range matches {
			if !currentFences.contains(meta.Identity) {
				kept = append(kept, meta)
			}
		}
		matches = kept
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Sequence > matches[j].Sequence })
	page = events.MetadataListPage{}
	if len(matches) > q.Limit {
		page.HasMore = true
		matches = matches[:q.Limit]
		page.NextCursor = matches[len(matches)-1].Sequence
	}
	for i, j := 0, len(matches)-1; i < j; i, j = i+1, j-1 {
		matches[i], matches[j] = matches[j], matches[i]
	}
	page.Events = matches
	return page, nil
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
		id       identity.Quadruple
		metadata []events.EventMetadata
	}
	var heads []sessionHead
	var adminFences durableFenceSnapshot
	if q.Admin {
		var err error
		adminFences, err = b.loadDurableFenceSnapshot(ctx)
		if err != nil {
			return events.EventListPage{}, err
		}
	}

	if !q.Admin {
		// The handler folded the caller's triple; the filter names exactly
		// one session. Resolve it and load just that head (no maintenance
		// scan for the common own-session read).
		quad := identity.Quadruple{Identity: identity.Identity{
			TenantID:  events.WireFilterFirst(q.Filter.TenantIDs),
			UserID:    events.WireFilterFirst(q.Filter.UserIDs),
			SessionID: events.WireFilterFirst(q.Filter.SessionIDs),
		}}
		hd, err := b.loadHead(ctx, quad)
		if err != nil {
			return events.EventListPage{}, fmt.Errorf("durable: events.list load head: %w", err)
		}
		hd, err = b.ensureHeadMetadata(ctx, quad, hd)
		if err != nil {
			return events.EventListPage{}, fmt.Errorf("durable: events.list index head: %w", err)
		}
		heads = append(heads, sessionHead{id: quad, metadata: metadataFromHead(hd)})
	} else {
		recs, err := b.store.ListKind(ctx, state.ListScope{MaintenanceScoped: true}, kindHead)
		if err != nil {
			return events.EventListPage{}, fmt.Errorf("durable: events.list scan head records: %w", err)
		}
		for _, rec := range recs {
			if err := ctx.Err(); err != nil {
				return events.EventListPage{}, err
			}
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
			if adminFences.contains(rec.Identity) {
				continue
			}
			hd, err := decodeHead(rec.Bytes)
			if err != nil {
				return events.EventListPage{}, fmt.Errorf("durable: events.list decode head (id=%s): %w", rec.ID, err)
			}
			hd, err = b.ensureHeadMetadata(ctx, rec.Identity, hd)
			if err != nil {
				return events.EventListPage{}, fmt.Errorf("durable: events.list index head (id=%s): %w", rec.ID, err)
			}
			heads = append(heads, sessionHead{id: rec.Identity, metadata: metadataFromHead(hd)})
		}
	}

	// Merge candidate (session, seq) pairs below the cursor, global-
	// descending by sequence.
	type candidate struct {
		id   identity.Quadruple
		meta events.EventMetadata
	}
	var cands []candidate
	for _, h := range heads {
		if err := ctx.Err(); err != nil {
			return events.EventListPage{}, err
		}
		for _, meta := range h.metadata {
			if q.Before != 0 && meta.Sequence >= q.Before {
				continue
			}
			if meta.Internal || !events.MatchMetadataWire(meta, q.Filter) {
				continue
			}
			cands = append(cands, candidate{id: h.id, meta: meta})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].meta.Sequence > cands[j].meta.Sequence })

	matches := make([]events.Event, 0, q.Limit+1)
	for _, c := range cands {
		if err := ctx.Err(); err != nil {
			return events.EventListPage{}, err
		}
		rec, err := b.store.Load(ctx, c.id, kindEntryPrefix+seqToken(c.meta.Sequence))
		if err != nil {
			if errors.Is(err, state.ErrNotFound) {
				// The head lists a sequence whose entry record is missing —
				// a torn write or storage bug. Fail loudly rather than
				// serving a gap (RFC §6.13 / CLAUDE.md §13).
				return events.EventListPage{}, fmt.Errorf("durable: events.list gap — index lists seq=%d but entry record is missing: %w",
					c.meta.Sequence, err)
			}
			return events.EventListPage{}, fmt.Errorf("durable: events.list load entry seq=%d: %w", c.meta.Sequence, err)
		}
		ev, err := decodeEvent(rec.Bytes)
		if err != nil {
			return events.EventListPage{}, fmt.Errorf("durable: events.list decode entry seq=%d: %w", c.meta.Sequence, err)
		}
		if err := validateMetadataEvent(c.meta, ev); err != nil {
			return events.EventListPage{}, fmt.Errorf("durable: events.list metadata mismatch seq=%d: %w", c.meta.Sequence, err)
		}
		if events.IsBusInternalNotice(ev.Type) || !events.MatchWire(ev, q.Filter) {
			continue
		}
		matches = append(matches, ev)
		if len(matches) > q.Limit {
			break
		}
	}

	if q.Admin {
		currentFences, err := b.loadDurableFenceSnapshot(ctx)
		if err != nil {
			return events.EventListPage{}, err
		}
		kept := matches[:0]
		for _, ev := range matches {
			if !currentFences.contains(ev.Identity) {
				kept = append(kept, ev)
			}
		}
		matches = kept
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
	_ events.EventBus              = (*bus)(nil)
	_ events.Replayer              = (*bus)(nil)
	_ events.HistoryReplayer       = (*bus)(nil)
	_ events.EventMetadataReplayer = (*bus)(nil)
	_ events.Fencer                = (*bus)(nil)
	_ events.ProjectionSource      = (*bus)(nil)
)
