package memory

import (
	"context"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// EmitHealthChanged publishes the `memory.health_changed` event on
// `bus`. The transition is validated against the documented
// `Health` FSM (see `ValidateHealthTransition`); an invalid
// transition returns wrapped `ErrInvalidHealthTransition` and does
// NOT publish — fail loudly, never silently coerce an illegal
// transition into a silent no-op (AGENTS.md §13).
//
// The `reason` string is a short static label
// ("summarizer_failed", "retries_exhausted",
// "recovery_loop_drained", etc.) that subscribers / SREs can
// filter on. It MUST NOT carry caller-controlled bytes.
//
// Identity is required (D-001) — the event's `Identity` field
// carries the executor's per-key triple so subscribers can scope
// the transition to a session.
//
// Self-loops (prior == next) ARE valid transitions per
// `ValidateHealthTransition`; this helper still emits the event so
// downstream observability can count "stayed healthy across
// summarisation success" — but it is the caller's choice to skip
// the self-loop emit if the cost is undesirable.
func EmitHealthChanged(ctx context.Context, bus events.EventBus, id identity.Quadruple, prior, next Health, reason string) error {
	if err := ValidateIdentity(id); err != nil {
		return err
	}
	if err := ValidateHealthTransition(prior, next); err != nil {
		return err
	}
	payload := HealthChangedPayload{
		PriorHealth: prior,
		NewHealth:   next,
		Reason:      reason,
	}
	ev := events.Event{
		Type:       EventTypeMemoryHealthChanged,
		Identity:   id,
		OccurredAt: time.Now(),
		Payload:    payload,
	}
	if err := bus.Publish(ctx, ev); err != nil {
		return fmt.Errorf("memory: publish health_changed: %w", err)
	}
	return nil
}

// EmitRecoveryDropped publishes the `memory.recovery_dropped` event
// on `bus`. Used by the `rolling_summary` recovery loop when the
// backlog overflows `RecoveryBacklogMax` and the oldest queued
// batch is dropped to make room (D-035 bounded recovery loop).
//
// Identity is required (D-001).
func EmitRecoveryDropped(ctx context.Context, bus events.EventBus, id identity.Quadruple, reason string) error {
	if err := ValidateIdentity(id); err != nil {
		return err
	}
	payload := RecoveryDroppedPayload{Reason: reason}
	ev := events.Event{
		Type:       EventTypeMemoryRecoveryDropped,
		Identity:   id,
		OccurredAt: time.Now(),
		Payload:    payload,
	}
	if err := bus.Publish(ctx, ev); err != nil {
		return fmt.Errorf("memory: publish recovery_dropped: %w", err)
	}
	return nil
}
