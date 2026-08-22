// Package state owns Harbor's persistence floor: the single mandatory
// `StateStore` interface that every persistence-shaped subsystem
// (sessions, tasks, governance accumulators, planner checkpoints,
// memory snapshots, steering events) saves through.
//
// The surface is generic by design: identity-scoped CRUD keyed
// on `(identity.Quadruple, Kind string, Bytes []byte)` with idempotency
// on a caller-supplied `EventID` (ULID), plus explicitly-elevated
// maintenance scans (`ListKind` and tenant-bounded `ScanKindForTenant` — RFC
// §6.11). Consuming
// subsystems land their typed wrappers at their own layer atop this
// interface — a `SessionRegistry.Save(s Session)` reduces to
// `StateStore.Save(StateRecord{Identity: s.Identity, Kind: "session.lifecycle", Bytes: marshal(s)})`.
//
// Three V1 drivers ship to the §9 persistence triad (in-memory,
// SQLite, Postgres). Harbor ships only the in-memory reference;
// SQLite and Postgres inherit the
// conformancetest suite verbatim.
//
// Identity is mandatory at the API boundary. Any `Quadruple` whose
// tenant / user / session is empty is rejected with
// `ErrIdentityRequired`. Empty `RunID` is acceptable for state that
// is session-scoped rather than run-scoped.
//
// Audit redaction is upstream of `Save`. The store stores opaque
// `Bytes`; mixing redaction into the persistence layer would couple
// a leaf package to the audit subsystem and split responsibility.
package state

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
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
// store does not interpret payloads or re-redact. Zero-length payloads
// are valid; nil and allocated empty slices are byte-equal for Save's
// idempotency contract.
//
// `Kind` is a free-form caller-namespaced key (e.g.
// "session.lifecycle", "task.checkpoint", "governance.cost"). Two
// records with the same (Quadruple, Kind) are treated as a single
// keyed slot — `Save` overwrites; `Load` returns the latest.
//
// `Version` is a hint for optimistic-concurrency at the typed-wrapper
// layer (e.g. `SessionRegistry` MAY refuse to apply an update whose
// Version is stale). It is not a StateStore compare-and-swap token:
// callers that need durable compare-and-swap use SaveIf with EventIDs.
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
	internal  bool
}

// SlotExpectation is one exact generation predicate for SaveIf. The complete
// identity plus Kind names a single StateStore slot. ExpectedEventID == ""
// means the slot must be absent; any other value must equal the current
// record's EventID exactly. Event IDs, rather than Version, are used because
// Save replaces a slot's EventID on every successful generation.
type SlotExpectation struct {
	Identity        identity.Quadruple
	Kind            string
	ExpectedEventID EventID
	internal        bool
}

// InternalKindPrefix reserves Harbor-owned coordination records.
const InternalKindPrefix = "harbor.internal/"

// NewInternalRecord authorizes a Harbor-owned internal record mutation.
func NewInternalRecord(id EventID, q identity.Quadruple, kind string, bytes []byte) StateRecord {
	return StateRecord{ID: id, Identity: q, Kind: kind, Bytes: bytes, internal: true}
}

// StoredRecord strips mutation authorization before a driver retains or
// returns a record, preventing an SDK caller from replaying an internal token.
func StoredRecord(r StateRecord) StateRecord {
	r.internal = false
	return r
}

// InternalSlotExpectation authorizes a condition/delete on an internal slot.
func InternalSlotExpectation(q identity.Quadruple, kind string, expected EventID) SlotExpectation {
	return SlotExpectation{Identity: q, Kind: kind, ExpectedEventID: expected, internal: true}
}

// IsInternalKind reports whether kind belongs to Harbor coordination state.
func IsInternalKind(kind string) bool { return strings.HasPrefix(kind, InternalKindPrefix) }

// ValidateExternalKind rejects ordinary mutations of internal coordination
// slots. Internal callers use the authorized conditional constructors above.
func ValidateExternalKind(kind string) error {
	if kind == "" {
		return ErrInvalidRecord
	}
	if IsInternalKind(kind) {
		return ErrReservedKind
	}
	return nil
}

// StateStore is Harbor's persistence interface — single mandatory
// surface, no `Supports*` capability ceremony (AGENTS.md §4.4 + §9).
//
// Implementations MUST be safe for concurrent use by N goroutines
// against a single shared instance. Mutable state must be
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

	// SaveIf atomically verifies every expectation and persists next. The
	// expectation set is non-empty, has no duplicate slots, and must include
	// next's slot. A mismatch returns ErrConditionFailed and leaves every slot
	// unchanged. Save's EventID idempotency contract applies to next only after
	// all predicates match; it never bypasses a failed predicate.
	SaveIf(ctx context.Context, expectations []SlotExpectation, next StateRecord) error

	// SaveBatchIf atomically verifies every expectation and persists every
	// record in writes. Both sets are non-empty and contain no duplicate slots;
	// every write slot must have an expectation, so no mutation is
	// unconditioned. A predicate, validation, idempotency, cancellation, or
	// storage failure leaves every slot unchanged.
	SaveBatchIf(ctx context.Context, expectations []SlotExpectation, writes []StateRecord) error

	// DeleteIf atomically removes exactly one present slot generation. A
	// different or absent generation is a normal concurrent-state outcome and
	// returns (false, nil); only an exact EventID match may be deleted. This is
	// the conditional-delete counterpart to SaveIf for compensations that must
	// restore a genuinely absent pre-operation state without writing a marker.
	DeleteIf(ctx context.Context, expectation SlotExpectation) (bool, error)

	// FenceIf acquires the driver's cross-instance lock for one exact slot
	// generation, verifies the EventID, runs fn while that generation cannot be
	// replaced, then releases the lock without mutating the slot. fn MUST NOT
	// call this StateStore; it exists for a short process-local publication
	// linearization that must serialize with SaveIf on the same durable slot.
	// Context cancellation is an admission condition before fn starts; callers
	// must not use cancellation to infer that an already-started callback did
	// not publish.
	FenceIf(ctx context.Context, expectation SlotExpectation, fn func() error) error

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

	// DeleteScope removes EVERY record whose (tenant, user, session)
	// matches id, regardless of run_id or kind. It is the kind-agnostic
	// cascade primitive a session-erasure (`sessions.delete`) runs to
	// remove all data scoped to a session in one call — the session
	// lifecycle record, run-scoped trajectories, planner checkpoints, and
	// the durable event stream all live under the triple and all go.
	//
	// Unlike ListKind, DeleteScope is identity-scoped — NOT a
	// maintenance-elevated cross-identity scan. It deletes only the
	// caller's OWN session, so it needs no ListScope claim: the triple IS
	// the scope. It fails closed with ErrIdentityRequired on an
	// incomplete triple (empty tenant / user / session); empty RunID is
	// irrelevant — the match ignores run_id entirely.
	//
	// It is idempotent: an absent scope returns (0, nil), never an error,
	// so a cascade interrupted mid-flight is safe to re-invoke to
	// convergence. Only reserved internal Kinds at the exact coordination
	// identity survive; prefix-shaped legacy rows under ordinary identities are
	// ordinary session data and are deleted. Returns the number of records deleted.
	DeleteScope(ctx context.Context, id identity.Identity) (int, error)

	// ListKind enumerates every record whose Kind starts with
	// kindPrefix — the store's unbounded cross-tenant maintenance scan
	// (RFC §6.11). Unlike every other method, the scan crosses
	// identity boundaries: it exists so runtime maintenance loops (the
	// pause sweeper's crash-orphan rescan is the first consumer) can
	// find records whose identities the process has never seen. That
	// elevation is explicit and fail-closed:
	//
	//   - scope.MaintenanceScoped MUST be true, or the call fails with
	//     ErrMaintenanceScopeRequired (CLAUDE.md §6 rule 5 / §13 "no
	//     cross-session queries without an explicit elevated scope
	//     claim"). There is no identity-scoped mode — identity-scoped
	//     reads stay on Load / LoadByEventID.
	//   - kindPrefix MUST be non-empty (ErrInvalidRecord) — a
	//     whole-store dump is never a valid maintenance scan.
	//   - Callers MUST act on each returned record under that record's
	//     OWN identity (every record carries its Quadruple); ListKind
	//     grants visibility for the scan, never a widened mutation
	//     scope.
	//
	// kindPrefix matches literally (no wildcard or case-folding
	// interpretation). Result order is
	// unspecified; an empty result is ([]StateRecord{} or nil, nil),
	// never an error.
	ListKind(ctx context.Context, scope ListScope, kindPrefix string) ([]StateRecord, error)

	// ListKindBounded is the storage-side bounded counterpart to ListKind.
	// Implementations MUST stop materializing after limit records and SHOULD
	// push the bound into the storage query. Callers that need to reject
	// overflow ask for their accepted bound plus one. The result order is
	// deterministic by the driver's maintenance key ordering, but callers
	// must not rely on it for correctness.
	ListKindBounded(ctx context.Context, scope ListScope, kindPrefix string, limit int) ([]StateRecord, error)

	// ListKindForIdentity enumerates records for one complete identity whose
	// Kind starts with kindPrefix. Unlike ListKind, this is not an elevated
	// maintenance scan: the supplied triple is the complete read boundary.
	// Prefix matching is literal and kindPrefix must be non-empty.
	ListKindForIdentity(ctx context.Context, id identity.Quadruple, kindPrefix string) ([]StateRecord, error)

	// ListKindForIdentityBounded is the identity-scoped counterpart for a
	// caller that must cap materialization before it processes records. It
	// returns at most limit rows, with the identity and literal-prefix
	// semantics of ListKindForIdentity. Callers that must reject overflow ask
	// for their accepted bound plus one. It is deliberately not a cursor: it
	// is a bounded admission check, not a maintenance traversal.
	ListKindForIdentityBounded(ctx context.Context, id identity.Quadruple, kindPrefix string, limit int) ([]StateRecord, error)

	// ScanKindForTenant returns one deterministic, tenant-bounded maintenance
	// page whose Kind begins with literalKindPrefix. It is deliberately a
	// keyset scan, not a database snapshot: callers that need convergence must
	// quiesce writers and complete a final verification pass. continuation is
	// an opaque cursor returned by the preceding page and is bound to this
	// exact maintenance scope, tenant, and literal prefix.
	ScanKindForTenant(ctx context.Context, scope ListScope, tenantID, literalKindPrefix string, limit int, continuation string) (StateScanPage, error)

	// Close releases driver resources. Subsequent calls return
	// ErrStoreClosed (wrapped). Implementations MUST honour ctx
	// during long teardowns.
	Close(ctx context.Context) error
}

// ListScope is the explicit scope claim maintenance scans require. The zero
// value fails closed: a caller must set MaintenanceScoped to assert it
// understands the call crosses identity boundaries (CLAUDE.md §6 rule
// 5; the §13 elevated-scope-claim rule applied to the persistence
// floor).
type ListScope struct {
	// MaintenanceScoped asserts the caller is a runtime maintenance
	// loop acting on each returned record under that record's own
	// identity. False (the zero value) is rejected with
	// ErrMaintenanceScopeRequired.
	MaintenanceScoped bool
}

// StateScanPage is one stable-ordered page from ScanKindForTenant. An empty
// Continuation marks the terminal page. Records are ordered lexicographically
// by (user_id, session_id, run_id, kind), which is also the cursor tuple.
type StateScanPage struct {
	Records      []StateRecord
	Continuation string
}

// StateScanCursor is the decoded, driver-neutral keyset position used by
// ScanKindForTenant. It is exposed only so all mandatory drivers share one
// strict opaque-cursor codec; callers receive and replay only its encoding.
type StateScanCursor struct {
	UserID    string
	SessionID string
	RunID     string
	Kind      string
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
	// ErrConditionFailed — SaveIf observed a slot whose EventID did not match
	// its exact expectation. No SaveIf write was applied.
	ErrConditionFailed = errors.New("state: condition failed")
	// ErrIdentityRequired — Save / Load / Delete called with a
	// Quadruple missing one of (tenant, user, session). Empty RunID
	// is allowed for session-scoped state.
	ErrIdentityRequired = errors.New("state: identity triple incomplete")
	// ErrStoreClosed — Save / Load / Delete called after Close.
	ErrStoreClosed = errors.New("state: store is closed")
	// ErrInvalidRecord — record fails structural validation
	// (empty Kind, empty EventID).
	ErrInvalidRecord = errors.New("state: invalid record")
	// ErrReservedKind rejects external mutation of Harbor coordination state.
	ErrReservedKind = errors.New("state: reserved internal kind")
	// ErrReservedIdentity rejects external mutation at Harbor's coordination
	// principal, preventing caller-controlled identity aliasing.
	ErrReservedIdentity = errors.New("state: reserved internal identity")
	// ErrCommitOutcomeUnknown means Commit was attempted but the driver could
	// not prove whether the server made the transaction durable.
	ErrCommitOutcomeUnknown = errors.New("state: commit outcome unknown")
	// ErrUnknownDriver — Open was asked for a driver name no
	// registered factory handles.
	ErrUnknownDriver = errors.New("state: unknown driver")
	// ErrMaintenanceScopeRequired — a maintenance scan was called without the
	// explicit ListScope.MaintenanceScoped claim. The cross-identity
	// scan fails closed (CLAUDE.md §6).
	ErrMaintenanceScopeRequired = errors.New("state: maintenance scan requires an explicit maintenance scope claim")
	// ErrInvalidScan — ScanKindForTenant received invalid bounds, scope,
	// tenant/prefix, or an invalid/mismatched opaque continuation.
	ErrInvalidScan = errors.New("state: invalid tenant scan")
)

const (
	// MaxStateScanLimit bounds one maintenance page so no caller can turn the
	// tenant scan into an accidental unbounded read.
	MaxStateScanLimit = 256
	// MaxStateMaintenanceListLimit bounds one cross-identity maintenance list.
	// The extra slot lets repair/admission callers ask for their accepted
	// maximum plus one without retaining an unbounded store-wide result.
	MaxStateMaintenanceListLimit = 10001
	// MaxStateIdentityListLimit bounds one identity-local admission read. It
	// prevents a caller from using the bounded surface as an unbounded dump.
	MaxStateIdentityListLimit = 1000
	maxStateScanCursorBytes   = 1024
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
	if IsInternalKind(r.Kind) && !r.internal {
		return ErrReservedKind
	}
	if identity.IsInternalCoordination(r.Identity.Identity) && !r.internal {
		return ErrReservedIdentity
	}
	return nil
}

// ValidateSaveIf validates the common conditional-save invariants before a
// driver touches storage. Every expected slot is identity scoped, unique, and
// the next slot is one of the predicates so SaveIf cannot become an
// unconstrained conditional write.
func ValidateSaveIf(expectations []SlotExpectation, next StateRecord) error {
	if err := ValidateRecord(next); err != nil {
		return err
	}
	if len(expectations) == 0 {
		return ErrInvalidRecord
	}
	type slot struct {
		q    identity.Quadruple
		kind string
	}
	seen := make(map[slot]struct{}, len(expectations))
	nextSlot := slot{q: next.Identity, kind: next.Kind}
	foundNext := false
	for _, expectation := range expectations {
		if err := ValidateIdentity(expectation.Identity); err != nil {
			return err
		}
		if expectation.Kind == "" {
			return ErrInvalidRecord
		}
		if IsInternalKind(expectation.Kind) && !expectation.internal {
			return ErrReservedKind
		}
		if identity.IsInternalCoordination(expectation.Identity.Identity) && !expectation.internal {
			return ErrReservedIdentity
		}
		s := slot{q: expectation.Identity, kind: expectation.Kind}
		if _, ok := seen[s]; ok {
			return ErrInvalidRecord
		}
		seen[s] = struct{}{}
		if s == nextSlot {
			foundNext = true
		}
	}
	if !foundNext {
		return ErrInvalidRecord
	}
	return nil
}

// ValidateSaveBatchIf validates the mandatory atomic multi-record save.
func ValidateSaveBatchIf(expectations []SlotExpectation, writes []StateRecord) error {
	if len(expectations) == 0 || len(writes) == 0 {
		return ErrInvalidRecord
	}
	type slot struct {
		q    identity.Quadruple
		kind string
	}
	expected := make(map[slot]struct{}, len(expectations))
	for _, expectation := range expectations {
		if err := ValidateIdentity(expectation.Identity); err != nil {
			return err
		}
		if expectation.Kind == "" {
			return ErrInvalidRecord
		}
		if IsInternalKind(expectation.Kind) && !expectation.internal {
			return ErrReservedKind
		}
		if identity.IsInternalCoordination(expectation.Identity.Identity) && !expectation.internal {
			return ErrReservedIdentity
		}
		s := slot{q: expectation.Identity, kind: expectation.Kind}
		if _, duplicate := expected[s]; duplicate {
			return ErrInvalidRecord
		}
		expected[s] = struct{}{}
	}
	written := make(map[slot]struct{}, len(writes))
	eventIDs := make(map[EventID]struct{}, len(writes))
	for _, write := range writes {
		if err := ValidateRecord(write); err != nil {
			return err
		}
		s := slot{q: write.Identity, kind: write.Kind}
		if _, duplicate := written[s]; duplicate {
			return ErrInvalidRecord
		}
		if _, conditioned := expected[s]; !conditioned {
			return ErrInvalidRecord
		}
		if _, duplicate := eventIDs[write.ID]; duplicate {
			return ErrInvalidRecord
		}
		written[s] = struct{}{}
		eventIDs[write.ID] = struct{}{}
	}
	return nil
}

// ValidateDeleteIf validates the exact-present generation predicate used by
// StateStore.DeleteIf. Conditional deletion never accepts the empty EventID
// sentinel because absence is not something it can delete.
func ValidateDeleteIf(expectation SlotExpectation) error {
	if err := ValidateIdentity(expectation.Identity); err != nil {
		return err
	}
	if expectation.Kind == "" || expectation.ExpectedEventID == "" {
		return ErrInvalidRecord
	}
	if IsInternalKind(expectation.Kind) && !expectation.internal {
		return ErrReservedKind
	}
	if identity.IsInternalCoordination(expectation.Identity.Identity) && !expectation.internal {
		return ErrReservedIdentity
	}
	return nil
}

// ValidateFenceIf validates one present exact-generation fence predicate.
func ValidateFenceIf(expectation SlotExpectation, fn func() error) error {
	if err := ValidateDeleteIf(expectation); err != nil {
		return err
	}
	if fn == nil {
		return ErrInvalidRecord
	}
	return nil
}

// ValidateListKind checks ListKind's fail-closed preconditions shared
// by every driver: the explicit maintenance scope claim must be set
// (ErrMaintenanceScopeRequired) and the kind prefix must be non-empty
// (ErrInvalidRecord — a whole-store dump is never a valid maintenance
// scan).
func ValidateListKind(scope ListScope, kindPrefix string) error {
	if !scope.MaintenanceScoped {
		return ErrMaintenanceScopeRequired
	}
	if kindPrefix == "" {
		return ErrInvalidRecord
	}
	return nil
}

// ValidateListKindBounded checks the explicit maintenance scope, literal
// prefix, and hard storage-side materialization bound shared by every driver.
func ValidateListKindBounded(scope ListScope, kindPrefix string, limit int) error {
	if err := ValidateListKind(scope, kindPrefix); err != nil {
		return err
	}
	if limit < 1 || limit > MaxStateMaintenanceListLimit {
		return ErrInvalidRecord
	}
	return nil
}

// ValidateListKindForIdentity checks the common preconditions for the
// identity-scoped enumeration surface.
func ValidateListKindForIdentity(id identity.Quadruple, kindPrefix string) error {
	if err := ValidateIdentity(id); err != nil {
		return err
	}
	if kindPrefix == "" {
		return ErrInvalidRecord
	}
	return nil
}

// ValidateListKindForIdentityBounded checks the identity-prefix and hard
// materialization bounds shared by every bounded identity-list driver.
func ValidateListKindForIdentityBounded(id identity.Quadruple, kindPrefix string, limit int) error {
	if err := ValidateListKindForIdentity(id, kindPrefix); err != nil {
		return err
	}
	if limit < 1 || limit > MaxStateIdentityListLimit {
		return ErrInvalidRecord
	}
	return nil
}

// ValidateScanKindForTenant checks the fail-closed preconditions shared by
// every ScanKindForTenant implementation. It intentionally returns
// ErrInvalidScan for all malformed scan request details so operators do not
// need driver-specific parsing errors to identify a bad continuation.
func ValidateScanKindForTenant(scope ListScope, tenantID, literalKindPrefix string, limit int) error {
	if !scope.MaintenanceScoped {
		return ErrMaintenanceScopeRequired
	}
	if tenantID == "" || literalKindPrefix == "" || limit < 1 || limit > MaxStateScanLimit {
		return ErrInvalidScan
	}
	return nil
}

type encodedStateScanCursor struct {
	Version   int    `json:"v"`
	TenantID  string `json:"t"`
	Prefix    string `json:"p"`
	Scoped    bool   `json:"m"`
	UserID    string `json:"u"`
	SessionID string `json:"s"`
	RunID     string `json:"r"`
	Kind      string `json:"k"`
}

// DecodeStateScanContinuation strictly decodes a ScanKindForTenant cursor.
// The cursor is intentionally bound to the query dimensions, preventing a
// cursor issued for one tenant or prefix from widening another scan.
func DecodeStateScanContinuation(continuation, tenantID, literalKindPrefix string, scope ListScope) (StateScanCursor, error) {
	if err := ValidateScanKindForTenant(scope, tenantID, literalKindPrefix, 1); err != nil {
		return StateScanCursor{}, err
	}
	if continuation == "" {
		return StateScanCursor{}, nil
	}
	if len(continuation) > base64.RawURLEncoding.EncodedLen(maxStateScanCursorBytes) {
		return StateScanCursor{}, ErrInvalidScan
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(continuation)
	if err != nil || len(raw) == 0 || len(raw) > maxStateScanCursorBytes {
		return StateScanCursor{}, ErrInvalidScan
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var encoded encodedStateScanCursor
	if err := decoder.Decode(&encoded); err != nil {
		return StateScanCursor{}, ErrInvalidScan
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return StateScanCursor{}, ErrInvalidScan
	}
	if encoded.Version != 1 || !encoded.Scoped || encoded.TenantID != tenantID || encoded.Prefix != literalKindPrefix || encoded.UserID == "" || encoded.SessionID == "" || encoded.Kind == "" || !strings.HasPrefix(encoded.Kind, literalKindPrefix) {
		return StateScanCursor{}, ErrInvalidScan
	}
	return StateScanCursor{UserID: encoded.UserID, SessionID: encoded.SessionID, RunID: encoded.RunID, Kind: encoded.Kind}, nil
}

// EncodeStateScanContinuation returns the sole opaque continuation format
// accepted by DecodeStateScanContinuation. Driver code must only pass a tuple
// it just returned, preserving strict monotonic keyset progression.
func EncodeStateScanContinuation(cursor StateScanCursor, tenantID, literalKindPrefix string, scope ListScope) (string, error) {
	if err := ValidateScanKindForTenant(scope, tenantID, literalKindPrefix, 1); err != nil {
		return "", err
	}
	if cursor.UserID == "" || cursor.SessionID == "" || cursor.Kind == "" || !strings.HasPrefix(cursor.Kind, literalKindPrefix) {
		return "", ErrInvalidScan
	}
	raw, err := json.Marshal(encodedStateScanCursor{Version: 1, TenantID: tenantID, Prefix: literalKindPrefix, Scoped: scope.MaintenanceScoped, UserID: cursor.UserID, SessionID: cursor.SessionID, RunID: cursor.RunID, Kind: cursor.Kind})
	if err != nil {
		return "", fmt.Errorf("state: encode scan continuation: %w", err)
	}
	if len(raw) > maxStateScanCursorBytes {
		return "", ErrInvalidScan
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
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
