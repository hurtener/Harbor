package harbortest

import "context"

// Agent is the unit of code a test author exercises via RunOnce.
// Implementations are typically thin wrappers around the test
// author's production code path — a planner step, a flow, a
// hand-rolled function — anything that consumes a Harbor identity
// context and produces an output value.
//
// The signature is intentionally narrower than the engine's full
// runtime surface: most test authors do not own engine graphs and
// should not be forced to construct them just to exercise a tool
// call. Agents that DO want full runtime semantics can construct
// an engine internally and Run it inside the Agent body — the
// kit's identity context flows through.
//
// Identity propagation. The ctx passed to Run carries the identity
// quadruple (TenantID, UserID, SessionID, RunID) via the
// internal/identity helpers; Agents that need it use
// identity.MustQuadrupleFrom(ctx). Tool drivers and bus publishers
// automatically attach the triple to events emitted during the run.
type Agent interface {
	// Run executes the agent's logic against input and returns the
	// produced output (or an error). The captured EventLog is built
	// from the events the agent's interior publishes against the
	// kit's event bus — the Agent does not return the log directly.
	Run(ctx context.Context, input any) (output any, err error)
}

// AgentFunc adapts a plain function into an Agent. Use this when
// the test author's code is a top-level function rather than a
// method on a typed receiver.
type AgentFunc func(ctx context.Context, input any) (any, error)

// Run implements Agent for AgentFunc.
func (f AgentFunc) Run(ctx context.Context, input any) (any, error) {
	return f(ctx, input)
}
