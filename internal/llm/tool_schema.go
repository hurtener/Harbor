package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// schemaAnnotationKeywords are JSON Schema keywords that carry no meaning for
// a model choosing arguments: the dialect URI generators stamp on every
// schema and free-form maintainer comments. They cost prompt tokens on every
// call and vary between generator versions, so they are removed from model
// declarations. Validation still uses the tool's original schema.
var schemaAnnotationKeywords = map[string]struct{}{
	"$schema":  {},
	"$comment": {},
}

// Keywords whose value is a map from names to subschemas. The map's keys are
// property or definition NAMES, never keywords, so a property literally named
// "$comment" survives.
var schemaMapKeywords = map[string]struct{}{
	"properties":        {},
	"patternProperties": {},
	"$defs":             {},
	"definitions":       {},
	"dependentSchemas":  {},
}

// Keywords whose value is one subschema.
var schemaSubschemaKeywords = map[string]struct{}{
	"items":                 {},
	"additionalItems":       {},
	"additionalProperties":  {},
	"unevaluatedItems":      {},
	"unevaluatedProperties": {},
	"contains":              {},
	"propertyNames":         {},
	"not":                   {},
	"if":                    {},
	"then":                  {},
	"else":                  {},
}

// Keywords whose value is an array of subschemas.
var schemaArrayKeywords = map[string]struct{}{
	"allOf":       {},
	"anyOf":       {},
	"oneOf":       {},
	"prefixItems": {},
	"items":       {}, // draft-04 tuple form
}

// StripSchemaAnnotations returns schema without the `$schema` and `$comment`
// annotation keywords, at the root and in every nested subschema. Only
// keyword positions are rewritten: property names, `enum`/`const`/`default`/
// `examples` data and unknown extension keywords pass through unchanged, and
// the remaining keys keep their original order.
//
// The input is returned as-is (same bytes) when it contains no annotation,
// is empty, or is not valid JSON — a malformed schema keeps failing loudly
// where it is parsed, never here.
func StripSchemaAnnotations(schema json.RawMessage) json.RawMessage {
	if len(schema) == 0 || (!bytes.Contains(schema, []byte(`"$schema"`)) && !bytes.Contains(schema, []byte(`"$comment"`))) {
		return schema
	}
	dec := json.NewDecoder(bytes.NewReader(schema))
	dec.UseNumber()
	var out bytes.Buffer
	if err := rewriteSchemaValue(dec, &out, true); err != nil {
		return schema
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return schema // trailing data: not one JSON value
	}
	if bytes.Equal(out.Bytes(), compactJSON(schema)) {
		return schema
	}
	return json.RawMessage(out.Bytes())
}

func compactJSON(raw []byte) []byte {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return nil
	}
	return b.Bytes()
}

var errSchemaShape = errors.New("schema: unexpected token")

// rewriteSchemaValue copies the next JSON value from dec to out. When
// asSchema is true and the value is an object, it is treated as a schema
// object: annotation keywords are dropped and known subschema keywords are
// recursed into.
func rewriteSchemaValue(dec *json.Decoder, out *bytes.Buffer, asSchema bool) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, isDelim := tok.(json.Delim)
	if !isDelim {
		return writeScalar(out, tok)
	}
	switch delim {
	case '{':
		out.WriteByte('{')
		first := true
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyTok.(string)
			if !ok {
				return errSchemaShape
			}
			if asSchema {
				if _, drop := schemaAnnotationKeywords[key]; drop {
					var skip json.RawMessage
					if err := dec.Decode(&skip); err != nil {
						return err
					}
					continue
				}
			}
			if !first {
				out.WriteByte(',')
			}
			first = false
			if err := writeScalar(out, key); err != nil {
				return err
			}
			out.WriteByte(':')
			if err := rewriteKeywordValue(dec, out, asSchema, key); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil { // '}'
			return err
		}
		out.WriteByte('}')
		return nil
	case '[':
		out.WriteByte('[')
		first := true
		for dec.More() {
			if !first {
				out.WriteByte(',')
			}
			first = false
			if err := rewriteSchemaValue(dec, out, asSchema); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil { // ']'
			return err
		}
		out.WriteByte(']')
		return nil
	}
	return errSchemaShape
}

// rewriteKeywordValue copies the value of key inside an object. Inside a
// schema object, subschema-bearing keywords recurse as schemas; every other
// value (and every value of a non-schema object) is copied verbatim.
func rewriteKeywordValue(dec *json.Decoder, out *bytes.Buffer, parentIsSchema bool, key string) error {
	if !parentIsSchema {
		return copyValue(dec, out)
	}
	if _, ok := schemaMapKeywords[key]; ok {
		return rewriteSchemaMap(dec, out)
	}
	_, single := schemaSubschemaKeywords[key]
	_, array := schemaArrayKeywords[key]
	if single || array {
		// A subschema keyword holds a schema object, a boolean schema, or
		// (for the array keywords) an array of schemas; rewriteSchemaValue
		// handles each shape.
		return rewriteSchemaValue(dec, out, true)
	}
	return copyValue(dec, out)
}

// rewriteSchemaMap copies a name→subschema map, recursing into each value.
func rewriteSchemaMap(dec *json.Decoder, out *bytes.Buffer) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		// Not a map (malformed schema): copy the scalar/array verbatim.
		if !ok {
			return writeScalar(out, tok)
		}
		return copyRest(dec, out, delim)
	}
	out.WriteByte('{')
	first := true
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := keyTok.(string)
		if !ok {
			return errSchemaShape
		}
		if !first {
			out.WriteByte(',')
		}
		first = false
		if err := writeScalar(out, key); err != nil {
			return err
		}
		out.WriteByte(':')
		if err := rewriteSchemaValue(dec, out, true); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	out.WriteByte('}')
	return nil
}

// copyValue copies the next value verbatim (compacted).
func copyValue(dec *json.Decoder, out *bytes.Buffer) error {
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	return json.Compact(out, raw)
}

// copyRest copies the remainder of an already-opened array verbatim.
func copyRest(dec *json.Decoder, out *bytes.Buffer, open json.Delim) error {
	if open != '[' {
		return errSchemaShape
	}
	out.WriteByte('[')
	first := true
	for dec.More() {
		if !first {
			out.WriteByte(',')
		}
		first = false
		if err := copyValue(dec, out); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	out.WriteByte(']')
	return nil
}

func writeScalar(out *bytes.Buffer, tok json.Token) error {
	switch v := tok.(type) {
	case json.Number:
		out.WriteString(v.String())
		return nil
	case nil:
		out.WriteString("null")
		return nil
	case string:
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			return err
		}
		out.Write(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
		return nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		out.Write(b)
		return nil
	}
}
