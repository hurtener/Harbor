// caller_memory.go — the ONE composition home for caller-supplied
// memory.
//
// A Protocol client may contribute content to a run's
// `<read_only_external_memory>` tier. That tier renders a MAP, and this
// file is what makes the map a composition point rather than a slot two
// producers fight over: the runtime's own recall writes its keys, this
// helper adds exactly one more, and neither can displace the other.
//
// The caller names no key. It supplies only a value, which lands under
// the fixed runtime-owned [CallerSuppliedKey]. That is the whole
// collision-freedom argument: runtime producers may add sibling keys
// forever and can never collide with a caller, and a caller can never
// shadow, rename or displace a runtime key. It is also what keeps
// provenance legible — a model, a trace reader and a log reader all see
// which half of the tier came from where.

package runctx

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/planner"
)

// CallerSuppliedKey is the fixed, runtime-owned `External` tier map key
// under which caller-supplied memory is composed. Runtime producers MUST
// NOT write it, and no caller input can name it.
const CallerSuppliedKey = "caller_supplied"

// ExternalTierName is the observability label for the `External` memory
// tier — the name a run-loop driver reports on the admission event so a
// Console reader can tell WHICH tier caller-supplied content reached
// without inspecting the prompt.
//
// It matches the ReAct planner's wrapper tag for that tier. It is a
// LABEL, not the renderer's source of truth: the planner owns how it
// renders its own wrappers, and nothing here feeds prompt assembly.
const ExternalTierName = "read_only_external_memory"

// ErrCallerMemoryInvalid is returned by [ComposeCallerMemory] for a
// payload it will not admit: an explicit JSON `null`, or bytes that are
// not a valid JSON document. It is a sentinel so a run-loop driver can
// classify the failure without string matching.
//
// The Protocol edge already refuses both shapes before a task exists;
// this check is the second, in-process gate for a headless caller that
// constructs a run without passing through the Protocol. Neither gate
// degrades silently (CLAUDE.md §13).
var ErrCallerMemoryInvalid = errors.New("runctx: caller-supplied memory is not admissible")

// ErrCallerMemoryTierShape is returned when the supplied MemoryBlocks
// carries an `External` tier that is not a `map[string]any` and therefore
// cannot be composed into at key granularity.
//
// This is a fail-loud on a broken runtime invariant, not a caller error:
// every runtime producer of the External tier writes a map. Overwriting
// a non-map tier would silently destroy whatever the other producer put
// there, which is precisely the two-producers-on-one-slot failure this
// design exists to avoid.
var ErrCallerMemoryTierShape = errors.New("runctx: External memory tier is not a map and cannot be composed into")

// ComposeCallerMemory composes caller-supplied content into the run's
// `External` memory tier under [CallerSuppliedKey].
//
// It returns a NEW *planner.MemoryBlocks and never mutates its argument:
// the run loop's value is derived from identity-scoped store reads and
// must stay untouched.
//
// The no-aliasing guarantee is scoped to the tier this function WRITES.
// `External` is rebuilt as a fresh map whose entries are copied across,
// so a later write to either side's `External` cannot be observed
// through the other. `Conversation` is carried across BY REFERENCE — it
// is an `any` this function never reads into and never writes, so
// copying it would mean a reflective or round-trip deep copy of an
// arbitrary value, and a partial (top-level-only) copy would be worse
// than none: a half-true invariant is what a future author reasons
// from. The sharing is safe today because nothing downstream writes the
// tier — the planner's renderer only reads it — and it is pinned by a
// test asserting BOTH halves, the copy AND the share, so a change to
// either forces this sentence to be re-derived rather than silently
// invalidated.
//
// Behaviour:
//   - An empty raw returns mb unchanged and byte-identically (the same
//     pointer), so a run that supplied no caller memory is indistinguish-
//     able from one made before the field existed.
//   - A nil mb with a non-empty raw allocates a MemoryBlocks carrying
//     only the External tier — composition must also work for a session
//     with no stored memory at all, and for a runtime with no memory
//     subsystem configured.
//   - An explicit JSON `null`, or invalid JSON, returns
//     [ErrCallerMemoryInvalid]. It is never treated as "absent".
//   - `Conversation` is copied across untouched. It is a claim about the
//     session's stored turns that only the runtime may make, and it is
//     written unconditionally by [ProjectMemoryBlocks] — a second writer
//     would be silent last-writer-wins.
func ComposeCallerMemory(mb *planner.MemoryBlocks, raw json.RawMessage) (*planner.MemoryBlocks, error) {
	if len(raw) == 0 {
		return mb, nil
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("%w: not a valid JSON document", ErrCallerMemoryInvalid)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		// Unreachable given json.Valid above; kept because a silent
		// fallthrough here would admit an undecoded payload. The decode
		// error is joined rather than formatted so a caller can still
		// errors.Is on the sentinel AND errors.As on the syntax error.
		return nil, fmt.Errorf("%w: %w", ErrCallerMemoryInvalid, err)
	}
	if value == nil {
		return nil, fmt.Errorf("%w: explicit null (omit the value to supply no caller memory)", ErrCallerMemoryInvalid)
	}

	external := make(map[string]any, 2)
	var conversation any
	if mb != nil {
		conversation = mb.Conversation
		switch existing := mb.External.(type) {
		case nil:
			// No runtime producer wrote the tier; the caller's key is the
			// only one.
		case map[string]any:
			for k, v := range existing {
				external[k] = v
			}
		default:
			return nil, fmt.Errorf("%w: got %T", ErrCallerMemoryTierShape, mb.External)
		}
	}
	external[CallerSuppliedKey] = value

	return &planner.MemoryBlocks{
		External:     external,
		Conversation: conversation,
	}, nil
}
