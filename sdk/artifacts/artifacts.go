// Package artifacts is the public SDK facade over Harbor's
// internal/artifacts package — the content-addressed, identity-scoped
// artifact store and the by-reference heavy-content contract
// (RFC §3.6, §6.10; D-204). Alias-based re-exports only: no behavior
// lives here. Driver factories and the conformance kit are
// deliberately private.
package artifacts

import (
	internal "github.com/hurtener/Harbor/internal/artifacts"
)

// Store vocabulary — aliases of the internal types.
type (
	// ArtifactStore is the identity-mandatory artifact store interface.
	ArtifactStore = internal.ArtifactStore
	// ArtifactRef is the by-reference handle to stored heavy content.
	ArtifactRef = internal.ArtifactRef
	// ArtifactScope is the (tenant, user, session) + run scope of a ref.
	ArtifactScope = internal.ArtifactScope
	// PutOpts carries optional Put metadata.
	PutOpts = internal.PutOpts
	// ScopedArtifacts is a store pre-bound to one ArtifactScope.
	ScopedArtifacts = internal.ScopedArtifacts
	// Presigner is the optional presigned-URL driver surface.
	Presigner = internal.Presigner
)

// DefaultDriver is the driver name Open resolves when the config
// names none.
const DefaultDriver = internal.DefaultDriver

// Re-exported sentinel errors callers compare via errors.Is.
var (
	// ErrNotFound — the ref does not exist.
	ErrNotFound = internal.ErrNotFound
	// ErrScopeMismatch — the ref belongs to a different scope.
	ErrScopeMismatch = internal.ErrScopeMismatch
	// ErrIdentityRequired — the identity triple is incomplete.
	ErrIdentityRequired = internal.ErrIdentityRequired
	// ErrInvalidScope — the scope failed validation.
	ErrInvalidScope = internal.ErrInvalidScope
	// ErrUnknownDriver — the named artifact driver is not registered.
	ErrUnknownDriver = internal.ErrUnknownDriver
	// ErrStoreClosed — the store has been closed.
	ErrStoreClosed = internal.ErrStoreClosed
	// ErrPresignUnsupported — this driver mints no presigned URLs.
	ErrPresignUnsupported = internal.ErrPresignUnsupported
)

// Open resolves the configured artifact driver and opens it.
var Open = internal.Open

// OpenDriver opens an artifact driver by explicit name.
var OpenDriver = internal.OpenDriver

// NewScoped binds a store to one ArtifactScope.
var NewScoped = internal.NewScoped

// RegisteredDrivers lists the seated artifact driver names
// (blank-import sdk/drivers/prod to seat the production set).
var RegisteredDrivers = internal.RegisteredDrivers
