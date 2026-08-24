// Package grant is the public SDK facade for Harbor's reference
// context-bound external execution grant signer and verifier. It exposes no
// provider secret and does not grant authority to caller-supplied identity.
package grant

import (
	internal "github.com/hurtener/Harbor/internal/llm/grant"
)

// Signer creates coordinator-signed grants.
type Signer = internal.Signer

// Verifier validates a signed grant against a verified runtime request.
type Verifier = internal.Verifier

// VerifierConfig configures audience, runtime, key and optional route fences.
type VerifierConfig = internal.VerifierConfig

// Binding is an opaque credential asset held by the resolver.
type Binding = internal.Binding

// BindingStore is the reference in-memory resolver for embedded runtimes and
// tests; production hosts may implement llm.CredentialResolver instead.
type BindingStore = internal.BindingStore

// NewSigner constructs the reference Ed25519 signer.
var NewSigner = internal.NewSigner

// NewVerifier constructs a fail-closed signed-grant verifier.
var NewVerifier = internal.NewVerifier

// NewBindingStore constructs an empty opaque credential resolver.
var NewBindingStore = internal.NewBindingStore

// CanonicalBodyHash computes the deterministic unsigned grant hash.
var CanonicalBodyHash = internal.CanonicalBodyHash

// ErrInvalidGrantShape identifies malformed signed claims.
var ErrInvalidGrantShape = internal.ErrInvalidGrantShape

// ErrUnknownKey identifies an untrusted key id.
var ErrUnknownKey = internal.ErrUnknownKey

// ErrBindingNotFound identifies an unknown credential binding handle.
var ErrBindingNotFound = internal.ErrBindingNotFound
