package react

import (
	"strings"

	"github.com/hurtener/Harbor/internal/planner"
)

// sanitizeToolName maps a catalog tool name to the provider-safe form
// native tool-calling requires. OpenAI-compatible providers reject a
// function name that is not ^[a-zA-Z0-9_-]{1,64}$ with a 400, so Harbor's
// dotted naming convention ("clock.now", "inventory.check") cannot be sent
// verbatim. The transform replaces every disallowed character with '_' and
// caps the length at 64 bytes (tool names are ASCII in practice).
//
// It is deterministic and has no inverse stored anywhere: resolveDeclared
// ToolName recovers the real catalog name by matching a provider-returned
// name against each catalog tool's sanitized name. Already-valid names
// (the reserved planner controls, discovered MCP tools) are unchanged, so
// they round-trip by exact match.
func sanitizeToolName(name string) string {
	clean := true
	for _, r := range name {
		if !isToolNameRune(r) {
			clean = false
			break
		}
	}
	if clean && len(name) <= 64 {
		return name
	}
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		if isToolNameRune(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	s := b.String()
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func isToolNameRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		return true
	default:
		return false
	}
}

// resolveDeclaredToolName maps a provider-returned tool-call name back to
// the real catalog tool name. Declarations are sent to the LLM under their
// sanitized names, so a returned name may differ from the catalog name
// (e.g. "inventory_check" → "inventory.check"). An exact catalog match
// wins (already-valid names + discovered MCP tools); otherwise the first
// catalog tool whose sanitized name matches is returned. An unmatched name
// is returned verbatim — the executor then fails loud on an unknown tool,
// exactly as before this sanitization existed.
func resolveDeclaredToolName(rc *planner.RunContext, name string) string {
	if rc == nil || rc.Catalog == nil {
		return name
	}
	if _, ok := rc.Catalog.Resolve(name); ok {
		return name
	}
	for _, t := range rc.Catalog.List() {
		if t.Name != "" && sanitizeToolName(t.Name) == name {
			return t.Name
		}
	}
	return name
}
