package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hurtener/Harbor/internal/protocol/singlesource"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// renderTypesPage emits types.md: every singlesource.CanonicalWireTypes
// entry, field-level with the snake_case wire tags, rendered via
// reflection over the typeInstanceIndex.
func renderTypesPage() (string, error) {
	names := make([]string, 0, len(singlesource.CanonicalWireTypes))
	for name := range singlesource.CanonicalWireTypes {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString(generatedHeader + "\n\n")
	b.WriteString("# Protocol wire types\n\n")
	fmt.Fprintf(&b, "The %d canonical Harbor Protocol wire types, generated from the single-source\n", len(names))
	b.WriteString("inventory (`internal/protocol/singlesource.CanonicalWireTypes`) by reflection over the\n")
	b.WriteString("declaring packages. Field order is wire order; the Wire key column is the JSON key a\n")
	fmt.Fprintf(&b, "client reads and writes. The Protocol version is `%s` (RFC §5.3 — bumping it is an\n", types.ProtocolVersion)
	b.WriteString("RFC change with a deprecation window).\n\n")
	b.WriteString("Unknown-field tolerance: clients SHOULD ignore response fields they do not know\n")
	b.WriteString("(additive evolution is non-breaking); the Runtime rejects unknown REQUEST fields on\n")
	b.WriteString("the surfaces that decode strictly, so send only what a type declares.\n")

	for _, name := range names {
		t, ok := typeInstanceIndex[name]
		if !ok {
			return "", fmt.Errorf("canonical wire type %q has no typeInstanceIndex entry — extend the index (the lockstep test pins this)", name)
		}
		home := singlesource.CanonicalWireTypes[name]
		fmt.Fprintf(&b, "\n## %s\n\n", name)
		fmt.Fprintf(&b, "Declared in `internal/protocol/%s`.\n\n", home)
		renderStructFields(&b, t, jsonTagKeys)
	}

	return b.String(), nil
}
