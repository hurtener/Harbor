package tools

import (
	"sort"
	"strings"
)

// Owner-derived physical source markers. A runtime-added MCP connection is
// registered under `<logical><marker><digest>`: the agent marker for a
// connection owned by one (tenant, agent) pair, the user marker for one
// owned by a single user of that pair. Boot-declared sources keep their bare
// logical name. The MCP registry derives the physical id with these markers;
// prompt ordering reads them back to classify a tool's stability tier.
const (
	AgentSourceMarker = "~a-"
	UserSourceMarker  = "~u-"
)

// PromptStabilityTier classifies how often a tool's presence changes for the
// same agent across turns, from most stable to least. Provider prompt caches
// match an exact token prefix, and every provider serialises the tool
// declarations ahead of the conversation, so one insertion invalidates every
// declaration after it. Ordering declarations by tier keeps the long, shared
// part of the catalog an unchanged prefix when a volatile tool attaches.
//
// Classification only: a tier is never an authorization or dispatch input.
type PromptStabilityTier int

const (
	// PromptTierBuiltin is in-process tools (runtime meta-tools and
	// operator-registered functions): fixed for the process lifetime.
	PromptTierBuiltin PromptStabilityTier = iota
	// PromptTierShared is boot-declared and operator sources (HTTP, A2A,
	// flow, bare-named MCP): shared by every user of the deployment.
	PromptTierShared
	// PromptTierAgent is a runtime-added connection owned by the agent:
	// shared by every user of that agent.
	PromptTierAgent
	// PromptTierUser is a connection owned by one user, which can attach
	// lazily between that user's turns.
	PromptTierUser
)

// PromptStabilityTierOf returns t's prompt stability tier.
func PromptStabilityTierOf(t Tool) PromptStabilityTier {
	source := string(t.Source)
	switch {
	case strings.Contains(source, UserSourceMarker):
		return PromptTierUser
	case strings.Contains(source, AgentSourceMarker):
		return PromptTierAgent
	case t.Transport == "" || t.Transport == TransportInProcess:
		return PromptTierBuiltin
	default:
		return PromptTierShared
	}
}

// OrderForPromptStability returns a copy of in ordered by
// [PromptStabilityTier] (builtin, shared, agent, user). The sort is stable,
// so within a tier the caller's deterministic order is kept: catalog List
// results are already sorted by name. The input slice is not modified.
func OrderForPromptStability(in []Tool) []Tool {
	out := append([]Tool(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		return PromptStabilityTierOf(out[i]) < PromptStabilityTierOf(out[j])
	})
	return out
}
