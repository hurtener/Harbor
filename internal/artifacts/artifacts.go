// Package artifacts defines Harbor's content-addressed blob store —
// the mandatory routing target for any output above the heavy-output
// threshold (default 32 KB; RFC §6.10).
//
// The surface is a single mandatory `ArtifactStore` interface (eight
// methods including `Close`); there is NO `NoOp` fallback.
// V1 ships two drivers — an in-memory floor for dev/embedded use and a
// filesystem driver for single-binary production deployments. Harbor
// also ships SQLite-blob, Postgres-blob, and the S3-style driver;
// all four downstream drivers inherit the conformance suite
// verbatim.
//
// Identity model. `ArtifactScope` is a flat `(TenantID, UserID,
// SessionID, TaskID)` tuple — deliberately distinct from the runtime's
// `identity.Quadruple{Identity, RunID}` shape. Keeping `ArtifactScope`
// as a flat-string struct lets the store stay dependency-free of
// `internal/identity`.
//
// THE READ KEY IS THE ISOLATION TRIPLE. `Get` / `GetRef` / `Exists` /
// `Delete` match `(TenantID, UserID, SessionID, id)`. `TaskID` is a
// PROVENANCE ANNOTATION — it records which task produced the bytes; it
// is not an isolation principal and does not participate in resolution.
// The isolation boundary is `(tenant, user, session)` and a task runs
// WITHIN it rather than widening or narrowing it.
//
// Two consequences follow, stated rather than left to be discovered:
//
//  1. An artifact written under one run's scope is readable by a
//     SIBLING RUN IN THE SAME SESSION. That is the intent: the session
//     is the innermost isolation scope, the session-artifact manifest
//     already enumerates across the runs inside it, and nothing crosses
//     a session, a user or a tenant. The content-addressed `ID` is what
//     makes it safe — within one session two artifacts sharing an id
//     ARE the same bytes, so dropping a field from the read key cannot
//     merge distinct content; it can only stop an artifact being hidden
//     from itself.
//  2. The WRITE/dedup key narrows with the read key. Two tasks storing
//     identical bytes in one session collapse to ONE artifact, and
//     `ArtifactRef.Scope.TaskID` is therefore FIRST-WRITER-WINS. See
//     `ArtifactStore.List` for what that costs a `TaskID` filter.
//
// Identity is mandatory at the API boundary. Empty tenant / user /
// session each return wrapped `ErrIdentityRequired` from Put*, Get,
// GetRef, Exists and Delete. Empty `TaskID` is acceptable for
// session-scoped artifacts (parallel to `state.StateStore`'s
// session-vs-run rule). `List` is a FILTER rather than a key: it
// requires a tenant and treats the remaining empty fields as wildcards
// WITHIN that tenant, so an unscoped all-tenants listing is not
// expressible at the store boundary.
//
// Get / GetRef return `(value, found, err)`. Found-false is NOT an
// error — the consumer pattern is "Exists → fetch." `ErrNotFound` is
// reserved for actual error contexts (e.g. corrupted indexing); the
// conformance suite tests the `(nil, false, nil)` shape explicitly.
//
// Audit redaction is upstream. The store stores opaque bytes
// and never re-redacts; mixing redaction into a leaf would couple the
// store to the audit subsystem and split responsibility.
package artifacts

import (
	"context"
	"errors"

	"github.com/hurtener/Harbor/internal/config"
)

// ArtifactScope carries the `(tenant, user, session)` isolation triple
// that OWNS an artifact plus the `TaskID` provenance annotation that
// records which task produced it. All four fields are flat strings; the
// consumer (tool dispatcher) is responsible for translating the
// runtime's `identity.Quadruple` (whose `RunID` becomes `TaskID`) into
// this shape.
//
// Mandatory at the API boundary: `TenantID`, `UserID`, `SessionID`
// must be non-empty for Put*, Get, GetRef, Exists and Delete. Empty
// `TaskID` is acceptable for session-scoped artifacts, and a populated
// one never narrows a read — see `Triple`.
type ArtifactScope struct {
	TenantID  string
	UserID    string
	SessionID string
	TaskID    string
}

// Validate returns wrapped `ErrIdentityRequired` when any of
// tenant / user / session is empty. Empty `TaskID` is accepted.
//
// Use the package-level `Validate(scope)` helper when you don't have
// an `ArtifactScope` value handy; both call sites converge on the
// same rule.
func (s ArtifactScope) Validate() error {
	if s.TenantID == "" || s.UserID == "" || s.SessionID == "" {
		return ErrIdentityRequired
	}
	return nil
}

// ValidateFilter is `List`'s precondition. A list filter is a predicate
// over a result set rather than an identity, so an empty `UserID` /
// `SessionID` / `TaskID` stays a wildcard — but the TENANT is required,
// because without it the zero-value scope is a legal all-tenants filter
// at the store boundary and every discovery surface built on `List`
// would inherit that. A caller who legitimately reads another tenant
// names that tenant explicitly, under the admin-scope gate its Protocol
// surface enforces.
func (s ArtifactScope) ValidateFilter() error {
	if s.TenantID == "" {
		return ErrIdentityRequired
	}
	return nil
}

// Triple returns the scope with `TaskID` cleared — the READ KEY. Drivers
// key `Get` / `GetRef` / `Exists` / `Delete` on this value plus the id,
// so a caller who stamped a task and a caller who did not resolve the
// same artifact.
func (s ArtifactScope) Triple() ArtifactScope {
	s.TaskID = ""
	return s
}

// Equal reports whether two scopes are field-for-field equal,
// INCLUDING the `TaskID` provenance annotation. It is therefore NOT the
// read key: two scopes that differ only in `TaskID` address the same
// artifact but are not `Equal`. Use `EqualTriple` for a resolution-side
// comparison; `Equal` remains the right answer when the question really
// is "is this the same stamp."
func (s ArtifactScope) Equal(other ArtifactScope) bool {
	return s.TenantID == other.TenantID &&
		s.UserID == other.UserID &&
		s.SessionID == other.SessionID &&
		s.TaskID == other.TaskID
}

// EqualTriple reports whether two scopes name the same isolation triple,
// ignoring the `TaskID` provenance annotation. This is the comparison
// that matches the read key.
func (s ArtifactScope) EqualTriple(other ArtifactScope) bool {
	return s.TenantID == other.TenantID &&
		s.UserID == other.UserID &&
		s.SessionID == other.SessionID
}

// ArtifactRef is the canonical reference returned by Put* and resolved
// by GetRef. `ID` is content-addressed: `{namespace}_{sha256_hex[:12]}`.
// Re-uploading identical bytes within the same isolation triple returns
// the existing ref (no duplicate storage) EVEN WHEN the two Puts carry
// different `Scope.TaskID` values — so `Scope.TaskID` on a resolved ref
// is the FIRST writer's stamp, not necessarily the caller's.
//
// `SHA256` carries the full hex digest (64 chars). `SizeBytes` is the
// length of the stored bytes. `Source` is opaque caller metadata —
// drivers persist it as-is; for the FS driver, values must be
// JSON-encodable (non-encodable values cause Put to fail at marshal
// time).
type ArtifactRef struct {
	ID        string
	MimeType  string
	SizeBytes int64
	Filename  string
	SHA256    string
	Scope     ArtifactScope
	Namespace string
	Source    map[string]any
}

// PutOpts carries optional metadata for Put* calls.
//
// `Namespace` is a logical bucket that participates in `ID`
// computation, so the same bytes under different namespaces produce
// distinct refs. Callers SHOULD provide a namespace; drivers default
// to `"default"` when empty.
//
// `Filename` is metadata only — never used in path construction. The
// FS driver's path-safety guard rejects traversal regardless.
//
// `Source` values must be JSON-encodable when targeting the FS
// driver (it persists `Source` to a sibling `.meta.json`). Use Go
// primitives, slices, and maps; non-encodable values (functions,
// channels, cyclic graphs) cause Put to fail at marshal time.
type PutOpts struct {
	MimeType  string
	Filename  string
	Namespace string
	Source    map[string]any
}

// ArtifactStore is Harbor's mandatory content-addressed blob store.
// All eight methods are required; there is no `Supports*` capability
// ceremony (AGENTS.md §4.4). Implementations MUST be safe for N
// concurrent goroutines on a single shared instance; the
// conformance suite's `Concurrent_PutGet_NoRace` is the gate.
//
// Identity is enforced at the API boundary: every Put*/Get/GetRef/
// Exists/Delete validates `scope`'s triple before touching storage, and
// `List` requires at least a tenant.
//
// KEY VERSUS FILTER — the distinction the interface is built on.
// `Get` / `GetRef` / `Exists` / `Delete` take an IDENTITY: they resolve
// on `(TenantID, UserID, SessionID, id)` and IGNORE `scope.TaskID`,
// which is a provenance annotation. `List` takes a PREDICATE over a
// result set: every empty field below the tenant is a wildcard,
// `TaskID` included. The two are deliberately different things and the
// difference is stated here so they stop being accidentally different.
//
// Get / GetRef return `(value, found, err)`. Found-false is NOT an
// error.
type ArtifactStore interface {
	// PutBytes stores data under scope, returning the canonical ref.
	// The ref's `ID` is `{namespace}_{sha256_hex[:12]}`. Re-Put with
	// identical (triple, namespace, bytes) is a no-op that returns the
	// EXISTING ref — including when the new call carries a different
	// `scope.TaskID`, in which case the returned ref carries the first
	// writer's stamp and nothing is stored under the caller's task.
	PutBytes(ctx context.Context, scope ArtifactScope, data []byte, opts PutOpts) (ArtifactRef, error)

	// PutText is a thin wrapper over PutBytes that stores `text` as
	// UTF-8 bytes. Recovered via Get as bytes. MimeType defaults to
	// `text/plain; charset=utf-8` when opts.MimeType is empty.
	PutText(ctx context.Context, scope ArtifactScope, text string, opts PutOpts) (ArtifactRef, error)

	// Get returns the bytes for `id` within `scope`'s isolation triple.
	// `scope.TaskID` is IGNORED: a caller that stamped a task and a
	// caller that did not read the same artifact. Found-false indicates
	// the ref does not exist under that triple; it is NOT an error.
	// ErrNotFound is reserved for actual error contexts.
	Get(ctx context.Context, scope ArtifactScope, id string) ([]byte, bool, error)

	// GetRef returns the metadata-only ref for `id` within `scope`'s
	// isolation triple. `scope.TaskID` is IGNORED for resolution; the
	// returned `Ref.Scope.TaskID` is the stored provenance stamp, which
	// is the first writer's and may differ from the caller's. Same
	// found-false semantics as Get.
	GetRef(ctx context.Context, scope ArtifactScope, id string) (*ArtifactRef, bool, error)

	// Exists reports whether `id` is stored under `scope`'s isolation
	// triple. `scope.TaskID` is IGNORED. Cheaper than GetRef when the
	// caller only needs presence.
	Exists(ctx context.Context, scope ArtifactScope, id string) (bool, error)

	// Delete removes `id` from `scope`'s isolation triple and returns
	// whether anything existed before delete. `scope.TaskID` is IGNORED,
	// and EVERY stored copy under the triple is removed — a Delete that
	// reported success while leaving a copy a later Get resolves would
	// be the silent degradation CLAUDE.md §13 forbids. Idempotent:
	// Delete on absent returns `(false, nil)`.
	Delete(ctx context.Context, scope ArtifactScope, id string) (bool, error)

	// List returns refs whose scope matches `filter`. `filter.TenantID`
	// is REQUIRED (wrapped `ErrIdentityRequired` otherwise) — an
	// unscoped all-tenants listing is not expressible here. Every other
	// empty field is a wildcard within that tenant:
	// `ArtifactScope{TenantID: "A"}` lists every artifact under tenant A
	// across users / sessions / tasks.
	//
	// THE TaskID FILTER IS LOSSY, AND SAYING SO IS PART OF THE CONTRACT.
	// Because the write key is the isolation triple, two tasks storing
	// identical bytes in one session collapse to ONE artifact carrying
	// the FIRST writer's stamp. So `filter.TaskID = "B"` does not return
	// an artifact whose bytes run B stored, if run A stored them first.
	// This is inherent to a content-addressed store rather than an
	// implementation shortfall: the id is derived from the bytes, so
	// "which run produced these bytes" has no single answer once two
	// runs produce them. `filter.TaskID` answers "which artifacts is
	// this run the recorded producer of", which is a weaker question
	// than "which artifacts did this run write".
	//
	// Order is not specified; callers that need stability sort the
	// returned slice themselves.
	List(ctx context.Context, filter ArtifactScope) ([]ArtifactRef, error)

	// Close releases driver resources. Subsequent calls return
	// wrapped `ErrStoreClosed`. Implementations MUST honour ctx
	// during long teardowns (none of V1's drivers have any).
	Close(ctx context.Context) error
}

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrNotFound — reserved for error contexts (e.g. corrupted
	// secondary index pointing at an absent primary). Get / GetRef
	// found-false is NOT this error; it is `(nil, false, nil)`.
	ErrNotFound = errors.New("artifacts: ref not found")
	// ErrScopeMismatch — `ScopedArtifacts` saw a returned ref whose
	// scope differs from the facade's fixed scope. Should be
	// impossible by construction; surfaced loudly when it isn't.
	ErrScopeMismatch = errors.New("artifacts: scope mismatch")
	// ErrIdentityRequired — Put*/Get/GetRef/Exists/Delete called
	// with a scope missing tenant/user/session, or List called with a
	// filter missing a tenant.
	ErrIdentityRequired = errors.New("artifacts: identity required (tenant/user/session)")
	// ErrInvalidScope — scope failed structural validation outside
	// the identity-required dimension (reserved; not currently
	// returned by V1 drivers).
	ErrInvalidScope = errors.New("artifacts: invalid scope")
	// ErrUnknownDriver — Open was asked for a driver name no
	// registered factory handles.
	ErrUnknownDriver = errors.New("artifacts: unknown driver")
	// ErrStoreClosed — any method called after Close.
	ErrStoreClosed = errors.New("artifacts: store is closed")
)

// Validate is the package-level helper that mirrors
// `ArtifactScope.Validate`. Returns wrapped `ErrIdentityRequired` when
// any of tenant / user / session is empty. Empty `TaskID` is accepted.
func Validate(scope ArtifactScope) error {
	return scope.Validate()
}

// ValidateFilter is the package-level helper that mirrors
// `ArtifactScope.ValidateFilter` — `List`'s precondition. Returns
// wrapped `ErrIdentityRequired` when the tenant is empty.
func ValidateFilter(filter ArtifactScope) error {
	return filter.ValidateFilter()
}

// Factory builds an ArtifactStore from an ArtifactsConfig. Drivers
// expose one Factory each via init() → Register.
type Factory func(config.ArtifactsConfig) (ArtifactStore, error)
