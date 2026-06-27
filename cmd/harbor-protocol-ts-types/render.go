package main

import (
	"fmt"
	"strings"
)

// render produces the deterministic, newline-terminated TypeScript module
// text from the module model. The output is byte-stable across builds
// (sorted sets, fixed layout) so the `git diff --exit-code` gate holds.
func render(m Module) []byte {
	var b strings.Builder

	// Banner.
	b.WriteString("/*\n * ")
	b.WriteString(m.Comment)
	b.WriteString("\n */\n\n")

	// Pinned surface constants.
	fmt.Fprintf(&b, "/** The Harbor Protocol version this module was generated against (RFC §5.3). */\n")
	fmt.Fprintf(&b, "export const PROTOCOL_VERSION = %q;\n\n", m.ProtocolVersion)
	fmt.Fprintf(&b, "/**\n")
	fmt.Fprintf(&b, " * The canonical wire-surface digest the runtime reports on `runtime.info`.\n")
	fmt.Fprintf(&b, " * Compare it against the live runtime's digest to detect a wire skew\n")
	fmt.Fprintf(&b, " * between what you vendored and what the runtime speaks.\n")
	fmt.Fprintf(&b, " */\n")
	fmt.Fprintf(&b, "export const WIRE_SURFACE_DIGEST = %q;\n\n", m.WireSurfaceDigest)

	// Method / error / event string-union types.
	writeUnion(&b, "HarborMethod", "Every canonical Harbor Protocol method name.", m.Methods)
	writeUnion(&b, "HarborErrorCode", "Every canonical Harbor Protocol error code.", m.Errors)
	writeUnion(&b, "HarborEventType", "Every canonical Harbor event-type wire string.", m.Events)

	// Wire-type interfaces.
	for _, iface := range m.Types {
		writeIface(&b, iface)
	}

	out := b.String()
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return []byte(out)
}

// writeUnion renders a sorted string-literal union type. An empty set
// renders as `never` so the module is always well-formed.
func writeUnion(b *strings.Builder, name, doc string, values []string) {
	fmt.Fprintf(b, "/** %s */\n", doc)
	if len(values) == 0 {
		fmt.Fprintf(b, "export type %s = never;\n\n", name)
		return
	}
	fmt.Fprintf(b, "export type %s =\n", name)
	for i, v := range values {
		end := ""
		if i == len(values)-1 {
			end = ";"
		}
		fmt.Fprintf(b, "  | %q%s\n", v, end)
	}
	b.WriteString("\n")
}

// writeIface renders one wire-type interface. A type with no wire fields
// renders as an empty interface body (a Record alias would lose the
// nominal name vendored clients branch on).
func writeIface(b *strings.Builder, iface TypeIface) {
	fmt.Fprintf(b, "export interface %s {\n", iface.Name)
	for _, f := range iface.Fields {
		opt := ""
		if f.Optional {
			opt = "?"
		}
		fmt.Fprintf(b, "  %s%s: %s;\n", renderKey(f.Key), opt, f.TSType)
	}
	b.WriteString("}\n\n")
}

// renderKey renders a wire key as a TypeScript property name, quoting it
// when it is not a bare identifier.
func renderKey(key string) string {
	if tsIdentRe.MatchString(key) {
		return key
	}
	return fmt.Sprintf("%q", key)
}
