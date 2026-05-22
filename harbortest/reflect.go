package harbortest

import (
	"reflect"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// reflectQuadruple inspects p via reflection for an exported
// `Identity` field of type identity.Quadruple. Returns the field
// value + ok=true on a match; (zero, false) otherwise. Pointer
// payloads are dereferenced once; deeper indirection is not
// supported (payloads in the wild are flat structs).
//
// The function is a fallback for AssertNoLeaks's payload-cross-talk
// check — payloads that embed identity via their owning subsystem's
// canonical shape can be discovered without per-package switches.
func reflectQuadruple(p events.EventPayload) (identity.Quadruple, bool) {
	v := reflect.ValueOf(p)
	if !v.IsValid() {
		return identity.Quadruple{}, false
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return identity.Quadruple{}, false
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return identity.Quadruple{}, false
	}
	f := v.FieldByName("Identity")
	if !f.IsValid() {
		return identity.Quadruple{}, false
	}
	if f.Type() == reflect.TypeOf(identity.Quadruple{}) {
		if q, ok := f.Interface().(identity.Quadruple); ok {
			return q, true
		}
	}
	if f.Type() == reflect.TypeOf(identity.Identity{}) {
		if id, ok := f.Interface().(identity.Identity); ok {
			return identity.Quadruple{Identity: id}, true
		}
	}
	return identity.Quadruple{}, false
}
