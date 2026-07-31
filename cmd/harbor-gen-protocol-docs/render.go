package main

import (
	"fmt"
	"reflect"
	"strings"
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
// fields, keyed the way encoding/json keys them: the `json` tag's name
// when the field carries one, the Go field name otherwise. Embedded seal
// markers are skipped. Nested anonymous structs are flattened the way
// encoding/json flattens them only for the seal markers — every other
// field (including embedded named structs) renders as one row whose
// type cell names the nested shape.
func renderStructFields(b *strings.Builder, t reflect.Type) {
	rows := structFieldRows(t)
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
// the wire order encoding/json emits). The wire key is derived exactly
// the way encoding/json derives it: a `json` tag name renames the field,
// `json:"-"` drops it, and an absent or name-less tag falls back to the
// Go field name. Protocol wire types are fully snake_case-tagged; event
// payloads are a mix (an untagged payload keeps its capitalised Go field
// names on the wire, a tagged one does not), so the derivation is one
// rule for both pages rather than a per-page assumption.
func structFieldRows(t reflect.Type) []fieldRow {
	var rows []fieldRow
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Anonymous && sealTypes[f.Type.Name()] {
			continue
		}

		tag, tagged := f.Tag.Lookup("json")
		if tagged && tag == "-" {
			// The one unescapable form: `json:"-"` drops the field.
			// (`json:"-,"` instead names the wire key "-".)
			continue
		}

		key := f.Name
		var opts []string
		if tagged {
			parts := strings.Split(tag, ",")
			if parts[0] != "" {
				key = parts[0]
			}
			for _, opt := range parts[1:] {
				switch opt {
				case "omitempty":
					opts = append(opts, "optional (`omitempty`)")
				case "omitzero":
					opts = append(opts, "optional (`omitzero`)")
				case "string":
					opts = append(opts, "JSON-string-encoded (`,string`)")
				}
			}
		}

		rows = append(rows, fieldRow{
			key:    key,
			goType: goTypeCell(f.Type),
			notes:  strings.Join(opts, "; "),
		})
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
