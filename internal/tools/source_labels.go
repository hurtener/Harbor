package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// QualifiedToolNameSeparator joins a rendered source label and a listed tool
// name in the qualified reference `<source label>/<tool name>` that the
// planner's deferred-tool index lists and that `tool_search` / `tool_get`
// resolve exactly. Source labels never contain it (see [SourceLabel]).
const QualifiedToolNameSeparator = "/"

// maxSourceLabelRunes bounds one source label and maxListedNameRunes one
// listed readable tool name. Both are provider-supplied metadata rendered
// into the system prompt.
const (
	maxSourceLabelRunes = 64
	maxListedNameRunes  = 64
)

// ReadableToolName returns t's provider-local name: its catalog key without
// the `<source>_` prefix a provider driver adds (an MCP tool `send` of
// source `mail~u-<hex>` is registered as `mail~u-<hex>_send` and reads as
// `send`). A tool without that prefix reads as its catalog key.
//
// The result is RAW provider text: a matching key, never prompt text by
// itself (see [DiscoveryNamespace.ListedName]). Never a dispatch or
// isolation input; unique only within one source.
func ReadableToolName(t Tool) string {
	if t.Source != "" {
		if rest, ok := strings.CutPrefix(t.Name, string(t.Source)+"_"); ok && rest != "" {
			return rest
		}
	}
	return t.Name
}

// IsListableToolName reports whether a readable tool name is safe to render
// verbatim into the prompt: 1–64 runes, each a model-tool-name rune
// ([IsModelToolNameRune]) or '.'.
func IsListableToolName(name string) bool {
	if name == "" || utf8.RuneCountInString(name) > maxListedNameRunes {
		return false
	}
	for _, r := range name {
		if !IsModelToolNameRune(r) && r != '.' {
			return false
		}
	}
	return true
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

// SourceLabel returns the BASE display label of t's source: the provider's
// own title when it supplied one, else the logical source name, else
// "built-in" for in-process tools. The label is provider-controlled text, so
// it is reduced to a conservative character class (letters, digits, space,
// '-', '_', '.') on one line of at most 64 runes; it can never contain the
// [QualifiedToolNameSeparator] or the qualifier delimiters `()[]`.
func SourceLabel(t Tool) string {
	if label := sanitizeSourceLabel(t.SourceTitle); label != "" {
		return label
	}
	if label := sanitizeSourceLabel(LogicalSourceName(t)); label != "" {
		return label
	}
	return "built-in"
}

// SourceTierQualifier names t's stability tier for label disambiguation:
// "built-in", "shared", "agent" or "personal".
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

// DisambiguatedSourceLabels returns one rendered label per source among ts,
// unique across those sources. Labels start as [SourceLabel]; sources sharing
// one get their tier in parentheses (`Docs (agent)`); still-identical ones
// add their logical source name in square brackets (`Docs (agent) [docs2]`);
// any residue adds a short digest of the source id in square brackets. Base
// labels contain neither delimiter, so a qualified form is only ever produced
// here, never by another source's base label.
//
// The result depends only on the set of sources, so the deferred-tool index
// and the discovery meta-tools agree on it when given the same tools.
func DisambiguatedSourceLabels(ts []Tool) map[ToolSourceID]string {
	first := map[ToolSourceID]Tool{}
	for _, t := range ts {
		if _, ok := first[t.Source]; !ok {
			first[t.Source] = t
		}
	}
	sources := make([]ToolSourceID, 0, len(first))
	for s := range first {
		sources = append(sources, s)
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i] < sources[j] })
	labels := make(map[ToolSourceID]string, len(sources))
	for _, s := range sources {
		labels[s] = SourceLabel(first[s])
	}
	steps := []func(ToolSourceID, string) string{
		func(s ToolSourceID, l string) string { return l + " (" + SourceTierQualifier(first[s]) + ")" },
		func(s ToolSourceID, l string) string {
			if logical := sanitizeSourceLabel(LogicalSourceName(first[s])); logical != "" {
				return l + " [" + logical + "]"
			}
			return l
		},
		func(s ToolSourceID, l string) string {
			sum := sha256.Sum256([]byte(s))
			return l + " [" + hex.EncodeToString(sum[:4]) + "]"
		},
	}
	for _, step := range steps {
		count := map[string]int{}
		for _, s := range sources {
			count[labels[s]]++
		}
		for _, s := range sources {
			if count[labels[s]] > 1 {
				labels[s] = step(s, labels[s])
			}
		}
	}
	return labels
}

func sanitizeSourceLabel(s string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.':
			return r
		default:
			return ' '
		}
	}, s)
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	if utf8.RuneCountInString(cleaned) > maxSourceLabelRunes {
		runes := []rune(cleaned)
		cleaned = strings.TrimSpace(string(runes[:maxSourceLabelRunes]))
	}
	return cleaned
}
