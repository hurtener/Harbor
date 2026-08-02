package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// AgentReachClaim is the signed JWT claim carrying the closed set of agent
// registration IDs a bearer may select. It is an entitlement, never an
// isolation principal.
const AgentReachClaim = "agent_reach"

const (
	maxAgentReachIDs     = 128
	maxAgentReachIDBytes = 128
)

var (
	// ErrAgentReachMalformed rejects a syntactically valid JWT whose signed
	// agent_reach claim is not the strict bounded string-array shape.
	ErrAgentReachMalformed = errors.New("auth: agent reach claim malformed")
	// ErrAgentReachDenied reports an authenticated caller that selected an
	// agent outside its signed reach (or whose reach was absent/empty).
	ErrAgentReachDenied = errors.New("auth: agent reach denied")
)

type agentReachKey struct{}

// AgentReachAuthorizer is the one shared authorization gate for an already
// resolved effective agent target. It is immutable and safe for concurrent
// use; request-specific authority is read only from ctx.
type AgentReachAuthorizer interface {
	AuthorizeAgentReach(ctx context.Context, effectiveAgentID string) error
}

// ReachAuthorizer is Harbor's canonical signed-agent-reach gate.
type ReachAuthorizer struct{}

// NewAgentReachAuthorizer constructs the stateless shared gate.
func NewAgentReachAuthorizer() *ReachAuthorizer { return &ReachAuthorizer{} }

// WithAgentReach seats a verified reach set on ctx. Middleware calls it only
// after JWT validation; tests may use it to construct an explicitly trusted
// in-process request. The copy prevents caller mutation after attachment.
func WithAgentReach(ctx context.Context, reach []string) context.Context {
	copyReach := append([]string(nil), reach...)
	return context.WithValue(ctx, agentReachKey{}, copyReach)
}

// AgentReachFrom returns a defensive copy of the verified reach set. Presence
// distinguishes an authenticated token with an empty reach from a context
// that never passed verified authority through the middleware.
func AgentReachFrom(ctx context.Context) ([]string, bool) {
	reach, ok := ctx.Value(agentReachKey{}).([]string)
	if !ok {
		return nil, false
	}
	return append([]string(nil), reach...), true
}

// AuthorizeAgentReach permits only a nonblank effective target explicitly in
// the verified signed reach set. Missing context authority fails closed, which
// covers carrier-identity and direct calls that did not establish a JWT claim.
func (*ReachAuthorizer) AuthorizeAgentReach(ctx context.Context, effectiveAgentID string) error {
	if strings.TrimSpace(effectiveAgentID) == "" {
		return fmt.Errorf("%w: effective agent id is empty", ErrAgentReachDenied)
	}
	reach, ok := AgentReachFrom(ctx)
	if !ok {
		return ErrAgentReachDenied
	}
	for _, id := range reach {
		if id == effectiveAgentID {
			return nil
		}
	}
	return ErrAgentReachDenied
}

// ParseAgentReach parses the optional signed claim. Absence is preserved as a
// nil reach (so unrelated methods remain backward compatible); any present
// value must be a unique, nonblank, bounded JSON string array.
func ParseAgentReach(raw any) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	values, ok := raw.([]any)
	if !ok {
		return nil, ErrAgentReachMalformed
	}
	if len(values) > maxAgentReachIDs {
		return nil, fmt.Errorf("%w: too many ids", ErrAgentReachMalformed)
	}
	seen := make(map[string]struct{}, len(values))
	reach := make([]string, 0, len(values))
	for _, rawID := range values {
		id, ok := rawID.(string)
		if !ok || strings.TrimSpace(id) == "" || len(id) > maxAgentReachIDBytes {
			return nil, ErrAgentReachMalformed
		}
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("%w: duplicate id", ErrAgentReachMalformed)
		}
		seen[id] = struct{}{}
		reach = append(reach, id)
	}
	return reach, nil
}

var _ AgentReachAuthorizer = (*ReachAuthorizer)(nil)
