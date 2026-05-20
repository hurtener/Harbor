package notifications

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/hurtener/Harbor/internal/events"
)

// Subscriber wires the rules-engine-lite mapper onto a long-lived bus
// subscription. NewSubscriber + Run together implement the §13
// primitive-with-consumer rule for Phase 72d: the Subscriber consumes
// the trigger events Phase 20 / 30 / 31 / 36a / 50 already emit and
// republishes a `notification.*` event for each match through the
// same bus.
//
// Concurrent-reuse contract (D-025): the Subscriber is a long-lived
// component constructed once and shared across the bus's lifetime. It
// owns no per-run state — every call into the mapper is pure. Multiple
// Subscribers may be wired onto the same bus (rare in production —
// the default boot wires one — but the type does not block it).
//
// Goroutine-leak contract (CLAUDE.md §11): Run blocks until ctx is
// cancelled OR the bus closes its delivery channel. On return, the
// subscription's Cancel() has been called and the bus's reaper has
// joined the underlying goroutine. The mandatory leak test
// (`subscriber_test.go::TestSubscriber_Run_GoroutineLeak`) asserts
// runtime.NumGoroutine() returns to baseline after Run returns.
type Subscriber struct {
	bus events.EventBus
	log *slog.Logger
}

// NewSubscriber constructs a Subscriber. The bus is mandatory (nil
// panics — the constructor fails loudly per CLAUDE.md §13 because a
// nil bus would silently degrade the entire subscriber to a no-op).
// The logger is mandatory; pass slog.Default() if no contextual
// logger is available.
func NewSubscriber(bus events.EventBus, log *slog.Logger) *Subscriber {
	if bus == nil {
		panic("notifications.NewSubscriber: bus is required (got nil)")
	}
	if log == nil {
		panic("notifications.NewSubscriber: log is required (got nil)")
	}
	return &Subscriber{bus: bus, log: log}
}

// Run opens an Admin-scope subscription on the V1 trigger event
// types and republishes each synthesised notification.* event onto
// the same bus. Blocks until ctx is cancelled OR the bus closes the
// subscription's delivery channel.
//
// The subscription is Admin-scope (Filter.Admin=true) because the
// Subscriber is a runtime-internal infrastructure consumer that
// must fan in across the full identity space — every tenant, user,
// session generates trigger events the notification topic should
// cover. The Admin scope use is audit-emitted by the bus on
// subscription open (events.EventTypeAdminScopeUsed); the Subscriber
// is therefore observable as a privileged consumer the same way every
// other Admin-scope subscriber is.
//
// Identity-rejection fail-loudly path: if a delivered trigger event
// arrives with the D-033 `<missing>` identity sentinel in any
// component, Run emits a `notification.identity_rejected` event
// (SafePayload — no caller bytes) AND logs at Error, then continues.
// The malformed trigger does NOT silently produce a malformed
// notification.
//
// Mapper-error fail-loudly path: if Map returns a non-nil error
// (always wrapped ErrUnmappable), Run logs at Error and emits a
// `runtime.error` event via the bus; no notification.* event is
// emitted for that trigger.
//
// Publish errors are logged at Error and counted as observability
// failures; Run continues so a transient bus issue does not collapse
// the subscriber.
func (s *Subscriber) Run(ctx context.Context) error {
	filter := events.Filter{
		Admin: true,
		Types: V1TriggerEventTypes(),
	}
	sub, err := s.bus.Subscribe(ctx, filter)
	if err != nil {
		return fmt.Errorf("notifications: subscribe: %w", err)
	}
	defer sub.Cancel()

	s.log.Info("notifications subscriber running",
		slog.Int("trigger_types", len(V1TriggerEventTypes())))

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-sub.Events():
			if !ok {
				// Bus closed the channel — return cleanly so the
				// caller's join completes.
				return nil
			}
			s.handle(ctx, ev)
		}
	}
}

// handle processes one delivered trigger event. Errors are logged
// + audit-emitted; the loop continues.
func (s *Subscriber) handle(ctx context.Context, trigger events.Event) {
	// Bus-internal events (BusDropped, IdleClosed, AdminScopeUsed,
	// AuditRedactionFailed) may be delivered to an Admin-scope filter
	// without an explicit Types narrowing. Our filter narrows to
	// V1TriggerEventTypes so this branch should not fire in practice,
	// but the defensive skip keeps the subscriber correct if a future
	// bus broadens the filter semantics.
	if !isV1Trigger(trigger.Type) {
		return
	}

	// Identity-rejection fail-loudly. The bus's ValidateEvent already
	// requires a non-empty triple on Publish, so a trigger reaching
	// us has at least the triple components populated. The D-033
	// `<missing>` sentinel substitutes the missing component on
	// upstream identity-rejection events (memory / skill / agent-
	// registry) — if such a substituted event ever flows into the
	// trigger set we reject loud rather than synthesising a
	// notification with a `<missing>` identity.
	if hasMissingIdentitySentinel(trigger) {
		s.emitIdentityRejected(ctx, trigger)
		return
	}

	synthesised, err := Map(ctx, trigger)
	if err != nil {
		s.log.Error("notifications mapper failed",
			slog.String("trigger_type", string(trigger.Type)),
			slog.Uint64("trigger_sequence", trigger.Sequence),
			slog.Any("error", err))
		s.emitRuntimeError(ctx, trigger, err)
		return
	}
	for i := range synthesised {
		if pubErr := s.bus.Publish(ctx, synthesised[i]); pubErr != nil {
			// Don't escalate a Publish failure into a panic; log + count
			// as observability failure and continue. The bus itself
			// emits audit.redaction_failed when the redactor refuses
			// the payload, so the failure is already on the bus surface
			// for an admin subscriber to observe.
			s.log.Error("notifications publish failed",
				slog.String("notification_type", string(synthesised[i].Type)),
				slog.String("trigger_type", string(trigger.Type)),
				slog.Uint64("trigger_sequence", trigger.Sequence),
				slog.Any("error", pubErr))
		}
	}
}

// emitIdentityRejected publishes a notification.identity_rejected
// event for a trigger that carried a `<missing>` sentinel. The event
// payload is SafePayload — no caller-controlled bytes.
//
// The Identity on the rejection event echoes whatever the trigger
// carried, including any `<missing>` substitutions, so the
// bus's ValidateEvent identity-triple check passes (the bus accepts
// `<missing>`-substituted identities by construction — D-033).
func (s *Subscriber) emitIdentityRejected(ctx context.Context, trigger events.Event) {
	reason := identityRejectionReason(trigger)
	s.log.Error("notifications identity rejection",
		slog.String("trigger_type", string(trigger.Type)),
		slog.Uint64("trigger_sequence", trigger.Sequence),
		slog.String("reason", reason))
	ev := events.Event{
		Type:     EventTypeNotificationIdentityRejected,
		Identity: trigger.Identity,
		Payload: IdentityRejectedPayload{
			Operation:       "Subscriber.Run",
			Reason:          reason,
			OriginEventType: trigger.Type,
		},
	}
	if err := s.bus.Publish(ctx, ev); err != nil {
		// Defensive: log but don't escalate. The bus emits
		// audit.redaction_failed on its own for any redactor refusal.
		s.log.Error("notifications identity_rejected publish failed",
			slog.String("trigger_type", string(trigger.Type)),
			slog.Any("error", err))
	}
}

// emitRuntimeError publishes a runtime.error event reporting a
// mapper failure. The payload is RuntimeErrorPayload (NOT SafePayload
// per the events package design — see internal/events/payloads.go).
// The audit redactor walks it; we keep the Fields map small and
// stable so the redactor walk is cheap.
func (s *Subscriber) emitRuntimeError(ctx context.Context, trigger events.Event, mapErr error) {
	ev := events.Event{
		Type:     events.EventTypeRuntimeError,
		Identity: trigger.Identity,
		Payload: events.RuntimeErrorPayload{
			Message: "notifications.Subscriber: mapper failed",
			Fields: map[string]any{
				"trigger_type":     string(trigger.Type),
				"trigger_sequence": trigger.Sequence,
				"error":            mapErr.Error(),
				"unmappable":       errors.Is(mapErr, ErrUnmappable),
			},
		},
	}
	if err := s.bus.Publish(ctx, ev); err != nil {
		s.log.Error("notifications runtime.error publish failed",
			slog.String("trigger_type", string(trigger.Type)),
			slog.Any("error", err))
	}
}

// missingIdentitySentinel is the D-033 substitute string upstream
// identity-rejection emitters use to make their rejection event
// itself bus-publishable. Mirrors memory.missingIdentitySentinel and
// skills.missingIdentitySentinel — same constant value, same role.
const missingIdentitySentinel = "<missing>"

func hasMissingIdentitySentinel(ev events.Event) bool {
	return ev.Identity.TenantID == missingIdentitySentinel ||
		ev.Identity.UserID == missingIdentitySentinel ||
		ev.Identity.SessionID == missingIdentitySentinel
}

// identityRejectionReason names the missing components on ev's
// identity for the rejection event's Reason field. Deterministic
// ordering so tests can pin the string.
func identityRejectionReason(ev events.Event) string {
	missing := make([]string, 0, 3)
	if ev.Identity.TenantID == missingIdentitySentinel {
		missing = append(missing, "tenant_id")
	}
	if ev.Identity.UserID == missingIdentitySentinel {
		missing = append(missing, "user_id")
	}
	if ev.Identity.SessionID == missingIdentitySentinel {
		missing = append(missing, "session_id")
	}
	switch len(missing) {
	case 0:
		return "identity components missing (none detected)"
	case 1:
		return missing[0] + " <missing>"
	case 2:
		return missing[0] + " and " + missing[1] + " <missing>"
	default:
		return missing[0] + ", " + missing[1] + " and " + missing[2] + " <missing>"
	}
}

// isV1Trigger reports whether t is in V1TriggerEventTypes. Helper for
// the defensive filter narrowing — the bus filter already narrows,
// but the Subscriber is correct even if a future bus broadens the
// semantics.
func isV1Trigger(t events.EventType) bool {
	for _, want := range V1TriggerEventTypes() {
		if t == want {
			return true
		}
	}
	return false
}
