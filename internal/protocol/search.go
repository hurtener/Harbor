package protocol

import (
	"context"
	stderrors "errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/search"
)

// SearchSurface is the Phase 72c (D-108) Protocol-side dispatcher for
// the five `search.*` methods. It is transport-agnostic — the Phase 60
// wire transport's `search_handler.go` calls Dispatch from
// internal/protocol/transports/control via the
// `transports/control.SearchSurface` interface that this type
// satisfies. A protocol/conformance test consumer calls Dispatch
// directly (no HTTP layer needed).
//
// Identity at the edge: every method reads `identity.From(ctx)` and
// fails closed with CodeIdentityRequired (mapped to 401) on a missing
// triple. Cross-tenant gating reads `search.ErrCrossTenantRequiresAdmin`
// from the search subsystem and surfaces it as CodeAuthRejected (403)
// per D-079.
//
// Concurrent reuse (D-025): the SearchSurface is a compiled artifact —
// the registry + adminScope predicate are set once at construction;
// per-call state lives in ctx + req.
type SearchSurface struct {
	registry   *search.SearcherRegistry
	adminScope search.ScopeChecker
}

// NewSearchSurface builds the Phase 72c search dispatcher. The
// registry is mandatory; the adminScope predicate is mandatory.
// Both nil-checked at construction so a misconfigured surface fails at
// boot instead of nil-panicking on the first request.
func NewSearchSurface(registry *search.SearcherRegistry, adminScope search.ScopeChecker) (*SearchSurface, error) {
	if registry == nil {
		return nil, fmt.Errorf("%w: nil SearcherRegistry", ErrMisconfigured)
	}
	if adminScope == nil {
		return nil, fmt.Errorf("%w: nil ScopeChecker", ErrMisconfigured)
	}
	return &SearchSurface{registry: registry, adminScope: adminScope}, nil
}

// Dispatch routes the request to the right Searcher (or the aggregate
// dispatcher for `search.query`). It is the entry point both the wire
// transport (via the SearchSurface interface in
// internal/protocol/transports/control) and any in-process Protocol
// consumer (test, conformance suite) call.
//
// The return is always `(*types.SearchResponse, *protoerrors.Error)`
// — every error case maps onto a canonical Protocol code so the wire
// layer never sees an unstructured runtime error.
func (s *SearchSurface) Dispatch(ctx context.Context, method methods.Method, req *types.SearchRequest) (*types.SearchResponse, error) {
	if !methods.IsSearchMethod(method) {
		return nil, protoerrors.Newf(protoerrors.CodeUnknownMethod,
			"method %q is not a canonical search method", string(method))
	}
	if req == nil {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: request is nil", string(method))
	}

	// Identity at the edge — read directly from ctx (the Phase 61
	// auth middleware places it there). Search has no body-side
	// IdentityScope to backfill from; it's purely ctx-driven.
	callerID, ok := identity.From(ctx)
	if !ok {
		return nil, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: identity scope is missing from ctx", string(method))
	}
	if err := identity.Validate(callerID); err != nil {
		return nil, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: identity incomplete: %v", string(method), err)
	}

	var resp types.SearchResponse
	var err error
	switch method {
	case methods.MethodSearchQuery:
		resp, err = search.Query(ctx, s.registry, callerID, s.adminScope, *req)
	default:
		idx := indexFor(method)
		searcher, has := s.registry.Get(idx)
		if !has {
			return nil, protoerrors.Newf(protoerrors.CodeUnknownMethod,
				"method %q: no Searcher registered for index %q on this Runtime", string(method), idx)
		}
		resp, err = searcher.Search(ctx, *req)
	}

	if err != nil {
		return nil, mapSearchError(string(method), err)
	}
	resp.ProtocolVersion = types.ProtocolVersion
	return &resp, nil
}

// indexFor maps a per-index search method onto its SearchIndex. The
// table is the single bridge between the Protocol method vocabulary
// and the SearchIndex enum — keeping them distinct (a Protocol-name
// vs a runtime-name) per the same posture as methodToControlType.
func indexFor(m methods.Method) types.SearchIndex {
	switch m {
	case methods.MethodSearchSessions:
		return types.SearchIndexSessions
	case methods.MethodSearchTasks:
		return types.SearchIndexTasks
	case methods.MethodSearchEvents:
		return types.SearchIndexEvents
	case methods.MethodSearchArtifacts:
		return types.SearchIndexArtifacts
	default:
		return ""
	}
}

// mapSearchError translates a search subsystem sentinel onto a
// canonical Protocol error code. The mapping closes the wire surface
// (every err shape is observable as a Code; CLAUDE.md §13).
func mapSearchError(method string, err error) error {
	switch {
	case err == nil:
		return nil
	case stderrors.Is(err, search.ErrIdentityRequired):
		return protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: %v", method, err)
	case stderrors.Is(err, search.ErrCrossTenantRequiresAdmin):
		// Cross-tenant search without auth.ScopeAdmin lands here per
		// D-108. Reuses CodeScopeMismatch (mapped to 403 by the wire
		// status table) — the caller IS authenticated, just not
		// privileged enough for the scope they asked for. Same shape
		// as the steering-control scope rejection that lives in
		// dispatchControl: an authenticated-but-not-authorized caller.
		// (CodeAuthRejected stays pinned to 401 per D-079 — that code
		// signals a JWT that failed cryptographic verification, not
		// a scope shortfall.)
		return protoerrors.Newf(protoerrors.CodeScopeMismatch,
			"method %q: %v", method, err)
	case stderrors.Is(err, search.ErrUnknownIndex):
		return protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: %v", method, err)
	case stderrors.Is(err, search.ErrInvalidRequest):
		return protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: %v", method, err)
	case stderrors.Is(err, search.ErrRedactionFailed):
		// Audit redaction failed during a row's preview emission. We
		// fail loud rather than ship un-redacted bytes (CLAUDE.md §7
		// rule 6). The wire-mapped 500 surfaces it as a runtime error.
		return protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: redaction failed", method)
	default:
		return protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: search failed: %v", method, err)
	}
}
