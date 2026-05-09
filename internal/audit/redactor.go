// Package audit owns Harbor's deep-redaction pass. Every emit path
// (event bus, logger, future Governance LLM-boundary hook) MUST run
// payloads through Redactor.Redact before persistence or transmission.
//
// The contract is fail-loudly: a Rule that returns an error means
// "do not emit." Callers that get an error from Redact must NOT fall
// back to the original payload. Tests pin this behaviour.
//
// The Redactor is a canonical reusable artifact (D-025): one instance
// is opened at boot via Open and shared across every emit path. It
// is safe to call Redact concurrently from N goroutines on the same
// instance. No per-run state lives on the Redactor itself.
package audit

import (
	"context"
	"errors"
)

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrRedactionFailed wraps any failure from a Rule.Apply. It is
	// the contract surface for "do not emit": when Redact returns an
	// error wrapping this sentinel, the caller MUST NOT persist or
	// transmit the original payload.
	ErrRedactionFailed = errors.New("audit: redaction failed")

	// ErrRedactorMissing — the context carries no Redactor. Returned
	// (or panicked, via MustFrom) when an emit path is reached
	// without a runtime-attached Redactor.
	ErrRedactorMissing = errors.New("audit: no Redactor in context")

	// ErrRedactionDepthExceeded — the deep-walk hit the depth cap.
	// Defended against pathologically nested or cyclic payloads.
	ErrRedactionDepthExceeded = errors.New("audit: redaction depth exceeded")

	// ErrUnknownDriver — Open was asked for a driver name that no
	// registered factory handles. The error text lists the names
	// currently registered so misconfigurations are obvious.
	ErrUnknownDriver = errors.New("audit: unknown driver")
)

// Redactor produces a deep-redacted copy of payload.
//
// Contract:
//
//   - Redact MUST NOT mutate its input.
//   - On nil error, the returned value is safe to persist or transmit.
//   - On non-nil error, the caller MUST treat the error as "do not
//     emit" — never persist or transmit the original payload as a
//     fallback. The returned value is undefined and may be nil.
//   - Implementations must be safe for concurrent use by N goroutines
//     against a single shared instance (D-025 concurrent reuse).
type Redactor interface {
	Redact(ctx context.Context, payload any) (any, error)
}

// Rule is one redaction step. Drivers compose rules and apply them
// in deterministic order; on the first error the driver returns
// (nil, wrapped error) — the fail-loudly contract from the package
// godoc.
//
// Apply MUST return a deep-copied payload; mutating the input is a
// bug. Rule.Name() is exposed via the patterns driver's Names()
// method so an operator can confirm which rules ran.
type Rule interface {
	Apply(ctx context.Context, payload any) (any, error)
	Name() string
}

// ctxKey is the unexported key under which the runtime stashes the
// Redactor on a context. The redactor key is intentionally NOT the
// same as identity.* keys — they have independent lifetimes.
type ctxKey int

const redactorKey ctxKey = iota

// WithRedactor attaches r to ctx so downstream emit paths can recover
// it via MustFrom or From.
func WithRedactor(ctx context.Context, r Redactor) context.Context {
	return context.WithValue(ctx, redactorKey, r)
}

// MustFrom returns the Redactor in ctx. Panics with ErrRedactorMissing
// when none is present. Use in handler / event-emit paths where a
// Redactor is mandatory.
func MustFrom(ctx context.Context) Redactor {
	r, ok := From(ctx)
	if !ok {
		panic(ErrRedactorMissing)
	}
	return r
}

// From returns the Redactor in ctx and a presence bool. Use when
// absence is recoverable.
func From(ctx context.Context) (Redactor, bool) {
	r, ok := ctx.Value(redactorKey).(Redactor)
	return r, ok
}
