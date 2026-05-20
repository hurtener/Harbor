// Package protocol implements the runtime side of the Console Flows
// page (Phase 73i / D-117). It is the transport-agnostic surface the
// six `flows.*` Protocol methods dispatch through:
//
//   - flows.list           — paginated catalog of registered flows
//   - flows.describe        — a flow's full engine-graph description
//   - flows.runs.list       — a flow's paginated run history
//   - flows.runs.describe   — a single run's per-node timeline
//   - flows.run             — invoke a one-shot run of a flow (mutating)
//   - flows.metrics         — a flow's time-bucketed sparkline metrics
//
// Five methods are read-only; `flows.run` is the single mutating
// method. The Flows page is view-only at V1 (D-063) — there is no
// authoring surface here, by construction.
//
// # The source-of-truth seam (§4.4)
//
// The Surface depends only on two interfaces — Catalog (the registered
// flows + their engine-graph descriptions + run history) and Invoker
// (the one-shot run launcher). The concrete Catalog is the runtime's
// flow registry; the concrete Invoker is the task registry's `start`
// path. Keeping the Surface behind interfaces means the wire surface is
// testable with deterministic fixtures (no live engine) and the
// concrete registry can evolve without a Protocol-shape break.
//
// # Multi-isolation (CLAUDE.md §6)
//
// Identity is mandatory on every method — an incomplete triple fails
// closed with ErrIdentityRequired. The catalog is per-runtime (flow
// definitions are tenant-agnostic descriptors), but run history is
// tenant-scoped: a non-admin caller sees only their own tenant's runs.
// A cross-tenant filter (a `Tenants` value reaching outside the
// caller's own tenant, or naming more than one tenant) requires the
// verified `auth.ScopeAdmin` claim (D-079) — the Surface fails closed
// with ErrCrossTenantScope when the claim is absent.
//
// # The mutating gate (D-079)
//
// `flows.run` mutates: it launches a run. It is gated on identity AND
// the verified `auth.ScopeAdmin` claim. The Surface never mints a new
// scope (D-079 closed two-scope set) — `auth.ScopeAdmin` is the run
// entitlement. A request without the claim fails closed with
// ErrRunScopeRequired.
//
// # Concurrent reuse (D-025)
//
// A Surface is a compiled artifact: every field is set once at
// construction and never mutated. ServeMethod holds no per-call state —
// per-call data flows through the request argument. One Surface is safe
// for N concurrent callers under -race; concurrent_reuse_test.go pins
// N≥100.
package protocol

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// Sentinel errors the Surface returns. Transport adapters map each onto
// a canonical Protocol error Code + HTTP status. They are explicit —
// the Surface never silently degrades (CLAUDE.md §5 fail-loudly).
var (
	// ErrMisconfigured — NewSurface was called with a nil mandatory
	// dependency.
	ErrMisconfigured = errors.New("flow/protocol: Surface missing a mandatory dependency")
	// ErrIdentityRequired — the request carried an incomplete identity
	// triple. Maps onto CodeIdentityRequired / HTTP 401.
	ErrIdentityRequired = errors.New("flow/protocol: identity scope incomplete")
	// ErrInvalidRequest — the request was structurally malformed (an
	// empty required id, an out-of-range page size). Maps onto
	// CodeInvalidRequest / HTTP 400.
	ErrInvalidRequest = errors.New("flow/protocol: request is invalid")
	// ErrNotFound — the request's target flow or run does not exist.
	// Maps onto CodeNotFound / HTTP 404.
	ErrNotFound = errors.New("flow/protocol: target not found")
	// ErrCrossTenantScope — a cross-tenant filter was requested without
	// the verified `auth.ScopeAdmin` claim. Maps onto
	// CodeIdentityScopeRequired / HTTP 403.
	ErrCrossTenantScope = errors.New("flow/protocol: cross-tenant filter requires the admin scope claim")
	// ErrRunScopeRequired — `flows.run` was called without the verified
	// `auth.ScopeAdmin` claim. Maps onto CodeScopeMismatch / HTTP 403.
	ErrRunScopeRequired = errors.New("flow/protocol: flows.run requires the admin scope claim")
	// ErrRuntime — a Catalog / Invoker call failed for a reason the
	// Surface could not classify. Maps onto CodeRuntimeError / HTTP 500.
	ErrRuntime = errors.New("flow/protocol: runtime error")
)

// Catalog is the read-only source-of-truth seam the Flows-page Surface
// depends on. The concrete implementation is the runtime's flow
// registry + run-history store; tests inject a deterministic fixture.
//
// Every method takes the caller identity + an adminScoped flag. The
// implementation MUST scope run history to the caller's tenant unless
// adminScoped is true. Flow definitions themselves are tenant-agnostic
// (they are descriptors registered at agent-definition time), so the
// catalog listing is not tenant-filtered — but the per-flow run
// aggregates ARE (a non-admin caller sees only their own tenant's run
// counts).
type Catalog interface {
	// ListFlows returns every registered graph-family flow with its
	// aggregate run metrics over the trailing 24h window, scoped to the
	// caller's identity (admin fans across tenants).
	ListFlows(ctx context.Context, id identity.Identity, adminScoped bool) ([]prototypes.Flow, error)
	// DescribeFlow returns a single flow's full engine-graph
	// description. An unknown id returns ErrNotFound.
	DescribeFlow(ctx context.Context, id identity.Identity, adminScoped bool, flowID string) (prototypes.FlowDescription, error)
	// ListRuns returns a flow's run history, newest first, scoped to the
	// caller's identity (admin fans across tenants when crossTenant is
	// requested). An unknown flow id returns ErrNotFound.
	ListRuns(ctx context.Context, id identity.Identity, adminScoped bool, flowID string, tenants []string) ([]prototypes.FlowRun, error)
	// DescribeRun returns a single run's per-node timeline + output
	// reference. An unknown run id returns ErrNotFound. The
	// implementation routes heavy outputs by-reference (D-026).
	DescribeRun(ctx context.Context, id identity.Identity, adminScoped bool, runID string) (prototypes.FlowRunDescription, error)
	// FlowMetrics returns a flow's time-bucketed sparkline aggregates
	// over the requested window. An unknown flow id returns ErrNotFound.
	FlowMetrics(ctx context.Context, id identity.Identity, adminScoped bool, flowID string, window, bucket time.Duration) (prototypes.FlowMetrics, error)
}

// Invoker launches a one-shot run of a registered flow. The concrete
// implementation wraps the task registry's `start` path; tests inject a
// deterministic fixture. Invoke runs under the caller's identity — the
// launched run inherits the (tenant, user, session) triple.
type Invoker interface {
	// Invoke launches a one-shot run of flowID with the supplied inputs
	// under the caller's identity. It returns the accepted run's
	// identifier + start time. An unknown flow id returns ErrNotFound; a
	// malformed input form returns ErrInvalidRequest.
	Invoke(ctx context.Context, id identity.Identity, flowID string, inputs map[string]any) (prototypes.FlowRunResponse, error)
}

// Surface is the transport-agnostic Flows-page Protocol surface. It is
// a compiled artifact (D-025): catalog + invoker are set once at
// construction and never mutated; ServeMethod holds no per-call state.
type Surface struct {
	catalog Catalog
	invoker Invoker
}

// NewSurface builds the Flows-page Surface over a Catalog + an Invoker.
// Both are mandatory — a nil fails loud with ErrMisconfigured rather
// than building a Surface that would nil-panic on the first request
// (CLAUDE.md §5). The returned *Surface is immutable after construction
// and safe for concurrent use by N goroutines.
func NewSurface(catalog Catalog, invoker Invoker) (*Surface, error) {
	if catalog == nil {
		return nil, fmt.Errorf("%w: Catalog is nil", ErrMisconfigured)
	}
	if invoker == nil {
		return nil, fmt.Errorf("%w: Invoker is nil", ErrMisconfigured)
	}
	return &Surface{catalog: catalog, invoker: invoker}, nil
}

// List handles `flows.list`. It validates identity, gates a cross-tenant
// filter on the admin scope, dispatches to the Catalog, and paginates +
// sorts the result deterministically (by flow ID).
func (s *Surface) List(ctx context.Context, req prototypes.FlowListRequest, adminScoped bool) (prototypes.FlowListResponse, error) {
	id, err := toIdentity(req.Identity)
	if err != nil {
		return prototypes.FlowListResponse{}, err
	}
	if err := validatePageSize(req.PageSize); err != nil {
		return prototypes.FlowListResponse{}, err
	}
	if crossTenantRequested(req.Filter.Tenants, id.TenantID) && !adminScoped {
		return prototypes.FlowListResponse{}, ErrCrossTenantScope
	}
	flows, err := s.catalog.ListFlows(ctx, id, adminScoped)
	if err != nil {
		return prototypes.FlowListResponse{}, classifyCatalogErr(err)
	}
	flows = filterFlows(flows, req.Filter)
	sort.Slice(flows, func(i, j int) bool { return flows[i].ID < flows[j].ID })
	page, pageSize := normalizePage(req.Page, req.PageSize)
	pageRows, pageCount := paginate(flows, page, pageSize)
	return prototypes.FlowListResponse{
		Flows:     pageRows,
		Page:      page,
		PageSize:  pageSize,
		PageCount: pageCount,
		TotalRows: len(flows),
	}, nil
}

// Describe handles `flows.describe`. It validates identity + the flow
// id, dispatches to the Catalog, and sorts the projection
// deterministically (nodes by ID, edges by From/To).
func (s *Surface) Describe(ctx context.Context, req prototypes.FlowDescribeRequest, adminScoped bool) (prototypes.FlowDescription, error) {
	id, err := toIdentity(req.Identity)
	if err != nil {
		return prototypes.FlowDescription{}, err
	}
	if strings.TrimSpace(req.ID) == "" {
		return prototypes.FlowDescription{}, fmt.Errorf("%w: flow id is empty", ErrInvalidRequest)
	}
	desc, err := s.catalog.DescribeFlow(ctx, id, adminScoped, req.ID)
	if err != nil {
		return prototypes.FlowDescription{}, classifyCatalogErr(err)
	}
	sortDescription(&desc)
	return desc, nil
}

// RunsList handles `flows.runs.list`. It validates identity + the flow
// id, gates a cross-tenant filter on the admin scope, dispatches to the
// Catalog, and paginates + sorts the runs (newest first).
func (s *Surface) RunsList(ctx context.Context, req prototypes.FlowRunsListRequest, adminScoped bool) (prototypes.FlowRunsListResponse, error) {
	id, err := toIdentity(req.Identity)
	if err != nil {
		return prototypes.FlowRunsListResponse{}, err
	}
	if strings.TrimSpace(req.FlowID) == "" {
		return prototypes.FlowRunsListResponse{}, fmt.Errorf("%w: flow_id is empty", ErrInvalidRequest)
	}
	if err := validatePageSize(req.PageSize); err != nil {
		return prototypes.FlowRunsListResponse{}, err
	}
	if crossTenantRequested(req.Tenants, id.TenantID) && !adminScoped {
		return prototypes.FlowRunsListResponse{}, ErrCrossTenantScope
	}
	runs, err := s.catalog.ListRuns(ctx, id, adminScoped, req.FlowID, req.Tenants)
	if err != nil {
		return prototypes.FlowRunsListResponse{}, classifyCatalogErr(err)
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].StartedAt.After(runs[j].StartedAt) })
	page, pageSize := normalizePage(req.Page, req.PageSize)
	pageRows, pageCount := paginateRuns(runs, page, pageSize)
	return prototypes.FlowRunsListResponse{
		Runs:      pageRows,
		Page:      page,
		PageSize:  pageSize,
		PageCount: pageCount,
		TotalRows: len(runs),
	}, nil
}

// RunsDescribe handles `flows.runs.describe`. It validates identity +
// the run id and dispatches to the Catalog. Heavy outputs are routed
// by-reference by the Catalog (D-026).
func (s *Surface) RunsDescribe(ctx context.Context, req prototypes.FlowRunDescribeRequest, adminScoped bool) (prototypes.FlowRunDescription, error) {
	id, err := toIdentity(req.Identity)
	if err != nil {
		return prototypes.FlowRunDescription{}, err
	}
	if strings.TrimSpace(req.RunID) == "" {
		return prototypes.FlowRunDescription{}, fmt.Errorf("%w: run_id is empty", ErrInvalidRequest)
	}
	desc, err := s.catalog.DescribeRun(ctx, id, adminScoped, req.RunID)
	if err != nil {
		return prototypes.FlowRunDescription{}, classifyCatalogErr(err)
	}
	return desc, nil
}

// Run handles `flows.run` — the single mutating Flows-page method. It
// validates identity, gates the call on the verified admin scope claim
// (D-079), and dispatches to the Invoker. A request without the claim
// fails closed with ErrRunScopeRequired.
func (s *Surface) Run(ctx context.Context, req prototypes.FlowRunRequest, adminScoped bool) (prototypes.FlowRunResponse, error) {
	id, err := toIdentity(req.Identity)
	if err != nil {
		return prototypes.FlowRunResponse{}, err
	}
	if strings.TrimSpace(req.FlowID) == "" {
		return prototypes.FlowRunResponse{}, fmt.Errorf("%w: flow_id is empty", ErrInvalidRequest)
	}
	if !adminScoped {
		return prototypes.FlowRunResponse{}, ErrRunScopeRequired
	}
	resp, err := s.invoker.Invoke(ctx, id, req.FlowID, req.Inputs)
	if err != nil {
		return prototypes.FlowRunResponse{}, classifyCatalogErr(err)
	}
	return resp, nil
}

// Metrics handles `flows.metrics`. It validates identity + the flow id
// and dispatches to the Catalog with the resolved window / bucket.
func (s *Surface) Metrics(ctx context.Context, req prototypes.FlowMetricsRequest, adminScoped bool) (prototypes.FlowMetrics, error) {
	id, err := toIdentity(req.Identity)
	if err != nil {
		return prototypes.FlowMetrics{}, err
	}
	if strings.TrimSpace(req.FlowID) == "" {
		return prototypes.FlowMetrics{}, fmt.Errorf("%w: flow_id is empty", ErrInvalidRequest)
	}
	window := time.Duration(req.WindowMS) * time.Millisecond
	if window <= 0 {
		window = 24 * time.Hour
	}
	bucket := time.Duration(req.BucketMS) * time.Millisecond
	if bucket <= 0 {
		bucket = time.Hour
	}
	if bucket > window {
		return prototypes.FlowMetrics{}, fmt.Errorf("%w: bucket_ms exceeds window_ms", ErrInvalidRequest)
	}
	m, err := s.catalog.FlowMetrics(ctx, id, adminScoped, req.FlowID, window, bucket)
	if err != nil {
		return prototypes.FlowMetrics{}, classifyCatalogErr(err)
	}
	return m, nil
}

// toIdentity converts the flat wire IdentityScope into a runtime
// identity triple, failing closed when any component is empty.
func toIdentity(scope prototypes.IdentityScope) (identity.Identity, error) {
	id := identity.Identity{
		TenantID:  strings.TrimSpace(scope.Tenant),
		UserID:    strings.TrimSpace(scope.User),
		SessionID: strings.TrimSpace(scope.Session),
	}
	if id.TenantID == "" || id.UserID == "" || id.SessionID == "" {
		return identity.Identity{}, fmt.Errorf("%w: (tenant=%q, user=%q, session=%q)",
			ErrIdentityRequired, id.TenantID, id.UserID, id.SessionID)
	}
	return id, nil
}

// validatePageSize rejects a page size above the documented maximum.
// Zero is permitted — it applies the default at normalizePage.
func validatePageSize(pageSize int) error {
	if pageSize < 0 {
		return fmt.Errorf("%w: page_size is negative", ErrInvalidRequest)
	}
	if pageSize > prototypes.MaxFlowListPageSize {
		return fmt.Errorf("%w: page_size %d exceeds max %d",
			ErrInvalidRequest, pageSize, prototypes.MaxFlowListPageSize)
	}
	return nil
}

// normalizePage resolves a (page, pageSize) pair to its effective
// values: page defaults to 1, pageSize defaults to the documented
// default when zero.
func normalizePage(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = prototypes.DefaultFlowListPageSize
	}
	return page, pageSize
}

// crossTenantRequested reports whether the supplied tenant filter
// reaches outside the caller's own tenant — a foreign tenant, or more
// than one tenant.
func crossTenantRequested(tenants []string, callerTenant string) bool {
	if len(tenants) == 0 {
		return false
	}
	if len(tenants) > 1 {
		return true
	}
	return tenants[0] != callerTenant
}

// filterFlows applies the planner-family + free-text filter to a flow
// catalog. The tenant filter is enforced by the Catalog implementation;
// this is the pure projection-side filter.
func filterFlows(flows []prototypes.Flow, f prototypes.FlowFilter) []prototypes.Flow {
	families := map[string]struct{}{}
	for _, fam := range f.PlannerFamilies {
		families[fam] = struct{}{}
	}
	query := strings.ToLower(strings.TrimSpace(f.Query))
	out := make([]prototypes.Flow, 0, len(flows))
	for _, fl := range flows {
		if len(families) > 0 {
			if _, ok := families[fl.PlannerFamily]; !ok {
				continue
			}
		}
		if query != "" {
			hay := strings.ToLower(fl.Name + " " + fl.ID + " " + fl.Owner)
			if !strings.Contains(hay, query) {
				continue
			}
		}
		out = append(out, fl)
	}
	return out
}

// paginate slices a flow catalog into the requested page and reports
// the total page count.
func paginate(flows []prototypes.Flow, page, pageSize int) ([]prototypes.Flow, int) {
	total := len(flows)
	pageCount := (total + pageSize - 1) / pageSize
	start := (page - 1) * pageSize
	if start >= total {
		return []prototypes.Flow{}, pageCount
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	out := make([]prototypes.Flow, end-start)
	copy(out, flows[start:end])
	return out, pageCount
}

// paginateRuns slices a run-history list into the requested page.
func paginateRuns(runs []prototypes.FlowRun, page, pageSize int) ([]prototypes.FlowRun, int) {
	total := len(runs)
	pageCount := (total + pageSize - 1) / pageSize
	start := (page - 1) * pageSize
	if start >= total {
		return []prototypes.FlowRun{}, pageCount
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	out := make([]prototypes.FlowRun, end-start)
	copy(out, runs[start:end])
	return out, pageCount
}

// sortDescription sorts a FlowDescription's nodes (by ID) and edges (by
// From then To) in place so two descriptions of the same flow marshal
// to byte-identical JSON.
func sortDescription(d *prototypes.FlowDescription) {
	sort.Slice(d.Nodes, func(i, j int) bool { return d.Nodes[i].ID < d.Nodes[j].ID })
	sort.Slice(d.Edges, func(i, j int) bool {
		if d.Edges[i].From != d.Edges[j].From {
			return d.Edges[i].From < d.Edges[j].From
		}
		return d.Edges[i].To < d.Edges[j].To
	})
}

// classifyCatalogErr maps a Catalog / Invoker error onto one of the
// Surface's sentinel errors. A Catalog implementation returns the
// Surface sentinels directly (ErrNotFound / ErrInvalidRequest); any
// other error is wrapped as ErrRuntime — never silently swallowed.
func classifyCatalogErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotFound):
		return err
	case errors.Is(err, ErrInvalidRequest):
		return err
	case errors.Is(err, ErrIdentityRequired):
		return err
	case errors.Is(err, ErrCrossTenantScope):
		return err
	default:
		return fmt.Errorf("%w: %w", ErrRuntime, err)
	}
}
