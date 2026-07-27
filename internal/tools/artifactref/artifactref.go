// Package artifactref carries Harbor's in-process pass-by-reference
// tool-argument routing: a tool declares an artifact-reference
// PARAMETER in its typed input struct, the model supplies an artifact
// id, and the runtime resolves that id to the stored bytes at DISPATCH
// so the tool function reads content the model never saw.
//
// Because in-process tool input schemas are reflection-derived from the
// Go type (internal/tools/drivers/inproc.RegisterFunc ->
// internal/tools/schema.Derive), a reference parameter is a declared
// FIELD TYPE rather than a hand-written schema convention:
//
//	type SummarizeArgs struct {
//	    Doc      artifactref.Ref `json:"doc"`
//	    MaxWords int             `json:"max_words"`
//	}
//
//	func summarize(ctx context.Context, in SummarizeArgs) (Out, error) {
//	    body, err := in.Doc.Bytes() // the store's bytes, never the model's
//	    ...
//	}
//
// The derived schema renders `doc` as a plain string, so the model
// authors `{"doc": "tool_ab12cd34ef56", "max_words": 200}` and never
// authors — or reads — content.
//
// # The substitution invariant
//
// A resolved artifact value never re-enters the model's context or the
// observable record. When the runtime substitutes a resolved value into
// a dispatched tool argument, that value is DISPATCH-LOCAL: it does not
// appear in the trajectory, in the observation the runtime interleaves
// into the next chat thread, in any canonical event payload, in an audit
// payload, or in a log. The model authored an id and continues to see an
// id; the substitution is the runtime's and it ends at the tool boundary.
//
// The invariant is held three ways, in descending order of durability:
//
//  1. A bound on PRODUCTION. [Substitute] is the ONE call site at which a
//     resolved value enters a dispatched argument, and [ScanSubstitutionSites]
//     holds it to one mechanically. An invariant about where a value does
//     not travel is enforced most durably by bounding where it is
//     produced; enumerating every place it might arrive is the check that
//     goes stale.
//
//  2. A projection bound on the CARRIER. [Ref] keeps the resolved bytes in
//     an unexported field and projects itself as its id through every
//     serialisation door the runtime has: [Ref.MarshalJSON] emits the id,
//     [Ref.String] returns the id, and [Ref.LogValue] renders the id. A
//     `Ref` that reaches `json.Marshal`, `fmt.Sprintf` or `slog` therefore
//     emits an id rather than content, by construction rather than by
//     discipline.
//
//  3. An arrival check at the LLM edge, which lives with the safety net
//     rather than here (internal/llm.findContextLeak).
//
// # Extent
//
// This is the IN-PROCESS arm. The consumer runs inside the Runtime, so
// resolution is a store read and a struct field: no URL, no third party,
// no egress. Handing a remote consumer something it can dereference is a
// separate, deliberately unbuilt design — see the routing notes in
// docs/decisions.md.
//
// # Concurrent reuse
//
// Nothing in this package holds mutable package-level state. A [Ref] is
// decoded fresh per invocation and lives on the per-call argument value;
// the resolver rides the dispatch ctx, scoped to the dispatching run's
// identity, and is never read from a shared artifact.
package artifactref

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
)

// ErrNoResolver is returned when a tool argument carries an artifact
// reference but the dispatch context seats no resolver. It is a loud
// failure on purpose: a silently unresolved reference would hand the
// tool function a zero value and let the call proceed against nothing,
// which is the silent-degradation shape Harbor forbids.
var ErrNoResolver = errors.New("artifactref: no artifact resolver on the dispatch context")

// ErrUnresolved is returned by [Ref.Bytes] when the reference was never
// resolved — either the argument omitted it, or the value was built
// outside a dispatch that ran [Substitute].
var ErrUnresolved = errors.New("artifactref: reference was not resolved at dispatch")

// ErrEmptyID is returned when an argument supplies an artifact
// reference whose id is empty. An omitted optional reference and an
// explicitly empty one are different facts, and only the second is a
// malformed argument.
var ErrEmptyID = errors.New("artifactref: artifact reference id is empty")

// ErrNotAddressable is returned by [Substitute] when it is handed
// something it cannot write through — anything other than a non-nil
// pointer to the decoded argument value.
var ErrNotAddressable = errors.New("artifactref: substitution target is not addressable")

// maxWalkDepth bounds the reflective walk so a pathological or cyclic
// argument type fails loud instead of recursing without end. Mirrors
// the derivation depth bound the schema package applies to the same
// types.
const maxWalkDepth = 32

// Ref is an artifact-reference tool PARAMETER.
//
// On the wire (and in the derived JSON Schema) a Ref is a plain string:
// the artifact id the model authors. In the tool function it is a handle
// the runtime has already resolved — call [Ref.Bytes] for the stored
// content.
//
// The resolved value is unexported and every serialisation door projects
// the id instead: [Ref.MarshalJSON], [Ref.String] and [Ref.LogValue].
// That is what makes "the resolved value never reaches the observable
// record" a property of the type rather than a rule contributors
// remember.
//
// The zero Ref is a valid "not supplied" value: [Ref.ID] is empty and
// [Ref.Bytes] fails with [ErrUnresolved].
type Ref struct {
	id string
	// present records that the argument actually carried this field, so
	// an omitted optional reference is distinguishable from one supplied
	// as an empty string.
	present bool
	value   []byte
	// resolved records that Substitute bound `value`. A nil-but-resolved
	// value (a legitimately empty artifact) stays distinguishable from an
	// unresolved one.
	resolved bool
}

// NewRef builds a Ref carrying id and no resolved value. It exists for
// callers assembling arguments in Go (tests, typed embedders) rather
// than decoding them from a model's JSON; the resulting Ref resolves at
// dispatch exactly as a decoded one does.
func NewRef(id string) Ref {
	return Ref{id: id, present: true}
}

// ID returns the artifact reference id the argument carried. It is the
// only part of a Ref that is safe to log, emit or persist.
func (r Ref) ID() string { return r.id }

// Supplied reports whether the argument carried this reference at all.
// An omitted optional reference reports false.
func (r Ref) Supplied() bool { return r.present }

// Resolved reports whether the runtime bound stored content to this
// reference at dispatch.
func (r Ref) Resolved() bool { return r.resolved }

// Bytes returns the stored artifact content the runtime resolved at
// dispatch. The returned slice is the tool's to read and MUST NOT be
// retained past the call or mutated — it is the per-invocation buffer,
// not a copy.
//
// Fails with [ErrUnresolved] when the reference was never resolved,
// rather than returning an empty slice a caller could mistake for an
// empty artifact.
func (r Ref) Bytes() ([]byte, error) {
	if !r.resolved {
		if r.id == "" {
			return nil, fmt.Errorf("%w: no artifact reference was supplied", ErrUnresolved)
		}
		return nil, fmt.Errorf("%w: %q", ErrUnresolved, r.id)
	}
	return r.value, nil
}

// Size returns the byte length of the resolved content, or 0 when the
// reference is unresolved.
func (r Ref) Size() int { return len(r.value) }

// String renders the reference as its id, so a `%v` or `%s` verb over a
// tool's argument struct prints ids and never content.
func (r Ref) String() string { return r.id }

// LogValue renders the reference as its id, so `slog.Any("args", in)`
// over a tool's argument struct logs ids and never content.
func (r Ref) LogValue() slog.Value { return slog.StringValue(r.id) }

// MarshalJSON emits the reference id. A Ref that reaches an event
// payload, a trajectory step, an audit view or a re-encoded argument
// map therefore serialises as the id the model authored — the
// substitution invariant expressed as a type property.
func (r Ref) MarshalJSON() ([]byte, error) { return json.Marshal(r.id) }

// UnmarshalJSON accepts the artifact id as a JSON string. A JSON null
// leaves the reference unsupplied.
func (r *Ref) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*r = Ref{}
		return nil
	}
	var id string
	if err := json.Unmarshal(b, &id); err != nil {
		return fmt.Errorf("artifactref: artifact reference must be a JSON string: %w", err)
	}
	*r = Ref{id: id, present: true}
	return nil
}

var (
	_ json.Marshaler   = Ref{}
	_ json.Unmarshaler = (*Ref)(nil)
	_ fmt.Stringer     = Ref{}
	_ slog.LogValuer   = Ref{}
)

// Resolver resolves an artifact reference id to its stored bytes. The
// dispatching runtime seats one on the ctx, closed over the run's own
// identity scope, so a tool reaches the bytes its own run's identity
// reaches and no others — there is no identity logic in any tool.
//
// A resolver reports a missing artifact as an error, never as empty
// content: a reference the model authored against nothing is a fact the
// tool must be told about.
type Resolver interface {
	ResolveArtifact(ctx context.Context, id string) ([]byte, error)
}

// ResolverFunc adapts a function to [Resolver].
type ResolverFunc func(ctx context.Context, id string) ([]byte, error)

// ResolveArtifact implements [Resolver].
func (f ResolverFunc) ResolveArtifact(ctx context.Context, id string) ([]byte, error) {
	return f(ctx, id)
}

type resolverCtxKey struct{}

// WithResolver seats r on ctx for the dispatch that follows. The
// dispatching runtime is the only caller: the resolver it seats is
// closed over the run's identity scope, so seating one elsewhere would
// be handing a tool a reach its run does not have.
func WithResolver(ctx context.Context, r Resolver) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, resolverCtxKey{}, r)
}

// ResolverFrom returns the resolver seated on ctx, if any.
func ResolverFrom(ctx context.Context) (Resolver, bool) {
	r, ok := ctx.Value(resolverCtxKey{}).(Resolver)
	return r, ok && r != nil
}

// Substitute is THE ONE call site at which a resolved artifact value
// enters a dispatched tool argument.
//
// It walks the decoded argument value behind ptr, and for every supplied
// [Ref] it resolves the id through the resolver seated on ctx and binds
// the stored bytes onto that reference. Everything it writes is
// dispatch-local: the bytes live on the per-invocation argument value
// and are unreachable through any of [Ref]'s serialisation doors.
//
// It fails loudly rather than degrading: a supplied reference with no
// seated resolver is [ErrNoResolver], an empty id is [ErrEmptyID], and a
// resolver error is wrapped with the id that produced it. An UNSUPPLIED
// reference is left alone — an optional parameter the model omitted is
// not an error, and [Ref.Bytes] fails loudly if the tool reads it
// anyway.
//
// Calls to this function are held to a reviewed list by
// [ScanSubstitutionSites]. Adding a second call site is a reviewed
// decision, not an edit.
func Substitute(ctx context.Context, ptr any) error {
	if ptr == nil {
		return fmt.Errorf("%w: nil", ErrNotAddressable)
	}
	rv := reflect.ValueOf(ptr)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("%w: want a non-nil pointer to the decoded argument value, got %T", ErrNotAddressable, ptr)
	}
	return substituteValue(ctx, rv.Elem(), 0)
}

var refType = reflect.TypeOf(Ref{})

func substituteValue(ctx context.Context, v reflect.Value, depth int) error {
	if depth > maxWalkDepth {
		return fmt.Errorf("artifactref: argument walk exceeded depth %d", maxWalkDepth)
	}
	if !v.IsValid() {
		return nil
	}
	if v.Type() == refType {
		return bindRef(ctx, v)
	}
	if !TypeContainsRef(v.Type()) {
		return nil
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		return substituteValue(ctx, v.Elem(), depth+1)
	case reflect.Struct:
		t := v.Type()
		for i := range t.NumField() {
			// Unexported fields are unreachable through reflection's
			// write path, and the schema deriver skips them too — so a
			// reference cannot be declared there and cannot be bound
			// there.
			if !t.Field(i).IsExported() {
				continue
			}
			if err := substituteValue(ctx, v.Field(i), depth+1); err != nil {
				return err
			}
		}
		return nil
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if err := substituteValue(ctx, v.Index(i), depth+1); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		// Map elements are not addressable, so each is copied into an
		// addressable temporary, bound, and written back.
		elemType := v.Type().Elem()
		for _, key := range v.MapKeys() {
			tmp := reflect.New(elemType).Elem()
			tmp.Set(v.MapIndex(key))
			if err := substituteValue(ctx, tmp, depth+1); err != nil {
				return err
			}
			v.SetMapIndex(key, tmp)
		}
		return nil
	default:
		return nil
	}
}

// bindRef resolves one reference and writes the stored bytes onto it.
// v is an addressable Ref.
func bindRef(ctx context.Context, v reflect.Value) error {
	if !v.CanAddr() {
		return fmt.Errorf("%w: artifact reference field is not addressable", ErrNotAddressable)
	}
	ref, ok := v.Addr().Interface().(*Ref)
	if !ok {
		return fmt.Errorf("%w: artifact reference field is not a *Ref", ErrNotAddressable)
	}
	if !ref.present {
		return nil
	}
	if ref.id == "" {
		return ErrEmptyID
	}
	if ref.resolved {
		return nil
	}
	resolver, ok := ResolverFrom(ctx)
	if !ok {
		return fmt.Errorf("%w: cannot resolve %q", ErrNoResolver, ref.id)
	}
	data, err := resolver.ResolveArtifact(ctx, ref.id)
	if err != nil {
		return fmt.Errorf("artifactref: resolve %q: %w", ref.id, err)
	}
	ref.value = data
	ref.resolved = true
	return nil
}

// TypeContainsRef reports whether t can hold a [Ref] anywhere a
// reflective walk could reach and write. Callers precompute it once per
// registered tool so an argument type with no reference parameter never
// pays for the walk.
//
// Interface-typed fields report false: a JSON decode cannot produce a
// Ref inside an `any`, and reflection cannot write through one. The
// schema deriver treats an empty interface as unconstrained for the same
// reason, so the two agree on what a reference parameter can be.
func TypeContainsRef(t reflect.Type) bool {
	return typeContainsRef(t, 0, make(map[reflect.Type]bool))
}

func typeContainsRef(t reflect.Type, depth int, visiting map[reflect.Type]bool) bool {
	if t == nil || depth > maxWalkDepth {
		return false
	}
	if t == refType {
		return true
	}
	if visiting[t] {
		return false
	}
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		visiting[t] = true
		defer delete(visiting, t)
		return typeContainsRef(t.Elem(), depth+1, visiting)
	case reflect.Map:
		visiting[t] = true
		defer delete(visiting, t)
		return typeContainsRef(t.Elem(), depth+1, visiting)
	case reflect.Struct:
		visiting[t] = true
		defer delete(visiting, t)
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			if typeContainsRef(f.Type, depth+1, visiting) {
				return true
			}
		}
		return false
	default:
		return false
	}
}
