package config

import (
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/virtualagent"
)

// ToProfiles converts the boot YAML declaration into the canonical
// virtual-agent profile map (the SAME representation an immutable
// AgentConfig revision's `virtual_agents` section carries). The
// canonical normalization (key-sort, dedup, list canonicalization) is
// applied here so the YAML door and the revision door produce
// byte-identical canonical maps for identical content.
func (v VirtualAgentsConfig) ToMap() (virtualagent.Map, error) {
	if v.IsZero() {
		return virtualagent.Map{}, nil
	}
	m := virtualagent.Map{Owner: strings.TrimSpace(v.Owner)}
	for _, p := range v.Profiles {
		prof, err := p.ToProfile(m.Owner)
		if err != nil {
			return virtualagent.Map{}, err
		}
		m.Profiles = append(m.Profiles, prof)
	}
	return virtualagent.NormalizeMap(m), nil
}

// ToProfile converts one YAML profile declaration into its canonical
// [virtualagent.Profile] form. owner is the block owner; an empty
// profile Parent defaults to it and a non-empty Parent must equal it
// (the one-owner invariant).
func (p VirtualAgentProfileConfig) ToProfile(owner string) (virtualagent.Profile, error) {
	parent := strings.TrimSpace(p.Parent)
	if parent == "" {
		parent = owner
	}
	prof := virtualagent.Profile{
		Key:    virtualagent.Key(strings.TrimSpace(p.Key)),
		Label:  strings.TrimSpace(p.Label),
		Parent: parent,
	}
	if p.LLM != nil {
		if m := strings.TrimSpace(p.LLM.Model); m != "" {
			prof.Overlay.Model = &m
		}
		prof.Overlay.Temperature = p.LLM.Temperature
		prof.Overlay.MaxTokens = p.LLM.MaxTokens
		if r := strings.TrimSpace(p.LLM.ReasoningEffort); r != "" {
			prof.Overlay.ReasoningEffort = &r
		}
	}
	if len(p.Skills) > 0 {
		s := append([]string(nil), p.Skills...)
		prof.Overlay.Skills = &s
	}
	if p.Tools != nil {
		prof.Overlay.DisabledTools = append([]string(nil), p.Tools.DisabledTools...)
		prof.Overlay.PausedServers = append([]string(nil), p.Tools.PausedServers...)
	}
	if p.Limits != nil {
		prof.Overlay.MaxSteps = p.Limits.MaxSteps
		prof.Overlay.TokenBudget = p.Limits.TokenBudget
	}
	prof.Overlay.Instructions = p.Instructions

	prof = virtualagent.NormalizeProfile(prof)
	if err := virtualagent.ValidateProfile(prof); err != nil {
		return virtualagent.Profile{}, fmt.Errorf("virtual_agents.profiles[%q]: %v", p.Key, err)
	}
	return prof, nil
}
