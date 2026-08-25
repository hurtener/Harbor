// Package receipts persists content-free provider-attempt usage facts until
// a coordinator acknowledges them.  The normal delivery path reads one
// durable due index; the old receipt-prefix scan is only a slow reconciliation
// path for records written by older Harbor versions.
package receipts

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/state"
)

const (
	kindPrefix       = state.InternalKindPrefix + "inference.receipt/"
	dueKind          = state.InternalKindPrefix + "inference.receipt.due"
	legacyMarkerKind = state.InternalKindPrefix + "inference.receipt.reconciled.v1"
)

var (
	ErrInvalidReceipt    = errors.New("llm/receipts: invalid usage receipt")
	ErrDeliveryFailed    = errors.New("llm/receipts: delivery failed")
	ErrCircuitOpen       = errors.New("llm/receipts: delivery circuit open")
	ErrReconcileOverflow = errors.New("llm/receipts: legacy reconciliation bound exceeded")
	ErrClosed            = errors.New("llm/receipts: outbox closed")
)

type Delivery interface {
	Deliver(context.Context, llm.AttemptUsageReceipt) error
}

// DeliveryAck is the only acknowledgement fact the outbox accepts. Both
// fields must match the exact canonical receipt that was delivered.
type DeliveryAck struct {
	ReceiptID         string `json:"receipt_id"`
	CanonicalBodyHash string `json:"canonical_body_hash"`
}

// BatchDelivery is an optional extension implemented by transports that can
// acknowledge a bounded receipt batch. Omitting an acknowledgement retains
// that receipt for replay; an acknowledgement for an unknown identity or a
// mismatched body hash fails the whole batch closed.
type BatchDelivery interface {
	DeliverBatch(context.Context, []llm.AttemptUsageReceipt) ([]DeliveryAck, error)
}

// PendingReceiptSource is a low-frequency crash-recovery source.  It is not
// used by Replay's hot path; implementations must return a bounded result.
type PendingReceiptSource interface {
	PendingReceipts(context.Context, int) ([]llm.AttemptUsageReceipt, error)
	AcknowledgePendingReceipt(context.Context, llm.AttemptUsageReceipt) error
}

// OutboxHealthReporter receives the secret-free liveness of background
// receipt reconciliation. Stock transports use it to keep runtime.info from
// reporting strict readiness after the durable worker has degraded.
type OutboxHealthReporter interface {
	SetOutboxHealth(bool)
}

type Config struct {
	Store           state.StateStore
	Delivery        Delivery
	PendingSource   PendingReceiptSource
	MaxBatch        int
	BaseBackoff     time.Duration
	MaxBackoff      time.Duration
	CircuitFailures int
	CircuitOpenFor  time.Duration
	// PollInterval is retained as a compatibility alias for the reconciliation
	// cadence. It is never used as a five-second receipt prefix poll.
	PollInterval      time.Duration
	ReconcileInterval time.Duration
	Clock             func() time.Time
}

type Outbox struct {
	store             state.StateStore
	delivery          Delivery
	pending           PendingReceiptSource
	maxBatch          int
	baseBackoff       time.Duration
	maxBackoff        time.Duration
	circuitFailures   int
	circuitOpenFor    time.Duration
	reconcileInterval time.Duration
	clock             func() time.Time
	wake              chan struct{}
	prepareMu         sync.Mutex
	prepared          bool

	mu             sync.Mutex
	failures       int
	circuitOpenTil time.Time
	replaying      bool
	closed         bool
}

type storedReceipt struct {
	Receipt         llm.AttemptUsageReceipt `json:"receipt"`
	Status          string                  `json:"status"`
	Attempts        int                     `json:"attempts"`
	NextAttemptAt   time.Time               `json:"next_attempt_at"`
	LastFailureCode string                  `json:"last_failure_code,omitempty"`
}

type dueEntry struct {
	ReceiptID     string             `json:"receipt_id"`
	Identity      identity.Quadruple `json:"identity"`
	NextAttemptAt time.Time          `json:"next_attempt_at"`
}

type dueIndex struct {
	Entries []dueEntry `json:"entries"`
}

type legacyReconcileMarker struct {
	Version int `json:"version"`
}

func New(cfg Config) (*Outbox, error) {
	if cfg.Store == nil || cfg.Delivery == nil {
		return nil, fmt.Errorf("%w: store and delivery are required", llm.ErrUsageReceiptUnavailable)
	}
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = 64
	}
	// Reconcile reserves one storage-side row beyond the work batch so it can
	// refuse an undisclosed legacy backlog instead of rescanning the same first
	// page forever.
	if cfg.MaxBatch >= state.MaxStateMaintenanceListLimit {
		return nil, fmt.Errorf("%w: max batch exceeds state bound", ErrInvalidReceipt)
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = time.Minute
	}
	if cfg.MaxBackoff < cfg.BaseBackoff {
		return nil, fmt.Errorf("%w: max backoff before base", ErrInvalidReceipt)
	}
	if cfg.CircuitFailures <= 0 {
		cfg.CircuitFailures = 3
	}
	if cfg.CircuitOpenFor <= 0 {
		cfg.CircuitOpenFor = 30 * time.Second
	}
	if cfg.ReconcileInterval <= 0 {
		cfg.ReconcileInterval = cfg.PollInterval
	}
	if cfg.ReconcileInterval <= 0 {
		cfg.ReconcileInterval = 5 * time.Minute
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	return &Outbox{
		store: cfg.Store, delivery: cfg.Delivery, pending: cfg.PendingSource,
		maxBatch: cfg.MaxBatch, baseBackoff: cfg.BaseBackoff, maxBackoff: cfg.MaxBackoff,
		circuitFailures: cfg.CircuitFailures, circuitOpenFor: cfg.CircuitOpenFor,
		reconcileInterval: cfg.ReconcileInterval, clock: cfg.Clock, wake: make(chan struct{}, 1),
	}, nil
}

// Close stops Run and rejects future enqueues. It does not close the caller's
// StateStore, which may be shared by other runtime subsystems.
func (o *Outbox) Close() error {
	o.mu.Lock()
	if !o.closed {
		o.closed = true
		close(o.wake)
	}
	o.mu.Unlock()
	return nil
}

func (o *Outbox) isClosed() bool { o.mu.Lock(); defer o.mu.Unlock(); return o.closed }

func (o *Outbox) signal() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.closed {
		select {
		case o.wake <- struct{}{}:
		default:
		}
	}
}

func (o *Outbox) Enqueue(ctx context.Context, receipt llm.AttemptUsageReceipt) error {
	if o.isClosed() {
		return ErrClosed
	}
	if err := validateReceipt(receipt); err != nil {
		return err
	}
	q, ok := identity.QuadrupleFrom(ctx)
	if !ok || q.TenantID != receipt.TenantID || q.UserID != receipt.UserID || q.SessionID != receipt.SessionID || q.RunID != receipt.LogicalRunID {
		return fmt.Errorf("%w: receipt identity is not the verified call scope", ErrInvalidReceipt)
	}
	if err := o.enqueueAt(ctx, q, receipt); err != nil {
		return err
	}
	o.signal()
	return nil
}

func (o *Outbox) enqueueAt(ctx context.Context, q identity.Quadruple, receipt llm.AttemptUsageReceipt) error {
	receiptKind := receiptKind(receipt.ReceiptID)
	for range 8 {
		rec, err := o.store.Load(ctx, q, receiptKind)
		if err != nil && !errors.Is(err, state.ErrNotFound) {
			return fmt.Errorf("llm/receipts: load: %w", err)
		}
		var stored storedReceipt
		if err == nil {
			stored, err = decodeStored(rec.Bytes)
			if err != nil {
				return err
			}
			if !receiptMatchesRecord(rec, stored.Receipt) || stored.Receipt.CanonicalBodyHash != receipt.CanonicalBodyHash {
				return fmt.Errorf("%w: receipt id reused with a different body", llm.ErrUsageReceiptUnavailable)
			}
			if stored.Status == "acked" {
				return nil
			}
		}
		idxRec, idx, idxErr := o.loadIndex(ctx)
		if idxErr != nil && !errors.Is(idxErr, state.ErrNotFound) {
			return idxErr
		}
		if hasDue(idx, receipt.ReceiptID, q) {
			return nil
		}
		idx.Entries = append(idx.Entries, dueEntry{ReceiptID: receipt.ReceiptID, Identity: q})
		idxBody, marshalErr := json.Marshal(idx)
		if marshalErr != nil {
			return fmt.Errorf("%w: encode due index", ErrInvalidReceipt)
		}
		idxNext := state.NewInternalRecord(state.NewEventID(), internalQ(), dueKind, idxBody)
		exps := []state.SlotExpectation{state.InternalSlotExpectation(internalQ(), dueKind, expectedID(idxRec))}
		writes := []state.StateRecord{idxNext}
		if errors.Is(err, state.ErrNotFound) {
			body, marshalErr := json.Marshal(storedReceipt{Receipt: receipt, Status: "pending"})
			if marshalErr != nil {
				return fmt.Errorf("%w: encode receipt", ErrInvalidReceipt)
			}
			exps = append(exps, state.InternalSlotExpectation(q, receiptKind, ""))
			writes = append(writes, state.NewInternalRecord(state.NewEventID(), q, receiptKind, body))
		} else {
			// The receipt body is immutable; only the index changed.
			exps = append(exps, state.InternalSlotExpectation(q, receiptKind, rec.ID))
			body, marshalErr := json.Marshal(stored)
			if marshalErr != nil {
				return fmt.Errorf("%w: encode receipt", ErrInvalidReceipt)
			}
			writes = append(writes, state.NewInternalRecord(state.NewEventID(), q, receiptKind, body))
		}
		// SaveBatchIf has no ordering requirement, but expectations and writes
		// must name the same slots in a one-to-one manner.
		if err := o.store.SaveBatchIf(ctx, exps, writes); err != nil {
			if errors.Is(err, state.ErrConditionFailed) {
				continue
			}
			return fmt.Errorf("llm/receipts: enqueue: %w", err)
		}
		return nil
	}
	return fmt.Errorf("%w: concurrent enqueue did not converge", llm.ErrUsageReceiptUnavailable)
}

type ReplayStats struct{ Seen, Delivered, Acknowledged, Deferred, Failed int }

type replayItem struct {
	entry  dueEntry
	stored storedReceipt
}

func (o *Outbox) Replay(ctx context.Context) (ReplayStats, error) {
	if err := o.breakerError(); err != nil {
		return ReplayStats{}, err
	}
	o.mu.Lock()
	if o.replaying {
		o.mu.Unlock()
		return ReplayStats{}, nil
	}
	o.replaying = true
	o.mu.Unlock()
	defer func() { o.mu.Lock(); o.replaying = false; o.mu.Unlock() }()
	_, idx, err := o.loadIndex(ctx)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return ReplayStats{}, nil
		}
		return ReplayStats{}, err
	}
	sort.SliceStable(idx.Entries, func(i, j int) bool {
		if idx.Entries[i].NextAttemptAt.Equal(idx.Entries[j].NextAttemptAt) {
			return idx.Entries[i].ReceiptID < idx.Entries[j].ReceiptID
		}
		return idx.Entries[i].NextAttemptAt.Before(idx.Entries[j].NextAttemptAt)
	})
	stats := ReplayStats{}
	now := o.clock().UTC()
	var firstErr error
	items := make([]replayItem, 0, o.maxBatch)
	for i, entry := range idx.Entries {
		if i >= o.maxBatch {
			break
		}
		stats.Seen++
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		rr, loadErr := o.store.Load(ctx, entry.Identity, receiptKind(entry.ReceiptID))
		if errors.Is(loadErr, state.ErrNotFound) {
			if updateErr := o.updateEntry(ctx, entry, nil, true); updateErr != nil && firstErr == nil {
				firstErr = updateErr
			}
			continue
		}
		if loadErr != nil {
			return stats, loadErr
		}
		stored, decodeErr := decodeStored(rr.Bytes)
		if decodeErr != nil {
			return stats, decodeErr
		}
		if !receiptMatchesRecord(rr, stored.Receipt) || stored.Receipt.ReceiptID != entry.ReceiptID {
			return stats, fmt.Errorf("%w: due entry identity mismatch", ErrInvalidReceipt)
		}
		if stored.Status == "acked" {
			if updateErr := o.updateEntry(ctx, entry, &stored, true); updateErr != nil && firstErr == nil {
				firstErr = updateErr
			}
			continue
		}
		if !stored.NextAttemptAt.IsZero() && now.Before(stored.NextAttemptAt) {
			stats.Deferred++
			continue
		}
		items = append(items, replayItem{entry: entry, stored: stored})
	}
	if len(items) == 0 {
		return stats, firstErr
	}
	if batch, ok := o.delivery.(BatchDelivery); ok {
		return o.replayBatch(ctx, now, items, stats, firstErr, batch)
	}
	for i := range items {
		item := &items[i]
		if deliveryErr := o.delivery.Deliver(ctx, item.stored.Receipt); deliveryErr != nil {
			stats.Failed++
			if updateErr := o.updateEntry(ctx, item.entry, &item.stored, false); updateErr != nil && firstErr == nil {
				firstErr = updateErr
			} else if firstErr == nil {
				firstErr = fmt.Errorf("%w: receipt delivery returned an error", ErrDeliveryFailed)
			}
			o.noteFailure(now)
			continue
		}
		stats.Delivered++
		item.stored.Status = "acked"
		item.stored.NextAttemptAt = time.Time{}
		item.stored.LastFailureCode = ""
		if updateErr := o.updateEntry(ctx, item.entry, &item.stored, true); updateErr != nil {
			if firstErr == nil {
				firstErr = updateErr
			}
			continue
		}
		stats.Acknowledged++
		o.noteSuccess()
	}
	return stats, firstErr
}

func (o *Outbox) replayBatch(ctx context.Context, now time.Time, items []replayItem, stats ReplayStats, firstErr error, delivery BatchDelivery) (ReplayStats, error) {
	receipts := make([]llm.AttemptUsageReceipt, len(items))
	expected := make(map[string]string, len(items))
	for i := range items {
		receipts[i] = items[i].stored.Receipt
		expected[receipts[i].ReceiptID] = receipts[i].CanonicalBodyHash
	}
	acks, deliveryErr := delivery.DeliverBatch(ctx, receipts)
	if deliveryErr != nil {
		for i := range items {
			stats.Failed++
			if updateErr := o.updateEntry(ctx, items[i].entry, &items[i].stored, false); updateErr != nil && firstErr == nil {
				firstErr = updateErr
			}
		}
		o.noteFailure(now)
		if firstErr == nil {
			firstErr = fmt.Errorf("%w: receipt batch delivery returned an error", ErrDeliveryFailed)
		}
		return stats, firstErr
	}
	stats.Delivered += len(items)
	acked := make(map[string]struct{}, len(acks))
	for _, ack := range acks {
		want, ok := expected[ack.ReceiptID]
		if !ok || want != ack.CanonicalBodyHash {
			return o.failBatch(ctx, now, items, stats, firstErr, "receipt acknowledgement identity or hash mismatch")
		}
		if _, duplicate := acked[ack.ReceiptID]; duplicate {
			return o.failBatch(ctx, now, items, stats, firstErr, "duplicate receipt acknowledgement")
		}
		acked[ack.ReceiptID] = struct{}{}
	}
	partial := false
	for i := range items {
		item := &items[i]
		if _, ok := acked[item.stored.Receipt.ReceiptID]; !ok {
			partial = true
			stats.Failed++
			if updateErr := o.updateEntry(ctx, item.entry, &item.stored, false); updateErr != nil && firstErr == nil {
				firstErr = updateErr
			}
			continue
		}
		item.stored.Status = "acked"
		item.stored.NextAttemptAt = time.Time{}
		item.stored.LastFailureCode = ""
		if updateErr := o.updateEntry(ctx, item.entry, &item.stored, true); updateErr != nil {
			if firstErr == nil {
				firstErr = updateErr
			}
			continue
		}
		stats.Acknowledged++
	}
	if partial {
		o.noteFailure(now)
		if firstErr == nil {
			firstErr = fmt.Errorf("%w: receipt batch was only partially acknowledged", ErrDeliveryFailed)
		}
	} else {
		o.noteSuccess()
	}
	return stats, firstErr
}

func (o *Outbox) failBatch(ctx context.Context, now time.Time, items []replayItem, stats ReplayStats, firstErr error, reason string) (ReplayStats, error) {
	for i := range items {
		stats.Failed++
		if updateErr := o.updateEntry(ctx, items[i].entry, &items[i].stored, false); updateErr != nil && firstErr == nil {
			firstErr = updateErr
		}
	}
	o.noteFailure(now)
	if firstErr == nil {
		firstErr = fmt.Errorf("%w: %s", ErrDeliveryFailed, reason)
	}
	return stats, firstErr
}

// Run wakes immediately after enqueue, sleeps until the next due item, and
// reconciles legacy/crash-recovery records only at a slow jittered cadence.
func (o *Outbox) Run(ctx context.Context) error {
	if err := o.Prepare(ctx); err != nil {
		o.reportHealth(false)
		return err
	}
	nextReconcile := o.clock().UTC().Add(o.reconcileInterval)
	// A delivery failure has a durable per-receipt deadline, while a failed
	// StateStore read, corrupt due record, or failed recovery handoff has no
	// trustworthy deadline to consult. Keep the latter on its own bounded
	// maintenance cadence. In particular, never return from this worker while
	// leaving runtime.info strict-ready: the assembly deliberately treats a
	// stopped worker as a degraded runtime, not a successful best effort.
	var retryAt time.Time
	maintenanceFailures := 0
	reconcileDegraded := false
	replayDegraded := false
	healthDegraded := false
	degrade := func() {
		if !healthDegraded {
			o.reportHealth(false)
			healthDegraded = true
		}
	}
	recoverHealth := func() {
		if healthDegraded && !reconcileDegraded && !replayDegraded {
			o.reportHealth(true)
			healthDegraded = false
		}
	}
	for {
		// Once the delivery breaker is open, Replay is intentionally a
		// pure in-memory refusal.  Do not ask the durable due index on every
		// pass: an immediately-due entry would otherwise turn the breaker
		// into a tight read loop.  We take one due snapshot when the breaker
		// opens and then sleep until the later of that due time and the
		// breaker deadline.  Enqueue still wakes the loop if a newer entry
		// arrives while it is parked.
		openBefore := o.circuitOpen()
		stats, replayErr := o.Replay(ctx)
		now := o.clock().UTC()
		if replayErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			replayDegraded = true
			degrade()
			// Delivery failures already persist a bounded next-attempt deadline
			// (and ErrCircuitOpen has its own deadline). Any other replay error
			// may be a transient StateStore outage, but it must not turn a
			// corrupt record into a tight read loop.
			if !errors.Is(replayErr, ErrDeliveryFailed) && !errors.Is(replayErr, ErrCircuitOpen) {
				maintenanceFailures++
				retryAt = now.Add(backoff(o.baseBackoff, o.maxBackoff, maintenanceFailures, "outbox-replay"))
			}
		} else if stats.Failed == 0 && stats.Deferred == 0 && !o.circuitOpen() {
			// A due receipt that is merely deferred has not recovered a prior
			// delivery failure. Recovery is observed only after the due work is
			// clean (or there is no work), not after an intervening no-op read.
			replayDegraded = false
			if !reconcileDegraded {
				maintenanceFailures = 0
				retryAt = time.Time{}
			}
			recoverHealth()
		}
		wakeAt := nextReconcile
		if !retryAt.IsZero() && retryAt.Before(wakeAt) {
			wakeAt = retryAt
		}
		if openUntil, open := o.circuitOpenUntil(); open {
			// A just-opened breaker gets one durable due lookup so that a
			// later due item can extend the sleep.  An already-open breaker
			// must not reload the index until the deadline has passed.
			if !openBefore {
				if due, ok := o.nextDue(ctx); ok && due.After(openUntil) {
					openUntil = due
				}
			}
			if openUntil.Before(wakeAt) {
				wakeAt = openUntil
			}
		} else if replayErr == nil || errors.Is(replayErr, ErrDeliveryFailed) {
			if due, ok := o.nextDue(ctx); ok && due.Before(wakeAt) {
				wakeAt = due
			}
		}
		delay := durationUntil(now, wakeAt)
		if delay <= 0 {
			delay = time.Nanosecond
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case _, ok := <-o.wake:
			timer.Stop()
			if !ok {
				return ErrClosed
			}
		case <-timer.C:
			now = o.clock().UTC()
			if !now.Before(nextReconcile) || (reconcileDegraded && !retryAt.IsZero() && !now.Before(retryAt)) {
				if err := o.Reconcile(ctx); err != nil && !errors.Is(err, state.ErrNotFound) {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					reconcileDegraded = true
					degrade()
					maintenanceFailures++
					retryAt = now.Add(backoff(o.baseBackoff, o.maxBackoff, maintenanceFailures, "outbox-reconcile"))
					nextReconcile = now.Add(o.reconcileInterval)
					continue
				}
				reconcileDegraded = false
				if !replayDegraded {
					maintenanceFailures = 0
					retryAt = time.Time{}
					recoverHealth()
				}
				nextReconcile = now.Add(o.reconcileInterval)
			}
		}
	}
}

// Prepare performs the one bounded startup reconciliation exactly once. Stock
// serve calls it synchronously so an undisclosed legacy backlog or store error
// fails boot before the outbox is advertised as ready. Direct embedders may
// call Run alone; Run invokes Prepare before entering the background loop.
func (o *Outbox) Prepare(ctx context.Context) error {
	o.prepareMu.Lock()
	defer o.prepareMu.Unlock()
	if o.prepared {
		return nil
	}
	if err := o.Reconcile(ctx); err != nil && !errors.Is(err, state.ErrNotFound) {
		o.reportHealth(false)
		return err
	}
	o.prepared = true
	o.reportHealth(true)
	return nil
}

func (o *Outbox) reportHealth(healthy bool) {
	if reporter, ok := o.delivery.(OutboxHealthReporter); ok {
		reporter.SetOutboxHealth(healthy)
	}
}

func durationUntil(now, target time.Time) time.Duration {
	if target.IsZero() {
		return 0
	}
	if !target.After(now) {
		return 0
	}
	return target.Sub(now)
}

// Reconcile imports settled receipts from a runtime reservation manager and
// adopts legacy receipt records. It is bounded and intentionally infrequent.
func (o *Outbox) Reconcile(ctx context.Context) error {
	if o.pending != nil {
		receipts, err := o.pending.PendingReceipts(ctx, o.maxBatch)
		if err != nil {
			return err
		}
		for _, receipt := range receipts {
			receiptCtx, err := verifiedReceiptContext(ctx, receipt)
			if err != nil {
				return err
			}
			if err := o.Enqueue(receiptCtx, receipt); err != nil {
				return err
			}
			if err := o.pending.AcknowledgePendingReceipt(ctx, receipt); err != nil {
				return err
			}
		}
	}
	return o.reconcileLegacyOnce(ctx)
}

func (o *Outbox) reconcileLegacyOnce(ctx context.Context) error {
	marker, err := o.store.Load(ctx, internalQ(), legacyMarkerKind)
	if err == nil {
		var value legacyReconcileMarker
		if jsonErr := json.Unmarshal(marker.Bytes, &value); jsonErr != nil || value.Version != 1 || marker.Kind != legacyMarkerKind || marker.Identity != internalQ() {
			return fmt.Errorf("%w: invalid legacy reconciliation marker", ErrInvalidReceipt)
		}
		return nil
	}
	if !errors.Is(err, state.ErrNotFound) {
		return err
	}
	legacy, err := o.store.ListKindBounded(ctx, state.ListScope{MaintenanceScoped: true}, kindPrefix, o.maxBatch+1)
	if err != nil {
		return err
	}
	if len(legacy) > o.maxBatch {
		return fmt.Errorf("%w: retained receipt records exceed the configured batch", ErrReconcileOverflow)
	}
	for _, rec := range legacy {
		stored, decodeErr := decodeStored(rec.Bytes)
		if decodeErr != nil {
			return decodeErr
		}
		if !receiptMatchesRecord(rec, stored.Receipt) {
			return fmt.Errorf("%w: legacy receipt identity mismatch", ErrInvalidReceipt)
		}
		if stored.Status == "acked" {
			// Acknowledged receipt bodies are retained for idempotent replay,
			// but must never be reintroduced into the due index by the slow
			// legacy reconciliation scan.
			continue
		}
		if err := o.ensureDue(ctx, rec.Identity, stored.Receipt); err != nil {
			return err
		}
	}
	body, err := json.Marshal(legacyReconcileMarker{Version: 1})
	if err != nil {
		return fmt.Errorf("%w: encode legacy reconciliation marker", ErrInvalidReceipt)
	}
	next := state.NewInternalRecord(state.NewEventID(), internalQ(), legacyMarkerKind, body)
	if err := o.store.SaveIf(ctx, []state.SlotExpectation{state.InternalSlotExpectation(internalQ(), legacyMarkerKind, "")}, next); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return o.reconcileLegacyOnce(ctx)
		}
		return err
	}
	return nil
}

func verifiedReceiptContext(ctx context.Context, receipt llm.AttemptUsageReceipt) (context.Context, error) {
	id := identity.Identity{TenantID: receipt.TenantID, UserID: receipt.UserID, SessionID: receipt.SessionID}
	verified, err := identity.WithVerified(ctx, id)
	if err != nil {
		return nil, err
	}
	return identity.WithRun(verified, id, receipt.LogicalRunID)
}

func (o *Outbox) ensureDue(ctx context.Context, q identity.Quadruple, receipt llm.AttemptUsageReceipt) error {
	for range 8 {
		rec, idx, err := o.loadIndex(ctx)
		if err != nil && !errors.Is(err, state.ErrNotFound) {
			return err
		}
		if hasDue(idx, receipt.ReceiptID, q) {
			return nil
		}
		idx.Entries = append(idx.Entries, dueEntry{ReceiptID: receipt.ReceiptID, Identity: q})
		body, marshalErr := json.Marshal(idx)
		if marshalErr != nil {
			return marshalErr
		}
		next := state.NewInternalRecord(state.NewEventID(), internalQ(), dueKind, body)
		if err := o.store.SaveIf(ctx, []state.SlotExpectation{state.InternalSlotExpectation(internalQ(), dueKind, expectedID(rec))}, next); err != nil {
			if errors.Is(err, state.ErrConditionFailed) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("%w: due-index reconciliation did not converge", llm.ErrUsageReceiptUnavailable)
}

func (o *Outbox) updateEntry(ctx context.Context, entry dueEntry, stored *storedReceipt, remove bool) error {
	idxRec, idx, err := o.loadIndex(ctx)
	if err != nil {
		return err
	}
	pos := findDue(idx, entry)
	if pos < 0 {
		return nil
	}
	if stored == nil {
		remove = true
	}
	if !remove && stored.Status != "acked" {
		stored.Attempts++
		stored.Status = "pending"
		stored.LastFailureCode = "delivery_failed"
		stored.NextAttemptAt = o.clock().UTC().Add(backoff(o.baseBackoff, o.maxBackoff, stored.Attempts, entry.ReceiptID))
		idx.Entries[pos].NextAttemptAt = stored.NextAttemptAt
	} else {
		idx.Entries = append(idx.Entries[:pos], idx.Entries[pos+1:]...)
	}
	receiptRec, recErr := o.store.Load(ctx, entry.Identity, receiptKind(entry.ReceiptID))
	if errors.Is(recErr, state.ErrNotFound) {
	} else if recErr != nil {
		return recErr
	}
	idxBody, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	exps := []state.SlotExpectation{state.InternalSlotExpectation(internalQ(), dueKind, idxRec.ID)}
	writes := []state.StateRecord{state.NewInternalRecord(state.NewEventID(), internalQ(), dueKind, idxBody)}
	if recErr == nil {
		if stored == nil {
			decoded, decodeErr := decodeStored(receiptRec.Bytes)
			if decodeErr != nil {
				return decodeErr
			}
			stored = &decoded
		}
		body, marshalErr := json.Marshal(*stored)
		if marshalErr != nil {
			return marshalErr
		}
		exps = append(exps, state.InternalSlotExpectation(entry.Identity, receiptRec.Kind, receiptRec.ID))
		writes = append(writes, state.NewInternalRecord(state.NewEventID(), entry.Identity, receiptRec.Kind, body))
	}
	if err := o.store.SaveBatchIf(ctx, exps, writes); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return fmt.Errorf("%w: concurrent receipt update", ErrDeliveryFailed)
		}
		return err
	}
	return nil
}

func (o *Outbox) nextDue(ctx context.Context) (time.Time, bool) {
	_, idx, err := o.loadIndex(ctx)
	if err != nil {
		return time.Time{}, false
	}
	var next time.Time
	for _, e := range idx.Entries {
		if next.IsZero() || e.NextAttemptAt.Before(next) {
			next = e.NextAttemptAt
		}
	}
	return next, !next.IsZero()
}

func (o *Outbox) loadIndex(ctx context.Context) (state.StateRecord, dueIndex, error) {
	rec, err := o.store.Load(ctx, internalQ(), dueKind)
	if err != nil {
		return state.StateRecord{}, dueIndex{}, err
	}
	var idx dueIndex
	if err := json.Unmarshal(rec.Bytes, &idx); err != nil {
		return state.StateRecord{}, dueIndex{}, fmt.Errorf("%w: due index JSON", ErrInvalidReceipt)
	}
	return rec, idx, nil
}

func internalQ() identity.Quadruple                { return identity.InternalCoordinationQuadruple() }
func receiptKind(id string) string                 { return kindPrefix + hex.EncodeToString([]byte(id)) }
func expectedID(r state.StateRecord) state.EventID { return r.ID }
func hasDue(idx dueIndex, id string, q identity.Quadruple) bool {
	return findDue(idx, dueEntry{ReceiptID: id, Identity: q}) >= 0
}
func findDue(idx dueIndex, entry dueEntry) int {
	for i, e := range idx.Entries {
		if e.ReceiptID == entry.ReceiptID && e.Identity == entry.Identity {
			return i
		}
	}
	return -1
}

func (o *Outbox) breakerError() error {
	if _, open := o.circuitOpenUntil(); open {
		return ErrCircuitOpen
	}
	return nil
}

func (o *Outbox) circuitOpen() bool {
	_, open := o.circuitOpenUntil()
	return open
}

func (o *Outbox) circuitOpenUntil() (time.Time, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.circuitOpenTil.IsZero() && o.clock().UTC().Before(o.circuitOpenTil) {
		return o.circuitOpenTil, true
	}
	return time.Time{}, false
}
func (o *Outbox) noteFailure(now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.failures++
	if o.failures >= o.circuitFailures {
		o.circuitOpenTil = now.Add(o.circuitOpenFor)
	}
}
func (o *Outbox) noteSuccess() {
	o.mu.Lock()
	o.failures = 0
	o.circuitOpenTil = time.Time{}
	o.mu.Unlock()
}
func backoff(base, max time.Duration, attempts int, receiptID string) time.Duration {
	if attempts <= 1 {
		return jitterBackoff(base, base, max, receiptID, attempts)
	}
	d := float64(base) * math.Pow(2, float64(attempts-1))
	if d >= float64(max) {
		return jitterBackoff(max, base, max, receiptID, attempts)
	}
	return jitterBackoff(time.Duration(d), base, max, receiptID, attempts)
}

// jitterBackoff adds stable per-receipt jitter so a coordinator recovery does
// not wake every runtime at once. Stability keeps response-loss replay
// deterministic while the +/-10% spread avoids synchronized retry bursts.
func jitterBackoff(value, base, max time.Duration, receiptID string, attempts int) time.Duration {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", receiptID, attempts)))
	spread := value / 10
	if spread <= 0 {
		return value
	}
	width := uint64(2*spread + 1)
	rawOffset := binary.BigEndian.Uint64(digest[:8]) % width
	// rawOffset is strictly below width, and width is at most one fifth of
	// time.Duration's positive range plus one, so this conversion cannot overflow.
	offset := time.Duration(rawOffset) - spread // #nosec G115 -- bounded by width above.
	value += offset
	if value < base {
		return base
	}
	if value > max {
		return max
	}
	return value
}

func decodeStored(body []byte) (storedReceipt, error) {
	var stored storedReceipt
	if err := json.Unmarshal(body, &stored); err != nil {
		return storedReceipt{}, fmt.Errorf("%w: stored receipt JSON", ErrInvalidReceipt)
	}
	if err := validateReceipt(stored.Receipt); err != nil {
		return storedReceipt{}, err
	}
	if stored.Status != "pending" && stored.Status != "acked" {
		return storedReceipt{}, fmt.Errorf("%w: unknown receipt status", ErrInvalidReceipt)
	}
	if stored.Attempts < 0 {
		return storedReceipt{}, fmt.Errorf("%w: negative attempt count", ErrInvalidReceipt)
	}
	return stored, nil
}
func receiptMatchesRecord(record state.StateRecord, receipt llm.AttemptUsageReceipt) bool {
	return record.Kind == receiptKind(receipt.ReceiptID) && record.Identity.TenantID == receipt.TenantID && record.Identity.UserID == receipt.UserID && record.Identity.SessionID == receipt.SessionID && record.Identity.RunID == receipt.LogicalRunID
}

func validateReceipt(receipt llm.AttemptUsageReceipt) error {
	if receipt.ReceiptID == "" || receipt.IdempotencyKey == "" || receipt.ReceiptID != receipt.IdempotencyKey {
		return fmt.Errorf("%w: receipt id and idempotency key must match", ErrInvalidReceipt)
	}
	if err := llm.ValidateAttemptUsageReceipt(receipt); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidReceipt, err)
	}
	return nil
}
