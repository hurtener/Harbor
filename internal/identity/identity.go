// Package identity defines Harbor's load-bearing isolation key.
//
// Every Runtime, Protocol, Memory, State, Skills, Tools, Planner and
// Governance code path scopes its work by the (TenantID, UserID, SessionID)
// triple. The triple is the isolation boundary; RunID is the per-execution
// scope inside a session and is carried by Quadruple — never substituted
// for Identity in scoping decisions.
//
// Identity is mandatory: there is no opt-out knob (decisions.md).
// Validate fails closed when any component is empty; With and WithRun
// validate at write time so bugs surface at the call site.
//
// # Verified identity and re-scoping
//
// A context carries up to three independent identity values:
//
//   - The VERIFIED identity (WithVerified / FromVerified) — the triple a
//     transport established for the request by a means the caller cannot
//     choose. It is written ONCE, at the request edge, and is the anchor
//     every later authorization decision reconciles against.
//   - The WORKING identity (With / From) — the triple the current unit of
//     work executes under. Internal re-scoping (dispatch, run contexts,
//     steering, pause/resume, spawned and resumed work) rewrites it
//     freely WITHIN the verified tenant.
//   - The run Quadruple (WithRun / QuadrupleFrom) — working identity plus
//     the per-execution RunID.
//
// With refuses to move the working identity to a tenant other than the
// verified one: widening the isolation boundary is an authorization
// decision, and an authorization decision does not belong in a plain
// context write. WithElevated is the one path that crosses the tenant
// boundary, and it demands a reason so the crossing is nameable in an
// audit record. Callers reach WithElevated only after an admin-tier
// scope check; the Protocol's body-scope gate is that check.
//
// This package is dependency-free and holds no package-level mutable state
// beyond four unexported context-key sentinels. Concurrent reuse is safe
// by construction (decisions.md).
package identity

import (
	"context"
	"errors"
	"fmt"
)

// Identity is the load-bearing isolation key. All three components are
// mandatory; an Identity with any empty component is rejected by Validate.
type Identity struct {
	TenantID  string
	UserID    string
	SessionID string
}

// Quadruple is Identity + the per-execution RunID. Used in Envelopes and
// run-scoped state. Quadruple is NOT a substitute for Identity in scoping
// decisions: the triple is the isolation boundary; RunID is the per-execution
// scope inside a session.
type Quadruple struct {
	Identity
	RunID string
}

var (
	// ErrIdentityMissing — the context carries no Identity AND no
	// Quadruple. Both MustFrom and MustQuadrupleFrom panic with
	// this sentinel when their respective key is absent.
	ErrIdentityMissing = errors.New("identity: no Identity or Quadruple in context")
	// ErrIdentityIncomplete — one or more components empty. Identity is mandatory.
	ErrIdentityIncomplete = errors.New("identity: one or more components empty")
	// ErrTenantWidening — a plain With attempted to move the working
	// identity to a tenant other than the ctx-verified one. The
	// isolation boundary is the tenant; moving it is an authorization
	// decision, so it fails closed here and is reachable only through
	// WithElevated.
	ErrTenantWidening = errors.New("identity: re-scoping outside the verified tenant requires an audited elevation")
	// ErrElevationReasonRequired — WithElevated was called with an empty
	// reason. An unnameable elevation cannot be audited, so it is refused.
	ErrElevationReasonRequired = errors.New("identity: elevation requires a non-empty reason")
)

type ctxKey int

const (
	identityKey ctxKey = iota
	quadrupleKey
	verifiedKey
	elevationKey
)

// Validate returns an error wrapping ErrIdentityIncomplete when any of
// (TenantID, UserID, SessionID) is empty. Whitespace-only strings pass;
// the caller is responsible for input normalization.
func Validate(id Identity) error {
	switch {
	case id.TenantID == "" && id.UserID == "" && id.SessionID == "":
		return fmt.Errorf("tenant_id, user_id, session_id all empty: %w", ErrIdentityIncomplete)
	case id.TenantID == "":
		return fmt.Errorf("tenant_id empty: %w", ErrIdentityIncomplete)
	case id.UserID == "":
		return fmt.Errorf("user_id empty: %w", ErrIdentityIncomplete)
	case id.SessionID == "":
		return fmt.Errorf("session_id empty: %w", ErrIdentityIncomplete)
	}
	return nil
}

// With attaches the WORKING Identity to ctx — the triple the current
// unit of work executes under. Returns the original ctx and a wrapped
// ErrIdentityIncomplete if the Identity fails Validate.
//
// When ctx already carries a verified identity (FromVerified), With
// refuses to move the working identity to a DIFFERENT tenant and
// returns a wrapped ErrTenantWidening — unless an audited crossing to
// exactly that tenant is in force (WithElevated). Re-scoping the user,
// the session, or both stays unrestricted — that is ordinary internal
// narrowing (dispatch, run contexts, steering, pause/resume, spawned
// and resumed work), and it never widens the isolation boundary.
//
// A ctx with no verified identity (in-process embedding, a unit test, a
// background worker rooted outside any request) is unrestricted: there
// is no anchor to widen beyond.
func With(ctx context.Context, id Identity) (context.Context, error) {
	if err := Validate(id); err != nil {
		return ctx, err
	}
	if err := checkTenantMove(ctx, id); err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, identityKey, id), nil
}

// WithVerified attaches the VERIFIED Identity to ctx and seats it as the
// working identity in the same call. It is the request-edge write: the
// Protocol's auth middleware calls it once, after the token's claims
// have been validated, and the transports' carrier-header choke point
// calls it once for the deployments that establish identity that way.
//
// The verified identity is the anchor every later authorization
// decision reconciles against — the body-scope gate compares a
// request body's identity scope to it, and With refuses to widen the
// tenant past it. Nothing downstream rewrites it.
//
// Returns the original ctx and a wrapped ErrIdentityIncomplete when the
// Identity fails Validate.
func WithVerified(ctx context.Context, id Identity) (context.Context, error) {
	if err := Validate(id); err != nil {
		return ctx, err
	}
	ctx = context.WithValue(ctx, verifiedKey, id)
	return context.WithValue(ctx, identityKey, id), nil
}

// FromVerified returns the request-edge verified Identity and a presence
// bool. Absence is meaningful and must be handled explicitly: a gate
// that reconciles caller-supplied input against the verified identity
// has nothing to reconcile against and fails closed rather than
// trusting the input.
func FromVerified(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(verifiedKey).(Identity)
	return id, ok
}

// elevation is the record of one audited tenant crossing: the tenant it
// authorized and why. It is deliberately NOT a bare boolean — an
// elevation authorizes ONE named tenant, so a second crossing to a
// DIFFERENT tenant still meets a closed door and still needs its own
// gate and its own record.
type elevation struct {
	tenant string
	reason string
}

// WithElevated attaches a working Identity that MAY sit outside the
// verified tenant, recording reason as the nameable justification. It is
// the one path across the tenant boundary.
//
// Callers reach WithElevated only after an admin-tier scope check has
// passed and the crossing has been recorded; the Protocol's body-scope
// gate is that check, and the set of call sites is held to a reviewed
// list by a lockstep test. An empty reason is refused with
// ErrElevationReasonRequired — an elevation nobody can name is an
// elevation nobody can audit.
//
// The marker rides the returned ctx SCOPED TO id's tenant: work spawned
// under this crossing re-scopes freely within THAT tenant, and a move to
// any other tenant is refused exactly as it would have been without the
// elevation. Authorizing one crossing never authorizes the next.
func WithElevated(ctx context.Context, id Identity, reason string) (context.Context, error) {
	if err := Validate(id); err != nil {
		return ctx, err
	}
	if reason == "" {
		return ctx, ErrElevationReasonRequired
	}
	ctx = context.WithValue(ctx, elevationKey, elevation{tenant: id.TenantID, reason: reason})
	return context.WithValue(ctx, identityKey, id), nil
}

// IsElevated reports whether ctx carries an audited tenant crossing.
// Prefer ElevatedTenant when the answer matters: a crossing authorizes
// one named tenant, not crossings in general.
func IsElevated(ctx context.Context) bool {
	_, ok := ElevatedTenant(ctx)
	return ok
}

// ElevatedTenant returns the tenant an audited crossing on ctx
// authorized, and a presence bool. A gate re-running behind another gate
// compares against this rather than a bare "already elevated" flag, so a
// crossing to a second, different tenant is not silently inherited from
// the first.
func ElevatedTenant(ctx context.Context) (string, bool) {
	e, ok := ctx.Value(elevationKey).(elevation)
	if !ok {
		return "", false
	}
	return e.tenant, true
}

// ElevationReason returns the reason recorded by WithElevated and a
// presence bool.
func ElevationReason(ctx context.Context) (string, bool) {
	e, ok := ctx.Value(elevationKey).(elevation)
	if !ok {
		return "", false
	}
	return e.reason, true
}

// WithRun attaches a Quadruple (Identity + RunID) to ctx. The Identity
// must Validate; the RunID must be non-empty. Returns the original ctx
// and a wrapped ErrIdentityIncomplete on either failure.
//
// The quadruple is read for scoping in its own right (tool provenance,
// per-run capability checks), so it carries the SAME tenant-widening
// rule as With: a quadruple naming a tenant other than the ctx-verified
// one is refused unless an audited crossing to that tenant is in force.
// Attaching a run must not become the seam that moves the isolation
// boundary sideways.
func WithRun(ctx context.Context, id Identity, runID string) (context.Context, error) {
	if err := Validate(id); err != nil {
		return ctx, err
	}
	if runID == "" {
		return ctx, fmt.Errorf("run_id empty: %w", ErrIdentityIncomplete)
	}
	if err := checkTenantMove(ctx, id); err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, quadrupleKey, Quadruple{Identity: id, RunID: runID}), nil
}

// checkTenantMove is the shared tenant-boundary rule for the two writers
// that seat a scoping identity. Moving to a tenant other than the
// ctx-verified one is refused unless an audited crossing to exactly that
// tenant is in force. A ctx with no verified identity has no boundary to
// move.
func checkTenantMove(ctx context.Context, id Identity) error {
	verified, ok := FromVerified(ctx)
	if !ok || id.TenantID == verified.TenantID {
		return nil
	}
	if elevated, ok := ElevatedTenant(ctx); ok && elevated == id.TenantID {
		return nil
	}
	return fmt.Errorf("tenant %q differs from the verified tenant %q: %w",
		id.TenantID, verified.TenantID, ErrTenantWidening)
}

// MustFrom returns the Identity in ctx. Panics with ErrIdentityMissing
// when none is present. Use in handler/runtime paths where the caller
// has already established that identity is mandatory at this point.
func MustFrom(ctx context.Context) Identity {
	id, ok := From(ctx)
	if !ok {
		panic(ErrIdentityMissing)
	}
	return id
}

// From returns the Identity in ctx and a presence bool. Use when absence
// is recoverable (e.g. cross-cutting middleware that may run pre-auth).
func From(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey).(Identity)
	return id, ok
}

// MustQuadrupleFrom returns the Quadruple in ctx. Panics with
// ErrIdentityMissing when none is present. The Quadruple key is
// independent from the Identity key: a context attached via With does
// NOT satisfy MustQuadrupleFrom, and vice versa.
func MustQuadrupleFrom(ctx context.Context) Quadruple {
	q, ok := QuadrupleFrom(ctx)
	if !ok {
		panic(ErrIdentityMissing)
	}
	return q
}

// QuadrupleFrom returns the Quadruple in ctx and a presence bool.
func QuadrupleFrom(ctx context.Context) (Quadruple, bool) {
	q, ok := ctx.Value(quadrupleKey).(Quadruple)
	return q, ok
}
