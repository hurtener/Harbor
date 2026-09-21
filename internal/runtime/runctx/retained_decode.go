package runctx

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strings"
)

// validateRetainedShape checks only typed host envelopes before encoding/json
// can merge duplicate fields or match a differently cased field name. Untyped
// results, maps, RawMessages and custom scalar decoders remain opaque; this is
// not a tool-payload schema or a replacement for the existing semantic checks.
func validateRetainedShape(data []byte, shape reflect.Type) error {
	for shape.Kind() == reflect.Pointer {
		shape = shape.Elem()
	}
	// time.Time and json.RawMessage own their scalar/opaque wire format.
	if reflect.PointerTo(shape).Implements(reflect.TypeFor[json.Unmarshaler]()) {
		return nil
	}
	switch shape.Kind() {
	case reflect.Slice, reflect.Array:
		if shape.Elem().Kind() == reflect.Uint8 {
			return nil
		}
		var entries []json.RawMessage
		if err := json.Unmarshal(data, &entries); err != nil {
			return ErrRetainedContextUnavailable
		}
		for _, entry := range entries {
			if err := validateRetainedShape(entry, shape.Elem()); err != nil {
				return err
			}
		}
		return nil
	case reflect.Struct:
	default:
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return ErrRetainedContextUnavailable
	}
	if token == nil {
		// Optional nested pointers may be null. Required fields and records
		// remain subject to their existing version and presence validation.
		return nil
	}
	if token != json.Delim('{') {
		return ErrRetainedContextUnavailable
	}
	fields := make(map[string]reflect.Type, shape.NumField())
	for i := range shape.NumField() {
		field := shape.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if field.PkgPath != "" || name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = field.Type
	}
	seen := make(map[string]bool, len(fields))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return ErrRetainedContextUnavailable
		}
		name, ok := token.(string)
		if !ok || seen[name] {
			return ErrRetainedContextUnavailable
		}
		field, ok := fields[name]
		if !ok {
			return ErrRetainedContextUnavailable
		}
		seen[name] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return ErrRetainedContextUnavailable
		}
		if err := validateRetainedShape(value, field); err != nil {
			return err
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return ErrRetainedContextUnavailable
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return ErrRetainedContextUnavailable
	}
	return nil
}
