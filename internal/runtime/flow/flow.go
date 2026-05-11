// Package flow implements Harbor's Flow-as-Tool registration
// (RFC §6.1, D-023): a typed DAG of Nodes assembled into a
// runnable Engine via `Compose(def)`, then registered as a single
// Tool in the catalog via `RegisterAsTool(catalog, def, eng)`. The
// planner sees one Tool with an args/result schema; invoking it
// runs the underlying DAG with the runtime's full reliability
// shell — per-node `NodePolicy` plus an aggregate `Budget`
// enforced at the flow boundary.
//
// Composition with the parent run + identity-tier ceilings is via
// `min()` on each axis (deadline / hop budget / cost cap); whichever
// fires first aborts the flow with `ErrFlowBudgetExceeded`. Identity-
// tier governance budgets (Phase 36a, not yet shipped) compose
// uniformly through the same min() path — the Budget composition
// is open-ended.
//
// Layering rule (D-024): when a flow is invoked AS a tool, the
// dispatcher's `ToolPolicy` wraps the OUTER invocation; the per-node
// `NodePolicy` runs INSIDE the flow's engine. No double-wrapping at
// any single layer.
//
// Concurrent reuse contract (D-025): a composed `engine.Engine` is
// reusable across invocations; each invocation gets its own per-call
// Budget accumulator (lock-free atomic counters) so budget state
// never bleeds between concurrent flow invocations.
package flow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/messages"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// NodeID is the in-flow identifier for a node.
type NodeID string

// NodeSpec describes a single node in a Flow Definition.
type NodeSpec struct {
	Name   string
	Func   engine.NodeFunc
	Policy engine.NodePolicy
	To     []NodeID
}

// Budget is the per-flow aggregate cap.
type Budget struct {
	Deadline  time.Duration
	HopBudget int
	CostCap   float64
}

// Definition is the canonical Go shape for a Flow.
type Definition struct {
	Name        string
	Description string
	Entry       NodeID
	Exit        NodeID
	Nodes       map[NodeID]NodeSpec
	Budget      Budget
	InSchema    json.RawMessage
	OutSchema   json.RawMessage
}

// Validate runs structural checks on the Definition.
func (d Definition) Validate() error {
	if d.Name == "" {
		return wrap(ErrFlowInvalidDefinition, "name is empty")
	}
	if d.Entry == "" {
		return wrap(ErrFlowInvalidDefinition, "entry is empty")
	}
	if d.Exit == "" {
		return wrap(ErrFlowInvalidDefinition, "exit is empty")
	}
	if len(d.Nodes) == 0 {
		return wrap(ErrFlowInvalidDefinition, "nodes is empty")
	}
	if _, ok := d.Nodes[d.Entry]; !ok {
		return wrap(ErrFlowEntryExitMismatch, "entry node %q not in graph", d.Entry)
	}
	if _, ok := d.Nodes[d.Exit]; !ok {
		return wrap(ErrFlowEntryExitMismatch, "exit node %q not in graph", d.Exit)
	}
	for id, spec := range d.Nodes {
		if spec.Func == nil {
			return wrap(ErrFlowInvalidDefinition, "node %q has nil Func", id)
		}
		for _, to := range spec.To {
			if _, ok := d.Nodes[to]; !ok {
				return wrap(ErrFlowEntryExitMismatch, "node %q references missing target %q", id, to)
			}
		}
	}
	if d.Budget.Deadline < 0 {
		return wrap(ErrFlowInvalidDefinition, "budget.deadline is negative")
	}
	if d.Budget.HopBudget < 0 {
		return wrap(ErrFlowInvalidDefinition, "budget.hop_budget is negative")
	}
	if d.Budget.CostCap < 0 {
		return wrap(ErrFlowInvalidDefinition, "budget.cost_cap is negative")
	}
	return nil
}

// ComposeOption configures a Compose call.
type ComposeOption func(*composeConfig)

type composeConfig struct {
	queueSize int
}

// WithComposeQueueSize overrides the engine's per-channel queue
// capacity (default 256). Higher values absorb more burst
// concurrency at the cost of memory; lower values apply tighter
// backpressure.
func WithComposeQueueSize(n int) ComposeOption {
	return func(c *composeConfig) { c.queueSize = n }
}

// Compose builds a runnable engine.Engine from a Definition.
// Engine sizing: per-channel queue size of 256 by default — large
// enough to absorb burst-concurrent invocations (N=100+) without
// blocking the worker on the dispatcher's anyRun channel.
func Compose(def Definition, opts ...ComposeOption) (engine.Engine, error) {
	if err := def.Validate(); err != nil {
		return nil, err
	}
	cfg := composeConfig{queueSize: 256}
	for _, opt := range opts {
		opt(&cfg)
	}
	nodes := make(map[NodeID]engine.Node, len(def.Nodes))
	for id, spec := range def.Nodes {
		name := spec.Name
		if name == "" {
			name = string(id)
		}
		nodes[id] = engine.Node{
			Name:   name,
			Func:   spec.Func,
			Policy: spec.Policy,
		}
	}
	adjs := make([]engine.Adjacency, 0, len(def.Nodes))
	for id, spec := range def.Nodes {
		from := nodes[id]
		var to []engine.Node
		for _, t := range spec.To {
			to = append(to, nodes[t])
		}
		adjs = append(adjs, engine.Adjacency{From: from, To: to})
	}
	return engine.New(adjs, engine.WithQueueSize(cfg.queueSize))
}

// RegisterAsTool wires a composed Engine into the Tool catalog
// with `Transport: TransportFlow`.
func RegisterAsTool(cat tools.ToolCatalog, def Definition, eng engine.Engine) (tools.Tool, error) {
	if cat == nil {
		return tools.Tool{}, fmt.Errorf("flow.RegisterAsTool: catalog is nil")
	}
	if eng == nil {
		return tools.Tool{}, fmt.Errorf("flow.RegisterAsTool: engine is nil")
	}
	if err := def.Validate(); err != nil {
		return tools.Tool{}, err
	}
	inSchema := def.InSchema
	if len(inSchema) == 0 {
		inSchema = json.RawMessage(`{}`)
	}
	outSchema := def.OutSchema
	if len(outSchema) == 0 {
		outSchema = json.RawMessage(`{}`)
	}

	tool := tools.Tool{
		Name:        def.Name,
		Description: def.Description,
		ArgsSchema:  inSchema,
		OutSchema:   outSchema,
		SideEffects: tools.SideEffectStateful,
		Loading:     tools.LoadingAlways,
		Transport:   tools.TransportFlow,
		// Per Phase 26 plan line 31: "No double-wrapping" — the outer
		// ToolPolicy provides timeout + validation around the flow,
		// but retries are handled INSIDE the engine by per-node
		// NodePolicy. Retrying a flow at the tool layer is also
		// semantically wrong for budget-exceeded outcomes (the same
		// budget would re-exhaust on the retry). Default policy here:
		// 30s timeout, validate-none (Flow's per-node validators run
		// inside the engine), empty RetryOn (no retries on any class).
		Policy: tools.ToolPolicy{
			TimeoutMS: 30000,
			RetryOn:   []tools.ErrorClass{},
			Validate:  tools.ValidateNone,
		},
	}

	descriptor := tools.ToolDescriptor{
		Tool: tool,
		Validate: func(args json.RawMessage) error {
			return nil
		},
		Invoke: func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			// D-024 / Phase 26 plan line 31: the OUTER flow
			// invocation wraps in ToolPolicy regardless of transport;
			// per-node NodePolicy lives INSIDE the engine. No
			// double-wrapping at either layer.
			return tools.RunWithPolicy(ctx, args,
				func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
					return invokeFlow(ctx, def, eng, args)
				},
				nil, nil, tool.Policy)
		},
	}

	if err := cat.Register(descriptor); err != nil {
		return tools.Tool{}, err
	}
	return tool, nil
}

// invokeFlow is the inner-most flow execution.
func invokeFlow(ctx context.Context, def Definition, eng engine.Engine, args json.RawMessage) (tools.ToolResult, error) {
	parent := budgetFromCtx(ctx)
	acc := newBudgetAccumulator(def.Budget, parent)

	flowCtx := ctx
	var cancelFn context.CancelFunc
	if acc.deadline.IsZero() {
		flowCtx, cancelFn = context.WithCancel(ctx)
	} else {
		flowCtx, cancelFn = context.WithDeadline(ctx, acc.deadline)
	}
	defer cancelFn()

	q, ok := identityQuadrupleFromCtx(flowCtx)
	if !ok {
		return tools.ToolResult{}, fmt.Errorf("flow: identity required on ctx")
	}

	env := messages.Envelope{
		Payload:   args,
		Headers:   messages.Headers{TenantID: q.TenantID, UserID: q.UserID},
		SessionID: q.SessionID,
		RunID:     q.RunID,
		Timestamp: time.Now(),
	}

	if err := eng.Emit(flowCtx, env); err != nil {
		if errors.Is(err, context.DeadlineExceeded) && !acc.deadline.IsZero() {
			emitBudgetExceeded(ctx, def.Name, q, "deadline")
			return tools.ToolResult{}, wrap(ErrFlowBudgetExceeded,
				"flow %q deadline exceeded during emit", def.Name)
		}
		return tools.ToolResult{}, fmt.Errorf("flow %q: emit: %w", def.Name, err)
	}

	out, err := eng.FetchByRun(flowCtx, q.RunID)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && !acc.deadline.IsZero() {
			emitBudgetExceeded(ctx, def.Name, q, "deadline")
			return tools.ToolResult{}, wrap(ErrFlowBudgetExceeded,
				"flow %q deadline exceeded during fetch", def.Name)
		}
		return tools.ToolResult{}, fmt.Errorf("flow %q: fetch: %w", def.Name, err)
	}

	if !acc.tryHop(1) {
		emitBudgetExceeded(ctx, def.Name, q, "hop_budget")
		return tools.ToolResult{}, wrap(ErrFlowBudgetExceeded,
			"flow %q hop budget exceeded", def.Name)
	}

	return tools.ToolResult{Value: out.Payload}, nil
}

// budgetCtxKey is the unexported key under which a parent Budget
// is propagated on ctx.
type budgetCtxKey struct{}

// WithBudget attaches a parent Budget to ctx.
func WithBudget(ctx context.Context, b Budget) context.Context {
	return context.WithValue(ctx, budgetCtxKey{}, b)
}

func budgetFromCtx(ctx context.Context) Budget {
	if b, ok := ctx.Value(budgetCtxKey{}).(Budget); ok {
		return b
	}
	return Budget{}
}

func identityQuadrupleFromCtx(ctx context.Context) (identity.Quadruple, bool) {
	if q, ok := identity.QuadrupleFrom(ctx); ok {
		if q.RunID == "" {
			q.RunID = newRunID()
		}
		return q, true
	}
	if id, ok := identity.From(ctx); ok {
		return identity.Quadruple{Identity: id, RunID: newRunID()}, true
	}
	return identity.Quadruple{}, false
}

func newRunID() string {
	now := time.Now().UnixNano()
	seq := runIDSeq.Add(1)
	return fmt.Sprintf("flow-%d-%d", now, seq)
}

var runIDSeq atomic.Uint64

// budgetAccumulator is per-invocation state carrying the
// resolved Budget caps.
type budgetAccumulator struct {
	deadline      time.Time
	hopsRemaining atomic.Int64
	costRemaining atomic.Int64
	costEnabled   bool
}

func newBudgetAccumulator(self, parent Budget) *budgetAccumulator {
	acc := &budgetAccumulator{}
	deadline := combineDuration(self.Deadline, parent.Deadline)
	if deadline > 0 {
		acc.deadline = time.Now().Add(deadline)
	}
	hops := combineInt(self.HopBudget, parent.HopBudget)
	if hops > 0 {
		acc.hopsRemaining.Store(int64(hops))
	} else {
		acc.hopsRemaining.Store(maxInt64)
	}
	cost := combineFloat(self.CostCap, parent.CostCap)
	if cost > 0 {
		acc.costEnabled = true
		acc.costRemaining.Store(int64(cost * 1e6))
	}
	return acc
}

const maxInt64 = int64(1<<63 - 1)

func combineDuration(a, b time.Duration) time.Duration {
	if a == 0 {
		return b
	}
	if b == 0 {
		return a
	}
	if a < b {
		return a
	}
	return b
}

func combineInt(a, b int) int {
	if a == 0 {
		return b
	}
	if b == 0 {
		return a
	}
	if a < b {
		return a
	}
	return b
}

func combineFloat(a, b float64) float64 {
	if a == 0 {
		return b
	}
	if b == 0 {
		return a
	}
	if a < b {
		return a
	}
	return b
}

func (a *budgetAccumulator) tryHop(n int) bool {
	next := a.hopsRemaining.Add(-int64(n))
	return next >= 0
}

func (a *budgetAccumulator) tryCost(usd float64) bool {
	if !a.costEnabled {
		return true
	}
	microcents := int64(usd * 1e6)
	next := a.costRemaining.Add(-microcents)
	return next >= 0
}

func emitBudgetExceeded(ctx context.Context, flowName string, q identity.Quadruple, axis string) {
	bus, ok := events.From(ctx)
	if !ok {
		return
	}
	ev := events.Event{
		Type:     EventTypeFlowBudgetExceeded,
		Identity: q,
		Payload: BudgetExceededPayload{
			FlowName: flowName,
			Axis:     axis,
		},
	}
	_ = bus.Publish(ctx, ev)
}

func wrap(sentinel error, format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{sentinel}, args...)...)
}

var (
	ErrFlowBudgetExceeded    = errors.New("flow: budget exceeded")
	ErrFlowInvalidDefinition = errors.New("flow: invalid definition")
	ErrFlowEntryExitMismatch = errors.New("flow: entry/exit node not in graph")
)

// WithSchemasFrom annotates a Definition with reflection-derived
// InSchema / OutSchema from the given Go input/output types.
func WithSchemasFrom[I any, O any](def Definition) (Definition, error) {
	var zeroIn I
	var zeroOut O
	inSchema, err := inproc.DeriveSchema(reflect.TypeOf(zeroIn))
	if err != nil {
		return def, fmt.Errorf("flow: derive input schema: %w", err)
	}
	outSchema, err := inproc.DeriveSchema(reflect.TypeOf(zeroOut))
	if err != nil {
		return def, fmt.Errorf("flow: derive output schema: %w", err)
	}
	def.InSchema, err = json.Marshal(inSchema)
	if err != nil {
		return def, fmt.Errorf("flow: marshal input schema: %w", err)
	}
	def.OutSchema, err = json.Marshal(outSchema)
	if err != nil {
		return def, fmt.Errorf("flow: marshal output schema: %w", err)
	}
	return def, nil
}
