package protocol

import (
	"context"
	"log/slog"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// ToolsAdminActionPayload is the typed SafePayload published on the
// canonical `audit.admin_scope_used` event when an operator invokes one
// of the two Tools-page admin methods (`tools.set_approval_policy` /
// `tools.revoke_oauth`). Phase 73f / D-116.
//
// SafePayload by construction: every field is a bounded identity
// component, a Protocol method name, or a tool ID — no caller-supplied
// bytes reach the bus. The Tools wire surface rejects malformed
// requests at the Protocol edge before the emit.
//
// The payload is distinct from `auth.AdminScopeUsedPayload` (the Phase
// 72b impersonation shape) and `events.AdminScopeUsedPayload` (the
// Phase 05 Subscribe shape): all three ride the same canonical
// `audit.admin_scope_used` event type, but each emit source declares
// its own typed payload (events.go §"Other emit sites ... MAY add new
// payload types"). A subscriber type-switches on the payload.
type ToolsAdminActionPayload struct {
	events.SafeSealed
	// Actor is the verified admin identity at the Protocol edge — the
	// (tenant, user, session) triple the JWT carried.
	Actor identity.Identity
	// Method is the Protocol method that carried the admin action
	// (`tools.set_approval_policy` or `tools.revoke_oauth`).
	Method string
	// ToolID is the catalog key of the tool the action mutated.
	ToolID string
}

// emitAdminAudit publishes an `audit.admin_scope_used` event recording
// a successful Tools-page admin action. The bus + redactor are
// optional (WithBus / WithRedactor); when either is unsupplied the
// action is logged at Info instead of published — the admin action is
// NEVER fully silent (CLAUDE.md §13 "silent degradation" rule). The
// reason argument is reserved for future per-method context (the
// applied policy on set_approval_policy); it is logged, not put on the
// wire payload.
func (s *Service) emitAdminAudit(ctx context.Context, actor identity.Identity, reason, method, toolID string) {
	logAttrs := []any{
		slog.String("method", method),
		slog.String("tool_id", toolID),
		slog.String("tenant_id", actor.TenantID),
		slog.String("user_id", actor.UserID),
		slog.String("session_id", actor.SessionID),
	}
	if reason != "" {
		logAttrs = append(logAttrs, slog.String("detail", reason))
	}

	if s.bus == nil {
		s.logger.InfoContext(ctx, "tools/protocol: admin action (bus not wired — audit logged only)", logAttrs...)
		return
	}

	payload := ToolsAdminActionPayload{
		Actor:  actor,
		Method: method,
		ToolID: toolID,
	}
	// Defence-in-depth: run the SafePayload through the redactor when
	// one is wired (parity with the Phase 72b emit site). A redactor
	// error means "do not emit" — log loudly and fall back, never
	// publish unredacted.
	if s.redactor != nil {
		if _, err := s.redactor.Redact(ctx, payload); err != nil {
			s.logger.ErrorContext(ctx, "tools/protocol: admin audit redaction failed — event NOT published",
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
		s.logger.WarnContext(ctx, "tools/protocol: admin audit event publish failed",
			append(logAttrs, slog.String("error", err.Error()))...)
		return
	}
	s.logger.InfoContext(ctx, "tools/protocol: admin action audited", logAttrs...)
}
