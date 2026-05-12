package governance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/state"
)

// kindGovernanceBucket is the StateStore Kind for per-(identity, model)
// token-bucket state. One record per identity carries the per-model
// bucket map (mirrors `kindGovernanceCost`).
const kindGovernanceBucket = "governance.bucket"

// RateLimiter is the Phase 36b `Subsystem` implementing a token bucket
// per `(identity, model)`. Bucket state lives in `state.StateStore` so
// it survives restart; latent default: zero `TierConfig.RateLimit` →
// no enforcement.
//
// Time math (Clock):
//
//   - `Capacity` is the bucket ceiling.
//   - `RefillTokens` are added every `RefillInterval` (continuous
//     accrual via `floor(elapsed / RefillInterval) * RefillTokens`).
//   - `expected_tokens` per call defaults to `req.MaxTokens` if set,
//     else 1. Phase 36b consciously does NOT pre-charge `MaxTokens` for
//     unbounded requests — the operator who needs that sets a tier
//     `MaxTokens` cap (Phase 36b's other policy).
//
// Concurrency model: per-key state is a `sync.Map`; each `bucketKeyState`
// has its own mutex for drain serialisation. The fan-out at scale is
// per-(identity, model), which is naturally sharded — N concurrent calls
// on different keys never contend.
type RateLimiter struct {
	cfg   Config
	state state.StateStore
	bus   events.EventBus
	clock Clock

	keys sync.Map // map[quadKey]*bucketIdentityState

	closed atomic.Bool
}

// bucketIdentityState aggregates every model's bucket under one
// identity. Persisting all of an identity's buckets in one record keeps
// the StateStore write count proportional to in-flight identities, not
// model-name proliferation.
type bucketIdentityState struct {
	mu       sync.Mutex
	buckets  map[string]*tokenBucket // keyed by model
	loaded   bool
	updateAt time.Time
}

// tokenBucket is the per-model state. `Level` is the current available
// token count; `LastRefill` is the most recent refill instant.
type tokenBucket struct {
	Level      int       `json:"level"`
	LastRefill time.Time `json:"last_refill"`
}

type bucketRecord struct {
	ByModel   map[string]*tokenBucket `json:"by_model,omitempty"`
	UpdatedAt time.Time               `json:"updated_at"`
	Schema    int                     `json:"schema"`
}

const bucketRecordSchema = 1

// NewRateLimiter constructs a Phase 36b RateLimiter. Validates deps.
func NewRateLimiter(s state.StateStore, bus events.EventBus, cfg Config) (*RateLimiter, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: state.StateStore is required", ErrInvalidConfig)
	}
	if bus == nil {
		return nil, fmt.Errorf("%w: events.EventBus is required", ErrInvalidConfig)
	}
	return &RateLimiter{cfg: cfg, state: s, bus: bus, clock: cfg.clock()}, nil
}

// PreCall drains the per-(identity, model) bucket by `expected_tokens`.
// Underflow → wrapped ErrRateLimited + governance.rate_limited event.
// Latent: a tier without RateLimit returns nil immediately.
func (r *RateLimiter) PreCall(ctx context.Context, req llm.CompleteRequest) error {
	if r.closed.Load() {
		return ErrClosed
	}
	id, err := identityFromCtx(ctx)
	if err != nil {
		return err
	}
	tier, ok := r.cfg.tierConfig(id)
	if !ok || !tier.RateLimit.IsEnabled() {
		return nil
	}
	quad, err := quadrupleFromCtx(ctx)
	if err != nil {
		return err
	}
	ks, err := r.keyState(ctx, quad)
	if err != nil {
		return err
	}
	want := requestedTokens(req)
	now := r.clock.Now()

	ks.mu.Lock()
	defer ks.mu.Unlock()

	b := ks.buckets[req.Model]
	if b == nil {
		b = &tokenBucket{Level: tier.RateLimit.Capacity, LastRefill: now}
		ks.buckets[req.Model] = b
	}
	r.refill(b, tier.RateLimit, now)
	if b.Level < want {
		r.emitRateLimited(ctx, quad, r.cfg.resolveTier(id), req.Model, want, b.Level, tier.RateLimit, now)
		return errorWith(ErrRateLimited,
			"identity=%s/%s/%s model=%q requested=%d available=%d capacity=%d",
			id.TenantID, id.UserID, id.SessionID, req.Model, want, b.Level, tier.RateLimit.Capacity)
	}
	b.Level -= want
	ks.updateAt = now
	if err := r.persistLocked(ctx, quad, ks); err != nil {
		return fmt.Errorf("governance/ratelimit: persist: %w", err)
	}
	return nil
}

// PostCall is a no-op for the rate limiter — drain happens entirely in
// PreCall. Per RFC §6.15 simplicity, there are no refunds on call
// failure.
func (r *RateLimiter) PostCall(_ context.Context, _ llm.CompleteRequest, _ llm.CompleteResponse, _ error) error {
	return nil
}

// Snapshot reports the per-model bucket levels for the identity. Used
// by tests and inspection paths.
func (r *RateLimiter) Snapshot(ctx context.Context, q identity.Quadruple) (map[string]int, error) {
	if r.closed.Load() {
		return nil, ErrClosed
	}
	if err := state.ValidateIdentity(q); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrIdentityRequired, err)
	}
	ks, err := r.keyState(ctx, q)
	if err != nil {
		return nil, err
	}
	ks.mu.Lock()
	defer ks.mu.Unlock()
	out := make(map[string]int, len(ks.buckets))
	for m, b := range ks.buckets {
		out[m] = b.Level
	}
	return out, nil
}

// Close marks the limiter closed; subsequent calls fail loud.
func (r *RateLimiter) Close(_ context.Context) error {
	r.closed.Store(true)
	return nil
}

func (r *RateLimiter) keyState(ctx context.Context, q identity.Quadruple) (*bucketIdentityState, error) {
	k := quadKeyFor(q)
	if v, ok := r.keys.Load(k); ok {
		ks := v.(*bucketIdentityState)
		if err := r.lazyLoad(ctx, q, ks); err != nil {
			return nil, err
		}
		return ks, nil
	}
	fresh := &bucketIdentityState{buckets: map[string]*tokenBucket{}}
	actual, _ := r.keys.LoadOrStore(k, fresh)
	ks := actual.(*bucketIdentityState)
	if err := r.lazyLoad(ctx, q, ks); err != nil {
		return nil, err
	}
	return ks, nil
}

func (r *RateLimiter) lazyLoad(ctx context.Context, q identity.Quadruple, ks *bucketIdentityState) error {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if ks.loaded {
		return nil
	}
	rec, err := r.state.Load(ctx, q, kindGovernanceBucket)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			ks.loaded = true
			return nil
		}
		return fmt.Errorf("%w: %v", ErrStateUnavailable, err)
	}
	if len(rec.Bytes) == 0 {
		ks.loaded = true
		return nil
	}
	var br bucketRecord
	if err := json.Unmarshal(rec.Bytes, &br); err != nil {
		return fmt.Errorf("%w: unmarshal bucket record: %v", ErrStateUnavailable, err)
	}
	// Forward-compat guard: a future-schema record would be partially
	// parsed silently; fail loud per AGENTS.md §13.
	if br.Schema != 0 && br.Schema != bucketRecordSchema {
		return fmt.Errorf("%w: bucket record schema=%d, runtime supports %d", ErrStateUnavailable, br.Schema, bucketRecordSchema)
	}
	if br.ByModel != nil {
		ks.buckets = br.ByModel
	}
	ks.updateAt = br.UpdatedAt
	ks.loaded = true
	return nil
}

// refill advances the bucket level toward Capacity based on elapsed
// time since LastRefill. Linear refill (`RefillTokens` per
// `RefillInterval`). A zero RefillInterval disables refill entirely
// (one-shot bucket: drains to zero, never refills).
func (r *RateLimiter) refill(b *tokenBucket, cfg RateLimitConfig, now time.Time) {
	if cfg.RefillInterval <= 0 || cfg.RefillTokens <= 0 {
		return
	}
	elapsed := now.Sub(b.LastRefill)
	if elapsed <= 0 {
		return
	}
	intervals := int(elapsed / cfg.RefillInterval)
	if intervals <= 0 {
		return
	}
	b.Level += intervals * cfg.RefillTokens
	if b.Level > cfg.Capacity {
		b.Level = cfg.Capacity
	}
	b.LastRefill = b.LastRefill.Add(time.Duration(intervals) * cfg.RefillInterval)
}

// persistLocked writes the canonical JSON record. Caller MUST hold
// `ks.mu`. State write failures are surfaced loud.
func (r *RateLimiter) persistLocked(ctx context.Context, q identity.Quadruple, ks *bucketIdentityState) error {
	br := bucketRecord{
		ByModel:   ks.buckets,
		UpdatedAt: ks.updateAt,
		Schema:    bucketRecordSchema,
	}
	buf, err := json.Marshal(br)
	if err != nil {
		return fmt.Errorf("marshal bucket record: %w", err)
	}
	rec := state.StateRecord{
		ID:        state.NewEventID(),
		Identity:  q,
		Kind:      kindGovernanceBucket,
		Version:   0,
		Bytes:     buf,
		UpdatedAt: br.UpdatedAt,
	}
	return r.state.Save(ctx, rec)
}

func (r *RateLimiter) emitRateLimited(ctx context.Context, q identity.Quadruple, tier, model string, requested, available int, cfg RateLimitConfig, now time.Time) {
	_ = r.bus.Publish(ctx, events.Event{
		Type:       EventTypeRateLimited,
		Identity:   q,
		OccurredAt: now,
		Payload: RateLimitedPayload{
			Identity:     q,
			Tier:         tier,
			Model:        model,
			Requested:    requested,
			Available:    available,
			Capacity:     cfg.Capacity,
			RefillTokens: cfg.RefillTokens,
			RefillEvery:  cfg.RefillInterval,
			OccurredAt:   now,
		},
	})
}

// requestedTokens classifies the per-call drain amount. `req.MaxTokens`
// if set; else 1 (the minimum). Phase 36b deliberately does NOT use the
// caller's `Usage.TotalTokens` from a previous call — the bucket models
// reservation, not actual consumption.
func requestedTokens(req llm.CompleteRequest) int {
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		return *req.MaxTokens
	}
	return 1
}

var _ Subsystem = (*RateLimiter)(nil)
