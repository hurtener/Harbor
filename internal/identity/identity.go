// Package identity defines Harbor's load-bearing isolation key.
//
// Every Runtime, Protocol, Memory, State, Skills, Tools, Planner and
// Governance code path scopes its work by the (TenantID, UserID, SessionID)
// triple. The triple is the isolation boundary; RunID is the per-execution
// scope inside a session and is carried by Quadruple — never substituted
// for Identity in scoping decisions.
//
// Identity is mandatory: there is no opt-out knob (decisions.md D-001).
// Validate fails closed when any component is empty; With and WithRun
// validate at write time so bugs surface at the call site.
//
// This package is dependency-free and holds no package-level mutable state
// beyond two unexported context-key sentinels. Concurrent reuse is safe
// by construction (decisions.md D-025).
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
	// ErrIdentityMissing — the context carries no Identity (or no Quadruple).
	ErrIdentityMissing = errors.New("identity: no Identity in context")
	// ErrIdentityIncomplete — one or more components empty. Identity is mandatory.
	ErrIdentityIncomplete = errors.New("identity: one or more components empty")
)

type ctxKey int

const (
	identityKey ctxKey = iota
	quadrupleKey
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

// With attaches Identity to ctx. Returns the original ctx and a wrapped
// ErrIdentityIncomplete if the Identity fails Validate.
func With(ctx context.Context, id Identity) (context.Context, error) {
	if err := Validate(id); err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, identityKey, id), nil
}

// WithRun attaches a Quadruple (Identity + RunID) to ctx. The Identity
// must Validate; the RunID must be non-empty. Returns the original ctx
// and a wrapped ErrIdentityIncomplete on either failure.
func WithRun(ctx context.Context, id Identity, runID string) (context.Context, error) {
	if err := Validate(id); err != nil {
		return ctx, err
	}
	if runID == "" {
		return ctx, fmt.Errorf("run_id empty: %w", ErrIdentityIncomplete)
	}
	return context.WithValue(ctx, quadrupleKey, Quadruple{Identity: id, RunID: runID}), nil
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
