package flow

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// Metadata carries the Console-facing descriptive fields a registered
// flow advertises beyond its runnable Definition. The runnable
// Definition (the engine graph) is tenant-agnostic; Metadata is the
// catalog-row decoration the Flows page (Phase 73i) renders.
type Metadata struct {
	// Owner is the agent / team that registered the flow. May be empty.
	Owner string
	// Version is the flow's version string. May be empty.
	Version string
	// PlannerFamily is the graph-family planner the flow runs on —
	// "graph" / "workflow" / "deterministic". Empty defaults to "graph".
	PlannerFamily string
	// Source is the source-of-truth reference — a Go path or a YAML
	// descriptor path (D-023: Go-coded V1; YAML V1.1). A string
	// reference, never executable code.
	Source string
}

// RunRecord is a single recorded invocation of a registered flow. The
// Registry keeps a bounded run-history ring per flow; the Flows-page
// Catalog (Phase 73i) projects these into the wire `FlowRun` rows.
//
// A RunRecord is identity-scoped: run history is tenant-scoped (a
// non-admin Console caller sees only their own tenant's runs), so the
// record carries the full triple it executed under.
type RunRecord struct {
	StartedAt  time.Time
	Identity   identity.Identity
	RunID      string
	FlowName   string
	Trigger    string
	Status     string
	ErrorClass string
	Output     string
	NodeStates []NodeRunRecord
	Duration   time.Duration
	CostUSD    float64
}

// NodeRunRecord is one node's slice of a run's per-node timeline.
type NodeRunRecord struct {
	NodeID     string
	Status     string
	ErrorClass string
	Duration   time.Duration
	Retries    int
}

// registeredFlow bundles a flow's runnable Definition with its catalog
// Metadata and its bounded run-history ring.
type registeredFlow struct {
	meta Metadata
	runs []RunRecord
	def  Definition
}

// maxRunHistoryPerFlow bounds the per-flow run-history ring. A flow
// with a high invocation rate keeps only the most recent N records;
// the Flows page paginates over this bounded window. The bound is the
// floor protection the Phase 73i plan names against an unbounded
// `flows.runs.list` cost.
const maxRunHistoryPerFlow = 1000

// Registry is the runtime's source-of-truth for registered flows and
// their run history. It is the seam the Phase 73i Console Flows-page
// Catalog reads from. It is NOT a test stub — it is a real runtime
// subsystem: a flow registers into it at agent-definition time, the run
// loop records each invocation, and the Console projects the catalog
// from it.
//
// Concurrent reuse (D-025): the Registry is safe for N concurrent
// callers — every field access is guarded by an RWMutex. Registration
// and run recording take the write lock; catalog reads take the read
// lock. The Registry holds no per-call state.
type Registry struct {
	flows map[string]*registeredFlow
	mu    sync.RWMutex
}

// NewRegistry builds an empty flow Registry.
func NewRegistry() *Registry {
	return &Registry{flows: map[string]*registeredFlow{}}
}

// Register adds a flow Definition + its catalog Metadata to the
// Registry. A flow with an empty name, or a name already registered,
// fails loud — registration is not silently overwritten (CLAUDE.md §5).
func (r *Registry) Register(def Definition, meta Metadata) error {
	if err := def.Validate(); err != nil {
		return fmt.Errorf("flow.Registry.Register: invalid definition: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.flows[def.Name]; exists {
		return fmt.Errorf("flow.Registry.Register: flow %q is already registered", def.Name)
	}
	r.flows[def.Name] = &registeredFlow{def: def, meta: meta}
	return nil
}

// RecordRun appends a RunRecord to the named flow's run-history ring.
// An unknown flow name fails loud. The ring is bounded — the oldest
// record is dropped once the ring is full.
func (r *Registry) RecordRun(rec RunRecord) error {
	if rec.FlowName == "" {
		return fmt.Errorf("flow.Registry.RecordRun: flow name is empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rf, ok := r.flows[rec.FlowName]
	if !ok {
		return fmt.Errorf("flow.Registry.RecordRun: flow %q is not registered", rec.FlowName)
	}
	rf.runs = append(rf.runs, rec)
	if len(rf.runs) > maxRunHistoryPerFlow {
		rf.runs = rf.runs[len(rf.runs)-maxRunHistoryPerFlow:]
	}
	return nil
}

// Names returns the registered flow names, sorted lexicographically.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.flows))
	for name := range r.flows {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Definition returns a registered flow's runnable Definition + its
// Metadata. The bool is false when the name is not registered.
func (r *Registry) Definition(name string) (Definition, Metadata, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rf, ok := r.flows[name]
	if !ok {
		return Definition{}, Metadata{}, false
	}
	return rf.def, rf.meta, true
}

// Runs returns a copy of the named flow's run-history ring. The bool is
// false when the name is not registered. The caller is free to mutate
// the returned slice — it is a defensive copy.
func (r *Registry) Runs(name string) ([]RunRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rf, ok := r.flows[name]
	if !ok {
		return nil, false
	}
	out := make([]RunRecord, len(rf.runs))
	copy(out, rf.runs)
	return out, true
}

// RunByID finds a run record by its RunID across every registered
// flow. The bool is false when no flow holds a run with that id.
func (r *Registry) RunByID(runID string) (RunRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, rf := range r.flows {
		for _, rec := range rf.runs {
			if rec.RunID == runID {
				return rec, true
			}
		}
	}
	return RunRecord{}, false
}
