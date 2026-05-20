// Package search owns the Harbor Phase 72c (D-108) cross-cutting
// search primitive — the four runtime-side per-subsystem indexes
// (sessions, tasks, events, artifacts) plus the `search.query` palette
// dispatcher that aggregates them.
//
// # Why this package exists
//
// Brief 11 §CC-4 split the Console's cross-cutting global search into
// two halves: runtime-side for high-cardinality entities (sessions,
// tasks, events, artifacts) and Console-side for slow-moving catalog
// data (tools, agents, flows, MCP connections). This package owns the
// runtime-side half. Console-side adapters do NOT live here — they
// land in their per-page Stage-2 Console phases (73c/d/e/f/g/i/k).
//
// # The §4.4 seam
//
// `Searcher` is the per-index interface; one Searcher instance per
// canonical index, all registered into a `SearcherRegistry` at boot.
// The `search.query` palette dispatcher (`aggregate.go::Query`) fans
// out concurrently to every requested index. There is no driver
// pluralism per index — the runtime owns one implementation per index;
// pluralism would be a post-V1 concern (an FTS sidecar swap, etc.).
//
// # Identity is mandatory (CLAUDE.md §6, RFC §5.5)
//
// Every search call rejects requests with an incomplete identity
// triple via `ErrIdentityRequired`. Cross-tenant search requires the
// `auth.ScopeAdmin` claim per D-079; an unauth'd cross-tenant request
// is rejected with `ErrCrossTenantRequiresAdmin`. The rejection is
// loud — there is no silent degradation to an empty result set
// (CLAUDE.md §13).
//
// # Heavy-payload bypass (D-026)
//
// Result rows whose underlying preview payload would exceed the
// heavy-content threshold ship as a populated `Ref` on the
// `SearchResultRow`, never inline bytes. Per-index searchers enforce
// this at the row-construction site.
//
// # Concurrent reuse (D-025)
//
// Every Searcher is a compiled artifact: the dependency reads are set
// once at construction (the SessionRegistry / TaskRegistry / Replayer
// / ArtifactStore / Redactor); per-call state lives in `ctx` + the
// `SearchRequest`. One Searcher serves N concurrent goroutines safely.
package search

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// HeavyPreviewThreshold is the D-026 bound — a `SearchResultRow`
// preview whose UTF-8 byte length would exceed this value ships as a
// `*SearchArtifactRef` instead of inline bytes. The threshold mirrors
// the runtime LLM-edge safety net (32 KiB) but stays a local constant
// so the search package has no dependency on internal/llm.
const HeavyPreviewThreshold = 32 * 1024

// PreviewMaxRunes is the soft cap applied at the row-construction site
// after redaction — a preview that fits under HeavyPreviewThreshold
// but exceeds this rune count is truncated with an ellipsis. Keeps the
// wire response compact even when individual previews are within the
// hard byte bound.
const PreviewMaxRunes = 256

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrIdentityRequired — the search request's identity scope is
	// missing one of (tenant, user, session). Identity is mandatory
	// (CLAUDE.md §6 rule 9, RFC §5.5).
	ErrIdentityRequired = errors.New("search: identity required (tenant/user/session)")
	// ErrCrossTenantRequiresAdmin — the request's filter expands the
	// query OUTSIDE the caller's authenticated tenant AND the caller
	// does not hold the `auth.ScopeAdmin` claim (D-079).
	ErrCrossTenantRequiresAdmin = errors.New("search: cross-tenant search requires the auth.ScopeAdmin claim")
	// ErrInvalidRequest — the request fails structural validation
	// (PageSize > MaxSearchPageSize, an unknown SearchIndex in
	// `Indexes`, etc.). Surfaces at the handler boundary as
	// CodeInvalidRequest.
	ErrInvalidRequest = errors.New("search: invalid request")
	// ErrRedactionFailed — a `SearchResultRow.Preview` failed
	// redaction. Per the CLAUDE.md §7 rule, the request fails loud
	// rather than emitting un-redacted bytes.
	ErrRedactionFailed = errors.New("search: redaction failed")
	// ErrUnknownIndex — an `Indexes` entry on a `SearchRequest` for
	// `search.query` is not one of the four canonical runtime-side
	// indexes. Distinct from ErrInvalidRequest so callers can branch.
	ErrUnknownIndex = errors.New("search: unknown index")
)

// Searcher is the §4.4 seam interface — one implementation per
// canonical runtime-side index. Each implementation is a D-025
// compiled artifact: every dependency is set once at construction;
// per-call state lives in `ctx` + `SearchRequest`.
//
// Implementations MUST:
//
//   - Reject incomplete identity (`ErrIdentityRequired`).
//   - Gate cross-tenant requests on `auth.ScopeAdmin` via the
//     `ScopeChecker` passed at construction (`ErrCrossTenantRequiresAdmin`).
//   - Redact every emitted `Preview` via the supplied `audit.Redactor`
//     before returning (`ErrRedactionFailed` on failure).
//   - Ship a `*SearchArtifactRef` instead of inline bytes when a
//     preview would exceed `HeavyPreviewThreshold` (D-026).
//   - Honor `ctx.Err()` between long phases of work.
type Searcher interface {
	// Index returns the canonical index this Searcher serves.
	Index() types.SearchIndex
	// Search runs the query against the index, applies the identity +
	// scope filter, redacts each preview, paginates, and returns.
	Search(ctx context.Context, req types.SearchRequest) (types.SearchResponse, error)
}

// ScopeChecker is the narrow predicate the Searchers consult to decide
// whether a cross-tenant request is allowed. The production
// implementation reads from `internal/protocol/auth.HasScope(ctx,
// ScopeAdmin)`; tests inject a deterministic predicate.
//
// The signature deliberately takes `ctx` (not a verified-identity
// struct) so the implementation can read the verified scope set from
// the auth context attached by the Phase 61 middleware.
type ScopeChecker func(ctx context.Context) bool

// Deps bundles the construction-time dependencies every per-index
// searcher needs. Fields documented per searcher; missing required
// dependencies fail loud at construction (CLAUDE.md §5).
type Deps struct {
	// Redactor is the audit redactor every Preview goes through before
	// emission. Required.
	Redactor audit.Redactor
	// AdminScope is the predicate the searcher consults to gate
	// cross-tenant requests. Required.
	AdminScope ScopeChecker
}

// Validate returns wrapped ErrInvalidRequest when a required Dep is
// missing.
func (d Deps) Validate() error {
	if d.Redactor == nil {
		return fmt.Errorf("%w: search.Deps.Redactor is nil", ErrInvalidRequest)
	}
	if d.AdminScope == nil {
		return fmt.Errorf("%w: search.Deps.AdminScope is nil", ErrInvalidRequest)
	}
	return nil
}

// SearcherRegistry is the registered set of per-index Searchers — one
// per canonical index. The aggregate dispatcher (`Query`) reads from
// it; the per-method protocol handlers route directly to the named
// Searcher via `Get`.
//
// A registry is built once at boot (`NewRegistry`) and shared across
// every search call. Registration is closed after construction — there
// is no Register-at-runtime seam (the canonical index set is closed).
type SearcherRegistry struct {
	byIndex map[types.SearchIndex]Searcher
}

// NewRegistry builds a SearcherRegistry from the given Searchers. Each
// supplied Searcher MUST report a canonical Index; duplicate indexes
// fail loud (the wiring is wrong). A registry MAY be incomplete (a
// subset of the four indexes) — `Query` skips unregistered indexes
// gracefully rather than failing the whole request, so a partial
// deployment is acceptable; the missing index simply contributes zero
// rows.
func NewRegistry(searchers ...Searcher) (*SearcherRegistry, error) {
	reg := &SearcherRegistry{byIndex: map[types.SearchIndex]Searcher{}}
	for _, s := range searchers {
		if s == nil {
			return nil, fmt.Errorf("%w: NewRegistry received a nil Searcher", ErrInvalidRequest)
		}
		idx := s.Index()
		if !types.IsValidSearchIndex(idx) {
			return nil, fmt.Errorf("%w: NewRegistry received a Searcher with non-canonical index %q", ErrInvalidRequest, idx)
		}
		if _, dupe := reg.byIndex[idx]; dupe {
			return nil, fmt.Errorf("%w: NewRegistry received two Searchers for index %q", ErrInvalidRequest, idx)
		}
		reg.byIndex[idx] = s
	}
	return reg, nil
}

// Get returns the Searcher registered for the given index, or false
// when no Searcher is registered for that index.
func (r *SearcherRegistry) Get(idx types.SearchIndex) (Searcher, bool) {
	if r == nil {
		return nil, false
	}
	s, ok := r.byIndex[idx]
	return s, ok
}

// Indexes returns the sorted list of indexes the registry knows about.
// Deterministic — sorted lexicographically.
func (r *SearcherRegistry) Indexes() []types.SearchIndex {
	if r == nil {
		return nil
	}
	out := make([]types.SearchIndex, 0, len(r.byIndex))
	for idx := range r.byIndex {
		out = append(out, idx)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ValidateRequest is the request-edge structural check shared by every
// `search.*` method. It enforces:
//
//   - Identity triple present (`ErrIdentityRequired`).
//   - PageSize within `[0, types.MaxSearchPageSize]` (`ErrInvalidRequest`).
//   - Page non-negative (`ErrInvalidRequest`).
//   - Every value in `Indexes` (when populated, for `search.query`) is a
//     canonical index (`ErrUnknownIndex`).
//
// The caller is the per-method handler (or the aggregate dispatcher);
// the result is normalised `(page, size)` defaults for the caller to
// apply.
func ValidateRequest(callerID identity.Identity, req types.SearchRequest) error {
	if err := identity.Validate(callerID); err != nil {
		return fmt.Errorf("%w: %v", ErrIdentityRequired, err)
	}
	if req.PageSize < 0 {
		return fmt.Errorf("%w: page_size %d must be non-negative", ErrInvalidRequest, req.PageSize)
	}
	if req.PageSize > types.MaxSearchPageSize {
		return fmt.Errorf("%w: page_size %d exceeds max %d", ErrInvalidRequest, req.PageSize, types.MaxSearchPageSize)
	}
	if req.Page < 0 {
		return fmt.Errorf("%w: page %d must be non-negative", ErrInvalidRequest, req.Page)
	}
	for _, idx := range req.Indexes {
		if !types.IsValidSearchIndex(idx) {
			return fmt.Errorf("%w: %q", ErrUnknownIndex, idx)
		}
	}
	return nil
}

// CrossTenantRequested reports whether the request's filter targets
// tenants OTHER than the caller's authenticated tenant. The handler
// gates this on the admin-scope predicate; CrossTenantRequested itself
// is a pure read.
//
// The rule:
//
//   - Empty Filter.TenantIDs → caller's own tenant only (no cross-tenant).
//   - Filter.TenantIDs contains exactly the caller's tenant → no cross-tenant.
//   - Any other shape → cross-tenant requested.
func CrossTenantRequested(callerTenant string, req types.SearchRequest) bool {
	if len(req.Filter.TenantIDs) == 0 {
		return false
	}
	for _, t := range req.Filter.TenantIDs {
		if t != callerTenant {
			return true
		}
	}
	return false
}

// EffectiveTenantSet returns the set of tenants the request should be
// scoped to AFTER admin-gating. When the caller's filter is empty, the
// effective set is `{callerTenant}` (the default). When the filter
// names tenants, the effective set is those tenants verbatim — the
// caller already passed the admin-scope gate (if needed) by the time
// this is called.
func EffectiveTenantSet(callerTenant string, req types.SearchRequest) []string {
	if len(req.Filter.TenantIDs) == 0 {
		return []string{callerTenant}
	}
	// Deduplicate + sort for determinism.
	seen := make(map[string]struct{}, len(req.Filter.TenantIDs))
	out := make([]string, 0, len(req.Filter.TenantIDs))
	for _, t := range req.Filter.TenantIDs {
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return []string{callerTenant}
	}
	return out
}

// MatchesQuery reports whether haystack contains needle, case-folded.
// Empty needle matches anything (the caller wants every row in scope).
// Used uniformly by every per-index Searcher so the substring semantics
// are identical across the cluster.
func MatchesQuery(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

// MatchesAnyField is a convenience over MatchesQuery — true when at
// least one of `fields` contains `needle`. Per-index Searchers call this
// to OR together the searchable fields (e.g. session ID + agent name +
// status).
func MatchesAnyField(needle string, fields ...string) bool {
	if needle == "" {
		return true
	}
	for _, f := range fields {
		if MatchesQuery(f, needle) {
			return true
		}
	}
	return false
}

// TimeInWindow reports whether t falls within the request's
// [Since, Until] window. Zero Since means "no lower bound"; zero Until
// means "no upper bound." When both are zero, every time passes.
func TimeInWindow(t time.Time, req types.SearchRequest) bool {
	if !req.Filter.Since.IsZero() && t.Before(req.Filter.Since) {
		return false
	}
	if !req.Filter.Until.IsZero() && t.After(req.Filter.Until) {
		return false
	}
	return true
}

// Paginate slices rows according to the request's (defaulted) page +
// size and returns the slice plus the pagination math (page,
// page_size, page_count, total_count, has_more). Returns a non-nil
// empty slice when the page is past the end (rather than nil) so the
// JSON wire form is `[]` consistently.
func Paginate(all []types.SearchResultRow, req types.SearchRequest) (page, pageSize, pageCount int, totalCount int64, hasMore bool, slice []types.SearchResultRow) {
	page, pageSize = req.PageBounds()
	totalCount = int64(len(all))
	if pageSize <= 0 {
		pageSize = types.DefaultSearchPageSize
	}
	pageCount = int((totalCount + int64(pageSize) - 1) / int64(pageSize))
	if pageCount < 1 {
		pageCount = 1
	}
	start := (page - 1) * pageSize
	if start < 0 {
		start = 0
	}
	if start >= len(all) {
		return page, pageSize, pageCount, totalCount, false, []types.SearchResultRow{}
	}
	end := start + pageSize
	if end > len(all) {
		end = len(all)
	}
	slice = make([]types.SearchResultRow, end-start)
	copy(slice, all[start:end])
	hasMore = int64(page) < int64(pageCount)
	return
}

// SortRowsByOccurredAtDesc orders rows newest-first. V1's ordering
// contract per the Phase 72c plan: lexicographic match + time-order;
// post-V1 may add relevance scoring.
func SortRowsByOccurredAtDesc(rows []types.SearchResultRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].OccurredAt.Equal(rows[j].OccurredAt) {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].OccurredAt.After(rows[j].OccurredAt)
	})
}

// RedactAndCapPreview is the standard preview-emission helper every
// Searcher uses: it (a) runs the raw preview through the audit
// Redactor; (b) checks the byte-length against HeavyPreviewThreshold —
// when over, returns the empty preview + a true bool signalling the
// caller MUST populate a Ref instead; (c) caps to PreviewMaxRunes with
// an ellipsis.
//
// On redaction failure: returns wrapped ErrRedactionFailed. The caller
// (typically inside Search) MUST NOT emit a row when this errors.
func RedactAndCapPreview(ctx context.Context, redactor audit.Redactor, preview string) (out string, heavy bool, err error) {
	if redactor == nil {
		return "", false, fmt.Errorf("%w: nil redactor", ErrRedactionFailed)
	}
	if preview == "" {
		return "", false, nil
	}
	redacted, rerr := redactor.Redact(ctx, preview)
	if rerr != nil {
		return "", false, fmt.Errorf("%w: %v", ErrRedactionFailed, rerr)
	}
	redactedStr, ok := redacted.(string)
	if !ok {
		// The pattern-based Redactor returns the same kind as input —
		// a string in / a string out. A non-string return is the audit
		// driver replacing the value with a structured marker (e.g. a
		// RedactedMap). Emit the empty string + ship a refless row;
		// the rest of the redactor contract is preserved.
		return "", false, nil
	}
	if len(redactedStr) >= HeavyPreviewThreshold {
		return "", true, nil
	}
	// Cap to PreviewMaxRunes for compact wire shape.
	runes := []rune(redactedStr)
	if len(runes) > PreviewMaxRunes {
		runes = runes[:PreviewMaxRunes]
		redactedStr = string(runes) + "…"
	}
	return redactedStr, false, nil
}

// ConcurrentReuseTag is a no-op assertion site — kept as a package-level
// var so a future static analyser can grep it as the canonical D-025
// witness across the search subsystem. The actual enforcement is the
// concurrent_reuse_test.go N≥100 stress.
var ConcurrentReuseTag = struct{ Note string }{Note: "search: per-index Searchers are D-025 compiled artifacts; per-call state lives in ctx + req"}

// mu is the search package's only package-level mutable state — and
// it guards exactly nothing. It's a placeholder so a future addition
// (driver registry, metric publisher) can land without a struct rename.
var _ = sync.Mutex{}
