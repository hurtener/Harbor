package protocol

import (
	"context"
	"log/slog"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// TasksAdminActionPayload is the typed SafePayload published on the
// canonical `audit.admin_scope_used` event when an operator issues a
// cross-tenant `tasks.list` fan-in (a query whose Filter.Identities
// names more than one distinct tenant). Phase 73d / D-123.
//
// SafePayload by construction: every field is a bounded identity
// component or a Protocol method name — no caller-supplied bytes reach
// the bus. The Tasks wire surface rejects malformed requests at the
// Protocol edge before the emit.
//
// The payload is distinct from `auth.AdminScopeUsedPayload` (the Phase
// 72b impersonation shape), `events.AdminScopeUsedPayload` (the Phase
// 05 Subscribe shape), and `ToolsAdminActionPayload` (Phase 73f): all
// ride the same canonical `audit.admin_scope_used` event type, but each
// emit source declares its own typed payload (events.go §"Other emit
// sites ... MAY add new payload types"). A subscriber type-switches on
// the payload.
type TasksAdminActionPayload struct {
	events.SafeSealed
	// Actor is the verified admin identity at the Protocol edge — the
	// (tenant, user, session) triple the JWT carried.
	Actor identity.Identity
	// Method is the Protocol method that carried the cross-tenant
	// fan-in (`tasks.list`).
	Method string
	// TenantCount is the number of distinct tenants the cross-tenant
	// `tasks.list` query named.
	TenantCount int
}

// emitAdminAudit publishes an `audit.admin_scope_used` event recording
// a successful cross-tenant `tasks.list` fan-in. The bus + redactor are
// optional (WithBus / WithRedactor); when either is unsupplied the
// fan-in is logged at Info instead of published — the admin action is
// NEVER fully silent (CLAUDE.md §13 "silent degradation" rule).
func (s *Service) emitAdminAudit(ctx context.Context, actor identity.Identity, method string, tenantCount int) {
	logAttrs := []any{
		slog.String("method", method),
		slog.Int("tenant_count", tenantCount),
		slog.String("tenant_id", actor.TenantID),
		slog.String("user_id", actor.UserID),
		slog.String("session_id", actor.SessionID),
	}

	if s.bus == nil {
		s.logger.InfoContext(ctx, "tasks/protocol: cross-tenant fan-in (bus not wired — audit logged only)", logAttrs...)
		return
	}

	payload := TasksAdminActionPayload{
		Actor:       actor,
		Method:      method,
		TenantCount: tenantCount,
	}
	// Defence-in-depth: run the SafePayload through the redactor when
	// one is wired. A redactor error means "do not emit" — log loudly
	// and fall back, never publish unredacted.
	if s.redactor != nil {
		if _, err := s.redactor.Redact(ctx, payload); err != nil {
			s.logger.ErrorContext(ctx, "tasks/protocol: admin audit redaction failed — event NOT published",
				append(logAttrs, slog.String("error", err.Error()))...)
			return
		}
	}

	ev := events.Event{
		Type: events.EventTypeAdminScopeUsed,
		Identity: identity.Quadruple{
			Identity: actor,
		},
		OccurredAt: time.Now().UTC(),
		Payload:    payload,
	}
	if err := s.bus.Publish(ctx, ev); err != nil {
		s.logger.WarnContext(ctx, "tasks/protocol: admin audit event publish failed",
			append(logAttrs, slog.String("error", err.Error()))...)
		return
	}
	s.logger.InfoContext(ctx, "tasks/protocol: cross-tenant fan-in audited", logAttrs...)
}
