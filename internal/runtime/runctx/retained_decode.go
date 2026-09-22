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
	// An opaque root has no host fields to constrain. The typed decoder in
	// decodeRetained still checks syntax, trailing data, value types and exact
	// numbers. Do not scan and copy the entire tool result a second time here.
	if !retainedHostContainer(shape) {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := validateRetainedValue(decoder, shape); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return ErrRetainedContextUnavailable
	}
	return nil
}

// Walk host containers on one decoder. Decoding a RawMessage and starting a new
// decoder for each nested container would rescan/copy every contained payload at
// every enclosing level. Only opaque leaves need to be consumed as raw JSON.
func validateRetainedValue(decoder *json.Decoder, shape reflect.Type) error {
	for shape.Kind() == reflect.Pointer {
		shape = shape.Elem()
	}
	// time.Time and json.RawMessage own their scalar/opaque wire format.
	if reflect.PointerTo(shape).Implements(reflect.TypeFor[json.Unmarshaler]()) {
		return consumeRetainedValue(decoder)
	}
	switch shape.Kind() {
	case reflect.Slice, reflect.Array:
		if shape.Elem().Kind() == reflect.Uint8 {
			return consumeRetainedValue(decoder)
		}
		token, err := decoder.Token()
		if err != nil {
			return ErrRetainedContextUnavailable
		}
		if token == nil {
			return nil
		}
		if token != json.Delim('[') {
			return ErrRetainedContextUnavailable
		}
		for decoder.More() {
			if err := validateRetainedValue(decoder, shape.Elem()); err != nil {
				return err
			}
		}
		if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
			return ErrRetainedContextUnavailable
		}
		return nil
	case reflect.Struct:
	default:
		return consumeRetainedValue(decoder)
	}
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
		if err := validateRetainedValue(decoder, field); err != nil {
			return err
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return ErrRetainedContextUnavailable
	}
	return nil
}

// retainedHostContainer mirrors the validator's existing opaque boundaries.
// In particular, maps/interfaces are data even if they contain host-like keys.
func retainedHostContainer(shape reflect.Type) bool {
	for shape.Kind() == reflect.Pointer {
		shape = shape.Elem()
	}
	if reflect.PointerTo(shape).Implements(reflect.TypeFor[json.Unmarshaler]()) {
		return false
	}
	switch shape.Kind() {
	case reflect.Struct:
		return true
	case reflect.Slice, reflect.Array:
		return retainedHostContainer(shape.Elem())
	default:
		return false
	}
}

// Decoder has already validated the JSON value before calling UnmarshalJSON.
// This first pass needs no retained bytes; the final typed decode owns them.
// Using RawMessage for this discard would copy every large opaque leaf.
type retainedDiscard struct{}

func (*retainedDiscard) UnmarshalJSON([]byte) error { return nil }

func consumeRetainedValue(decoder *json.Decoder) error {
	var opaque retainedDiscard
	if err := decoder.Decode(&opaque); err != nil {
		return ErrRetainedContextUnavailable
	}
	return nil
}
