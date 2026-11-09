package tools

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// QualifiedToolNameSeparator joins a source label and a readable tool name
// in the qualified reference `<source label>/<tool name>` that the planner's
// deferred-tool index lists and that `tool_search` / `tool_get` resolve
// exactly. Source labels never contain it (see [SourceLabel]).
const QualifiedToolNameSeparator = "/"

// maxSourceLabelRunes bounds one source label. Labels are provider-supplied
// metadata rendered into the system prompt.
const maxSourceLabelRunes = 64

// ReadableToolName returns t's provider-local name: its catalog key without
// the `<source>_` prefix a provider driver adds (an MCP tool `send` of
// source `mail~u-<hex>` is registered as `mail~u-<hex>_send` and reads as
// `send`). A tool without that prefix reads as its catalog key.
//
// Discovery metadata only: never a dispatch or isolation input. It is
// unique only within one source.
func ReadableToolName(t Tool) string {
	if t.Source != "" {
		if rest, ok := strings.CutPrefix(t.Name, string(t.Source)+"_"); ok && rest != "" {
			return rest
		}
	}
	return t.Name
}

// LogicalSourceName returns t's source without the owner-scoping suffix
// (`<logical>~u-<hex>` and `<logical>~a-<hex>` read as `<logical>`).
func LogicalSourceName(t Tool) string {
	source := string(t.Source)
	for _, marker := range []string{AgentSourceMarker, UserSourceMarker} {
		if i := strings.Index(source, marker); i > 0 {
			source = source[:i]
		}
	}
	return source
}

// SourceLabel returns the display label of t's source: the provider's own
// title when it supplied one, else the logical source name, else
// "built-in" for in-process tools. The label is provider-controlled text,
// so it is reduced to one bounded line without markup delimiters,
// parentheses or the [QualifiedToolNameSeparator].
func SourceLabel(t Tool) string {
	if label := sanitizeSourceLabel(t.SourceTitle); label != "" {
		return label
	}
	if label := sanitizeSourceLabel(LogicalSourceName(t)); label != "" {
		return label
	}
	return "built-in"
}

// SourceTierQualifier is the short qualifier that disambiguates identical
// source labels of different stability tiers: "built-in", "shared",
// "agent" or "personal".
func SourceTierQualifier(t Tool) string {
	switch PromptStabilityTierOf(t) {
	case PromptTierBuiltin:
		return "built-in"
	case PromptTierAgent:
		return "agent"
	case PromptTierUser:
		return "personal"
	default:
		return "shared"
	}
}

// SourceLabelVariants returns every label under which t's source may be
// named in a qualified reference: the plain label, the label qualified by
// its tier, and the label qualified by its logical source name.
func SourceLabelVariants(t Tool) []string {
	label := SourceLabel(t)
	variants := []string{label, label + " (" + SourceTierQualifier(t) + ")"}
	if logical := sanitizeSourceLabel(LogicalSourceName(t)); logical != "" {
		variants = append(variants, label+" ("+logical+")")
	}
	return variants
}

// MatchesQualifiedToolName reports whether query names t as
// `<source label>/<readable name>` under any of t's label variants.
func MatchesQualifiedToolName(t Tool, query string) bool {
	label, name, ok := strings.Cut(query, QualifiedToolNameSeparator)
	if !ok || name != ReadableToolName(t) {
		return false
	}
	label = strings.TrimSpace(label)
	for _, variant := range SourceLabelVariants(t) {
		if label == variant {
			return true
		}
	}
	return false
}

func sanitizeSourceLabel(s string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r == '<' || r == '>' || r == '(' || r == ')' || r == '/':
			return ' '
		case unicode.IsControl(r) || unicode.IsSpace(r):
			return ' '
		case !unicode.IsPrint(r):
			return -1
		}
		return r
	}, s)
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	if utf8.RuneCountInString(cleaned) > maxSourceLabelRunes {
		runes := []rune(cleaned)
		cleaned = strings.TrimSpace(string(runes[:maxSourceLabelRunes-1])) + "…"
	}
	return cleaned
}
