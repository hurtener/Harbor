package main

import (
	"fmt"
	"reflect"
	"strings"
)

// fieldKeyMode selects how a struct field's wire key is derived.
type fieldKeyMode int

const (
	// fieldNameKeys — the wire key is the Go field name (event
	// payloads carry no json tags; encoding/json uses the field name
	// verbatim, hence the documented capital-letter keys).
	fieldNameKeys fieldKeyMode = iota
	// jsonTagKeys — the wire key is the json struct tag (the Protocol
	// wire types are fully snake_case-tagged).
	jsonTagKeys
)

// sealTypes are the embedded marker types the renderer hides from
// payload field tables — they are compile-time seals, not wire fields
// (encoding/json emits nothing for them).
var sealTypes = map[string]bool{
	"Sealed":       true,
	"SafeSealed":   true,
	"SafePayload":  true,
	"EventPayload": true,
}

// renderStructFields writes one markdown table covering t's exported
// fields. mode selects the wire-key derivation; embedded seal markers
// are skipped. Nested anonymous structs are flattened the way
// encoding/json flattens them only for the seal markers — every other
// field (including embedded named structs) renders as one row whose
// type cell names the nested shape.
func renderStructFields(b *strings.Builder, t reflect.Type, mode fieldKeyMode) {
	rows := structFieldRows(t, mode)
	if len(rows) == 0 {
		b.WriteString("_(no wire fields)_\n")
		return
	}
	b.WriteString("| Wire key | Go type | Notes |\n")
	b.WriteString("|---|---|---|\n")
	for _, r := range rows {
		fmt.Fprintf(b, "| `%s` | %s | %s |\n", r.key, r.goType, r.notes)
	}
}

// fieldRow is one rendered struct-field table row.
type fieldRow struct {
	key    string
	goType string
	notes  string
}

// structFieldRows derives the deterministic field-table rows for t
// (declaration order — reflect guarantees it, and declaration order is
// the wire order encoding/json emits).
func structFieldRows(t reflect.Type, mode fieldKeyMode) []fieldRow {
	var rows []fieldRow
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Anonymous && sealTypes[f.Type.Name()] {
			continue
		}

		key := f.Name
		notes := ""
		if mode == jsonTagKeys {
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			parts := strings.Split(tag, ",")
			if parts[0] != "" {
				key = parts[0]
			}
			for _, opt := range parts[1:] {
				if opt == "omitempty" {
					notes = "optional (`omitempty`)"
				}
			}
		}

		rows = append(rows, fieldRow{key: key, goType: goTypeCell(f.Type), notes: notes})
	}
	return rows
}

// goTypeCell renders a field type for a table cell: the Go type string
// (with `interface {}` normalised to `any`), cross-linked into types.md
// when the named type — unwrapped through pointer / slice / array /
// map-value indirection — is itself a canonical wire type.
func goTypeCell(t reflect.Type) string {
	s := "`" + strings.ReplaceAll(t.String(), "interface {}", "any") + "`"
	if name, ok := canonicalElemName(t); ok {
		s += fmt.Sprintf(" — see [`%s`](./types.md#%s)", name, strings.ToLower(name))
	}
	return s
}

// canonicalElemName unwraps pointers / slices / arrays / map values and
// reports the underlying named type when it is a canonical Protocol
// wire type (an entry of the typeInstanceIndex declared in the
// canonical wire packages).
func canonicalElemName(t reflect.Type) (string, bool) {
	for {
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			t = t.Elem()
			continue
		default:
		}
		break
	}
	name := t.Name()
	if name == "" {
		return "", false
	}
	pkg := t.String()
	if !strings.HasPrefix(pkg, "types.") && pkg != "errors.Error" {
		return "", false
	}
	if _, ok := typeInstanceIndex[name]; !ok {
		return "", false
	}
	return name, true
}
