package protocol

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// ErrKeyRotateMisconfigured — NewKeyRotateService was called with a nil
// rotator.
var ErrKeyRotateMisconfigured = errors.New("governance/protocol: NewKeyRotateService missing a mandatory dependency")

// KeyRotator is the narrow seam the KeyRotateService drives — the
// `*llm.ProviderKeyRotator` concrete satisfies it. The implementation
// validates the provider + key and swaps the atomic key holder; it returns
// the resolved provider + a NON-REVERSIBLE fingerprint of the new key,
// never the key itself.
type KeyRotator interface {
	RotateKey(provider, newKey string) (resolvedProvider, fingerprint string, err error)
}

// KeyRotateService implements the admin-scoped `governance.rotate_key`
// method. It validates identity, drives the rotator, and emits the
// secret-free `governance.key_rotated` audit event. The KEY VALUE is
// handled only on the request leg + inside the rotator's holder — it never
// touches a log, the audit payload, or the response (CLAUDE.md §7).
//
// Concurrent reuse: immutable after construction; per-call state lives in
// arguments + locals. The rotator's holder is internally synchronised.
type KeyRotateService struct {
	rotator  KeyRotator
	bus      events.EventBus // optional — nil ⇒ the audit event is logged only
	redactor audit.Redactor  // optional — defence-in-depth before the emit
	logger   *slog.Logger
	now      Clock
}

// KeyRotateOption configures NewKeyRotateService.
type KeyRotateOption func(*KeyRotateService)

// WithKeyRotateBus wires the events.EventBus the `governance.key_rotated`
// audit event publishes onto. A nil bus logs the rotation at Info instead
// (never fully silent — CLAUDE.md §13).
func WithKeyRotateBus(b events.EventBus) KeyRotateOption {
	return func(s *KeyRotateService) {
		if b != nil {
			s.bus = b
		}
	}
}

// WithKeyRotateRedactor wires the audit.Redactor the secret-free payload
// runs through before the emit (defence-in-depth — the payload carries no
// secret by construction, but the redactor pass is mandatory per §7 rule 6).
func WithKeyRotateRedactor(r audit.Redactor) KeyRotateOption {
	return func(s *KeyRotateService) {
		if r != nil {
			s.redactor = r
		}
	}
}

// WithKeyRotateLogger sets the slog.Logger. A nil logger routes to
// slog.Default().
func WithKeyRotateLogger(l *slog.Logger) KeyRotateOption {
	return func(s *KeyRotateService) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithKeyRotateClock injects the time source. Defaults to time.Now.
func WithKeyRotateClock(c Clock) KeyRotateOption {
	return func(s *KeyRotateService) {
		if c != nil {
			s.now = c
		}
	}
}

// NewKeyRotateService builds the rotate-key service over a KeyRotator.
// rotator is mandatory — a nil fails loud with ErrKeyRotateMisconfigured.
//
// The returned *KeyRotateService is immutable after construction and safe
// for concurrent use by N goroutines.
func NewKeyRotateService(rotator KeyRotator, opts ...KeyRotateOption) (*KeyRotateService, error) {
	if rotator == nil {
		return nil, fmt.Errorf("%w: KeyRotator is nil", ErrKeyRotateMisconfigured)
	}
	s := &KeyRotateService{rotator: rotator, logger: slog.Default(), now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// RotateKey validates identity, rotates the provider key via the rotator,
// emits the secret-free audit event, and returns the provider +
// fingerprint + rotation timestamp. The new key never appears in the
// response, the audit event, or any log line.
func (s *KeyRotateService) RotateKey(ctx context.Context, req prototypes.GovernanceRotateKeyRequest) (prototypes.GovernanceRotateKeyResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.GovernanceRotateKeyResponse{}, err
	}
	id, err := identityFromScope(req.Identity)
	if err != nil {
		return prototypes.GovernanceRotateKeyResponse{}, err
	}
	provider, fingerprint, err := s.rotator.RotateKey(req.Provider, req.Key)
	if err != nil {
		return prototypes.GovernanceRotateKeyResponse{}, err
	}
	at := s.now().UTC()
	actor := identity.Quadruple{Identity: id}
	s.emitRotated(ctx, actor, provider, fingerprint, at)
	return prototypes.GovernanceRotateKeyResponse{
		Provider:        provider,
		Fingerprint:     fingerprint,
		RotatedAt:       at,
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// emitRotated publishes the secret-free `governance.key_rotated` event.
// The payload carries the provider + fingerprint + actor — never the key.
// A privileged mutation is never silent (CLAUDE.md §13): with no bus wired
// the rotation is logged at Info; a publish failure is logged at Warn (the
// swap already happened — the audit-trail drift must be visible).
func (s *KeyRotateService) emitRotated(ctx context.Context, actor identity.Quadruple, provider, fingerprint string, at time.Time) {
	logAttrs := []any{
		slog.String("tenant_id", actor.TenantID),
		slog.String("actor_user", actor.UserID),
		slog.String("provider", provider),
		slog.String("fingerprint", fingerprint),
	}
	if s.bus == nil {
		s.logger.InfoContext(ctx, "governance/protocol: key rotated (bus not wired — audit logged only)", logAttrs...)
		return
	}
	// Defence-in-depth redactor pass over the audit-visible fields before
	// the emit. The payload carries no secret by construction (fingerprint,
	// not key), but the redactor pass is mandatory per §7 rule 6.
	if s.redactor != nil {
		if _, err := s.redactor.Redact(ctx, map[string]any{
			"tenant_id": actor.TenantID, "actor_user": actor.UserID,
			"provider": provider, "fingerprint": fingerprint,
		}); err != nil {
			s.logger.WarnContext(ctx, "governance/protocol: redactor refused key_rotated payload; skipping emit",
				append(logAttrs, slog.String("error", err.Error()))...)
			return
		}
	}
	ev := events.Event{
		Type:       governance.EventTypeKeyRotated,
		Identity:   actor,
		OccurredAt: at,
		Payload: governance.KeyRotatedPayload{
			Actor:       actor,
			Provider:    provider,
			Fingerprint: fingerprint,
			OccurredAt:  at,
		},
	}
	if err := s.bus.Publish(ctx, ev); err != nil {
		s.logger.WarnContext(ctx, "governance/protocol: failed to publish key_rotated audit event",
			append(logAttrs, slog.String("error", err.Error()))...)
	}
}
