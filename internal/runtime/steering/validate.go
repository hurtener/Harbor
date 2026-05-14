package steering

import (
	"encoding/json"
	"fmt"
)

// Payload bounds — RFC §6.3 "Steering payload bounds" (Settled),
// verbatim from the predecessor's steering constants (brief 02 §6
// finding 8: "Harbor keeps the same caps"). Enforced at the Protocol
// edge by Validate before a ControlEvent is ever enqueued.
const (
	// MaxPayloadDepth — the deepest nesting level a payload may reach.
	// The top-level map is depth 1; a value nested one map / list
	// inside it is depth 2; and so on. Depth > 6 is rejected.
	MaxPayloadDepth = 6
	// MaxPayloadKeys — the most keys any single map in the payload
	// may carry. Applies per-map, not cumulatively (a payload with
	// two maps of 64 keys each is valid; one map of 65 is not).
	MaxPayloadKeys = 64
	// MaxPayloadListItems — the most elements any single list in the
	// payload may carry.
	MaxPayloadListItems = 50
	// MaxPayloadStringLen — the most characters (runes) any single
	// string leaf may carry.
	MaxPayloadStringLen = 4096
	// MaxPayloadTotalBytes — the cap on the payload's canonical JSON
	// encoding. 16 KiB. The total-size check runs first so a
	// pathologically large flat payload is rejected before the
	// structural walk.
	MaxPayloadTotalBytes = 16 * 1024
)

// ValidatePayload runs the RFC §6.3 payload bounds against p. It
// returns nil for a valid payload (including a nil / empty payload —
// a control event with no payload is valid, e.g. a bare CANCEL) and a
// wrapped ErrPayloadInvalid / ErrUnsupportedPayloadValue naming the
// violated bound otherwise.
//
// Order of checks (each fails loud, none truncates — CLAUDE.md §5):
//
//  1. Total-bytes: the canonical JSON encoding must be ≤ 16 KiB. A
//     payload that cannot be JSON-encoded at all (a channel / func
//     leaf) fails here with ErrUnsupportedPayloadValue.
//  2. Structural walk: depth ≤ 6, per-map keys ≤ 64, per-list items
//     ≤ 50, per-string runes ≤ 4096. The walk also rejects any leaf
//     whose Go type is outside the JSON-shaped accepted set.
//
// ValidatePayload is pure and holds no state — safe for concurrent
// use by N goroutines (D-025).
func ValidatePayload(p map[string]any) error {
	if len(p) == 0 {
		return nil
	}

	// (1) Total-bytes. json.Marshal fails loud on an unencodable
	// leaf (chan, func, complex) — surface that as
	// ErrUnsupportedPayloadValue rather than letting a malformed
	// payload through.
	encoded, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("%w: payload is not JSON-encodable: %v", ErrUnsupportedPayloadValue, err)
	}
	if len(encoded) > MaxPayloadTotalBytes {
		return fmt.Errorf("%w: total size %d bytes exceeds %d-byte cap",
			ErrPayloadInvalid, len(encoded), MaxPayloadTotalBytes)
	}

	// (2) Structural walk. The top-level map is depth 1.
	return walkValue(p, 1)
}

// walkValue recursively enforces the depth / keys / list / string
// bounds. depth is the current container-nesting level (1 == the
// top-level payload map). Depth counts CONTAINERS only — a scalar
// leaf inside a depth-6 map does not push the count to 7; a payload
// nested map-in-map-in-map... to six container levels is the cap, a
// seventh container level is rejected.
func walkValue(v any, depth int) error {
	switch tv := v.(type) {
	case map[string]any:
		if depth > MaxPayloadDepth {
			return fmt.Errorf("%w: nesting depth exceeds %d levels", ErrPayloadInvalid, MaxPayloadDepth)
		}
		if len(tv) > MaxPayloadKeys {
			return fmt.Errorf("%w: map has %d keys, exceeds %d-key cap",
				ErrPayloadInvalid, len(tv), MaxPayloadKeys)
		}
		for _, child := range tv {
			if err := walkValue(child, depth+1); err != nil {
				return err
			}
		}
		return nil

	case []any:
		if depth > MaxPayloadDepth {
			return fmt.Errorf("%w: nesting depth exceeds %d levels", ErrPayloadInvalid, MaxPayloadDepth)
		}
		if len(tv) > MaxPayloadListItems {
			return fmt.Errorf("%w: list has %d items, exceeds %d-item cap",
				ErrPayloadInvalid, len(tv), MaxPayloadListItems)
		}
		for _, child := range tv {
			if err := walkValue(child, depth+1); err != nil {
				return err
			}
		}
		return nil

	case string:
		if n := len([]rune(tv)); n > MaxPayloadStringLen {
			return fmt.Errorf("%w: string of %d chars exceeds %d-char cap",
				ErrPayloadInvalid, n, MaxPayloadStringLen)
		}
		return nil

	// JSON-shaped scalar leaves. json.Unmarshal produces float64 for
	// every number and bool for booleans; a payload constructed
	// in-Go may also carry the native integer / float kinds, so the
	// accepted set is the union. nil is a valid JSON leaf (null).
	case bool, float64, float32,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		json.Number, nil:
		return nil

	default:
		// A leaf of any other Go type (chan, func, complex, a struct,
		// a typed slice that is not []any, a typed map) is rejected
		// loud. The total-bytes json.Marshal would already have
		// caught the truly unencodable kinds; this branch also
		// rejects encodable-but-non-canonical shapes so the walk's
		// bounds are not silently bypassed by a typed container.
		return fmt.Errorf("%w: leaf of type %T is not a canonical JSON value",
			ErrUnsupportedPayloadValue, v)
	}
}
