// Package messages defines Harbor's wire-shaped runtime types: the
// Envelope every channel carries, the Headers it routes by, and the
// helpers (WithRunID, MergeMeta, Identity) downstream phases lean on.
//
// Phase 09 (RFC §6.1) ships only the types and pure helpers. The
// engine itself lands in Phase 10; the bus-emit hooks, reliability
// shell, streaming primitive, cancellation, and routers all layer on
// top of these envelopes without changing them.
//
// Identity is the runtime concurrency quadruple
// `(TenantID, UserID, SessionID, RunID)`. RunID is Harbor's term for
// what predecessors call `trace_id`; TraceID is reserved for OTel-
// style traces (which may span multiple runs) and is carried by ctx
// via internal/telemetry — NOT by the Envelope (RFC §6.1, brief 01).
//
// Concurrent reuse contract: this package is types-only. There are no
// compiled artifacts, no goroutines, no shared mutable state — D-025
// trivially holds. The pre-merge checkbox documents this as N/A.
package messages

import (
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// Envelope is the canonical message shape on every Harbor channel.
// Carries the identity quadruple (Tenant, User, Session, Run) plus
// timing and free-form Meta. RunID is Harbor's runtime concurrency
// boundary; TraceID is reserved for OpenTelemetry traces and lives
// outside the Envelope (carried by ctx via internal/telemetry).
//
// Empty fields pass through unchanged at this layer; the engine
// (Phase 10) enforces non-empty identity at the API boundary.
type Envelope struct {
	Payload    any            `json:"payload"`
	Headers    Headers        `json:"headers"`
	RunID      string         `json:"run_id"`
	SessionID  string         `json:"session_id"`
	Timestamp  time.Time      `json:"timestamp"`
	DeadlineAt *time.Time     `json:"deadline_at,omitempty"`
	Meta       map[string]any `json:"meta,omitempty"`
}

// Headers carries routing + identity. TenantID and UserID complete
// the triple Headers also names, alongside Topic for routing and
// Priority for ordering. The runtime layer reads Tenant/User from
// Headers; SessionID and RunID live on Envelope directly so they're
// not buried in routing metadata.
type Headers struct {
	TenantID string `json:"tenant_id"`
	UserID   string `json:"user_id"`
	Topic    string `json:"topic,omitempty"`
	Priority int    `json:"priority,omitempty"`
}

// WithRunID returns a copy of e with RunID replaced. Never mutates
// the receiver — Envelopes are values that flow through channels, not
// shared state. Aligns with D-025: per-run state lives in the value
// passed along the channel, never on a shared artifact.
//
// Meta is shallow-copied so the returned envelope can be mutated
// independently of the original (a node that calls WithRunID and then
// adds a Meta key must not bleed back into the source envelope).
func (e Envelope) WithRunID(id string) Envelope {
	out := e
	out.RunID = id
	if e.Meta != nil {
		out.Meta = make(map[string]any, len(e.Meta))
		for k, v := range e.Meta {
			out.Meta[k] = v
		}
	}
	return out
}

// Identity returns the identity.Quadruple derived from the Envelope's
// (Headers.TenantID, Headers.UserID, SessionID, RunID). Empty fields
// pass through unchanged — the engine (Phase 10) enforces non-empty
// at the API boundary; this helper itself never panics so callers can
// inspect partial envelopes (e.g. in tests, in the JSON unmarshal
// path) without ceremony.
func (e Envelope) Identity() identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  e.Headers.TenantID,
			UserID:    e.Headers.UserID,
			SessionID: e.SessionID,
		},
		RunID: e.RunID,
	}
}

// MergeMeta applies last-write-wins semantics: keys from src overwrite
// keys in dst. The result is dst mutated in place AND returned (the
// return is for fluent-style use; the mutation is the canonical
// effect). An explicit merge-function registry for non-LWW semantics
// is reserved for a future RFC follow-up — V1 documents the rule and
// ships the simplest correct implementation.
//
// Edge cases:
//   - nil src returns dst unchanged (including nil).
//   - nil dst returns a fresh shallow copy of src so the caller owns
//     the result; mutating it doesn't affect src.
func MergeMeta(dst, src map[string]any) map[string]any {
	if src == nil {
		return dst
	}
	if dst == nil {
		out := make(map[string]any, len(src))
		for k, v := range src {
			out[k] = v
		}
		return out
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
