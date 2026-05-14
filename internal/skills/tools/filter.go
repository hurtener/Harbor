package tools

import (
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/capfilter"
)

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
// The subset logic lives in [capfilter] — the single source shared
// with the virtual directory (internal/skills, Phase 39). Brief
// 04 §4.5: "the skill's `RequiredTools/Namespaces/Tags` must be
// subsets of the allowed sets."
func Filter(in []skills.Skill, cap CapabilityContext) []skills.Skill {
	if len(in) == 0 {
		return nil
	}
	allowTools := capfilter.BuildSet(cap.AllowedTools)
	allowNS := capfilter.BuildSet(cap.AllowedNamespaces)
	allowTags := capfilter.BuildSet(cap.AllowedTags)

	out := make([]skills.Skill, 0, len(in))
	for _, s := range in {
		if !capfilter.Subset(s.RequiredTools, allowTools) {
			continue
		}
		if !capfilter.Subset(s.RequiredNS, allowNS) {
			continue
		}
		if !capfilter.Subset(s.RequiredTags, allowTags) {
			continue
		}
		out = append(out, s)
	}
	return out
}
