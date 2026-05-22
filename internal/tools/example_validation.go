package tools

import (
	"encoding/json"
	"errors"
	"sort"
)

// ErrToolExampleInvalid — a [ToolExample] registered on a [Tool]
// references an argument key that is not declared in the tool's
// [Tool.ArgsSchema] `properties`. The catalog rejects the registration
// loudly (AGENTS.md §5 fail-loudly): a passing example is a working
// example, so an example whose `Args` cannot match the schema would
// teach the planner a shape the catalog edge would then reject.
var ErrToolExampleInvalid = errors.New("tools: invalid tool example")

// validateExamples checks every [ToolExample] on t against the tool's
// declared `args_schema`. Each example's `Args` keys MUST be a subset
// of the schema's top-level `properties`. Returns a wrapped
// [ErrToolExampleInvalid] on the first mismatch, naming the tool, the
// example index, and the offending key.
//
// Validation is skipped (returns nil) when the tool declares no
// `ArgsSchema` or no `properties` — a tool with no schema makes no
// claims about its argument shape, so an example cannot contradict it.
// This keeps schema-free tools (and the Phase 83a no-examples shape)
// registrable without ceremony.
//
// The check is read-side only: it does NOT validate JSON-Schema types,
// `required`, or value constraints — Phase 26's catalog-edge validator
// owns runtime arg validation. validateExamples guards the narrower
// invariant that a curated example does not name a key the tool does
// not accept.
func validateExamples(t Tool) error {
	if len(t.Examples) == 0 {
		return nil
	}
	props, ok := schemaProperties(t.ArgsSchema)
	if !ok {
		// No schema, or a schema without `properties` — the tool
		// makes no shape claim, so no example can contradict it.
		return nil
	}
	for i, ex := range t.Examples {
		for key := range ex.Args {
			if _, declared := props[key]; !declared {
				return wrap(ErrToolExampleInvalid,
					"tool %q example #%d: arg key %q is not in args_schema.properties %v",
					t.Name, i, key, sortedProps(props))
			}
		}
	}
	return nil
}

// schemaProperties extracts the top-level `properties` object from a
// JSON-Schema document. ok=false when the schema is empty, not an
// object, or carries no `properties` key.
func schemaProperties(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var doc struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		// A malformed schema is a separate failure surfaced by the
		// driver's own schema-compilation step; example validation
		// does not double-report it.
		return nil, false
	}
	if doc.Properties == nil {
		return nil, false
	}
	return doc.Properties, true
}

// sortedProps returns the property names sorted, for a deterministic
// error message.
func sortedProps(props map[string]json.RawMessage) []string {
	out := make([]string, 0, len(props))
	for k := range props {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
