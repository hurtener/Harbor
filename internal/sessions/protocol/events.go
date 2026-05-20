package protocol

import (
	"context"
	"log/slog"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// SessionsAdminQueryPayload is the typed SafePayload published on the
// canonical `audit.admin_scope_used` event when an operator runs a
// cross-tenant `sessions.list` / `sessions.inspect` query under the
// verified `auth.ScopeAdmin` claim. Phase 73c / D-122.
//
// SafePayload by construction: every field is a bounded identity
// component or a Protocol method name — no caller-supplied bytes reach
// the bus. The Sessions wire surface rejects malformed requests at the
// Protocol edge before the emit.
//
// The payload is distinct from `auth.AdminScopeUsedPayload` (Phase 72b
// impersonation), `events.AdminScopeUsedPayload` (Phase 05 Subscribe),
// and `toolsprotocol.ToolsAdminActionPayload` (Phase 73f) — all ride
// the same canonical `audit.admin_scope_used` event type, but each emit
// source declares its own typed payload. A subscriber type-switches.
type SessionsAdminQueryPayload struct {
	events.SafeSealed
	// Actor is the verified admin identity at the Protocol edge — the
	// (tenant, user, session) triple the JWT carried.
	Actor identity.Identity
	// Method is the Protocol method that carried the cross-tenant query
	// (`sessions.list` or `sessions.inspect`).
	Method string
}

// emitAdminAudit publishes an `audit.admin_scope_used` event recording
// a successful cross-tenant Sessions-page query. The bus + redactor are
// optional (WithBus / WithRedactor); when the bus is unsupplied the
// query is logged at Info instead of published — the admin action is
// NEVER fully silent (CLAUDE.md §13 "silent degradation" rule).
func (s *Service) emitAdminAudit(ctx context.Context, actor identity.Identity, method string) {
	logAttrs := []any{
		slog.String("method", method),
		slog.String("tenant_id", actor.TenantID),
		slog.String("user_id", actor.UserID),
		slog.String("session_id", actor.SessionID),
	}
	if s.bus == nil {
		s.logger.InfoContext(ctx, "sessions/protocol: admin-scope query (bus not wired — audit logged only)", logAttrs...)
		return
	}
	payload := SessionsAdminQueryPayload{Actor: actor, Method: method}
	// Defence-in-depth: run the SafePayload through the redactor when
	// one is wired. A redactor error means "do not emit" — log loudly
	// and fall back, never publish unredacted (CLAUDE.md §7 rule 6).
	if s.redactor != nil {
		if _, err := s.redactor.Redact(ctx, payload); err != nil {
			s.logger.ErrorContext(ctx, "sessions/protocol: admin audit redaction failed — event NOT published",
				append(logAttrs, slog.String("error", err.Error()))...)
			return
		}
	}
	ev := events.Event{
		Type:       events.EventTypeAdminScopeUsed,
		Identity:   identity.Quadruple{Identity: actor},
		OccurredAt: time.Now().UTC(),
		Payload:    payload,
	}
	if err := s.bus.Publish(ctx, ev); err != nil {
		s.logger.WarnContext(ctx, "sessions/protocol: admin_scope_used emit failed",
			append(logAttrs, slog.String("error", err.Error()))...)
	}
}
