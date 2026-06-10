// Package builtin is the public SDK facade over Harbor's
// internal/tools/builtin package — the runtime's built-in tool set
// (clock, text, artifact_fetch, tool/skill discovery, declarative
// actions) registered by name onto a catalog (RFC §3.6, §6.10;
// D-204). Alias-based re-exports only: no behavior lives here. The
// per-tool args/output structs and handler internals are deliberately
// private.
package builtin

import (
	internal "github.com/hurtener/Harbor/internal/tools/builtin"
)

// RegistryContext bundles the dependencies built-in tools need
// (catalog, stores, bus). Alias of the internal type.
type RegistryContext = internal.RegistryContext

// Re-exported sentinel errors callers compare via errors.Is.
var (
	// ErrUnknownBuiltIn — the name is not a known built-in tool.
	ErrUnknownBuiltIn = internal.ErrUnknownBuiltIn
	// ErrRegisterFailed — a built-in tool failed to register.
	ErrRegisterFailed = internal.ErrRegisterFailed
	// ErrIdentityRequired — built-ins are identity-mandatory.
	ErrIdentityRequired = internal.ErrIdentityRequired
)

// RegisterWith registers the named built-in tools with the full
// dependency bundle (stores, bus) a stateful built-in needs. The
// internal catalog-only Register variant is deprecated and therefore
// deliberately not re-exported (the curation contract).
var RegisterWith = internal.RegisterWith

// KnownNames lists every registrable built-in tool name.
var KnownNames = internal.KnownNames
