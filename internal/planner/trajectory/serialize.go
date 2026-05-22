package trajectory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
)

// Serialize returns the canonical JSON byte representation of the
// trajectory. On ANY non-JSON-encodable leaf, returns
// (nil, ErrUnserializable{Field: "<dotted.path>"}) — never silently.
//
// The contract (RFC §6.2 + §3.4 + brief 02 §4):
//
//   - Success: returns canonical JSON bytes; the round-trip
//     Serialize → Deserialize → Serialize is byte-identical for any
//     Trajectory whose `any`-valued fields hold JSON-tree shapes
//     (map[string]any / []any / primitives).
//   - Failure: returns (nil, ErrUnserializable{Field: <path>}). Field
//     names the offending leaf (e.g. "Trajectory.Steps[3].Action.fn").
//     Callers extract the path via errors.As.
//
// No silent-drop path. The predecessor's
// `try { json.dumps } catch { return None }` shape is rejected.
//
// Implementation: a reflective pre-flight walker validates every leaf
// is JSON-encodable; on the first non-encodable leaf it returns
// (nil, ErrUnserializable{Field: <path>}). If the walker passes, the
// trajectory is marshalled via the stdlib `encoding/json` (which
// alphabetically orders map keys; struct fields encode in declaration
// order per JSON tags). The two-pass design is deliberate — the
// walker provides the actionable field path, which `json.Marshal`'s
// `*UnsupportedTypeError` does not.
func (t *Trajectory) Serialize() ([]byte, error) {
	if t == nil {
		// A nil trajectory is unserialisable — fail loudly.
		return nil, ErrUnserializable{Field: "Trajectory"}
	}

	// Pre-flight: walk the trajectory reflectively, fail on the first
	// non-encodable leaf with a precise field path. ValidateEncodable
	// is the exported entry point — Serialize and external consumers
	// (the Phase 51 pause-record envelope) share the SAME walker.
	if err := ValidateEncodable(*t, "Trajectory"); err != nil {
		return nil, err
	}

	// Happy path: stdlib json.Marshal is the canonical encoder. Map
	// keys are alphabetised; struct fields encode per JSON tag in
	// declaration order. These two orderings combined produce the
	// byte-stable canonical form. The walker above mirrors json's
	// encoding rules, so json.Marshal here cannot fail on the happy
	// path; any future-tightened json.Marshal edge case would still
	// surface (encoding/json wraps such failures as
	// *UnsupportedTypeError, which the walker matches structurally —
	// see walkEncodable).
	return json.Marshal(t)
}

// ValidateEncodable reports whether v is fully JSON-encodable, failing
// loud with ErrUnserializable{Field: <dotted.path>} on the FIRST
// non-encodable leaf — never silently. It is the reusable primitive
// behind Trajectory.Serialize's pre-flight pass, exported so other
// runtime serialise contracts that share the fail-loudly invariant
// (the Phase 51 pause-record envelope is the first such consumer) walk
// the SAME walker rather than re-implementing it — re-implementing it
// would be the CLAUDE.md §13 two-parallel-implementations anti-pattern
// (RFC §3.4, D-049, D-069).
//
// root is the field-path prefix the returned error is rooted at — pass
// the consumer's own envelope name (e.g. "PauseRecord") so the error
// path is actionable from the caller's vocabulary, not "Trajectory".
//
// The encoding rules mirror encoding/json verbatim (see walkEncodable):
// chan / func / unsafe.Pointer / complex are rejected; nil interfaces /
// nil pointers / nil slices encode as JSON null; []byte encodes as
// base64; json.Marshaler implementers are probed; struct fields tagged
// json:"-" are skipped; cyclic graphs surface as
// ErrUnserializable{Field: ... <cycle>}.
//
// Callers extract the offending path via errors.As:
//
//	var unserr trajectory.ErrUnserializable
//	if errors.As(err, &unserr) {
//	    log.Printf("non-encodable leaf at %s", unserr.Field)
//	}
func ValidateEncodable(v any, root string) error {
	return walkEncodable(reflect.ValueOf(v), root, make(map[uintptr]struct{}))
}

// Deserialize parses canonical JSON bytes into a Trajectory. The
// returned *Trajectory has all `any`-valued fields decoded as the
// natural JSON tree (map[string]any / []any / float64 / string / bool
// / nil). The round-trip Serialize → Deserialize → Serialize is
// byte-identical for trajectories whose `any` fields were originally
// JSON-tree shapes.
//
// On a malformed input (invalid JSON, type mismatch on a typed field)
// Deserialize returns the underlying json error wrapped — Deserialize
// does NOT use the fail-loudly ErrUnserializable contract (that is
// strictly the Serialize-side concern).
func Deserialize(b []byte) (*Trajectory, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("trajectory: deserialize: empty input")
	}

	// Use a Decoder to surface the offending offset on malformed
	// inputs (slightly more actionable than json.Unmarshal's error).
	dec := json.NewDecoder(bytes.NewReader(b))

	var t Trajectory
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("trajectory: deserialize: %w", err)
	}
	return &t, nil
}

// walkEncodable recurses through v, returning ErrUnserializable for
// the first non-JSON-encodable leaf encountered. The fieldPath
// accumulates the dotted path so the returned error names the offending
// location.
//
// The visited map tracks pointer addresses to detect cyclic graphs;
// re-visiting a node returns ErrUnserializable with a "<cycle>" suffix.
//
// Encoding-rules summary (must mirror encoding/json):
//
//   - chan, func, unsafe.Pointer: ErrUnserializable.
//   - complex64, complex128: ErrUnserializable (json doesn't support).
//   - nil interface / nil pointer: encodable as JSON null.
//   - map keys must be string (or implement encoding.TextMarshaler);
//     non-string keys ErrUnserializable.
//   - struct fields with `json:"-"` tag are skipped from the walk
//     (they're skipped by Marshal too).
//   - cyclic graphs ErrUnserializable.
func walkEncodable(v reflect.Value, fieldPath string, visited map[uintptr]struct{}) error {
	// Unwrap interfaces eagerly so we walk the underlying concrete.
	for v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil // nil interface encodes as JSON null
		}
		v = v.Elem()
	}

	switch v.Kind() {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return nil // primitives are always JSON-encodable

	case reflect.Complex64, reflect.Complex128:
		return ErrUnserializable{Field: fieldPath}

	case reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return ErrUnserializable{Field: fieldPath}

	case reflect.Ptr:
		if v.IsNil() {
			return nil // nil pointer encodes as JSON null
		}
		// Cycle check on pointer types.
		addr := v.Pointer()
		if _, seen := visited[addr]; seen {
			return ErrUnserializable{Field: fieldPath + " <cycle>"}
		}
		visited[addr] = struct{}{}
		defer delete(visited, addr) // allow re-visit on sibling branches
		return walkEncodable(v.Elem(), fieldPath, visited)

	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			return nil
		}
		// []byte is JSON-encoded as a base64 string by encoding/json;
		// the bytes themselves are always encodable.
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return nil
		}
		for i := range v.Len() {
			childPath := fmt.Sprintf("%s[%d]", fieldPath, i)
			if err := walkEncodable(v.Index(i), childPath, visited); err != nil {
				return err
			}
		}
		return nil

	case reflect.Map:
		if v.IsNil() {
			return nil
		}
		// Cycle check on map header (maps are reference types).
		// reflect.Value.Pointer() works for maps too.
		addr := v.Pointer()
		if _, seen := visited[addr]; seen {
			return ErrUnserializable{Field: fieldPath + " <cycle>"}
		}
		visited[addr] = struct{}{}
		defer delete(visited, addr)

		// JSON requires string-shaped keys (or encoding.TextMarshaler).
		// Reject other key kinds.
		keyKind := v.Type().Key().Kind()
		switch keyKind {
		case reflect.String:
			// OK
		default:
			// Check for TextMarshaler. If not, fail loudly.
			textMarshalerType := reflect.TypeOf((*interface {
				MarshalText() ([]byte, error)
			})(nil)).Elem()
			if !v.Type().Key().Implements(textMarshalerType) {
				return ErrUnserializable{Field: fieldPath + " <non-string-map-key>"}
			}
		}

		iter := v.MapRange()
		for iter.Next() {
			k := iter.Key()
			val := iter.Value()
			keyStr := ""
			if k.Kind() == reflect.String {
				keyStr = k.String()
			} else {
				keyStr = fmt.Sprintf("%v", k.Interface())
			}
			childPath := fmt.Sprintf("%s.%s", fieldPath, keyStr)
			if err := walkEncodable(val, childPath, visited); err != nil {
				return err
			}
		}
		return nil

	case reflect.Struct:
		// time.Time and similar JSON-marshalable types implement
		// json.Marshaler. We honour that here — they are leaves.
		jsonMarshalerType := reflect.TypeOf((*json.Marshaler)(nil)).Elem()
		if v.Type().Implements(jsonMarshalerType) || reflect.PointerTo(v.Type()).Implements(jsonMarshalerType) {
			// Probe the marshaller to surface its error as
			// ErrUnserializable if it fails.
			var mv reflect.Value
			if v.Type().Implements(jsonMarshalerType) {
				mv = v
			} else {
				// PointerReceiver variant. We need an addressable
				// value; create one if not addressable.
				if v.CanAddr() {
					mv = v.Addr()
				} else {
					tmp := reflect.New(v.Type())
					tmp.Elem().Set(v)
					mv = tmp
				}
			}
			out := mv.MethodByName("MarshalJSON").Call(nil)
			if !out[1].IsNil() {
				err, _ := out[1].Interface().(error) //nolint:errcheck // type assertion on a checked-non-nil MarshalJSON error; nil fallback is fine
				return ErrUnserializable{Field: fmt.Sprintf("%s (MarshalJSON: %v)", fieldPath, err)}
			}
			return nil
		}

		// Walk exported fields per their JSON tag (matching
		// encoding/json's behaviour). Skip unexported fields and
		// fields tagged `json:"-"`.
		ty := v.Type()
		for i := range ty.NumField() {
			ft := ty.Field(i)
			if !ft.IsExported() {
				continue
			}
			tag, ok := ft.Tag.Lookup("json")
			fieldName := ft.Name
			if ok {
				// Tag of `-` means skip; otherwise the first
				// comma-segment is the on-wire name.
				if tag == "-" {
					continue
				}
				if comma := indexByte(tag, ','); comma >= 0 {
					tag = tag[:comma]
				}
				if tag != "" {
					fieldName = tag
				}
			}
			childPath := fieldPath + "." + fieldName
			if err := walkEncodable(v.Field(i), childPath, visited); err != nil {
				return err
			}
		}
		return nil

	}
	// Reflect.Invalid lands here (a zero reflect.Value, which only
	// occurs when v originated from a nil interface that was already
	// short-circuited above) and any kind we didn't enumerate.
	// Treating either as a JSON null is correct — encoding/json
	// emits null for the zero reflect.Value and never sees an
	// unknown kind because every Go kind is in the switch above.
	return nil
}

// indexByte is a tiny inlined helper — avoids importing strings for
// one call.
func indexByte(s string, c byte) int {
	for i := range len(s) {
		if s[i] == c {
			return i
		}
	}
	return -1
}
