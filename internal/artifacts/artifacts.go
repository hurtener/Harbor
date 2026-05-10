// Package artifacts defines Harbor's content-addressed blob store —
// the mandatory routing target for any output above the heavy-output
// threshold (default 32 KB; D-022, D-026, RFC §6.10).
//
// The surface is a single mandatory `ArtifactStore` interface (eight
// methods including `Close`); there is NO `NoOp` fallback (brief 05 §1).
// V1 ships two drivers — an in-memory floor for dev/embedded use and a
// filesystem driver for single-binary production deployments. Phase 18
// adds SQLite-blob + Postgres-blob; Phase 19 adds the S3-style driver;
// all four downstream drivers inherit Phase 17's conformance suite
// verbatim.
//
// Identity model. `ArtifactScope` is a flat `(TenantID, UserID,
// SessionID, TaskID)` tuple — deliberately distinct from the runtime's
// `identity.Quadruple{Identity, RunID}` shape. RFC §6.10 keys artifacts
// on tasks (foreground OR background); for foreground tasks the
// consumer (tool dispatcher, Phase 26) maps `RunID → TaskID`. Keeping
// `ArtifactScope` as a flat-string struct lets the store stay
// dependency-free of `internal/identity` while still reading like the
// brief's wire shape.
//
// Identity is mandatory at the API boundary. Empty tenant / user /
// session each return wrapped `ErrIdentityRequired` from Put*. Empty
// `TaskID` is acceptable for session-scoped artifacts (parallel to
// `state.StateStore`'s session-vs-run rule). `List` treats empty
// fields as wildcards: `ArtifactScope{TenantID: "A"}` lists every
// artifact under tenant A across users / sessions / tasks.
//
// Get / GetRef return `(value, found, err)`. Found-false is NOT an
// error — the consumer pattern is "Exists → fetch." `ErrNotFound` is
// reserved for actual error contexts (e.g. corrupted indexing); the
// conformance suite tests the `(nil, false, nil)` shape explicitly.
//
// Audit redaction is upstream (D-020). The store stores opaque bytes
// and never re-redacts; mixing redaction into a leaf would couple the
// store to the audit subsystem and split responsibility.
package artifacts

import (
	"context"
	"errors"

	"github.com/hurtener/Harbor/internal/config"
)

// ArtifactScope identifies the (tenant, user, session, task) owner of
// an artifact. All four fields are flat strings; the consumer (tool
// dispatcher Phase 26+) is responsible for translating the runtime's
// `identity.Quadruple` (whose `RunID` becomes `TaskID` for foreground
// runs) into this shape.
//
// Mandatory at the API boundary: `TenantID`, `UserID`, `SessionID`
// must be non-empty for Put*. Empty `TaskID` is acceptable for
// session-scoped artifacts. `List` treats empty fields as wildcards.
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

// Equal reports whether two scopes are field-for-field equal.
// Used by `ScopedArtifacts` for read-side scope checks and by drivers
// for cross-tenant isolation enforcement.
func (s ArtifactScope) Equal(other ArtifactScope) bool {
	return s.TenantID == other.TenantID &&
		s.UserID == other.UserID &&
		s.SessionID == other.SessionID &&
		s.TaskID == other.TaskID
}

// ArtifactRef is the canonical reference returned by Put* and resolved
// by GetRef. `ID` is content-addressed: `{namespace}_{sha256_hex[:12]}`.
// Re-uploading identical bytes within the same scope returns the
// existing ref (no duplicate storage).
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
// concurrent goroutines on a single shared instance (D-025); the
// conformance suite's `Concurrent_PutGet_NoRace` is the gate.
//
// Identity is enforced at the API boundary: every Put*/Get/GetRef/
// Exists/Delete validates `scope` before touching storage. `List`
// accepts a partial filter (empty fields are wildcards).
//
// Get / GetRef return `(value, found, err)`. Found-false is NOT an
// error.
type ArtifactStore interface {
	// PutBytes stores data under scope, returning the canonical ref.
	// The ref's `ID` is `{namespace}_{sha256_hex[:12]}`. Re-Put with
	// identical (scope, namespace, bytes) is a no-op that returns the
	// existing ref.
	PutBytes(ctx context.Context, scope ArtifactScope, data []byte, opts PutOpts) (ArtifactRef, error)

	// PutText is a thin wrapper over PutBytes that stores `text` as
	// UTF-8 bytes. Recovered via Get as bytes. MimeType defaults to
	// `text/plain; charset=utf-8` when opts.MimeType is empty.
	PutText(ctx context.Context, scope ArtifactScope, text string, opts PutOpts) (ArtifactRef, error)

	// Get returns the bytes for `id` within `scope`. Found-false
	// indicates the ref does not exist in this scope; it is NOT an
	// error. ErrNotFound is reserved for actual error contexts.
	Get(ctx context.Context, scope ArtifactScope, id string) ([]byte, bool, error)

	// GetRef returns the metadata-only ref for `id` within `scope`.
	// Same found-false semantics as Get.
	GetRef(ctx context.Context, scope ArtifactScope, id string) (*ArtifactRef, bool, error)

	// Exists reports whether `id` is stored in `scope`. Cheaper than
	// GetRef when the caller only needs presence.
	Exists(ctx context.Context, scope ArtifactScope, id string) (bool, error)

	// Delete removes `id` from `scope` and returns whether anything
	// existed before delete. Idempotent: Delete on absent returns
	// `(false, nil)`.
	Delete(ctx context.Context, scope ArtifactScope, id string) (bool, error)

	// List returns refs whose scope matches `filter`. Empty fields in
	// `filter` are wildcards: `ArtifactScope{TenantID: "A"}` lists
	// every artifact under tenant A across users / sessions / tasks.
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
	// with a scope missing tenant/user/session.
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

// Factory builds an ArtifactStore from an ArtifactsConfig. Drivers
// expose one Factory each via init() → Register.
type Factory func(config.ArtifactsConfig) (ArtifactStore, error)
