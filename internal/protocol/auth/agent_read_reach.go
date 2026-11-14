package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// AgentReadReachClaim is the optional signed JWT claim carrying agent
// registration IDs whose rows a bearer may READ on the session history
// surfaces, without being able to select those agents. It lets an issuer hand a
// person the history of their own session after the session moved to another
// agent, or after the agent stopped being available to them, while start,
// App tool calls and every other agent-addressed write still require
// `agent_reach`. D-510 extends it to App resource reads and tool listings.
// Steering is authorized by run authorship, never by reach.
//
// It is an entitlement, never an isolation principal: the exact-session
// identity boundary is unchanged, and a bearer carrying a non-empty read reach
// must also carry a non-empty `session_reach` (the history it unlocks is pinned
// to named sessions).
const AgentReadReachClaim = "agent_read_reach"

// ErrAgentReadReachMalformed rejects a token whose signed agent_read_reach
// claim is not the strict bounded string-array shape, or that carries it
// without a non-empty session_reach.
var ErrAgentReadReachMalformed = errors.New("auth: agent read reach claim malformed")

type agentReadReachKey struct{}

// WithAgentReadReach seats a verified read-reach set on ctx. Middleware calls
// it only after JWT validation. The copy prevents caller mutation.
func WithAgentReadReach(ctx context.Context, reach []string) context.Context {
	return context.WithValue(ctx, agentReadReachKey{}, append([]string(nil), reach...))
}

// AgentReadReachFrom returns a defensive copy of the verified read-reach set.
func AgentReadReachFrom(ctx context.Context) ([]string, bool) {
	reach, ok := ctx.Value(agentReadReachKey{}).([]string)
	if !ok {
		return nil, false
	}
	return append([]string(nil), reach...), true
}

// AuthorizeAgentRead permits READING a named agent's rows when the agent is in
// the verified agent_read_reach set. It never authorizes selecting the agent;
// read surfaces call it only after the ordinary agent-reach gate denied.
func AuthorizeAgentRead(ctx context.Context, agentID string) error {
	if strings.TrimSpace(agentID) == "" {
		return fmt.Errorf("%w: agent id is empty", ErrAgentReachDenied)
	}
	reach, ok := AgentReadReachFrom(ctx)
	if !ok {
		return ErrAgentReachDenied
	}
	for _, id := range reach {
		if id == agentID {
			return nil
		}
	}
	return ErrAgentReachDenied
}

// ParseAgentReadReach parses the optional claim with the same strict bounded
// shape as agent_reach. Absence is preserved as nil.
func ParseAgentReadReach(raw any) ([]string, error) {
	reach, err := ParseAgentReach(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrAgentReadReachMalformed, err)
	}
	return reach, nil
}

type agentReadAdmissionKey struct{}

// WithAgentReadAdmission records that a READ-ONLY Protocol surface admitted
// agentID through agent_read_reach (D-510). Only the read-admission helper in
// the protocol package (reads and own-session App callbacks) and the
// live-profile restore seat it, after the verified read reach authorized the
// effective agent; downstream source visibility may then honour the same read
// reach for that one agent. No other write surface seats it.
func WithAgentReadAdmission(ctx context.Context, agentID string) context.Context {
	return context.WithValue(ctx, agentReadAdmissionKey{}, agentID)
}

// AuthorizeAdmittedAgentRead permits agentID only when a read-only surface
// admitted exactly that agent on ctx AND the verified agent_read_reach still
// names it. Absent the read admission it denies, so a write path that reaches
// the same downstream gate keeps requiring agent_reach.
func AuthorizeAdmittedAgentRead(ctx context.Context, agentID string) error {
	admitted, ok := ctx.Value(agentReadAdmissionKey{}).(string)
	if !ok || admitted == "" || admitted != agentID {
		return ErrAgentReachDenied
	}
	return AuthorizeAgentRead(ctx, agentID)
}

// AgentReadSessionAppsClaim is the optional signed boolean that opts the
// agent_read_reach agents into USE inside the caller's own session (D-510):
// render-admission App callbacks for Apps a turn of that session recorded,
// and the caller's own live-profile restore. Absent or false keeps read reach
// strictly read-only (history, App documents, tool listings). An issuer sets
// it only on bearers whose read reach names agents the person may still use.
const AgentReadSessionAppsClaim = "agent_read_session_apps"

type agentReadSessionAppsKey struct{}

// ParseAgentReadSessionApps parses the optional claim. It must be a JSON
// boolean, and true requires a non-empty agent_read_reach.
func ParseAgentReadSessionApps(raw any, readReach []string) (bool, error) {
	if raw == nil {
		return false, nil
	}
	enabled, ok := raw.(bool)
	if !ok || (enabled && len(readReach) == 0) {
		return false, fmt.Errorf("%w: %s must be a boolean with a non-empty %s", ErrAgentReadReachMalformed, AgentReadSessionAppsClaim, AgentReadReachClaim)
	}
	return enabled, nil
}

// WithAgentReadSessionApps seats the verified opt-in. Middleware only.
func WithAgentReadSessionApps(ctx context.Context, enabled bool) context.Context {
	return context.WithValue(ctx, agentReadSessionAppsKey{}, enabled)
}

// AgentReadSessionAppsAllowed reports the verified opt-in. Absent is false.
func AgentReadSessionAppsAllowed(ctx context.Context) bool {
	enabled, ok := ctx.Value(agentReadSessionAppsKey{}).(bool)
	if !ok {
		return false
	}
	return enabled
}
