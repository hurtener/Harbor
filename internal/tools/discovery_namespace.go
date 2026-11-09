package tools

import "strings"

// DiscoveryNamespace is one run's discovery namespace as the discovery
// meta-tools (`tool_search`, `tool_get`, `declarative_action`) and the
// planner's deferred-tool index see it: the run view's discovery set, kept
// by the reserved-control declared-name projection and narrowed by the
// granted scopes. Building both surfaces through this one constructor is
// what makes every name the index lists resolve in the meta-tools.
//
// Concurrent reuse: immutable after construction; build one per decision.
type DiscoveryNamespace struct {
	projection ModelToolNameProjection
	// visible maps a physical catalog name to the run view's Tool for every
	// tool the run may discover.
	visible map[string]Tool
	// order lists visible's keys in namespace-precedence order.
	order []string
	// labels is the rendered label of every source with a deferred tool.
	labels map[ToolSourceID]string
	// listed maps a deferred tool's catalog name to its listed name.
	listed map[string]string
}

// NewDiscoveryNamespace builds the namespace from a view's discovery set
// (see [DiscoveryToolsOf]) in the view's order. grantedScopes narrows it:
// a tool is visible only when every AuthScope it requires is granted — the
// same predicate for every consumer. It never widens the view.
func NewDiscoveryNamespace(candidates []Tool, grantedScopes []string) DiscoveryNamespace {
	names := make([]string, 0, len(candidates))
	for _, tool := range candidates {
		names = append(names, tool.Name)
	}
	projection := NewModelToolNameProjectionWithReservedControls(names)
	granted := make(map[string]struct{}, len(grantedScopes))
	for _, s := range grantedScopes {
		granted[s] = struct{}{}
	}
	n := DiscoveryNamespace{
		projection: projection,
		visible:    make(map[string]Tool, len(candidates)),
		order:      make([]string, 0, len(candidates)),
	}
	var deferred []Tool
	for _, tool := range candidates {
		if _, dup := n.visible[tool.Name]; dup {
			continue
		}
		// A residual-collision loser is not callable under the declared
		// namespace, so it is neither searched nor listed.
		if _, kept := projection.DeclaredName(tool.Name); !kept {
			continue
		}
		if !ScopesGranted(tool.AuthScopes, granted) {
			continue
		}
		n.visible[tool.Name] = tool
		n.order = append(n.order, tool.Name)
		if tool.Loading == LoadingDeferred {
			deferred = append(deferred, tool)
		}
	}
	n.labels = DisambiguatedSourceLabels(deferred)
	n.listed = listedNames(deferred, projection)
	return n
}

// ScopesGranted is the discovery scope predicate: every scope required is
// in granted. A tool requiring no scope always passes.
func ScopesGranted(required []string, granted map[string]struct{}) bool {
	for _, s := range required {
		if _, ok := granted[s]; !ok {
			return false
		}
	}
	return true
}

// listedNames picks each deferred tool's listed name: its readable name when
// that is prompt-safe ([IsListableToolName]) and unique within its source,
// else its declared name (already provider-safe and unique).
func listedNames(deferred []Tool, projection ModelToolNameProjection) map[string]string {
	count := map[ToolSourceID]map[string]int{}
	for _, t := range deferred {
		if count[t.Source] == nil {
			count[t.Source] = map[string]int{}
		}
		count[t.Source][ReadableToolName(t)]++
	}
	out := make(map[string]string, len(deferred))
	for _, t := range deferred {
		name := ReadableToolName(t)
		if !IsListableToolName(name) || count[t.Source][name] > 1 {
			name, _ = projection.DeclaredName(t.Name)
		}
		out[t.Name] = name
	}
	return out
}

// Projection returns the declared-name projection over the discovery set.
func (n DiscoveryNamespace) Projection() ModelToolNameProjection { return n.projection }

// Visible returns the run view's Tool for a visible catalog name.
func (n DiscoveryNamespace) Visible(catalogName string) (Tool, bool) {
	t, ok := n.visible[catalogName]
	return t, ok
}

// Order lists the visible catalog names in namespace-precedence order. The
// slice is shared; callers must not mutate it.
func (n DiscoveryNamespace) Order() []string { return n.order }

// DeferredTools returns the visible deferred tools in namespace order.
func (n DiscoveryNamespace) DeferredTools() []Tool {
	out := make([]Tool, 0, len(n.listed))
	for _, name := range n.order {
		if t := n.visible[name]; t.Loading == LoadingDeferred {
			out = append(out, t)
		}
	}
	return out
}

// SourceLabel returns the rendered, disambiguated label of a source that has
// a visible deferred tool.
func (n DiscoveryNamespace) SourceLabel(source ToolSourceID) (string, bool) {
	l, ok := n.labels[source]
	return l, ok
}

// ListedName returns the name the deferred-tool index lists for a visible
// deferred tool: prompt-safe, unique within its source line.
func (n DiscoveryNamespace) ListedName(catalogName string) (string, bool) {
	l, ok := n.listed[catalogName]
	return l, ok
}

// ExactMatches resolves query as an exact tool reference, in precedence
// order, and reports whether the matches are the whole answer (exactOnly):
//
//  1. `<source label>/<tool name>` — the rendered label of a deferred
//     source (exactly as the index renders it) and that tool's listed or
//     declared name. Exact-only.
//  2. A declared model name. Exact-only.
//  3. A physical catalog name. Exact-only.
//  4. A bare readable provider-local name, possibly shared by several
//     sources: all matches, in namespace order, NOT exact-only — callers
//     follow them with their ordinary ranked results.
//
// Only visible tools can match, so scoping is exactly the namespace's.
func (n DiscoveryNamespace) ExactMatches(query string) (matches []string, exactOnly bool) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, false
	}
	if label, name, ok := strings.Cut(query, QualifiedToolNameSeparator); ok {
		for _, catalogName := range n.order {
			t := n.visible[catalogName]
			listed, isDeferred := n.listed[catalogName]
			if !isDeferred || n.labels[t.Source] != label {
				continue
			}
			declared, _ := n.projection.DeclaredName(catalogName)
			if name == listed || name == declared {
				matches = append(matches, catalogName)
			}
		}
		if len(matches) > 0 {
			return matches, true
		}
	}
	if catalogName, ok := n.projection.ResolveDeclared(query); ok {
		if _, visible := n.visible[catalogName]; visible {
			return []string{catalogName}, true
		}
	}
	if _, visible := n.visible[query]; visible {
		return []string{query}, true
	}
	for _, catalogName := range n.order {
		if ReadableToolName(n.visible[catalogName]) == query {
			matches = append(matches, catalogName)
		}
	}
	return matches, false
}
