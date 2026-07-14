package annotate

import (
	"context"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// bindingStatuser is the read-time OAuth-status surface the annotator's
// OAuth reader needs from a provider. The tools/auth *Provider satisfies
// it; the OAuthProvider interface alone does not (its Token method
// mutates — refreshes / parks — so it is unfit for a projection read).
type bindingStatuser interface {
	BindingStatus(ctx context.Context, source tools.ToolSourceID) (auth.BindingState, error)
}

// ProviderOAuthReader is the production OAuthReader over the runtime's
// OAuth provider set (the same map[name]OAuthProvider the dispatch path
// holds). It resolves a tool's binding status by asking each provider,
// read-time, whether it configures the tool's Source and whether a live
// binding exists — never triggering a refresh or a network call.
//
// Immutable after construction; safe for concurrent reuse.
type ProviderOAuthReader struct {
	providers map[string]auth.OAuthProvider
}

// NewProviderOAuthReader builds an OAuthReader over the provider set. A
// nil / empty set is valid — every tool then reads "n/a" (no source
// requires OAuth), the honest posture for a runtime with no OAuth
// providers configured.
func NewProviderOAuthReader(providers map[string]auth.OAuthProvider) *ProviderOAuthReader {
	return &ProviderOAuthReader{providers: providers}
}

// Status implements OAuthReader.Status. It scans the provider set for the
// one that configures source and maps its read-time binding state onto
// the wire status. A source no provider configures reads "n/a".
func (r *ProviderOAuthReader) Status(ctx context.Context, id identity.Identity, source tools.ToolSourceID) prototypes.ToolOAuthStatus {
	if source == "" || len(r.providers) == 0 {
		return prototypes.ToolOAuthNotApplicable
	}
	ctx, err := identity.With(ctx, id)
	if err != nil {
		return prototypes.ToolOAuthNotApplicable
	}
	for _, p := range r.providers {
		bs, ok := p.(bindingStatuser)
		if !ok {
			continue
		}
		state, err := bs.BindingStatus(ctx, source)
		if err != nil {
			continue
		}
		switch state {
		case auth.BindingBound:
			return prototypes.ToolOAuthBound
		case auth.BindingExpired:
			return prototypes.ToolOAuthExpired
		case auth.BindingUnbound:
			return prototypes.ToolOAuthRequired
		case auth.BindingNotConfigured:
			// This provider does not configure source; try the next.
			continue
		}
	}
	return prototypes.ToolOAuthNotApplicable
}

// Revoke implements OAuthReader.Revoke. It revokes the binding on every
// provider that configures source, counting successful revocations. A
// source no provider configures revokes zero (an honest count, never a
// fabricated success). The first revoke error is returned once every
// provider has been attempted.
func (r *ProviderOAuthReader) Revoke(ctx context.Context, id identity.Identity, source tools.ToolSourceID) (int64, error) {
	if source == "" || len(r.providers) == 0 {
		return 0, nil
	}
	ctx, err := identity.With(ctx, id)
	if err != nil {
		return 0, err
	}
	var count int64
	var firstErr error
	for _, p := range r.providers {
		// hadBinding gates the count: only a provider that held a REAL token
		// (Bound / Expired) counts toward RevokedCount. A configured-but-
		// unbound source has nothing to revoke — its terminal idempotent
		// delete succeeds, but reporting it as a revocation would over-count a
		// no-op. A provider that does not expose a status read is counted on a
		// successful revoke (we cannot tell, and under-counting a real
		// revocation is worse than over-counting an unknown one).
		hadBinding := true
		if bs, ok := p.(bindingStatuser); ok {
			state, sErr := bs.BindingStatus(ctx, source)
			if sErr == nil {
				if state == auth.BindingNotConfigured {
					// Not this provider's source — skip (Revoke would only
					// return a config-invalid error).
					continue
				}
				hadBinding = state == auth.BindingBound || state == auth.BindingExpired
			}
		}
		if rErr := p.Revoke(ctx, source); rErr != nil {
			if firstErr == nil {
				firstErr = rErr
			}
			continue
		}
		if hadBinding {
			count++
		}
	}
	return count, firstErr
}
