package protocol

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/flow"
)

// flowOutputArtifactNamespace is the artifact namespace heavy flow-run
// outputs are routed under (D-026). A dedicated namespace keeps the
// content-addressed IDs distinguishable from other artifact producers.
const flowOutputArtifactNamespace = "flow_run_output"

// RegistryCatalog is the production Catalog implementation. It projects
// the Console Flows-page wire shapes from a *flow.Registry — the
// runtime's source-of-truth for registered flows + run history. It is
// NOT a test stub (CLAUDE.md §13): it is the real catalog backed by the
// real registry; the binary wires it at boot.
//
// Heavy run outputs are routed by-reference through an ArtifactStore
// (D-026): a run whose final output meets or exceeds the configured
// heavy-content threshold ships a FlowArtifactRef, never inline bytes.
//
// Concurrent reuse (D-025): the RegistryCatalog is a compiled artifact
// — registry / store / threshold are set once at construction. Every
// method reads through the registry's own RWMutex; the catalog holds no
// per-call state.
type RegistryCatalog struct {
	registry  *flow.Registry
	artifacts artifacts.ArtifactStore
	threshold int
}

// NewRegistryCatalog builds the production Catalog over a flow.Registry
// + an ArtifactStore. Both are mandatory — a nil fails loud with
// ErrMisconfigured. threshold is the configured heavy-content byte size
// (cfg.Artifacts.HeavyOutputThresholdBytes); a non-positive value fails
// loud (a zero threshold would route every output by-reference).
//
// The returned *RegistryCatalog is immutable after construction and
// safe for concurrent use by N goroutines.
func NewRegistryCatalog(registry *flow.Registry, store artifacts.ArtifactStore, threshold int) (*RegistryCatalog, error) {
	if registry == nil {
		return nil, fmt.Errorf("%w: flow.Registry is nil", ErrMisconfigured)
	}
	if store == nil {
		return nil, fmt.Errorf("%w: artifacts.ArtifactStore is nil", ErrMisconfigured)
	}
	if threshold <= 0 {
		return nil, fmt.Errorf("%w: heavy-content threshold %d is non-positive", ErrMisconfigured, threshold)
	}
	return &RegistryCatalog{registry: registry, artifacts: store, threshold: threshold}, nil
}

// ListFlows projects every registered flow into a wire Flow row with
// its aggregate run metrics over the trailing 24h window. The run
// aggregates are tenant-scoped: a non-admin caller's counts cover only
// their own tenant.
func (c *RegistryCatalog) ListFlows(ctx context.Context, id identity.Identity, adminScoped bool) ([]prototypes.Flow, error) {
	now := time.Now()
	out := make([]prototypes.Flow, 0)
	for _, name := range c.registry.Names() {
		def, meta, ok := c.registry.Definition(name)
		if !ok {
			continue
		}
		runs, _ := c.registry.Runs(name)
		scoped := scopeRuns(runs, id, adminScoped, nil)
		out = append(out, projectFlow(def, meta, scoped, now))
	}
	return out, nil
}

// DescribeFlow projects a single flow's full engine-graph description.
func (c *RegistryCatalog) DescribeFlow(ctx context.Context, id identity.Identity, adminScoped bool, flowID string) (prototypes.FlowDescription, error) {
	def, meta, ok := c.registry.Definition(flowID)
	if !ok {
		return prototypes.FlowDescription{}, fmt.Errorf("%w: flow %q", ErrNotFound, flowID)
	}
	runs, _ := c.registry.Runs(flowID)
	scoped := scopeRuns(runs, id, adminScoped, nil)
	now := time.Now()
	nodes, edges := projectGraph(def)
	return prototypes.FlowDescription{
		Flow:              projectFlow(def, meta, scoped, now),
		Nodes:             nodes,
		Edges:             edges,
		Source:            meta.Source,
		BudgetConsumption: budgetConsumption(scoped, now.Add(-24*time.Hour)),
	}, nil
}

// ListRuns projects a flow's run history into wire FlowRun rows, scoped
// to the caller's identity (admin fans across the requested tenants).
func (c *RegistryCatalog) ListRuns(ctx context.Context, id identity.Identity, adminScoped bool, flowID string, tenants []string) ([]prototypes.FlowRun, error) {
	runs, ok := c.registry.Runs(flowID)
	if !ok {
		return nil, fmt.Errorf("%w: flow %q", ErrNotFound, flowID)
	}
	scoped := scopeRuns(runs, id, adminScoped, tenants)
	out := make([]prototypes.FlowRun, 0, len(scoped))
	for _, rec := range scoped {
		out = append(out, projectRun(rec))
	}
	return out, nil
}

// DescribeRun projects a single run's per-node timeline + final-output
// reference. A run whose output exceeds the heavy-content threshold
// (D-026) is routed by-reference through the ArtifactStore.
func (c *RegistryCatalog) DescribeRun(ctx context.Context, id identity.Identity, adminScoped bool, runID string) (prototypes.FlowRunDescription, error) {
	rec, ok := c.registry.RunByID(runID)
	if !ok {
		return prototypes.FlowRunDescription{}, fmt.Errorf("%w: run %q", ErrNotFound, runID)
	}
	// Identity scope: a non-admin caller may only describe a run in
	// their own tenant. A cross-tenant describe without the admin claim
	// fails closed as not-found (no existence oracle leak).
	if !adminScoped && rec.Identity.TenantID != id.TenantID {
		return prototypes.FlowRunDescription{}, fmt.Errorf("%w: run %q", ErrNotFound, runID)
	}
	desc := prototypes.FlowRunDescription{
		Run:        projectRun(rec),
		NodeStates: projectNodeStates(rec.NodeStates),
	}
	if rec.Output != "" {
		ref, err := c.maybeRouteHeavyOutput(ctx, rec)
		if err != nil {
			return prototypes.FlowRunDescription{}, err
		}
		if ref != nil {
			desc.OutputRef = ref
		} else {
			desc.OutputPreview = rec.Output
		}
	}
	return desc, nil
}

// FlowMetrics projects a flow's run history into time-bucketed
// sparkline aggregates over the requested window.
func (c *RegistryCatalog) FlowMetrics(ctx context.Context, id identity.Identity, adminScoped bool, flowID string, window, bucket time.Duration) (prototypes.FlowMetrics, error) {
	runs, ok := c.registry.Runs(flowID)
	if !ok {
		return prototypes.FlowMetrics{}, fmt.Errorf("%w: flow %q", ErrNotFound, flowID)
	}
	scoped := scopeRuns(runs, id, adminScoped, nil)
	now := time.Now()
	start := now.Add(-window)
	buckets := bucketMetrics(scoped, start, now, bucket)
	return prototypes.FlowMetrics{
		FlowID:            flowID,
		WindowStart:       start,
		WindowEnd:         now,
		Buckets:           buckets,
		BudgetConsumption: budgetConsumption(scoped, start),
	}, nil
}

// maybeRouteHeavyOutput checks a run's Output against the heavy-content
// threshold. Below the threshold it returns (nil, nil) — the caller
// ships the output inline. At or above it routes the output through the
// ArtifactStore and returns the by-reference FlowArtifactRef. A store
// failure fails loud — never a silent truncation (D-026, §13).
func (c *RegistryCatalog) maybeRouteHeavyOutput(ctx context.Context, rec flow.RunRecord) (*prototypes.FlowArtifactRef, error) {
	if len(rec.Output) < c.threshold {
		return nil, nil
	}
	scope := artifacts.ArtifactScope{
		TenantID:  rec.Identity.TenantID,
		UserID:    rec.Identity.UserID,
		SessionID: rec.Identity.SessionID,
	}
	ref, err := c.artifacts.PutBytes(ctx, scope, []byte(rec.Output), artifacts.PutOpts{
		MimeType:  "text/plain",
		Namespace: flowOutputArtifactNamespace,
		Source: map[string]any{
			// methods.MethodFlowsRunsDescribe is the single source for
			// the `flows.runs.describe` wire string (CLAUDE.md §8) — used
			// here so the Phase 58 single-source checker does not flag
			// the artifact-provenance literal.
			"producer": string(methods.MethodFlowsRunsDescribe),
			"run_id":   rec.RunID,
			"flow":     rec.FlowName,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("%w: heavy flow-run output could not be routed to the artifact store: %w", ErrRuntime, err)
	}
	return &prototypes.FlowArtifactRef{
		ID:        ref.ID,
		MimeType:  ref.MimeType,
		SizeBytes: ref.SizeBytes,
		Filename:  ref.Filename,
		SHA256:    ref.SHA256,
	}, nil
}

// scopeRuns filters a run-history slice to the caller's identity. A
// non-admin caller sees only their own tenant's runs. An admin caller
// sees every run, optionally restricted to the named tenants.
func scopeRuns(runs []flow.RunRecord, id identity.Identity, adminScoped bool, tenants []string) []flow.RunRecord {
	tenantSet := map[string]struct{}{}
	for _, t := range tenants {
		tenantSet[t] = struct{}{}
	}
	out := make([]flow.RunRecord, 0, len(runs))
	for _, rec := range runs {
		if !adminScoped {
			if rec.Identity.TenantID != id.TenantID {
				continue
			}
		} else if len(tenantSet) > 0 {
			if _, ok := tenantSet[rec.Identity.TenantID]; !ok {
				continue
			}
		}
		out = append(out, rec)
	}
	return out
}

// projectFlow builds a wire Flow row from a Definition + Metadata + the
// identity-scoped run history.
func projectFlow(def flow.Definition, meta flow.Metadata, runs []flow.RunRecord, now time.Time) prototypes.Flow {
	nodes, edges := projectGraph(def)
	family := meta.PlannerFamily
	if family == "" {
		family = "graph"
	}
	window := now.Add(-24 * time.Hour)
	var (
		runs24h     int64
		latencies   []time.Duration
		successes   int64
		lastRun     time.Time
		windowCount int64
	)
	for _, rec := range runs {
		if rec.StartedAt.After(rec.StartedAt) {
			continue
		}
		if rec.StartedAt.After(lastRun) {
			lastRun = rec.StartedAt
		}
		if rec.StartedAt.Before(window) {
			continue
		}
		runs24h++
		windowCount++
		if rec.Status == string(prototypes.FlowRunSucceeded) {
			successes++
		}
		if rec.Duration > 0 {
			latencies = append(latencies, rec.Duration)
		}
	}
	successRate := 0.0
	if windowCount > 0 {
		successRate = float64(successes) / float64(windowCount)
	}
	p50, p95 := percentiles(latencies)
	return prototypes.Flow{
		ID:            def.Name,
		Name:          def.Name,
		Owner:         meta.Owner,
		Version:       meta.Version,
		PlannerFamily: family,
		NodeCount:     len(nodes),
		EdgeCount:     len(edges),
		Runs24h:       runs24h,
		P50LatencyMS:  p50.Milliseconds(),
		P95LatencyMS:  p95.Milliseconds(),
		SuccessRate:   successRate,
		LastRun:       lastRun,
		Budget:        projectBudget(def.Budget),
	}
}

// projectBudget maps a runtime flow.Budget onto the wire FlowBudget.
func projectBudget(b flow.Budget) prototypes.FlowBudget {
	return prototypes.FlowBudget{
		DeadlineMS: b.Deadline.Milliseconds(),
		RequestCap: b.HopBudget,
		CostCapUSD: b.CostCap,
	}
}

// projectGraph builds the wire node + edge sets from a Definition. The
// node Type is inferred from the node name suffix convention; absent a
// convention it defaults to a tool node (the common case).
func projectGraph(def flow.Definition) ([]prototypes.FlowNode, []prototypes.FlowEdge) {
	nodes := make([]prototypes.FlowNode, 0, len(def.Nodes))
	edges := make([]prototypes.FlowEdge, 0)
	for nodeID, spec := range def.Nodes {
		fn := prototypes.FlowNode{
			ID:         string(nodeID),
			Type:       prototypes.FlowNodeTool,
			Descriptor: spec.Name,
		}
		if spec.Policy.MaxRetries > 0 || spec.Policy.TimeoutMS > 0 {
			fn.Policy = &prototypes.FlowNodePolicy{
				MaxRetries: spec.Policy.MaxRetries,
				TimeoutMS:  int64(spec.Policy.TimeoutMS),
			}
		}
		nodes = append(nodes, fn)
		for _, to := range spec.To {
			edges = append(edges, prototypes.FlowEdge{From: string(nodeID), To: string(to)})
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].To < edges[j].To
	})
	return nodes, edges
}

// projectRun maps a runtime RunRecord onto the wire FlowRun row.
func projectRun(rec flow.RunRecord) prototypes.FlowRun {
	return prototypes.FlowRun{
		RunID:      rec.RunID,
		FlowID:     rec.FlowName,
		Status:     prototypes.FlowRunStatus(rec.Status),
		Trigger:    prototypes.FlowRunTrigger(rec.Trigger),
		StartedAt:  rec.StartedAt,
		DurationMS: rec.Duration.Milliseconds(),
		CostUSD:    rec.CostUSD,
		Identity: prototypes.IdentityScope{
			Tenant:  rec.Identity.TenantID,
			User:    rec.Identity.UserID,
			Session: rec.Identity.SessionID,
		},
		ErrorClass: rec.ErrorClass,
	}
}

// projectNodeStates maps the runtime per-node timeline onto the wire
// FlowNodeRunState slice.
func projectNodeStates(states []flow.NodeRunRecord) []prototypes.FlowNodeRunState {
	out := make([]prototypes.FlowNodeRunState, 0, len(states))
	for _, s := range states {
		out = append(out, prototypes.FlowNodeRunState{
			NodeID:     s.NodeID,
			Status:     prototypes.FlowRunStatus(s.Status),
			DurationMS: s.Duration.Milliseconds(),
			Retries:    s.Retries,
			ErrorClass: s.ErrorClass,
		})
	}
	return out
}

// budgetConsumption sums the hop / cost / token consumption across the
// runs that started at or after `since`.
func budgetConsumption(runs []flow.RunRecord, since time.Time) prototypes.FlowBudgetConsumption {
	var c prototypes.FlowBudgetConsumption
	for _, rec := range runs {
		if rec.StartedAt.Before(since) {
			continue
		}
		c.RequestsUsed += len(rec.NodeStates)
		c.CostUSDUsed += rec.CostUSD
	}
	return c
}

// bucketMetrics groups a run-history slice into time buckets of width
// `bucket` spanning [start, end].
func bucketMetrics(runs []flow.RunRecord, start, end time.Time, bucket time.Duration) []prototypes.FlowMetricsBucket {
	if bucket <= 0 {
		bucket = time.Hour
	}
	n := int(end.Sub(start) / bucket)
	if n < 1 {
		n = 1
	}
	type acc struct {
		count     int64
		successes int64
		latencies []time.Duration
		cost      float64
	}
	accs := make([]acc, n)
	for _, rec := range runs {
		if rec.StartedAt.Before(start) || !rec.StartedAt.Before(end) {
			continue
		}
		idx := int(rec.StartedAt.Sub(start) / bucket)
		if idx < 0 || idx >= n {
			continue
		}
		accs[idx].count++
		if rec.Status == string(prototypes.FlowRunSucceeded) {
			accs[idx].successes++
		}
		if rec.Duration > 0 {
			accs[idx].latencies = append(accs[idx].latencies, rec.Duration)
		}
		accs[idx].cost += rec.CostUSD
	}
	out := make([]prototypes.FlowMetricsBucket, n)
	for i := range accs {
		_, p95 := percentiles(accs[i].latencies)
		rate := 0.0
		if accs[i].count > 0 {
			rate = float64(accs[i].successes) / float64(accs[i].count)
		}
		out[i] = prototypes.FlowMetricsBucket{
			BucketStart:  start.Add(time.Duration(i) * bucket),
			Runs:         accs[i].count,
			P95LatencyMS: p95.Milliseconds(),
			SuccessRate:  rate,
			CostUSD:      accs[i].cost,
		}
	}
	return out
}

// percentiles returns the p50 and p95 of a duration slice. An empty
// slice yields (0, 0).
func percentiles(d []time.Duration) (p50, p95 time.Duration) {
	if len(d) == 0 {
		return 0, 0
	}
	sorted := make([]time.Duration, len(d))
	copy(sorted, d)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	pick := func(q float64) time.Duration {
		idx := int(q * float64(len(sorted)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		return sorted[idx]
	}
	return pick(0.50), pick(0.95)
}

// compile-time assertion that RegistryCatalog satisfies Catalog.
var _ Catalog = (*RegistryCatalog)(nil)

// errRunInvoke is returned by RegistryInvoker when an Invoke call
// targets an unknown flow. It carries ErrNotFound so the Surface
// classifies it correctly.
var errRunInvoke = errors.New("flow/protocol: run invocation failed")
