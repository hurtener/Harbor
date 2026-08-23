// Package receipts persists content-free provider-attempt usage facts until
// a coordinator acknowledges them. It deliberately sits on StateStore rather
// than introducing a receipt-specific database: the same outbox works with
// Harbor's in-memory, SQLite, and PostgreSQL drivers.
package receipts

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/state"
)

const kindPrefix = state.InternalKindPrefix + "inference.receipt/"

var (
	// ErrInvalidReceipt is returned when a receipt is malformed or its
	// canonical body hash does not match. The outbox never stores unverifiable
	// usage facts.
	ErrInvalidReceipt = errors.New("llm/receipts: invalid usage receipt")
	// ErrDeliveryFailed reports that at least one pending receipt could not be
	// delivered. The record remains pending and is retried after backoff.
	ErrDeliveryFailed = errors.New("llm/receipts: delivery failed")
	// ErrCircuitOpen prevents a replay storm while the coordinator is down.
	ErrCircuitOpen = errors.New("llm/receipts: delivery circuit open")
)

// Delivery receives one content-free receipt. Implementations must treat
// ReceiptID + CanonicalBodyHash as an idempotency key and acknowledge a
// previously accepted duplicate, including after a response-loss retry.
type Delivery interface {
	Deliver(context.Context, llm.AttemptUsageReceipt) error
}

// Config controls bounded replay. Zero values select conservative defaults.
type Config struct {
	Store           state.StateStore
	Delivery        Delivery
	MaxBatch        int
	BaseBackoff     time.Duration
	MaxBackoff      time.Duration
	CircuitFailures int
	CircuitOpenFor  time.Duration
	PollInterval    time.Duration
	Clock           func() time.Time
}

// Outbox is safe for concurrent Enqueue and Replay calls. StateStore is the
// durable authority; the mutex only protects the process-local breaker.
type Outbox struct {
	store           state.StateStore
	delivery        Delivery
	maxBatch        int
	baseBackoff     time.Duration
	maxBackoff      time.Duration
	circuitFailures int
	circuitOpenFor  time.Duration
	pollInterval    time.Duration
	clock           func() time.Time

	mu             sync.Mutex
	failures       int
	circuitOpenTil time.Time
}

type storedReceipt struct {
	Receipt         llm.AttemptUsageReceipt `json:"receipt"`
	Status          string                  `json:"status"`
	Attempts        int                     `json:"attempts"`
	NextAttemptAt   time.Time               `json:"next_attempt_at"`
	LastFailureCode string                  `json:"last_failure_code,omitempty"`
}

// New constructs an outbox and validates its mandatory durable collaborators.
func New(cfg Config) (*Outbox, error) {
	if cfg.Store == nil || cfg.Delivery == nil {
		return nil, fmt.Errorf("%w: store and delivery are required", llm.ErrUsageReceiptUnavailable)
	}
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = 64
	}
	if cfg.MaxBatch > state.MaxStateMaintenanceListLimit {
		return nil, fmt.Errorf("%w: max batch %d exceeds state bound", ErrInvalidReceipt, cfg.MaxBatch)
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = time.Minute
	}
	if cfg.MaxBackoff < cfg.BaseBackoff {
		return nil, fmt.Errorf("%w: max backoff before base backoff", ErrInvalidReceipt)
	}
	if cfg.CircuitFailures <= 0 {
		cfg.CircuitFailures = 3
	}
	if cfg.CircuitOpenFor <= 0 {
		cfg.CircuitOpenFor = 30 * time.Second
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	return &Outbox{
		store: cfg.Store, delivery: cfg.Delivery, maxBatch: cfg.MaxBatch,
		baseBackoff: cfg.BaseBackoff, maxBackoff: cfg.MaxBackoff,
		circuitFailures: cfg.CircuitFailures, circuitOpenFor: cfg.CircuitOpenFor,
		pollInterval: cfg.PollInterval, clock: cfg.Clock,
	}, nil
}

// Enqueue durably stores one verified attempt receipt. It is idempotent by
// receipt identity and canonical body hash, including when an earlier replay
// already advanced the StateStore EventID for the slot.
func (o *Outbox) Enqueue(ctx context.Context, receipt llm.AttemptUsageReceipt) error {
	if err := validateReceipt(receipt); err != nil {
		return err
	}
	q, ok := identity.QuadrupleFrom(ctx)
	if !ok || q.TenantID != receipt.TenantID || q.UserID != receipt.UserID || q.SessionID != receipt.SessionID || q.RunID != receipt.LogicalRunID {
		return fmt.Errorf("%w: receipt identity is not the verified call scope", ErrInvalidReceipt)
	}
	return o.enqueueAt(ctx, q, receipt)
}

func (o *Outbox) enqueueAt(ctx context.Context, q identity.Quadruple, receipt llm.AttemptUsageReceipt) error {
	kind := receiptKind(receipt.ReceiptID)
	for attempt := 0; attempt < 3; attempt++ {
		existing, err := o.store.Load(ctx, q, kind)
		if err == nil {
			stored, decodeErr := decodeStored(existing.Bytes)
			if decodeErr != nil {
				return decodeErr
			}
			if !receiptMatchesRecord(existing, stored.Receipt) {
				return fmt.Errorf("%w: stored receipt identity or kind mismatch", ErrInvalidReceipt)
			}
			if stored.Receipt.CanonicalBodyHash != receipt.CanonicalBodyHash {
				return fmt.Errorf("%w: receipt id reused with a different body", llm.ErrUsageReceiptUnavailable)
			}
			return nil
		}
		if !errors.Is(err, state.ErrNotFound) {
			return fmt.Errorf("llm/receipts: load pending receipt: %w", err)
		}
		body, marshalErr := json.Marshal(storedReceipt{Receipt: receipt, Status: "pending"})
		if marshalErr != nil {
			return fmt.Errorf("%w: encode receipt: %v", ErrInvalidReceipt, marshalErr)
		}
		id := state.NewEventID()
		next := state.NewInternalRecord(id, q, kind, body)
		err = o.store.SaveIf(ctx, []state.SlotExpectation{state.InternalSlotExpectation(q, kind, "")}, next)
		if err == nil {
			return nil
		}
		if !errors.Is(err, state.ErrConditionFailed) {
			return fmt.Errorf("llm/receipts: enqueue: %w", err)
		}
	}
	return fmt.Errorf("%w: concurrent enqueue did not converge", llm.ErrUsageReceiptUnavailable)
}

// Replay delivers at most MaxBatch pending receipts whose durable backoff has
// elapsed, then ACKs successful deliveries. It returns aggregate counts and a
// wrapped ErrDeliveryFailed if any item remains pending.
type ReplayStats struct {
	Seen         int
	Delivered    int
	Acknowledged int
	Deferred     int
	Failed       int
}

func (o *Outbox) Replay(ctx context.Context) (ReplayStats, error) {
	if err := o.breakerError(); err != nil {
		return ReplayStats{}, err
	}
	records, err := o.store.ListKindBounded(ctx, state.ListScope{MaintenanceScoped: true}, kindPrefix, o.maxBatch)
	if err != nil {
		return ReplayStats{}, fmt.Errorf("llm/receipts: list pending: %w", err)
	}
	stats := ReplayStats{Seen: len(records)}
	var firstErr error
	now := o.clock().UTC()
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		stored, decodeErr := decodeStored(record.Bytes)
		if decodeErr != nil {
			return stats, decodeErr
		}
		if !receiptMatchesRecord(record, stored.Receipt) {
			return stats, fmt.Errorf("%w: receipt record identity or kind mismatch", ErrInvalidReceipt)
		}
		if stored.Status == "acked" {
			continue
		}
		if !stored.NextAttemptAt.IsZero() && now.Before(stored.NextAttemptAt) {
			stats.Deferred++
			continue
		}
		if err := o.delivery.Deliver(ctx, stored.Receipt); err != nil {
			stats.Failed++
			if updateErr := o.markFailure(ctx, record, stored); updateErr != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("%w: %v; retry state: %w", ErrDeliveryFailed, err, updateErr)
				}
			} else if firstErr == nil {
				firstErr = fmt.Errorf("%w: receipt delivery returned an error", ErrDeliveryFailed)
			}
			o.noteFailure(now)
			continue
		}
		stats.Delivered++
		if ackErr := o.ackRecord(ctx, record, stored); ackErr != nil {
			if firstErr == nil {
				firstErr = ackErr
			}
			continue
		}
		stats.Acknowledged++
		o.noteSuccess()
	}
	return stats, firstErr
}

// Run is an optional bounded replay loop. It owns no goroutine until called,
// and always joins on context cancellation.
func (o *Outbox) Run(ctx context.Context) error {
	ticker := time.NewTicker(o.pollInterval)
	defer ticker.Stop()
	for {
		if _, err := o.Replay(ctx); err != nil && !errors.Is(err, ErrCircuitOpen) && !errors.Is(err, ErrDeliveryFailed) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (o *Outbox) ackRecord(ctx context.Context, record state.StateRecord, stored storedReceipt) error {
	stored.Status = "acked"
	stored.NextAttemptAt = time.Time{}
	stored.LastFailureCode = ""
	body, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("llm/receipts: encode ACK: %w", err)
	}
	next := state.NewInternalRecord(state.NewEventID(), record.Identity, record.Kind, body)
	if err := o.store.SaveIf(ctx, []state.SlotExpectation{state.InternalSlotExpectation(record.Identity, record.Kind, record.ID)}, next); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return fmt.Errorf("%w: ACK generation changed", ErrDeliveryFailed)
		}
		return fmt.Errorf("llm/receipts: ACK: %w", err)
	}
	return nil
}

func (o *Outbox) markFailure(ctx context.Context, record state.StateRecord, stored storedReceipt) error {
	stored.Attempts++
	stored.Status = "pending"
	stored.LastFailureCode = "delivery_failed"
	stored.NextAttemptAt = o.clock().UTC().Add(backoff(o.baseBackoff, o.maxBackoff, stored.Attempts))
	body, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("llm/receipts: encode retry state: %w", err)
	}
	next := state.NewInternalRecord(state.NewEventID(), record.Identity, record.Kind, body)
	if err := o.store.SaveIf(ctx, []state.SlotExpectation{state.InternalSlotExpectation(record.Identity, record.Kind, record.ID)}, next); err != nil {
		return fmt.Errorf("llm/receipts: persist retry state: %w", err)
	}
	return nil
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
	seconds := float64(base) * math.Pow(2, float64(attempts-1))
	if seconds >= float64(max) {
		return max
	}
	return time.Duration(seconds)
}

func receiptKind(id string) string {
	return kindPrefix + hex.EncodeToString([]byte(id))
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
	return record.Kind == receiptKind(receipt.ReceiptID) &&
		record.Identity.TenantID == receipt.TenantID &&
		record.Identity.UserID == receipt.UserID &&
		record.Identity.SessionID == receipt.SessionID &&
		record.Identity.RunID == receipt.LogicalRunID
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
