// Package credentials exposes Harbor's canonical, provider-neutral external
// grant credential-resolution exchange to coordinators and runtime hosts.
package credentials

import internal "github.com/hurtener/Harbor/internal/llm/credentials"

// Request carries only an already-verified complete signed grant.
type Request = internal.Request

// Response carries one exact generation-bound provider credential.
type Response = internal.Response

const (
	// Version is the credential-resolution contract version.
	Version = internal.Version
	// MaxRequestBytes bounds the canonical request.
	MaxRequestBytes = internal.MaxRequestBytes
	// MaxResponseBytes bounds the canonical response.
	MaxResponseBytes = internal.MaxResponseBytes
)

var (
	// ErrInvalidRequest identifies invalid canonical request bytes.
	ErrInvalidRequest = internal.ErrInvalidRequest
	// ErrInvalidResponse identifies invalid or mismatched response bytes.
	ErrInvalidResponse = internal.ErrInvalidResponse
	// NewRequest validates a coordinator-bound grant for resolution.
	NewRequest = internal.NewRequest
	// MarshalCanonicalRequest emits the exact request wire.
	MarshalCanonicalRequest = internal.MarshalCanonicalRequest
	// UnmarshalCanonicalRequest parses the exact request wire.
	UnmarshalCanonicalRequest = internal.UnmarshalCanonicalRequest
	// MarshalCanonicalResponse emits the exact response wire.
	MarshalCanonicalResponse = internal.MarshalCanonicalResponse
	// ParseCanonicalResponse parses and binds the exact response wire.
	ParseCanonicalResponse = internal.ParseCanonicalResponse
)
