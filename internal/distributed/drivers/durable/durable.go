// Package durable is Harbor's StateStore-backed MessageBus driver. It
// persists every published BusEnvelope through the runtime's
// state.StateStore and projects it onto the local events.EventBus
// (the same projection contract the loopback driver uses), so bus
// traffic is replayable after a restart. A background poller scans the
// shared store for envelopes published by OTHER instances (or left by a
// crash, or whose local projection failed) and projects them onto the
// local event bus — at-least-once cross-instance fan-out.
//
// Backend. StateStore-backed: each envelope is one record keyed by a
// time-ordered ULID under a common prefix; the poller enumerates them
// with a maintenance-scoped prefix scan. On a shared Postgres store
// this is Postgres-as-queue across instances; on SQLite it is
// single-instance restart-replay. NATS / Redis Streams remain future
// drivers in the same set. The StateStore exposes no change-notification
// primitive, so delivery is poll-based; a Postgres LISTEN/NOTIFY
// fast-path is a future optimization behind this same driver.
//
// Scope. Opt-in via distributed.bus_driver: durable; the loopback
// driver stays the default. The driver fails loud at construction when
// no StateStore is wired (an operator who selected durable must get
// durability). Delivery is at-least-once: a restart re-projects
// persisted envelopes and a crash mid-publish may re-deliver, so
// consumers MUST be idempotent on (TaskID, Edge, EventID) — the
// MessageBus contract.
package durable

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/distributed"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// defaultPollInterval is the poller's scan cadence when
// DistributedConfig.BusPollInterval is unset.
const defaultPollInterval = time.Second

func init() {
	distributed.RegisterBus("durable", New)
}

// New constructs the durable MessageBus and starts its background
// poller (joined by Close). It fails loud when no StateStore or
// EventBus is wired — the durable driver refuses to fall back to a
// non-durable posture (CLAUDE.md §13 "no silent stub default").
func New(deps distributed.Dependencies) (distributed.MessageBus, error) {
	if deps.State == nil {
		return nil, fmt.Errorf(
			"distributed/durable: the durable bus driver requires a StateStore but none was wired; " +
				"configure a durable store via state.driver (sqlite or postgres — see examples/) " +
				"and ensure it is passed to OpenBus")
	}
	if deps.EventBus == nil {
		return nil, fmt.Errorf("distributed/durable: the durable bus driver requires a non-nil EventBus to project envelopes onto")
	}
	interval := defaultPollInterval
	if deps.Cfg.BusPollInterval > 0 {
		interval = deps.Cfg.BusPollInterval
	}
	pollCtx, pollCancel := context.WithCancel(context.Background())
	b := &bus{
		store:      deps.State,
		eventBus:   deps.EventBus,
		interval:   interval,
		projected:  make(map[string]struct{}),
		logger:     slog.Default().With(slog.String("driver", "distributed/durable")),
		pollCtx:    pollCtx,
		pollCancel: pollCancel,
	}
	b.wg.Add(1)
	go b.pollLoop()
	return b, nil
}

// bus is the durable MessageBus driver. It is a compiled artifact:
// every field is set once at construction; per-publish state lives
// under mu, and the poller goroutine is the only driver-owned
// goroutine (joined on Close). Safe for N concurrent Publish callers.
type bus struct {
	store    state.StateStore
	eventBus events.EventBus
	interval time.Duration
	logger   *slog.Logger

	mu sync.Mutex
	// projected is the set of entry keys this instance has already
	// projected onto the local event bus. It is the in-memory dedup that
	// stops the poller re-projecting an envelope this instance already
	// delivered. An entry is added ONLY after a successful projection
	// (or pre-reserved by Publish, which rolls the reservation back if
	// its own projection fails) — so a failed projection is retried by
	// the poller, never silently stranded. It is NOT persisted: across a
	// restart it is empty, so a fresh instance re-projects persisted
	// envelopes (restart-replay) — at-least-once, consumers dedupe on
	// (TaskID, Edge, EventID).
	projected map[string]struct{}

	closed     atomic.Bool
	pollCtx    context.Context
	pollCancel context.CancelFunc
	wg         sync.WaitGroup
}

var _ distributed.MessageBus = (*bus)(nil)

// Publish persists env to its own per-ULID slot and projects it onto
// the local event bus immediately (so local subscribers do not wait for
// a poll). The entry key is reserved in `projected` BEFORE the persist
// so a concurrent poll skips it; the reservation is rolled back if
// either the persist OR the local projection fails, so the poller
// re-delivers it on a later tick (at-least-once, never stranded).
func (b *bus) Publish(ctx context.Context, env distributed.BusEnvelope) error {
	if b.closed.Load() {
		return distributed.ErrBusClosed
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("distributed/durable: publish cancelled: %w", err)
	}
	if err := env.Validate(); err != nil {
		return err
	}
	payload, err := encodeEnvelope(env)
	if err != nil {
		return err // fail loud — unserializable envelope, never a silent drop
	}

	// Storage keys are per-publish (a fresh ULID), NOT keyed on
	// env.EventID: the at-least-once contract dedupes downstream on
	// (TaskID, Edge, EventID), so a re-publish persists a new record and
	// is deduped by consumers rather than collapsed at the store.
	eid := state.NewEventID()
	entryKey := entryKindPrefix + string(eid)

	b.mu.Lock()
	b.projected[entryKey] = struct{}{}
	b.mu.Unlock()

	if err := b.store.Save(ctx, state.StateRecord{
		ID:       eid,
		Identity: sessionScope(env.Identity),
		Kind:     entryKey,
		Bytes:    payload,
	}); err != nil {
		b.unreserve(entryKey)
		return fmt.Errorf("distributed/durable: persist envelope: %w", err)
	}

	if err := b.project(ctx, env); err != nil {
		// Local projection failed — roll back the reservation so the
		// poller re-delivers this (now durably persisted) envelope on a
		// later tick rather than stranding it.
		b.unreserve(entryKey)
		return fmt.Errorf("distributed/durable: project envelope: %w", err)
	}
	return nil
}

func (b *bus) unreserve(entryKey string) {
	b.mu.Lock()
	delete(b.projected, entryKey)
	b.mu.Unlock()
}

// Close stops the poller (cancelling any in-flight scan so Close never
// blocks on a slow store) and joins it. Idempotent.
func (b *bus) Close(_ context.Context) error {
	if !b.closed.CompareAndSwap(false, true) {
		return nil
	}
	b.pollCancel()
	b.wg.Wait()
	return nil
}

// project publishes env onto the local event bus as the shared
// projection event (the same contract loopback uses), so one set of
// EventBus subscribers consumes envelopes regardless of bus driver.
func (b *bus) project(ctx context.Context, env distributed.BusEnvelope) error {
	return b.eventBus.Publish(ctx, events.Event{
		Type:       distributed.EventTypeDistributedBusEnvelope,
		Identity:   env.Identity,
		OccurredAt: env.Timestamp,
		Payload:    distributed.BusEnvelopePayload{Envelope: env},
	})
}

// pollLoop scans the shared store on a ticker and projects any
// not-yet-delivered envelopes. The only driver-owned goroutine; exits
// when the poll context is cancelled by Close.
func (b *bus) pollLoop() {
	defer b.wg.Done()
	t := time.NewTicker(b.interval)
	defer t.Stop()
	for {
		select {
		case <-b.pollCtx.Done():
			return
		case <-t.C:
			b.pollOnce(b.pollCtx)
		}
	}
}

// pollOnce enumerates every persisted bus entry and projects the ones
// this instance has not already delivered (cross-instance envelopes +,
// after a restart, the persisted history + any local publish whose
// projection failed). Entries are projected in ULID (≈ publication)
// order. A corrupt entry is marked seen + skipped (logged), never
// retried forever; a transient projection failure is left UNMARKED so
// the next tick retries it.
func (b *bus) pollOnce(ctx context.Context) {
	recs, err := b.store.ListKind(ctx, state.ListScope{MaintenanceScoped: true}, entryKindPrefix)
	if err != nil {
		b.logger.Warn("durable bus poll: list entries failed", slog.String("error", err.Error()))
		return
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Kind < recs[j].Kind })

	type pending struct {
		kind string
		env  distributed.BusEnvelope
	}
	var todo []pending
	b.mu.Lock()
	for _, rec := range recs {
		if _, seen := b.projected[rec.Kind]; seen {
			continue
		}
		env, derr := decodeEnvelope(rec.Bytes)
		if derr != nil {
			b.logger.Warn("durable bus poll: decode entry failed",
				slog.String("kind", rec.Kind), slog.String("error", derr.Error()))
			b.projected[rec.Kind] = struct{}{} // do not retry a corrupt entry forever
			continue
		}
		todo = append(todo, pending{kind: rec.Kind, env: env})
	}
	b.mu.Unlock()

	// Project outside the lock; mark each entry projected only on
	// success so a transient EventBus failure is retried next tick. The
	// poller is the sole projector of these entries (own-instance
	// publishes are pre-reserved by Publish), and pollOnce runs serially,
	// so project-then-mark cannot double-deliver.
	for _, p := range todo {
		if err := b.project(ctx, p.env); err != nil {
			b.logger.Warn("durable bus poll: project failed",
				slog.String("edge", p.env.Edge), slog.String("error", err.Error()))
			continue
		}
		b.mu.Lock()
		b.projected[p.kind] = struct{}{}
		b.mu.Unlock()
	}
}

// sessionScope drops the RunID so an envelope's storage slot is scoped
// to its session triple. The full identity (including RunID) is
// preserved in the persisted bytes; this only governs the StateStore
// record identity.
func sessionScope(q identity.Quadruple) identity.Quadruple {
	return identity.Quadruple{Identity: q.Identity}
}
