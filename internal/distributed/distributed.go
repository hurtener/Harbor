// Package distributed defines Harbor's V1 distributed-edge contracts:
//
//   - `MessageBus` — the at-least-once cross-worker fan-out edge.
//     `Publish` is the canonical publication primitive. V1 ships an
//     in-process loopback driver (`internal/distributed/drivers/loopback`);
//     durable backends (NATS / Redis Streams / Postgres-as-queue) land in
//     post-V1 phase 86.
//
//   - `RemoteTransport` — the cross-process / cross-host call surface,
//     designed end-to-end against the full A2A v1 spec (vendored at
//     `docs/specifications/a2a.proto`). Every A2A RPC maps to a Go
//     method here; every A2A message type has a counterpart in
//     `internal/distributed/a2a`. V1 ships an in-process loopback
//     driver; the actual A2A wire driver lands in Phase 29 (southbound).
//
// Both surfaces follow the §4.4 driver-registry seam: an interface here,
// concrete drivers under `drivers/<name>/`, a factory + registry that
// dispatches by name, drivers self-registering from `init()`.
//
// Identity contract. `MessageBus.Publish` rejects envelopes whose
// `Identity` quadruple is missing any of (tenant, user, session) with
// `ErrIdentityRequired`. `RemoteTransport` methods rely on
// `identity.Quadruple` being present in `ctx`; missing identity surfaces
// as `ErrIdentityRequired` at the driver boundary (caller-side check
// before reaching the wire). The contracts NEVER carry an identity-
// downgrading knob — identity is mandatory at every distributed edge
// (per AGENTS.md §6 + §13).
//
// At-least-once contract. `MessageBus.Publish` is documented at-least-
// once. Consumers MUST be idempotent on `(TaskID, Edge, EventID)`. The
// in-process loopback driver delivers exactly-once today; future
// durable drivers WILL produce duplicates under partition + retry.
// Pinning the contract at at-least-once from t=0 means consumers
// that work against loopback also work against post-V1 drivers — this
// is intentional API hardening per D-031.
package distributed

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"
)

// BusEnvelope is the unit `MessageBus.Publish` accepts. It carries
// identity quadruple, task ID, edge / source / target labels, an
// opaque (caller-redacted) payload, free-form headers + metadata, a
// caller-supplied event ID for idempotency, and a wall-clock timestamp.
//
// Per AGENTS.md §6, the `Identity` triple (tenant / user / session) is
// mandatory; `Publish` rejects empty triples with `ErrIdentityRequired`.
// `RunID` (the fourth component) is optional — set when the envelope
// originates inside a run scope.
type BusEnvelope struct {
	// Edge labels the conceptual fan-out edge ("planner.next",
	// "tool.dispatch.completed", ...). Free-form; consumers use it
	// for routing + idempotency keying.
	Edge string
	// Source labels the originating subsystem ("planner", "tools", ...).
	Source string
	// Target labels the destination class ("memory", "audit", ...).
	// Empty when the envelope is a broadcast.
	Target string
	// Identity is the load-bearing isolation key. The triple
	// (tenant, user, session) is mandatory.
	Identity identity.Quadruple
	// TaskID associates the envelope with a Harbor `tasks.Task`. Empty
	// when the envelope is not task-scoped (rare; most distributed
	// edges fire inside a task).
	TaskID tasks.TaskID
	// EventID is the caller-supplied idempotency key. Consumers dedupe
	// on `(TaskID, Edge, EventID)`. ULID-shaped; callers SHOULD use
	// `events.NewEventID` (or a state-store equivalent) to generate.
	EventID events.EventID
	// Payload is the redacted bytes the consumer will see. Phase 22's
	// loopback driver passes the bytes through verbatim; durable
	// drivers may compress / encrypt. Caller-side redaction (D-020)
	// is mandatory BEFORE Publish.
	Payload json.RawMessage
	// Headers carry transport-level key/value pairs (trace context,
	// tenant overrides for fan-out scopes, ...). Free-form.
	Headers map[string]string
	// Meta carries free-form metadata (mostly for tests / debugging).
	// Drivers SHOULD NOT depend on Meta for correctness.
	Meta map[string]any
	// Timestamp records when the publisher considered the envelope
	// ready. Set by the caller; drivers do NOT rewrite this field.
	Timestamp time.Time
}

// Validate reports whether the envelope is structurally valid.
// Returns wrapped sentinels:
//
//   - ErrIdentityRequired when any of (tenant, user, session) is empty.
//
// Drivers SHOULD call Validate before publishing; consumers MAY rely
// on the bus having already validated.
func (e BusEnvelope) Validate() error {
	if err := identity.Validate(e.Identity.Identity); err != nil {
		return errors.Join(ErrIdentityRequired, err)
	}
	return nil
}

// MessageBus is Harbor's at-least-once cross-worker fan-out edge.
// Implementations MUST be safe for concurrent use by N goroutines
// against a single shared instance (D-025).
//
// Subscribe is intentionally NOT on this interface in V1: the loopback
// driver projects the bus through the typed `events.EventBus` so
// subscribers wired to the event bus see the envelopes as typed
// events. A `Subscribe`-shaped method lands when a durable driver
// does (post-V1 phase 86); the contract is purposely narrow at V1.
type MessageBus interface {
	// Publish delivers env to its subscribers. At-least-once delivery;
	// consumers MUST be idempotent on `(TaskID, Edge, EventID)`.
	//
	// Returns ErrBusClosed when called after Close.
	// Returns ErrIdentityRequired when env's identity triple is empty.
	Publish(ctx context.Context, env BusEnvelope) error
	// Close shuts down the bus, joining any driver-owned goroutines.
	// Idempotent: a second Close is a no-op + returns nil.
	Close(ctx context.Context) error
}

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrBusClosed — Publish was called after Close.
	ErrBusClosed = errors.New("distributed: bus is closed")
	// ErrTransportClosed — a RemoteTransport method was called after Close.
	ErrTransportClosed = errors.New("distributed: remote transport is closed")
	// ErrIdentityRequired — the call lacks one or more identity components.
	ErrIdentityRequired = errors.New("distributed: identity required (tenant/user/session)")
	// ErrUnknownDriver — the configured driver name is not registered.
	ErrUnknownDriver = errors.New("distributed: unknown driver")
	// ErrAgentNotFound — the RemoteTransport could not resolve the agent URL.
	ErrAgentNotFound = errors.New("distributed: A2A agent not registered with this transport")
	// ErrTaskNotFound — the requested A2A task does not exist on the target.
	ErrTaskNotFound = errors.New("distributed: A2A task not found")
	// ErrInvalidPart — an A2A Part oneof was empty or unrecognised.
	ErrInvalidPart = errors.New("distributed: invalid A2A Part (oneof empty)")
)
