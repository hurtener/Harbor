package bodyscope

import (
	"context"
	"log/slog"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// Elevation is the record of one permitted identity crossing: which
// surface granted it, which verified actor asked for it, which identity
// the request now runs under, and why.
type Elevation struct {
	// Surface is the registry key of the surface that granted the
	// crossing.
	Surface Surface
	// Actor is the ctx-verified identity — the triple the transport
	// established for the request. It is the audit anchor: the answer to
	// "who did this".
	Actor identity.Identity
	// Target is the identity the reconciled request runs under.
	Target identity.Identity
	// Reason names the surface and the entitlement that permitted the
	// crossing.
	Reason string
}

// Auditor records a permitted identity crossing. Reconcile calls it
// BEFORE granting the crossing, so a record exists for every request that
// was allowed to reach outside the verified identity.
//
// A surface whose registered policy permits a crossing MUST be given a
// non-nil Auditor; Reconcile refuses such a surface with a nil sink
// rather than granting an unrecorded crossing.
type Auditor interface {
	// AdminScopeUsed records one permitted crossing. Implementations
	// never fail the request: the crossing has already passed its scope
	// check, and an emit failure is logged loudly rather than converted
	// into a client error.
	AdminScopeUsed(ctx context.Context, e Elevation)
}

// AdminScopeUsedPayload is the typed SafePayload published on the
// canonical `audit.admin_scope_used` event when the body-scope gate
// grants an identity crossing.
//
// SafePayload by construction: every field is a bounded identity
// component, a registry key, or a surface-authored reason string. No
// caller-supplied bytes reach the bus — the reason is built from the
// policy's own Wire name, never from the request.
//
// The payload is distinct from the per-subsystem admin payloads (the
// Tools, Tasks, Agents and impersonation shapes). All ride the same
// canonical event type; a subscriber type-switches on the payload.
type AdminScopeUsedPayload struct {
	events.SafeSealed
	// Actor is the verified identity at the Protocol edge.
	Actor identity.Identity
	// Surface is the Protocol surface that granted the crossing.
	Surface string
	// TargetTenant / TargetUser are the identity components the granted
	// request reaches. TargetSession is omitted: a crossing is a tenant
	// or user reach, and the session is the caller's own.
	TargetTenant string
	TargetUser   string
	// Reason names the surface and the entitlement.
	Reason string
}

// BusAuditor publishes each granted crossing as an
// `audit.admin_scope_used` event on the wired event bus. It is a
// compiled artifact: every field is set at construction and never
// mutated, so one instance is safe to share across concurrent requests.
type BusAuditor struct {
	bus      events.EventBus
	redactor audit.Redactor
	logger   *slog.Logger
}

// NewBusAuditor builds an Auditor over the supplied bus.
//
// bus may be nil (an embedder that wired no bus): the crossing is then
// logged at Info instead of published. It is never fully silent — a
// granted crossing that left no trace anywhere is the failure mode this
// whole gate exists to close.
//
// redactor may be nil; when supplied, the payload is run through it
// before publishing and a redaction failure suppresses the publish in
// favour of a loud log, never an unredacted event.
//
// logger may be nil; slog.Default() is used.
func NewBusAuditor(bus events.EventBus, redactor audit.Redactor, logger *slog.Logger) *BusAuditor {
	if logger == nil {
		logger = slog.Default()
	}
	return &BusAuditor{bus: bus, redactor: redactor, logger: logger}
}

// AdminScopeUsed implements Auditor.
func (a *BusAuditor) AdminScopeUsed(ctx context.Context, e Elevation) {
	logAttrs := []any{
		slog.String("surface", string(e.Surface)),
		slog.String("tenant_id", e.Actor.TenantID),
		slog.String("user_id", e.Actor.UserID),
		slog.String("session_id", e.Actor.SessionID),
		slog.String("target_tenant", e.Target.TenantID),
		slog.String("target_user", e.Target.UserID),
		slog.String("reason", e.Reason),
	}

	payload := AdminScopeUsedPayload{
		Actor:        e.Actor,
		Surface:      string(e.Surface),
		TargetTenant: e.Target.TenantID,
		TargetUser:   e.Target.UserID,
		Reason:       e.Reason,
	}

	if a.bus == nil {
		a.logger.InfoContext(ctx, "protocol: body-scope granted an identity crossing (bus not wired — audit logged only)", logAttrs...)
		return
	}
	if a.redactor != nil {
		if _, err := a.redactor.Redact(ctx, payload); err != nil {
			a.logger.ErrorContext(ctx, "protocol: body-scope crossing audit redaction failed — event NOT published",
				append(logAttrs, slog.String("error", err.Error()))...)
			return
		}
	}
	ev := events.Event{
		Type:       events.EventTypeAdminScopeUsed,
		Identity:   identity.Quadruple{Identity: e.Actor},
		OccurredAt: time.Now().UTC(),
		Payload:    payload,
	}
	if err := a.bus.Publish(ctx, ev); err != nil {
		a.logger.WarnContext(ctx, "protocol: body-scope crossing audit publish failed",
			append(logAttrs, slog.String("error", err.Error()))...)
		return
	}
	a.logger.InfoContext(ctx, "protocol: body-scope granted an identity crossing", logAttrs...)
}
