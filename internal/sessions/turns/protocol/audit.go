package protocol

import (
	"context"
	"log/slog"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// TurnsAdminQueryPayload is the typed SafePayload published on the
// canonical `audit.admin_scope_used` event when a widened
// `sessions.turns.get` operations-projection read leaves the caller's
// own session under a verified `admin` or `console:fleet` claim.
//
// SafePayload by construction: every field is a bounded identity
// component or a Protocol method name — no caller-supplied bytes reach
// the bus, and the read target carries no transcript content (the
// operations lane returns the structurally distinct OpsTurnRow only).
//
// The payload is distinct from `SessionsAdminQueryPayload` (
// sessions lifecycle), `auth.AdminScopeUsedPayload` (impersonation),
// `events.AdminScopeUsedPayload` (Subscribe), and
// `toolsprotocol.ToolsAdminActionPayload` — all ride the same
// canonical `audit.admin_scope_used` event type, but each emit source
// declares its own typed payload. A subscriber type-switches.
type TurnsAdminQueryPayload struct {
	events.SafeSealed
	// Actor is the verified admin/fleet identity at the request edge —
	// the (tenant, user, session) triple the JWT carried.
	Actor identity.Identity
	// Target is the read triple the widened operations read was
	// scoped to — the session whose operations row was observed.
	Target identity.Identity
	// Method is the Protocol method that carried the widened read
	// (`sessions.turns.get` with the operations projection — the only
	// operations read surface).
	Method string
}

// widenedOperationsMethod is the one Protocol method the operations
// read lane serves: `sessions.turns.get` is get-only (the projection
// core ships OpsTurn, not an ops list).
const widenedOperationsMethod = "sessions.turns.get"

// emitAdminAudit publishes an `audit.admin_scope_used` event recording
// a widened operations read. The bus + redactor are optional
// (WithBus / WithRedactor); when the bus is unsupplied the widened
// read is logged at Info instead of published — the admin action is
// NEVER fully silent (CLAUDE.md §13 "silent degradation" rule).
func (s *Service) emitAdminAudit(ctx context.Context, actor, target identity.Identity) {
	logAttrs := []any{
		slog.String("method", widenedOperationsMethod),
		slog.String("actor_tenant_id", actor.TenantID),
		slog.String("actor_user_id", actor.UserID),
		slog.String("actor_session_id", actor.SessionID),
		slog.String("target_session_id", target.SessionID),
	}
	if s.bus == nil {
		s.logger.InfoContext(ctx, "sessions/turns/protocol: widened operations read (bus not wired — audit logged only)", logAttrs...)
		return
	}
	payload := TurnsAdminQueryPayload{Actor: actor, Target: target, Method: widenedOperationsMethod}
	// Defence-in-depth: run the SafePayload through the redactor when
	// one is wired. A redactor error means "do not emit" — log loudly
	// and fall back, never publish unredacted (CLAUDE.md §7 rule 6).
	if s.redactor != nil {
		if _, err := s.redactor.Redact(ctx, payload); err != nil {
			s.logger.ErrorContext(ctx, "sessions/turns/protocol: admin audit redaction failed — event NOT published",
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
		s.logger.WarnContext(ctx, "sessions/turns/protocol: admin_scope_used emit failed",
			append(logAttrs, slog.String("error", err.Error()))...)
	}
}
