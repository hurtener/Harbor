// runtyped.go — the facade's SECOND documented generic-func carve-out
// (D-273, amending D-205 item 1): Go has no generic function values,
// so RunTyped cannot be expressed as a `var = internal.RunTyped`
// forward the way every other re-export in this tree is. The wrapper
// body is exactly one return statement and adds no behavior — the
// IDENTICAL rationale and shape as the facade's first carve-out,
// sdk/tools/inproc.RegisterFunc. The no-behavior smoke's func
// allow-list is the enumerated set of exactly these two.
package assemble

import (
	"context"

	internal "github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/sdk/identity"
	"github.com/hurtener/Harbor/sdk/planner"
)

// Re-exported sentinel errors callers compare via errors.Is.
var (
	// ErrRunTypedSchemaConflict — a WithOutputSchema option was
	// supplied alongside RunTyped; RunTyped derives its own schema
	// from T.
	ErrRunTypedSchemaConflict = internal.ErrRunTypedSchemaConflict
	// ErrRunTypedUnmarshal — the validated answer payload failed to
	// unmarshal into the target type T.
	ErrRunTypedUnmarshal = internal.ErrRunTypedUnmarshal
)

// RunTyped derives a JSON Schema from T (via Harbor's shared Go-type
// derivation — the same machinery sdk/tools/inproc.RegisterFunc uses
// for tool registration), drives goal through Stack.RunOnce with that
// schema applied, and unmarshals the validated answer payload into T.
//
// An unsupported T (a non-empty interface, a channel or function
// field, a cyclic structure) fails loud at call time, before any run
// starts. A caller-supplied WithOutputSchema alongside RunTyped is a
// loud conflict (ErrRunTypedSchemaConflict) — RunTyped owns the schema
// for the call it drives. A schema-invalid terminal answer surfaces
// planner.ErrOutputInvalid, exactly as it would from a hand-rolled
// RunOnce + WithOutputSchema call. A payload that validated against
// the schema but still failed to unmarshal into T is
// ErrRunTypedUnmarshal — RunTyped never returns a zero value alongside
// a nil error.
//
// Thin generic forward over the internal runner; no behavior is added
// here (CLAUDE.md §13, D-273).
func RunTyped[T any](
	ctx context.Context,
	s *Stack,
	goal string,
	id identity.Identity,
	opts ...RunOption,
) (T, planner.AnswerEnvelope, error) {
	return internal.RunTyped[T](ctx, s, goal, id, opts...)
}
