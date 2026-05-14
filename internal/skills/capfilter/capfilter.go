// Package capfilter holds the primitive capability-filter and
// tool-name-scrub logic shared by the skills planner tools
// (internal/skills/tools, Phase 38) and the virtual directory
// (internal/skills, Phase 39).
//
// It exists to close an import cycle: internal/skills/tools imports
// internal/skills, so internal/skills cannot import
// internal/skills/tools. Before this package, the directory
// re-implemented Filter/Redact inline — two parallel implementations
// of one conceptual feature, the exact CLAUDE.md §13 anti-pattern.
// capfilter is a leaf package (it imports only the standard library),
// so BOTH internal/skills and internal/skills/tools can depend on it.
// The capability-filter logic now lives in exactly one place.
//
// Every function here operates on primitive inputs (string slices,
// string sets, plain strings) — never on internal/skills.Skill —
// precisely so the package stays a cycle-free leaf. Callers do their
// own per-Skill field plumbing; the load-bearing logic (subset gate,
// disallowed-name computation, replacement selection, word-boundary
// scrub) is shared.
//
// Brief 04 §4.5 is the design source for both the subset gate and the
// scrub semantics. D-052 (Phase 39) records the extraction.
package capfilter

import "regexp"

// ToolSearchName is the planner tool name reserved for the
// "search for a tool" capability. When it is present in a run's
// allowed-tools set, [Replacement] selects the "(use tool_search)"
// variant — the run has the search escape hatch.
const ToolSearchName = "tool_search"

// Replacement variants for a disallowed tool name in skill text.
// Brief 04 §4.5: "replacement is `'a suitable tool (use tool_search)'`
// when search is available, else `'a suitable tool'`."
const (
	ReplacementWithSearch    = "a suitable tool (use tool_search)"
	ReplacementWithoutSearch = "a suitable tool"
)

// BuildSet returns a hash-set lookup over `in`. Empty input yields a
// nil set; callers test membership via `_, ok := set[k]`, so a nil
// map reads as "no entries" — matching the brief's default-deny
// stance.
func BuildSet(in []string) map[string]struct{} {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(in))
	for _, k := range in {
		out[k] = struct{}{}
	}
	return out
}

// Subset reports whether every entry in `required` exists in
// `allowed`. Empty `required` is vacuously a subset of any allowed
// set (including a nil one). A nil/empty `allowed` rejects any
// non-empty `required` — the default-deny stance.
func Subset(required []string, allowed map[string]struct{}) bool {
	if len(required) == 0 {
		return true
	}
	if allowed == nil {
		return false
	}
	for _, r := range required {
		if _, ok := allowed[r]; !ok {
			return false
		}
	}
	return true
}

// DisallowedNames returns every name in `required` that is NOT in
// `allowed`. Empty names are skipped. The result is the input to
// [Scrub] — the names whose mentions must be rewritten out of
// planner-facing skill text. Brief 04 §4.5: the scrub operates on
// skill text, never on the skill's RequiredTools slice itself.
func DisallowedNames(required []string, allowed map[string]struct{}) []string {
	if len(required) == 0 {
		return nil
	}
	out := make([]string, 0, len(required))
	for _, t := range required {
		if t == "" {
			continue
		}
		if _, ok := allowed[t]; !ok {
			out = append(out, t)
		}
	}
	return out
}

// Replacement picks the disallowed-tool-name replacement variant
// based on whether [ToolSearchName] is in `allowedTools` — a proxy
// for "the run has the search escape hatch."
func Replacement(allowedTools map[string]struct{}) string {
	if _, ok := allowedTools[ToolSearchName]; ok {
		return ReplacementWithSearch
	}
	return ReplacementWithoutSearch
}

// Scrub rewrites every occurrence of a disallowed tool name in `text`
// with `replacement`. Word boundaries prevent a tool named `email`
// from matching `emails`. Each disallowed name is regexp-escaped so a
// name containing regex metacharacters cannot smuggle in a pattern.
//
// A name whose escaped pattern somehow fails to compile is skipped
// defensively — other rewrites still apply, and the canonical audit
// redactor remains the last line of defence.
func Scrub(text string, disallowed []string, replacement string) string {
	if text == "" || len(disallowed) == 0 {
		return text
	}
	for _, name := range disallowed {
		if name == "" {
			continue
		}
		pat := `\b` + regexp.QuoteMeta(name) + `\b`
		re, err := regexp.Compile(pat)
		if err != nil {
			continue
		}
		text = re.ReplaceAllString(text, replacement)
	}
	return text
}
