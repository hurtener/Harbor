package protocol

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// setposture.go — the admin-scoped `governance.set_posture` write service.
// It is the write sibling of the read-only `governance.posture` posture
// method: an admin replaces the WHOLE identity-tier policy table (per tier:
// budget-ceiling USD / max-tokens cap / rate-limit capacity, plus the
// default-tier assignment) live, with no redeploy. The write is a FULL
// REPLACE (never a partial merge) and is validated fail-closed by the
// policy before any StateStore mutation.
//
// # The seam (CLAUDE.md §4.4)
//
// The Service depends on a narrow `PostureWriteStore` interface — the
// `*governance.SetPosturePolicy` concrete satisfies it, and tests inject a
// fake. The Service owns wire↔spec mapping; the policy owns validation +
// persistence + the best-effort audit event.
//
// # Authority is server-side
//
// The request wire type carries NO identity / scope field — authority is
// derived server-side from the verified session, never the request body.
// The wire handler resolves the verified identity and passes it as the
// `actor` (the audit anchor). The admin gate (`auth.ScopeAdmin` ONLY —
// explicitly NOT the read's `admin` OR `console:fleet` set) is enforced at
// the wire handler before dispatch.

// ErrPostureWriteMisconfigured — NewPostureWriteService was called with a
// nil store.
var ErrPostureWriteMisconfigured = errors.New("governance/protocol: NewPostureWriteService missing a mandatory dependency")

// PostureWriteStore is the narrow seam the PostureWriteService drives. The
// `*governance.SetPosturePolicy` concrete satisfies it.
type PostureWriteStore interface {
	// Set validates the submitted table as a full replace, rejects a
	// widening / zeroing / empty write fail-closed, persists it, and emits
	// the audit event. actor is the admin caller (the audit anchor).
	// Returns the resolved persisted posture.
	Set(ctx context.Context, actor identity.Quadruple, spec governance.SetPostureSpec) (governance.Snapshot, error)
	// EnforcementActive reports whether a governance enforcement wrapper was
	// composed at boot. When false, a persisted write does NOT enforce until
	// the next restart — the response surfaces this via
	// `enforcement_pending_restart`.
	EnforcementActive() bool
}

// PostureWriteService implements the admin-scoped `governance.set_posture`
// method. It maps the wire request into the governance spec, drives the
// store, and projects the resolved snapshot back onto the wire response.
//
// Concurrent reuse: immutable after NewPostureWriteService; per-call state
// lives in arguments + locals.
type PostureWriteService struct {
	store  PostureWriteStore
	logger *slog.Logger
}

// PostureWriteOption configures NewPostureWriteService.
type PostureWriteOption func(*PostureWriteService)

// WithPostureWriteLogger sets the slog.Logger. A nil logger routes to
// slog.Default().
func WithPostureWriteLogger(l *slog.Logger) PostureWriteOption {
	return func(s *PostureWriteService) {
		if l != nil {
			s.logger = l
		}
	}
}

// NewPostureWriteService builds the set-posture service over a
// PostureWriteStore. store is mandatory — a nil fails loud with
// ErrPostureWriteMisconfigured (CLAUDE.md §5).
//
// The returned *PostureWriteService is immutable after construction and
// safe for concurrent use by N goroutines.
func NewPostureWriteService(store PostureWriteStore, opts ...PostureWriteOption) (*PostureWriteService, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: PostureWriteStore is nil", ErrPostureWriteMisconfigured)
	}
	s := &PostureWriteService{store: store, logger: slog.Default()}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// SetPosture maps the wire request into the governance spec, persists it
// through the store (which validates the full-replace fail-closed and emits
// the audit event), and echoes the persisted posture. `actor` is the
// verified admin caller, derived server-side by the wire handler — never
// the request body (which carries no identity field).
func (s *PostureWriteService) SetPosture(ctx context.Context, actor identity.Quadruple, req prototypes.GovernanceSetPostureRequest) (prototypes.GovernanceSetPostureResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.GovernanceSetPostureResponse{}, err
	}
	if err := identity.Validate(actor.Identity); err != nil {
		return prototypes.GovernanceSetPostureResponse{}, fmt.Errorf("%w: %w", ErrIdentityRequired, err)
	}
	spec := governance.SetPostureSpec{
		DefaultTier:   req.DefaultTier,
		IdentityTiers: wireTiersToConfig(req.IdentityTiers),
	}
	snap, err := s.store.Set(ctx, actor, spec)
	if err != nil {
		return prototypes.GovernanceSetPostureResponse{}, err
	}
	return prototypes.GovernanceSetPostureResponse{
		DefaultTier:   snap.DefaultTier,
		IdentityTiers: configTiersToWire(snap.IdentityTiers),
		// Honest signal: the write persisted, but on a fully-latent runtime no
		// wrapper reads the enforcement source, so the new policy enforces only
		// after the next restart.
		EnforcementPendingRestart: !s.store.EnforcementActive(),
		ProtocolVersion:           prototypes.ProtocolVersion,
	}, nil
}

// wireTiersToConfig maps the wire tier table onto the governance tier map.
// The wire `RefillIntervalMS` (int64 milliseconds) is converted back into
// the `time.Duration` the governance config carries.
func wireTiersToConfig(in map[string]prototypes.IdentityTierView) map[string]governance.TierConfig {
	out := make(map[string]governance.TierConfig, len(in))
	for name, v := range in {
		out[name] = governance.TierConfig{
			BudgetCeilingUSD: v.BudgetCeilingUSD,
			RateLimit: governance.RateLimitConfig{
				Capacity:       v.RateLimit.Capacity,
				RefillTokens:   v.RateLimit.RefillTokens,
				RefillInterval: time.Duration(v.RateLimit.RefillIntervalMS) * time.Millisecond,
			},
			MaxTokens: v.MaxTokens,
		}
	}
	return out
}

// configTiersToWire maps the governance tier map onto the wire projection.
// The `time.Duration` refill interval is projected to int64 milliseconds so
// the JSON is unambiguous across language clients. Always non-nil.
func configTiersToWire(in map[string]governance.TierConfig) map[string]prototypes.IdentityTierView {
	out := make(map[string]prototypes.IdentityTierView, len(in))
	for name, tc := range in {
		out[name] = prototypes.IdentityTierView{
			BudgetCeilingUSD: tc.BudgetCeilingUSD,
			RateLimit: prototypes.RateLimitView{
				Capacity:         tc.RateLimit.Capacity,
				RefillTokens:     tc.RateLimit.RefillTokens,
				RefillIntervalMS: tc.RateLimit.RefillInterval.Milliseconds(),
			},
			MaxTokens: tc.MaxTokens,
		}
	}
	return out
}
