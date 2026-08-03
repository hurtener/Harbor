// agent_provenance.go — the ctx-carried acting-agent provenance seam.
//
// Southbound tool transports (the MCP driver today; the interactive-OAuth
// and HTTP paths later) stamp the acting agent's registration id alongside
// the isolation triple so a shared downstream server can attribute a call
// per agent. This file is the single carrier: the run loop stamps the run's
// agent-config registration id at run start, and the transport reads it at
// dispatch.
//
// PROVENANCE, NEVER AN ISOLATION PRINCIPAL. The isolation boundary is and
// stays (tenant, user, session) — the agent id is attribution metadata only.
// Nothing Harbor-side keys storage, event filters, memory/state drivers, or
// token caches by it, and a downstream server MUST NOT treat it as an
// isolation filter. Absence is valid: a bare embedder run (no agent-config
// registration) carries no agent id, and the transport simply omits the
// provenance stamp.
package tools

import "context"

// agentCtxKey is the unexported key under which the acting agent's
// registration id is propagated on a context. Independent from the catalog /
// identity / events / audit ctx keys.
type agentCtxKey int

const (
	invokingAgentCtxKey agentCtxKey = iota
	effectiveAgentConfigCtxKey
)

// WithInvokingAgent returns a child ctx carrying agentID as the acting
// agent's registration id (RFC §6.16 registration identity — provenance, not
// an isolation principal). An empty agentID is a no-op: the returned ctx
// carries no provenance, so a bare-embedder run (no agent-config id) never
// stamps an empty agent id downstream.
func WithInvokingAgent(ctx context.Context, agentID string) context.Context {
	if agentID == "" {
		return ctx
	}
	return context.WithValue(ctx, invokingAgentCtxKey, agentID)
}

// InvokingAgentFrom returns the acting agent's registration id carried on ctx
// and a presence bool. The bool is false (and the string empty) when no agent
// provenance was stamped — the valid, common case for a bare-embedder run.
// Callers stamp the value as attribution metadata only; it is never an
// isolation filter (see the package-level note).
func InvokingAgentFrom(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	v, ok := ctx.Value(invokingAgentCtxKey).(string)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// WithEffectiveAgentConfig carries the effective agent-config selection that
// admitted a run. The control surface establishes that selection only after
// the verified caller's signed agent_reach authorizes it; the run loop then
// carries it to pair-owned credentials. It is distinct from InvokingAgent:
// the latter is boot-derived southbound attribution, while this value binds a
// run to the agent configuration it is permitted to consume.
//
// This is an internal execution capability, never an isolation principal or a
// Protocol field. An empty id carries no authority, so pair-owned credentials
// fail closed for bare or incompletely admitted calls.
func WithEffectiveAgentConfig(ctx context.Context, agentID string) context.Context {
	if agentID == "" {
		return ctx
	}
	return context.WithValue(ctx, effectiveAgentConfigCtxKey, agentID)
}

// EffectiveAgentConfigFrom returns the run's reach-admitted agent-config
// selection. Absence means the caller has no runtime admission capability.
func EffectiveAgentConfigFrom(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	v, ok := ctx.Value(effectiveAgentConfigCtxKey).(string)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}
