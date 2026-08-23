// Package receipts persists content-free provider-attempt usage facts until
// a coordinator acknowledges them.  The normal delivery path reads one
// durable due index; the old receipt-prefix scan is only a slow reconciliation
// path for records written by older Harbor versions.
package receipts

import (
	"context"
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
	kindPrefix = state.InternalKindPrefix + "inference.receipt/"
	dueKind    = state.InternalKindPrefix + "inference.receipt.due"
)

var (
	ErrInvalidReceipt = errors.New("llm/receipts: invalid usage receipt")
	ErrDeliveryFailed = errors.New("llm/receipts: delivery failed")
	ErrCircuitOpen    = errors.New("llm/receipts: delivery circuit open")
	ErrClosed         = errors.New("llm/receipts: outbox closed")
)

type Delivery interface {
	Deliver(context.Context, llm.AttemptUsageReceipt) error
}

// PendingReceiptSource is a low-frequency crash-recovery source.  It is not
// used by Replay's hot path; implementations must return a bounded result.
type PendingReceiptSource interface {
	PendingReceipts(context.Context, int) ([]llm.AttemptUsageReceipt, error)
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

func New(cfg Config) (*Outbox, error) {
	if cfg.Store == nil || cfg.Delivery == nil {
		return nil, fmt.Errorf("%w: store and delivery are required", llm.ErrUsageReceiptUnavailable)
	}
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = 64
	}
	if cfg.MaxBatch > state.MaxStateMaintenanceListLimit {
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
	for n := 0; n < 8; n++ {
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
			writes = append(writes, state.NewInternalRecord(state.NewEventID(), q, receiptKind, mustJSON(stored)))
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
	rec, idx, err := o.loadIndex(ctx)
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
			if updateErr := o.updateEntry(ctx, rec, idx, entry, nil, true); updateErr != nil && firstErr == nil {
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
			_ = o.updateEntry(ctx, rec, idx, entry, &stored, true)
			continue
		}
		if !stored.NextAttemptAt.IsZero() && now.Before(stored.NextAttemptAt) {
			stats.Deferred++
			continue
		}
		if deliveryErr := o.delivery.Deliver(ctx, stored.Receipt); deliveryErr != nil {
			stats.Failed++
			if updateErr := o.updateEntry(ctx, rec, idx, entry, &stored, false); updateErr != nil && firstErr == nil {
				firstErr = updateErr
			} else if firstErr == nil {
				firstErr = fmt.Errorf("%w: receipt delivery returned an error", ErrDeliveryFailed)
			}
			o.noteFailure(now)
			continue
		}
		stats.Delivered++
		stored.Status = "acked"
		stored.NextAttemptAt = time.Time{}
		stored.LastFailureCode = ""
		if updateErr := o.updateEntry(ctx, rec, idx, entry, &stored, true); updateErr != nil {
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

// Run wakes immediately after enqueue, sleeps until the next due item, and
// reconciles legacy/crash-recovery records only at a slow jittered cadence.
func (o *Outbox) Run(ctx context.Context) error {
	if err := o.Reconcile(ctx); err != nil && !errors.Is(err, state.ErrNotFound) {
		return err
	}
	for {
		if _, err := o.Replay(ctx); err != nil && !errors.Is(err, ErrCircuitOpen) && !errors.Is(err, ErrDeliveryFailed) {
			return err
		}
		delay := o.reconcileInterval
		if due, ok := o.nextDue(ctx); ok {
			until := time.Until(due)
			if until < 0 {
				until = 0
			}
			if until < delay {
				delay = until
			}
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
			if err := o.Reconcile(ctx); err != nil && !errors.Is(err, state.ErrNotFound) {
				return err
			}
		}
	}
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
		}
	}
	legacy, err := o.store.ListKindBounded(ctx, state.ListScope{MaintenanceScoped: true}, kindPrefix, o.maxBatch)
	if err != nil {
		return err
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
	for n := 0; n < 8; n++ {
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

func (o *Outbox) updateEntry(ctx context.Context, oldIdxRec state.StateRecord, oldIdx dueIndex, entry dueEntry, stored *storedReceipt, remove bool) error {
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
		stored.NextAttemptAt = o.clock().UTC().Add(backoff(o.baseBackoff, o.maxBackoff, stored.Attempts))
		idx.Entries[pos].NextAttemptAt = stored.NextAttemptAt
	} else {
		idx.Entries = append(idx.Entries[:pos], idx.Entries[pos+1:]...)
	}
	receiptRec, recErr := o.store.Load(ctx, entry.Identity, receiptKind(entry.ReceiptID))
	if errors.Is(recErr, state.ErrNotFound) {
		remove = true
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
func mustJSON(v any) []byte                        { b, _ := json.Marshal(v); return b }
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
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.circuitOpenTil.IsZero() && o.clock().UTC().Before(o.circuitOpenTil) {
		return ErrCircuitOpen
	}
	return nil
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
func backoff(base, max time.Duration, attempts int) time.Duration {
	if attempts <= 1 {
		return base
	}
	d := float64(base) * math.Pow(2, float64(attempts-1))
	if d >= float64(max) {
		return max
	}
	return time.Duration(d)
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
	if receipt.ReceiptID == "" || receipt.IdempotencyKey == "" || receipt.ReceiptID != receipt.IdempotencyKey || receipt.GrantID == "" || receipt.OrganizationID == "" || receipt.RuntimeID == "" || receipt.TenantID == "" || receipt.UserID == "" || receipt.SessionID == "" || receipt.LogicalRunID == "" || receipt.Provider == "" || receipt.ProviderModelID == "" || receipt.ProviderConnectionID == "" || receipt.ProviderConnectionGeneration == 0 || receipt.RouteID == "" || receipt.CredentialAssetGeneration == 0 || receipt.PolicyGeneration == 0 || receipt.AttemptNumber <= 0 || receipt.RetryNumber < 0 || receipt.FallbackHop < 0 {
		return fmt.Errorf("%w: missing identity, route, generation, or idempotency field", ErrInvalidReceipt)
	}
	if receipt.PromptTokens < 0 || receipt.CompletionTokens < 0 || receipt.ReasoningTokens < 0 || receipt.TotalTokens < 0 || receipt.CacheReadTokens < 0 || receipt.CacheWriteTokens < 0 || receipt.InputCostMicros < 0 || receipt.OutputCostMicros < 0 || receipt.ReasoningCostMicros < 0 || receipt.TotalCostMicros < 0 || receipt.LatencyMS < 0 {
		return fmt.Errorf("%w: negative usage value", ErrInvalidReceipt)
	}
	if receipt.Status != "success" && receipt.Status != "error" && receipt.Status != "canceled" {
		return fmt.Errorf("%w: unknown receipt status", ErrInvalidReceipt)
	}
	if receipt.StartedAt.IsZero() || receipt.CompletedAt.IsZero() || receipt.CompletedAt.Before(receipt.StartedAt) {
		return fmt.Errorf("%w: invalid receipt interval", ErrInvalidReceipt)
	}
	hash, err := llm.CanonicalAttemptUsageReceiptBodyHash(receipt)
	if err != nil || receipt.CanonicalBodyHash != hash {
		return fmt.Errorf("%w: canonical body hash mismatch", ErrInvalidReceipt)
	}
	return nil
}
