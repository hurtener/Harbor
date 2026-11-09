package react

import (
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
)

// The `<deferred_tools>` budget defaults, in the canonical chars/4 token
// estimate (`llm.EstimateRequestTokens`). A non-positive configured budget
// resolves to its default.
const (
	// DefaultDeferredToolIndexMaxTokensPerSource bounds one source's line:
	// sized to list about 200 names in full even when they carry a
	// runtime-added connection's owner-scoped prefix (~10.3 tokens each).
	DefaultDeferredToolIndexMaxTokensPerSource = 2200
	// DefaultDeferredToolIndexMaxTokens is the whole section's safety bound.
	DefaultDeferredToolIndexMaxTokens = 8000
)

// toolGetToolName is the builtin meta-tool that describes one tool by its
// declared name. The index mentions it only when the run may call it.
const toolGetToolName = "tool_get"

// maxDeferredOwnerLabelRunes bounds one owner label. Labels are
// provider-supplied metadata rendered into the system prompt.
const maxDeferredOwnerLabelRunes = 64

const (
	deferredToolsOpen  = "<deferred_tools>\n"
	deferredToolsClose = "</deferred_tools>"
	// deferredToolsLead tells the model the listed names are not yet
	// declared and how they become callable: only a tool_search result is
	// harvested into the next turn's declarations.
	deferredToolsLead = "These tools are available on demand but are not declared yet. Before calling one, load it with tool_search, using its exact name or a capability query; matching tools become callable on your next turn."
	deferredToolsGet  = " tool_get(name) returns one tool's description and argument schema."
)

// deferredIndexOwner is one owner group of the index: every deferred tool
// the run may discover from one provider source.
type deferredIndexOwner struct {
	key   string
	label string
	tier  tools.PromptStabilityTier
	names []string // declared names, sorted
	// nameBytes[k] is the byte length of the first k names.
	nameBytes []int
}

// renderDeferredToolIndex renders the `<deferred_tools>` section: the
// declared names of every deferred tool the run may discover, grouped by
// the owning provider source, within perSourceTokens per source and
// maxTokens overall. It returns "" when the index has nothing to say.
//
// # Source of truth
//
// The set is the run view's complete discovery snapshot (DiscoveryTools —
// owner scope, exclusions, allowlist, effective loading) restricted to
// effective LoadingDeferred tools, which is exactly what `tool_search`
// may surface. Names are projected with the same reserved-control
// projection over the same discovery order that `tool_search`, `tool_get`
// and the planner's discovery resolution use, so every listed name
// resolves; a residual-collision loser is not listed. A view without a
// complete deferred snapshot, or a run that cannot call `tool_search`,
// renders nothing: the model could not load what the index would name.
//
// # Stability
//
// The section depends only on the run view, never on per-run discoveries
// (a discovered tool stays listed), and owners and names are sorted, so it
// is byte-stable across the turns of a run while the view is unchanged.
func renderDeferredToolIndex(rc planner.RunContext, perSourceTokens, maxTokens int) string {
	if rc.Catalog == nil {
		return ""
	}
	discoveryView, ok := rc.Catalog.(interface{ DiscoveryTools() ([]tools.Tool, bool) })
	if !ok {
		return ""
	}
	if _, ok := rc.Catalog.Resolve(toolSearchToolName); !ok {
		return ""
	}
	candidates, complete := discoveryView.DiscoveryTools()
	if !complete || len(candidates) == 0 {
		return ""
	}
	names := make([]string, 0, len(candidates))
	for _, tool := range candidates {
		names = append(names, tool.Name)
	}
	projection := tools.NewModelToolNameProjectionWithReservedControls(names)

	byOwner := map[string]*deferredIndexOwner{}
	seen := make(map[string]struct{}, len(candidates))
	for _, tool := range candidates {
		if tool.Loading != tools.LoadingDeferred {
			continue
		}
		if _, dup := seen[tool.Name]; dup {
			continue
		}
		seen[tool.Name] = struct{}{}
		declared, kept := projection.DeclaredName(tool.Name)
		if !kept {
			continue
		}
		key := string(tool.Source)
		owner, ok := byOwner[key]
		if !ok {
			owner = &deferredIndexOwner{key: key, label: deferredOwnerLabel(tool), tier: tools.PromptStabilityTierOf(tool)}
			byOwner[key] = owner
		}
		owner.names = append(owner.names, declared)
	}
	if len(byOwner) == 0 {
		return ""
	}
	owners := make([]*deferredIndexOwner, 0, len(byOwner))
	for _, owner := range byOwner {
		sort.Strings(owner.names)
		owner.nameBytes = make([]int, len(owner.names)+1)
		for i, name := range owner.names {
			owner.nameBytes[i+1] = owner.nameBytes[i] + len(name)
		}
		owners = append(owners, owner)
	}
	sort.Slice(owners, func(i, j int) bool {
		a, b := owners[i], owners[j]
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		if a.label != b.label {
			return a.label < b.label
		}
		return a.key < b.key
	})

	lead := deferredToolsLead
	if _, ok := rc.Catalog.Resolve(toolGetToolName); ok {
		lead += deferredToolsGet
	}
	if perSourceTokens <= 0 {
		perSourceTokens = DefaultDeferredToolIndexMaxTokensPerSource
	}
	if maxTokens <= 0 {
		maxTokens = DefaultDeferredToolIndexMaxTokens
	}
	return layoutDeferredToolIndex(lead, owners, perSourceTokens*4, maxTokens*4)
}

// layoutDeferredToolIndex fits the owner lines into the chars/4 estimate's
// byte budgets: each owner line into perOwnerBytes, the whole section into
// budgetBytes. Each owner's cap is the most sorted names whose line fits
// perOwnerBytes. Every owner first gets a count-only line; the overall
// budget is then shared round-robin, one name at a time in sorted order up
// to each owner's cap, so a large owner cannot crowd out a small one. An
// owner whose next name does not fit stops growing; the others continue.
// When even the count-only lines exceed the overall budget, trailing owners
// collapse into one summary line — the only case an owner is not named.
func layoutDeferredToolIndex(lead string, owners []*deferredIndexOwner, perOwnerBytes, budgetBytes int) string {
	fixed := len(deferredToolsOpen) + len(lead) + 1 + len(deferredToolsClose)
	shown := make([]int, len(owners))
	total := fixed
	for _, owner := range owners {
		total += deferredOwnerLineLen(owner, 0)
	}

	included := len(owners)
	if total > budgetBytes {
		// Count-only lines do not all fit: keep the longest owner prefix
		// that fits together with the summary line for the rest.
		total = fixed
		included = 0
		for included < len(owners) {
			next := total + deferredOwnerLineLen(owners[included], 0)
			if next+len(deferredOverflowLine(owners[included+1:])) > budgetBytes {
				break
			}
			total = next
			included++
		}
	} else {
		limit := make([]int, len(owners))
		for i, owner := range owners {
			limit[i] = deferredOwnerCap(owner, perOwnerBytes)
		}
		stopped := make([]bool, len(owners))
		for grew := true; grew; {
			grew = false
			for i, owner := range owners {
				if stopped[i] || shown[i] == limit[i] {
					continue
				}
				delta := deferredOwnerLineLen(owner, shown[i]+1) - deferredOwnerLineLen(owner, shown[i])
				if total+delta > budgetBytes {
					stopped[i] = true
					continue
				}
				shown[i]++
				total += delta
				grew = true
			}
		}
	}

	var b strings.Builder
	b.Grow(total)
	b.WriteString(deferredToolsOpen)
	b.WriteString(lead)
	b.WriteByte('\n')
	for i, owner := range owners[:included] {
		writeDeferredOwnerLine(&b, owner, shown[i])
	}
	if included < len(owners) {
		b.WriteString(deferredOverflowLine(owners[included:]))
	}
	b.WriteString(deferredToolsClose)
	return b.String()
}

// deferredOwnerLine shapes, for an owner with N tools and k shown names:
//
//	k == N: "- <label> (N tools): a, b\n"
//	0<k<N:  "- <label> (N tools): a, … and M more; use tool_search\n"
//	k == 0: "- <label> (N tools): names omitted; use tool_search\n"
const (
	deferredMoreTail    = " more; use tool_search"
	deferredOmittedTail = "names omitted; use tool_search"
	deferredMoreLead    = "… and "
)

func deferredOwnerHead(owner *deferredIndexOwner) string {
	return "- " + owner.label + " (" + deferredToolCount(len(owner.names)) + "): "
}

func deferredToolCount(n int) string {
	if n == 1 {
		return "1 tool"
	}
	return strconv.Itoa(n) + " tools"
}

// deferredOwnerCap returns the most names the owner's line can show within
// perOwnerBytes (0 = count-only). The full list drops the "more" suffix, so
// it is checked on its own.
func deferredOwnerCap(owner *deferredIndexOwner, perOwnerBytes int) int {
	if deferredOwnerLineLen(owner, len(owner.names)) <= perOwnerBytes {
		return len(owner.names)
	}
	k := 0
	for k+1 < len(owner.names) && deferredOwnerLineLen(owner, k+1) <= perOwnerBytes {
		k++
	}
	return k
}

func deferredOwnerLineLen(owner *deferredIndexOwner, shown int) int {
	n := len(owner.names)
	size := len(deferredOwnerHead(owner)) + 1
	if shown == 0 {
		return size + len(deferredOmittedTail)
	}
	size += owner.nameBytes[shown] + 2*(shown-1)
	if shown < n {
		size += len(", ") + len(deferredMoreLead) + len(strconv.Itoa(n-shown)) + len(deferredMoreTail)
	}
	return size
}

func writeDeferredOwnerLine(b *strings.Builder, owner *deferredIndexOwner, shown int) {
	b.WriteString(deferredOwnerHead(owner))
	if shown == 0 {
		b.WriteString(deferredOmittedTail)
		b.WriteByte('\n')
		return
	}
	b.WriteString(strings.Join(owner.names[:shown], ", "))
	if rest := len(owner.names) - shown; rest > 0 {
		b.WriteString(", ")
		b.WriteString(deferredMoreLead)
		b.WriteString(strconv.Itoa(rest))
		b.WriteString(deferredMoreTail)
	}
	b.WriteByte('\n')
}

// deferredOverflowLine summarizes owners that do not fit even as
// count-only lines: "- … and K more sources (M tools); use tool_search\n".
func deferredOverflowLine(rest []*deferredIndexOwner) string {
	if len(rest) == 0 {
		return ""
	}
	toolCount := 0
	for _, owner := range rest {
		toolCount += len(owner.names)
	}
	sources := "sources"
	if len(rest) == 1 {
		sources = "source"
	}
	return "- " + deferredMoreLead + strconv.Itoa(len(rest)) + " more " + sources + " (" + deferredToolCount(toolCount) + "); use tool_search\n"
}

// deferredOwnerLabel returns the owner's display label: the provider's own
// title when it supplied one, else the logical source name (the physical
// owner-scoping suffix stripped), else "built-in" for in-process tools.
// The label is provider-controlled text entering the system prompt, so it
// is reduced to one bounded line without markup delimiters.
func deferredOwnerLabel(tool tools.Tool) string {
	if label := sanitizeDeferredOwnerLabel(tool.SourceTitle); label != "" {
		return label
	}
	source := string(tool.Source)
	for _, marker := range []string{tools.AgentSourceMarker, tools.UserSourceMarker} {
		if i := strings.Index(source, marker); i > 0 {
			source = source[:i]
		}
	}
	if label := sanitizeDeferredOwnerLabel(source); label != "" {
		return label
	}
	return "built-in"
}

func sanitizeDeferredOwnerLabel(s string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r == '<' || r == '>' || r == '(' || r == ')':
			return ' '
		case unicode.IsControl(r) || unicode.IsSpace(r):
			return ' '
		case !unicode.IsPrint(r):
			return -1
		}
		return r
	}, s)
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	if utf8.RuneCountInString(cleaned) > maxDeferredOwnerLabelRunes {
		runes := []rune(cleaned)
		cleaned = strings.TrimSpace(string(runes[:maxDeferredOwnerLabelRunes-1])) + "…"
	}
	return cleaned
}
