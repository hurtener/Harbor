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
//   - Scope-checks on `GetRef`, and ONLY on `GetRef`: if the underlying
//     store returned a ref outside the facade's isolation triple, the
//     facade returns wrapped `ErrScopeMismatch` rather than leaking
//     cross-scope bytes. `Get` / `Exists` / `Delete` / `List` pass
//     straight through — the DRIVERS are the enforcement point, and
//     they resolve on the triple. (This list used to claim all five
//     methods checked; only one ever did. The claim is corrected rather
//     than the other four grown a check they do not need.)
//
// The check compares the ISOLATION TRIPLE, not the whole scope. A ref
// carries the producing task's `TaskID` as a provenance stamp, and a
// facade constructed without one — the ordinary case for a tool reading
// back what an earlier run in the same session wrote — must not read
// that stamp as a mismatch. Comparing the full scope here would refuse
// exactly the read the triple-keyed store exists to allow.
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
// boundary (e.g. the tool dispatcher); they then never see the
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

// Get returns the bytes for `id` within the facade's isolation triple.
// Found-false is NOT an error. The store resolves on the triple, so a
// facade constructed with or without a `TaskID` reads the same bytes.
func (s *ScopedArtifacts) Get(ctx context.Context, id string) ([]byte, bool, error) {
	return s.store.Get(ctx, s.scope, id)
}

// GetRef returns the ref for `id` within the facade's isolation triple.
// Found-false is NOT an error. If the underlying store returns a ref
// from a DIFFERENT triple, GetRef returns wrapped `ErrScopeMismatch`
// (defensive — V1 drivers resolve on the triple, so this can only fire
// on a driver bug or a future cross-scope driver).
//
// The comparison is `EqualTriple`, not `Equal`: `Ref.Scope.TaskID` is
// the producing task's provenance stamp and is expected to differ from
// the facade's whenever a sibling run in the same session wrote the
// bytes. Comparing the full scope would turn the read the reconciled
// key exists to enable into an error.
func (s *ScopedArtifacts) GetRef(ctx context.Context, id string) (*ArtifactRef, bool, error) {
	ref, found, err := s.store.GetRef(ctx, s.scope, id)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	if !ref.Scope.EqualTriple(s.scope) {
		return nil, false, fmt.Errorf("%w: facade=%+v, ref=%+v",
			ErrScopeMismatch, s.scope, ref.Scope)
	}
	return ref, true, nil
}

// Exists reports whether `id` is stored under the facade's isolation
// triple.
func (s *ScopedArtifacts) Exists(ctx context.Context, id string) (bool, error) {
	return s.store.Exists(ctx, s.scope, id)
}

// Delete removes `id` from the facade's isolation triple. Returns
// whether anything existed before delete; idempotent.
func (s *ScopedArtifacts) Delete(ctx context.Context, id string) (bool, error) {
	return s.store.Delete(ctx, s.scope, id)
}

// List returns every artifact under the facade's isolation TRIPLE — the
// facade's own `TaskID` is deliberately dropped from the filter.
//
// The facade's reads resolve on the triple, so listing on the full scope
// would enumerate a strict subset of what the same facade can `Get`:
// the caller would be shown rows for its own task and hidden from rows
// a sibling run wrote that it can nonetheless fetch. Listing and reading
// answer the same question here, which is the whole point of the
// reconciled key. Callers that want a per-task provenance filter call
// the underlying store's `List` with a `TaskID` — and read that method's
// godoc on why the answer is first-writer-lossy.
func (s *ScopedArtifacts) List(ctx context.Context) ([]ArtifactRef, error) {
	return s.store.List(ctx, s.scope.Triple())
}
