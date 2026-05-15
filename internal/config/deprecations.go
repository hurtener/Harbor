package config

import (
	"fmt"
	"log/slog"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

// deprecatedGovernanceKeys is the closed set of YAML keys the loader
// strips from the `governance:` block and warns about (D-081). Each
// key was a PRE-Phase-36a single-knob stub that the loader validated
// but the governance enforcement engine never consumed — exactly the
// confusion trap CLAUDE.md §13's "Test stubs as production defaults
// on operator-facing seams" entry warns against, one layer up. An
// operator setting `cost_ceiling_usd: 100` in YAML expected
// enforcement and got none.
//
// The keys are recorded here so a future contributor adding a real
// `governance.<knob>` knob does not collide with a removed name; the
// loader's strip-then-warn path treats anything in this map as
// removed-and-warned, not as a typo.
var deprecatedGovernanceKeys = map[string]struct{}{
	"default_max_tokens": {},
	"cost_ceiling_usd":   {},
	"rate_limit_tps":     {},
}

// deprecatedFieldReplacement is the migration pointer emitted with
// every `config.deprecated_field` warning. Every removed governance
// knob is rebuilt under `governance.identity_tiers` (Phase 36a/36b)
// — operators do not get individual replacement keys.
const deprecatedFieldReplacement = "governance.identity_tiers"

// deprecatedFieldRemovedIn names the release the validated-but-ignored
// keys were removed in. Kept as a constant so the warning text stays
// in lockstep with the decision (D-081) and the example yaml.
const deprecatedFieldRemovedIn = "v0.x"

// stripDeprecatedGovernanceKeys parses `data` as YAML, removes every
// key under the top-level `governance:` mapping that matches the
// `deprecatedGovernanceKeys` set, and returns the re-serialised bytes.
// For each stripped key the function emits a single structured
// `config.deprecated_field` warning on `logger` with attrs `field`,
// `replacement`, `removed_in`, and `source` — so an operator's logs
// show one line per legacy key per load.
//
// When the input contains none of the deprecated keys the byte stream
// is returned unchanged. A parse failure surfaces with the offending
// source name so the wrapping error carries the same context the
// caller's `parse:` error would.
func stripDeprecatedGovernanceKeys(data []byte, source string, logger *slog.Logger) ([]byte, error) {
	if logger == nil {
		logger = slog.Default()
	}
	// Empty input is a no-op; the strict decode that follows will
	// produce the documented defaults.
	if len(data) == 0 {
		return data, nil
	}
	file, err := parser.ParseBytes(data, 0)
	if err != nil {
		return nil, fmt.Errorf("yaml AST parse: %w", err)
	}
	// Walk every document body and, when we find a top-level
	// `governance:` mapping, drop the deprecated child keys. YAML
	// supports multi-document streams (`---`-separated); Harbor's
	// config is single-document by convention, but the loop is
	// cheap and keeps the pre-processor honest if that ever changes.
	stripped := false
	for _, doc := range file.Docs {
		if doc == nil || doc.Body == nil {
			continue
		}
		root, ok := mappingFrom(doc.Body)
		if !ok {
			continue
		}
		governance := findChildMapping(root, "governance")
		if governance == nil {
			continue
		}
		filtered := governance.Values[:0]
		for _, kv := range governance.Values {
			name := mappingKeyName(kv.Key)
			if _, deprecated := deprecatedGovernanceKeys[name]; deprecated {
				logger.Warn(
					"config.deprecated_field",
					slog.String("field", "governance."+name),
					slog.String("replacement", deprecatedFieldReplacement),
					slog.String("removed_in", deprecatedFieldRemovedIn),
					slog.String("source", source),
				)
				stripped = true
				continue
			}
			filtered = append(filtered, kv)
		}
		governance.Values = filtered
	}
	if !stripped {
		// No deprecated keys touched the YAML — keep the original byte
		// stream so the strict decoder operates on exactly what the
		// operator wrote (preserves comments + formatting in any error
		// position references).
		return data, nil
	}
	// Re-serialise the cleaned AST. We marshal the File (rather than a
	// single doc) so multi-document inputs round-trip; for the typical
	// single-doc case this is identical to marshalling the root body.
	return []byte(file.String()), nil
}

// mappingFrom returns the underlying `*ast.MappingNode` for a node,
// unwrapping the `*ast.MappingValueNode` shape goccy/go-yaml uses for
// a single-entry root mapping. Returns (nil, false) when n is not a
// mapping shape.
func mappingFrom(n ast.Node) (*ast.MappingNode, bool) {
	switch m := n.(type) {
	case *ast.MappingNode:
		return m, true
	case *ast.MappingValueNode:
		// goccy emits a single-entry root as a `MappingValueNode`
		// rather than a one-entry `MappingNode`. Synthesise a
		// one-entry `MappingNode` so the rest of the walker only
		// needs to handle one shape.
		return &ast.MappingNode{
			BaseNode: m.BaseNode,
			Start:    m.GetToken(),
			Values:   []*ast.MappingValueNode{m},
		}, true
	}
	return nil, false
}

// findChildMapping returns the `*ast.MappingNode` value for the child
// whose key equals `name`, or nil when the child is absent or not a
// mapping. A `governance:` block written as a YAML alias or a
// non-mapping scalar is silently skipped — the strict decode that
// follows will surface the wrong-shape error with the precise YAML
// position.
func findChildMapping(root *ast.MappingNode, name string) *ast.MappingNode {
	for _, kv := range root.Values {
		if mappingKeyName(kv.Key) != name {
			continue
		}
		child, ok := kv.Value.(*ast.MappingNode)
		if !ok {
			return nil
		}
		return child
	}
	return nil
}

// mappingKeyName returns the string form of a map key node. goccy
// emits unquoted YAML keys as `*ast.StringNode`; merge keys + numeric
// keys are not deprecation candidates so they return "" — which
// matches no entry in `deprecatedGovernanceKeys`.
func mappingKeyName(k ast.MapKeyNode) string {
	if s, ok := k.(*ast.StringNode); ok {
		return s.Value
	}
	return ""
}
