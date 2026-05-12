package tools

import "github.com/hurtener/Harbor/internal/skills"

// Filter applies the capability subset gate to `in`. A skill passes
// when, for each of `RequiredTools / RequiredNS / RequiredTags`, the
// required entries are a subset of the corresponding `Allowed*` set
// in `cap`. A skill with empty required lists for every axis is
// always allowed.
//
// The order of `in` is preserved. The returned slice is a fresh
// allocation — callers may mutate it freely. Concurrent-safe by
// construction (pure function over value-type inputs).
//
// Brief 04 §4.5: "the skill's `RequiredTools/Namespaces/Tags` must
// be subsets of the allowed sets" — ported verbatim.
func Filter(in []skills.Skill, cap CapabilityContext) []skills.Skill {
	if len(in) == 0 {
		return nil
	}
	allowTools := buildSet(cap.AllowedTools)
	allowNS := buildSet(cap.AllowedNamespaces)
	allowTags := buildSet(cap.AllowedTags)

	out := make([]skills.Skill, 0, len(in))
	for _, s := range in {
		if !subsetOf(s.RequiredTools, allowTools) {
			continue
		}
		if !subsetOf(s.RequiredNS, allowNS) {
			continue
		}
		if !subsetOf(s.RequiredTags, allowTags) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// buildSet returns a hash-set lookup over `in`. Empty input yields a
// nil set; callers test membership via `_, ok := set[k]`, so a nil
// map reads as "no entries" — matching the brief's default-deny
// stance.
func buildSet(in []string) map[string]struct{} {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(in))
	for _, k := range in {
		out[k] = struct{}{}
	}
	return out
}

// subsetOf reports whether every entry in `required` exists in
// `allowed`. Empty `required` is vacuously a subset of any allowed
// set (including a nil one). Empty `allowed` rejects any non-empty
// `required` — the default-deny stance.
func subsetOf(required []string, allowed map[string]struct{}) bool {
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
