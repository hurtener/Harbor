package auth

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/tools"
)

// BindingState is the read-time OAuth binding posture of a tool source
// under a given identity — the catalog-lens projection the Tools page's
// OAuth-status facet rides. It is a NEUTRAL runtime enum (no Protocol
// wire coupling): the catalog annotator maps it onto the wire
// ToolOAuthStatus.
type BindingState int

const (
	// BindingNotConfigured — the source has no OAuthConfig; it requires
	// no OAuth (the wire "n/a").
	BindingNotConfigured BindingState = iota
	// BindingUnbound — the source requires OAuth but no token binding
	// exists for this identity (the wire "Required").
	BindingUnbound
	// BindingBound — a live, unexpired token binding exists (the wire
	// "Bound").
	BindingBound
	// BindingExpired — a token binding exists but has lapsed (the wire
	// "Expired").
	BindingExpired
)

// BindingStatus reports the read-time OAuth binding posture of source
// under the ctx identity WITHOUT triggering a refresh or any network
// call — the catalog-projection read path. It is the observation
// counterpart to Token (which mutates: refreshes, parks on
// ErrAuthRequired). A source this provider does not configure returns
// BindingNotConfigured (the caller should try another provider, or map
// it to "n/a"). Identity is mandatory — an incomplete ctx triple fails
// closed.
//
// BindingStatus is safe for concurrent use: it reads the immutable
// config map + the internally-synchronised TokenStore, holding no
// per-call state on the provider.
func (p *Provider) BindingStatus(ctx context.Context, source tools.ToolSourceID) (BindingState, error) {
	if p.closed.Load() {
		return BindingNotConfigured, ErrProviderClosed
	}
	if err := ctx.Err(); err != nil {
		return BindingNotConfigured, fmt.Errorf("auth: BindingStatus cancelled: %w", err)
	}
	id, err := identityFromCtx(ctx)
	if err != nil {
		return BindingNotConfigured, err
	}
	cfg, ok := p.configs[source]
	if !ok {
		return BindingNotConfigured, nil
	}
	subj := cfg.SubjectID(id)
	if subj == "" {
		// The source requires OAuth but the caller's identity carries no
		// subject for the configured binding scope (e.g. an agent-bound
		// source read under a user-only ctx). There is no binding to
		// observe — honestly Unbound, never a fabricated Bound.
		return BindingUnbound, nil
	}
	tok, found, err := p.store.Get(ctx, cfg.BindingScope, subj, source)
	if err != nil {
		return BindingNotConfigured, fmt.Errorf("auth: BindingStatus load: %w", err)
	}
	if !found {
		return BindingUnbound, nil
	}
	if p.isExpired(tok) {
		return BindingExpired, nil
	}
	return BindingBound, nil
}
