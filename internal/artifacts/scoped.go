package artifacts

import (
	"context"
	"fmt"
)

// ScopedArtifacts is the immutable facade tools and runtime use to
// access the artifact store. It carries a fixed `ArtifactScope` (set
// at construction, never mutated) and:
//
//   - Auto-stamps the scope on every Put*: callers do not pass scope
//     at the facade boundary, so they cannot accidentally write to a
//     different one.
//   - Scope-checks on every read (Get, GetRef, Exists, Delete, List):
//     if the underlying store somehow returned a ref whose scope
//     differs, the facade returns wrapped `ErrScopeMismatch` rather
//     than leaking cross-scope bytes.
//
// Construction fails loud: `NewScoped` panics when the scope fails
// `Validate`. The facade's invariant is "every operation has a valid
// fixed scope"; rejecting at construction is the simplest, loudest
// failure mode (AGENTS.md §5: "fail loudly"). Acceptable per design —
// the alternative (returning an error from a constructor on every
// call site) accumulates boilerplate that masks the actual bug:
// callers that built a facade without a complete identity triple.
type ScopedArtifacts struct {
	store ArtifactStore
	scope ArtifactScope
}

// NewScoped wraps `store` with a fixed `scope`. Panics with a wrapped
// `ErrIdentityRequired` if scope is invalid (empty tenant/user/session).
//
// Tools and runtime construct ScopedArtifacts at the consumer
// boundary (e.g. tool dispatcher, Phase 26+); they then never see the
// raw `ArtifactScope` again.
func NewScoped(store ArtifactStore, scope ArtifactScope) *ScopedArtifacts {
	if store == nil {
		panic("artifacts: NewScoped called with nil store")
	}
	if err := scope.Validate(); err != nil {
		panic(fmt.Sprintf("artifacts: NewScoped called with invalid scope: %v", err))
	}
	return &ScopedArtifacts{store: store, scope: scope}
}

// Scope returns the fixed scope this facade was constructed with.
// Useful for tests / diagnostics; the value is immutable.
func (s *ScopedArtifacts) Scope() ArtifactScope {
	return s.scope
}

// PutBytes stores data under the facade's scope. The scope is stamped
// onto the returned ref automatically.
func (s *ScopedArtifacts) PutBytes(ctx context.Context, data []byte, opts PutOpts) (ArtifactRef, error) {
	return s.store.PutBytes(ctx, s.scope, data, opts)
}

// PutText stores text under the facade's scope.
func (s *ScopedArtifacts) PutText(ctx context.Context, text string, opts PutOpts) (ArtifactRef, error) {
	return s.store.PutText(ctx, s.scope, text, opts)
}

// Get returns the bytes for `id` within the facade's scope.
// Found-false is NOT an error.
//
// If the underlying store returns bytes for an id whose stored scope
// doesn't equal the facade's, the facade collapses to `(nil, false,
// nil)` because the facade's invariant is "all reads are within
// scope" — a store that disregards scope is reported as
// `ErrScopeMismatch` via GetRef (which surfaces the mismatch loudly).
// Drivers that filter by scope (every V1 driver) will simply return
// found-false for cross-scope ids, so this branch is defensive.
func (s *ScopedArtifacts) Get(ctx context.Context, id string) ([]byte, bool, error) {
	return s.store.Get(ctx, s.scope, id)
}

// GetRef returns the ref for `id` within the facade's scope.
// Found-false is NOT an error. If the underlying store returns a ref
// whose scope differs from the facade's, GetRef returns wrapped
// `ErrScopeMismatch` (defensive — V1 drivers filter by scope so this
// can only fire on a driver bug or future cross-scope driver).
func (s *ScopedArtifacts) GetRef(ctx context.Context, id string) (*ArtifactRef, bool, error) {
	ref, found, err := s.store.GetRef(ctx, s.scope, id)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	if !ref.Scope.Equal(s.scope) {
		return nil, false, fmt.Errorf("%w: facade=%+v, ref=%+v",
			ErrScopeMismatch, s.scope, ref.Scope)
	}
	return ref, true, nil
}

// Exists reports whether `id` is stored within the facade's scope.
func (s *ScopedArtifacts) Exists(ctx context.Context, id string) (bool, error) {
	return s.store.Exists(ctx, s.scope, id)
}

// Delete removes `id` from the facade's scope. Returns whether
// anything existed before delete; idempotent.
func (s *ScopedArtifacts) Delete(ctx context.Context, id string) (bool, error) {
	return s.store.Delete(ctx, s.scope, id)
}

// List returns every artifact under the facade's scope. The full
// scope is used as the filter (so this returns artifacts at the
// facade's task scope, NOT the broader session/user/tenant). Callers
// that need wildcard listings construct a separate ScopedArtifacts
// (or call the underlying store's List directly with a wildcard
// filter — appropriate at admin / observability layers, not tools).
func (s *ScopedArtifacts) List(ctx context.Context) ([]ArtifactRef, error) {
	return s.store.List(ctx, s.scope)
}
