// runtyped.go — the typed embed binding: a generic free function that
// derives a run's output schema from a Go type, drives RunOnce with
// WithOutputSchema, and unmarshals the validated answer payload into
// that type. It is sugar over RunOnce + WithOutputSchema — no second
// construction path, no second validation loop, no second output
// shaping (CLAUDE.md §13 "two parallel implementations").
//
// Deliberately NOT a stateful binding object: RunTyped is a free
// function over the shared immutable *Stack. Identity stays a per-call
// argument, exactly like RunOnce. It is also deliberately NOT named
// "Agent" — that noun is taken twice in Harbor's vocabulary
// (harbortest.Agent, the Agent Registry's registration entities whose
// agent_id is explicitly not an isolation principal).
package assemble

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools/schema"
)

// Sentinel errors RunTyped returns for its two typed-binding-specific
// failure modes. Callers compare via errors.Is. Schema-derivation
// failures and schema-invalidation failures surface through
// schema.ErrUnsupportedType and planner.ErrOutputInvalid respectively
// (RunTyped adds no new sentinel for those — it reuses the mechanism's
// own typed errors, per the "one mechanism" rule this binding deepens).
var (
	// ErrRunTypedSchemaConflict is returned when the caller supplies a
	// WithOutputSchema option alongside RunTyped. RunTyped derives its
	// own schema from the type parameter T; a caller-supplied schema
	// would silently either conflict or be silently overridden — both
	// forbidden (CLAUDE.md §13 "silent degradation"). The caller must
	// remove the explicit WithOutputSchema, or call RunOnce directly
	// when a hand-authored schema is required.
	ErrRunTypedSchemaConflict = errors.New("assemble: RunTyped: a WithOutputSchema option was supplied alongside RunTyped (RunTyped derives its own schema from T)")

	// ErrRunTypedUnmarshal is returned when the runtime's own schema
	// validation passed but json.Unmarshal into T failed anyway — this
	// is "impossible by construction" territory (CLAUDE.md §5): the
	// schema derived FROM T should always accept a payload shaped like
	// T. It can still happen (a narrower numeric type than the derived
	// schema's unconstrained "integer"/"number", for instance) so
	// RunTyped surfaces it loud rather than returning a zero value with
	// a nil error (CLAUDE.md §13 "silent degradation").
	ErrRunTypedUnmarshal = errors.New("assemble: RunTyped: the validated answer payload failed to unmarshal into the target type")
)

// RunTyped derives a JSON Schema from T (via the shared
// internal/tools/schema deriver — the SAME implementation
// internal/tools/drivers/inproc.RegisterFunc consumes for tool
// registration, §13), appends it as WithOutputSchema, drives RunOnce,
// and unmarshals the validated answer_payload into T.
//
// Ordering (binding):
//  1. The schema is derived from T FIRST, before any option processing
//     or LLM spend. An unsupported T (a non-empty interface, a channel
//     or function field, a cyclic structure) fails loud here with the
//     derivation's typed error (errors.Is(err, schema.ErrUnsupportedType))
//     — never a wasted run.
//  2. A caller-supplied WithOutputSchema alongside RunTyped is a loud
//     conflict (ErrRunTypedSchemaConflict), never a silent override —
//     RunTyped owns the schema for the call it drives.
//  3. RunOnce executes exactly as it would for any other schema-
//     constrained call: the terminal answer is validated against the
//     derived schema for EVERY planner, and (for the React driver) the
//     generation-steering + corrective-retry loop is engaged. A
//     schema-invalid answer after the correction budget returns
//     planner.ErrOutputInvalid, unwrapped from RunOnce — RunTyped adds
//     no second error shape for this case.
//  4. The validated raw JSON (env.AnswerPayload) is unmarshaled into a
//     fresh T. A payload that validated against the schema but still
//     fails to unmarshal into T is ErrRunTypedUnmarshal — never a zero
//     value returned alongside a nil error.
//
// On any error RunTyped returns the zero value of T alongside whatever
// AnswerEnvelope RunOnce itself returned (zero-valued on a pre-run
// failure), so callers that only check the error never observe a
// misleadingly-populated T.
//
// Concurrent reuse: RunTyped is a free function over the shared
// immutable *Stack, safe to call from N concurrent goroutines. The
// schema is derived FRESH on every call (no reflect.Type-keyed cache)
// — per-call derivation is the default absent a benchmark showing a
// cache is not a wash; this keeps RunTyped free of any package-level
// mutable state to reason about under the concurrent-reuse contract.
func RunTyped[T any](
	ctx context.Context,
	s *Stack,
	goal string,
	id identity.Identity,
	opts ...RunOption,
) (T, planner.AnswerEnvelope, error) {
	var zero T

	// 1. Derive the schema from T BEFORE any run starts / any LLM
	// spend — an unsupported T fails loud here.
	derived, err := schema.Derive(reflect.TypeOf(zero))
	if err != nil {
		return zero, planner.AnswerEnvelope{}, fmt.Errorf("assemble: RunTyped[%T]: derive output schema: %w", zero, err)
	}
	schemaBytes, err := json.Marshal(derived)
	if err != nil {
		return zero, planner.AnswerEnvelope{}, fmt.Errorf("assemble: RunTyped[%T]: marshal derived schema: %w", zero, err)
	}

	// 2. A caller-supplied WithOutputSchema alongside RunTyped is a
	// loud conflict, never a silent override. runOnceConfig is this
	// package's own private type, so RunTyped (same package as
	// RunOnce) can probe the caller's opts directly without a second
	// config-parsing path.
	var probe runOnceConfig
	for _, o := range opts {
		o(&probe)
	}
	if probe.outputSchemaSet {
		return zero, planner.AnswerEnvelope{}, fmt.Errorf("assemble: RunTyped[%T]: %w", zero, ErrRunTypedSchemaConflict)
	}

	// 3. Run with the derived schema appended. The caller's own opts
	// come first so WithRunID / WithInputArtifacts / WithStream still
	// apply exactly as they would for a plain RunOnce call.
	allOpts := make([]RunOption, 0, len(opts)+1)
	allOpts = append(allOpts, opts...)
	allOpts = append(allOpts, WithOutputSchema(schemaBytes))

	env, err := s.RunOnce(ctx, goal, id, allOpts...)
	if err != nil {
		return zero, env, fmt.Errorf("assemble: RunTyped[%T]: %w", zero, err)
	}

	// 4. Unmarshal the validated payload into a fresh T. A schema-valid
	// payload that still fails to unmarshal is loud, typed, and never a
	// zero-value-with-nil-error return.
	var out T
	if uErr := json.Unmarshal(env.AnswerPayload, &out); uErr != nil {
		return zero, env, fmt.Errorf("%w: RunTyped[%T]: %w", ErrRunTypedUnmarshal, zero, uErr)
	}
	return out, env, nil
}
