// Package audit is the public SDK facade over Harbor's internal/audit
// package — the mandatory redaction seam every payload passes through
// before leaving the process (RFC §3.6; D-204/D-206). Alias-based
// re-exports only: no behavior lives here. Added in Phase 112b: the
// external consumers flushed the gap out — sdk/events.Open and
// harbortest.Deps both take an audit.Redactor, which 112a's inventory
// left unnameable and unconstructible outside the module. Driver
// registration (Register) and the rule vocabulary internals are
// deliberately private.
package audit

import (
	internal "github.com/hurtener/Harbor/internal/audit"
)

// Redactor + rule vocabulary — aliases of the internal types.
type (
	// Redactor redacts a payload before it is logged, emitted, or
	// persisted. Every Harbor signal passes through one.
	Redactor = internal.Redactor
	// Rule is one redaction step a driver composes.
	Rule = internal.Rule
)

// DefaultDriver is the production redactor driver name ("patterns").
const DefaultDriver = internal.DefaultDriver

// ErrUnknownDriver — the named redactor driver is not registered.
// Callers compare via errors.Is.
var ErrUnknownDriver = internal.ErrUnknownDriver

// Open resolves the default redactor driver and opens it
// (blank-import sdk/drivers/prod to seat the production set; the
// production driver name is "patterns").
var Open = internal.Open

// OpenDriver opens a redactor driver by explicit name.
var OpenDriver = internal.OpenDriver

// RegisteredDrivers lists the seated redactor driver names.
var RegisteredDrivers = internal.RegisteredDrivers

// WithRedactor returns a child context carrying the redactor.
var WithRedactor = internal.WithRedactor

// From extracts the redactor from ctx, reporting presence.
var From = internal.From

// MustFrom extracts the redactor from ctx, panicking when absent.
var MustFrom = internal.MustFrom
