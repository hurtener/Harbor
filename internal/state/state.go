// Package state owns Harbor's persistence floor: the single mandatory
// `StateStore` interface that every persistence-shaped subsystem
// (sessions, tasks, governance accumulators, planner checkpoints,
// memory snapshots, steering events) saves through.
//
// The surface is generic by design (D-027): five methods keyed on
// `(identity.Quadruple, Kind string, Bytes []byte)` with idempotency
// on a caller-supplied `EventID` (ULID). Consuming subsystems land
// their typed wrappers at their own layer atop this interface — a
// `SessionRegistry.Save(s Session)` reduces to
// `StateStore.Save(StateRecord{Identity: s.Identity, Kind: "session.lifecycle", Bytes: marshal(s)})`.
//
// Three V1 drivers ship to the §9 persistence triad (in-memory,
// SQLite, Postgres). Phase 07 ships only the in-memory reference;
// SQLite (Phase 15) and Postgres (Phase 16) inherit the
// conformancetest suite verbatim.
//
// Identity is mandatory at the API boundary. Any `Quadruple` whose
// tenant / user / session is empty is rejected with
// `ErrIdentityRequired`. Empty `RunID` is acceptable for state that
// is session-scoped rather than run-scoped.
//
// Audit redaction is upstream of `Save`. The store stores opaque
// `Bytes`; mixing redaction into the persistence layer would couple
// a leaf package to the audit subsystem and split responsibility
// (D-020).
package state

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/hurtener/Harbor/internal/identity"
)

// EventID is a caller-supplied ULID used as the canonical idempotency
// key for `Save`. ULID gives us monotonic, lexicographically sortable
// IDs that work as both primary keys and secondary indices.
//
// Callers are free to construct the value externally; `NewEventID`
// is provided as a convenience that uses crypto-strong entropy.
type EventID string

// NewEventID generates a fresh ULID-shaped EventID using
// crypto/rand. Implementations may use any ULID source; this helper
// exists so callers don't need a separate dependency just for
// generating idempotency keys.
func NewEventID() EventID {
	return EventID(ulid.MustNew(ulid.Now(), rand.Reader).String())
}

// StateRecord is the unit of persistence.
//
// `Bytes` is opaque to the store — callers serialize their domain
// types and run them through audit redaction upstream of `Save`. The
// store does not interpret payloads or re-redact.
//
// `Kind` is a free-form caller-namespaced key (e.g.
// "session.lifecycle", "task.checkpoint", "governance.cost"). Two
// records with the same (Quadruple, Kind) are treated as a single
// keyed slot — `Save` overwrites; `Load` returns the latest.
//
// `Version` is a hint for optimistic-concurrency at the typed-wrapper
// layer (e.g. `SessionRegistry` MAY refuse to apply an update whose
// Version is stale). The StateStore itself does NOT enforce CAS — it
// stores and returns the int.
//
// `UpdatedAt` is set by the store at `Save` time when zero; callers
// MAY override (useful for tests with controllable clocks).
type StateRecord struct {
	ID        EventID
	Identity  identity.Quadruple
	Kind      string
	Version   int
	Bytes     []byte
	UpdatedAt time.Time
}

// StateStore is Harbor's persistence interface — single mandatory
// surface, no `Supports*` capability ceremony (AGENTS.md §4.4 + §9).
//
// Implementations MUST be safe for concurrent use by N goroutines
// against a single shared instance (D-025). Mutable state must be
// guarded; per-call state lives in `ctx`, never on the driver.
type StateStore interface {
	// Save persists a record. Idempotent on `EventID`:
	//
	//   - Same EventID + byte-equal Bytes: no-op (no error, no
	//     duplicate write).
	//   - Same EventID + different Bytes: ErrIdempotencyConflict.
	//
	// If a record already exists at (Identity, Kind) but with a
	// different EventID, Save overwrites it (the new EventID becomes
	// the active one for that slot; the previous EventID is no
	// longer LoadByEventID-resolvable).
	Save(ctx context.Context, r StateRecord) error

	// Load returns the record at (id, kind). Returns ErrNotFound
	// (wrapped) when no record exists for that key.
	Load(ctx context.Context, id identity.Quadruple, kind string) (StateRecord, error)

	// LoadByEventID returns the record whose ID matches eventID.
	// Useful for replaying a specific event by its idempotency key.
	// Returns ErrNotFound (wrapped) when not present.
	LoadByEventID(ctx context.Context, eventID EventID) (StateRecord, error)

	// Delete removes the record at (id, kind). Returns nil when the
	// record is absent (idempotent), wrapped error on store failure.
	Delete(ctx context.Context, id identity.Quadruple, kind string) error

	// Close releases driver resources. Subsequent calls return
	// ErrStoreClosed (wrapped). Implementations MUST honour ctx
	// during long teardowns.
	Close(ctx context.Context) error
}

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrNotFound — Load / LoadByEventID was called for a key that
	// has no record. Wraps drivers' own not-found shapes.
	ErrNotFound = errors.New("state: record not found")
	// ErrIdempotencyConflict — Save with a previously-seen EventID
	// but different Bytes (or routed to a different key). Tells the
	// caller a retry policy bug exists upstream.
	ErrIdempotencyConflict = errors.New("state: idempotency conflict")
	// ErrIdentityRequired — Save / Load / Delete called with a
	// Quadruple missing one of (tenant, user, session). Empty RunID
	// is allowed for session-scoped state.
	ErrIdentityRequired = errors.New("state: identity triple incomplete")
	// ErrStoreClosed — Save / Load / Delete called after Close.
	ErrStoreClosed = errors.New("state: store is closed")
	// ErrInvalidRecord — record fails structural validation
	// (empty Kind, empty EventID).
	ErrInvalidRecord = errors.New("state: invalid record")
	// ErrUnknownDriver — Open was asked for a driver name no
	// registered factory handles.
	ErrUnknownDriver = errors.New("state: unknown driver")
)

// ValidateIdentity checks that the triple is fully specified. Empty
// RunID is accepted (session-scoped state). Returns wrapped
// ErrIdentityRequired when any of tenant/user/session is empty.
func ValidateIdentity(q identity.Quadruple) error {
	if q.TenantID == "" || q.UserID == "" || q.SessionID == "" {
		return ErrIdentityRequired
	}
	return nil
}

// ValidateRecord checks structural invariants Save needs before
// touching driver storage: identity triple present, EventID
// non-empty, Kind non-empty.
func ValidateRecord(r StateRecord) error {
	if err := ValidateIdentity(r.Identity); err != nil {
		return err
	}
	if r.ID == "" {
		return ErrInvalidRecord
	}
	if r.Kind == "" {
		return ErrInvalidRecord
	}
	return nil
}

// ctxKey is the unexported key under which a StateStore is propagated
// on a context. Independent from identity / audit / events ctx keys.
type ctxKey int

const storeCtxKey ctxKey = iota

// WithStore attaches store to ctx for downstream handlers.
func WithStore(ctx context.Context, store StateStore) context.Context {
	return context.WithValue(ctx, storeCtxKey, store)
}

// MustFrom returns the StateStore in ctx; panics with ErrStoreClosed
// (used as the sentinel for "no store configured") when none is
// present. Use in handler/runtime paths where a store is mandatory.
func MustFrom(ctx context.Context) StateStore {
	s, ok := From(ctx)
	if !ok {
		panic(ErrStoreClosed)
	}
	return s
}

// From returns the StateStore in ctx and a presence bool. Use when
// absence is recoverable.
func From(ctx context.Context) (StateStore, bool) {
	s, ok := ctx.Value(storeCtxKey).(StateStore)
	return s, ok
}
