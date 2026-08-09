package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// SessionReachClaim is the signed JWT claim carrying the closed set of
// session IDs a bearer may select per request. It is an OPTIONAL
// entitlement that narrows the dynamic per-request session selection to
// a bounded set; it is never an isolation principal, never a storage
// filter, and never part of a request body.
const SessionReachClaim = "session_reach"

// The bounded-array limits mirror the established agent_reach
// conventions: a strict unique array of at most 128 nonblank
// strings, each at most 128 bytes. No repository authority artifact
// requires a narrower bound for session IDs, so the same ceiling
// applies.
const (
	maxSessionReachIDs     = 128
	maxSessionReachIDBytes = 128
)

var (
	// ErrSessionReachMalformed rejects a syntactically valid JWT whose
	// signed session_reach claim is not the strict bounded string-array
	// shape (unique, nonblank, bounded session IDs).
	ErrSessionReachMalformed = errors.New("auth: session reach claim malformed")
	// ErrSessionReachDenied reports an authenticated caller whose
	// effective session is outside its signed reach (or whose reach was
	// explicitly empty).
	ErrSessionReachDenied = errors.New("auth: session reach denied")
)

type sessionReachKey struct{}

// SessionReachAuthorizer is the one shared authorization gate for an
// already-resolved effective session. It is immutable and safe for
// concurrent use; request-specific authority is read only from ctx.
type SessionReachAuthorizer interface {
	AuthorizeSessionReach(ctx context.Context, effectiveSessionID string) error
}

// SessionReachGate is Harbor's canonical signed-session-reach gate.
type SessionReachGate struct{}

// NewSessionReachAuthorizer constructs the stateless shared gate.
func NewSessionReachAuthorizer() *SessionReachGate { return &SessionReachGate{} }

// WithSessionReach seats a verified reach set on ctx. Middleware calls
// it only after JWT validation (and only when the claim is present);
// tests may use it to construct an explicitly trusted in-process
// request. The copy prevents caller mutation after attachment.
func WithSessionReach(ctx context.Context, reach []string) context.Context {
	copyReach := append([]string(nil), reach...)
	return context.WithValue(ctx, sessionReachKey{}, copyReach)
}

// SessionReachFrom returns a defensive copy of the verified reach set.
// Presence distinguishes an authenticated token with an explicitly empty
// reach from a context that never carried the claim — only the former
// denies every session (the latter preserves dynamic per-request
// selection).
func SessionReachFrom(ctx context.Context) ([]string, bool) {
	reach, ok := ctx.Value(sessionReachKey{}).([]string)
	if !ok {
		return nil, false
	}
	return append([]string(nil), reach...), true
}

// AuthorizeSessionReach enforces a PRESENT session_reach claim: the
// effective session must be a nonblank member of the verified signed
// reach set. An absent claim (no authority on ctx) is NOT a denial — it
// preserves the dynamic per-request session selection exactly. An
// explicitly empty present claim grants no session and denies every
// request, and a missing context authority covers carrier-identity and
// direct calls that did not establish a JWT claim.
func (*SessionReachGate) AuthorizeSessionReach(ctx context.Context, effectiveSessionID string) error {
	if strings.TrimSpace(effectiveSessionID) == "" {
		return fmt.Errorf("%w: effective session id is empty", ErrSessionReachDenied)
	}
	reach, ok := SessionReachFrom(ctx)
	if !ok {
		// No session_reach claim: dynamic per-request selection preserved.
		return nil
	}
	for _, id := range reach {
		if id == effectiveSessionID {
			return nil
		}
	}
	return ErrSessionReachDenied
}

// ParseSessionReach parses the optional signed claim. Absence is
// preserved as a nil reach (so the dynamic per-request session
// selection remains unchanged); any present value must be a unique,
// nonblank, bounded JSON string array. An explicitly empty array is
// valid and grants no session.
func ParseSessionReach(raw any) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	values, ok := raw.([]any)
	if !ok {
		return nil, ErrSessionReachMalformed
	}
	if len(values) > maxSessionReachIDs {
		return nil, fmt.Errorf("%w: too many session ids", ErrSessionReachMalformed)
	}
	seen := make(map[string]struct{}, len(values))
	reach := make([]string, 0, len(values))
	for _, rawID := range values {
		id, ok := rawID.(string)
		if !ok || strings.TrimSpace(id) == "" || len(id) > maxSessionReachIDBytes {
			return nil, ErrSessionReachMalformed
		}
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("%w: duplicate session id", ErrSessionReachMalformed)
		}
		seen[id] = struct{}{}
		reach = append(reach, id)
	}
	return reach, nil
}

var _ SessionReachAuthorizer = (*SessionReachGate)(nil)
