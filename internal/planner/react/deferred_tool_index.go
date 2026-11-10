package react

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
)

// The `<deferred_tools>` budget defaults, in the index's own conservative
// token estimate ([estimateIndexTokens]). A non-positive configured budget
// resolves to its default.
const (
	// DefaultDeferredToolIndexMaxTokensPerSource bounds one source's line:
	// sized to list about 200 readable MCP-style tool names in full.
	DefaultDeferredToolIndexMaxTokensPerSource = 1600
	// DefaultDeferredToolIndexMaxTokens is the whole section's safety bound.
	DefaultDeferredToolIndexMaxTokens = 8000
)

// toolGetToolName is the builtin meta-tool that describes one tool. The
// index mentions it only when the run may call it.
const toolGetToolName = "tool_get"

const (
	deferredToolsOpen  = "<deferred_tools>\n"
	deferredToolsClose = "</deferred_tools>"
	// deferredToolsLead tells the model the listed tools are not declared
	// yet and how one becomes callable: only a tool_search result is
	// harvested into the next turn's declarations. tool_search resolves the
	// listed `<source>/<tool>` reference exactly.
	deferredToolsLead = `These tools are not declared yet; tools in available_tools can be called directly. To load one, call tool_search with query "<source>/<tool>" exactly as listed (for example "Mail/send" for send under Mail); it becomes callable on your next turn. Only tool_search loads tools.`
	deferredToolsGet  = ` tool_get("<source>/<tool>") only describes a tool; it does not load it.`
)

// deferredIndexOwner is one owner group of the index: every deferred tool
// the run may discover from one provider source.
type deferredIndexOwner struct {
	key   string
	label string
	tier  tools.PromptStabilityTier
	names []string // listed names, sorted, unique within the owner
	// nameTokens[k] is the estimated token cost of the first k names.
	nameTokens []int
}

// finish sorts the owner's names and precomputes their token costs.
func (o *deferredIndexOwner) finish() {
	sort.Strings(o.names)
	o.nameTokens = make([]int, len(o.names)+1)
	for i, name := range o.names {
		o.nameTokens[i+1] = o.nameTokens[i] + estimateIndexTokens(name)
	}
}

// discoverySnapshot is one turn's read of the run view's discovery set,
// taken at most once and shared by the declaration projection and the
// deferred-tool index (rebuilds included), so a turn scans the catalog
// once and both surfaces see the same snapshot.
type discoverySnapshot struct {
	view     planner.ToolCatalogView
	once     sync.Once
	tools    []tools.Tool
	complete bool
	ok       bool
}

func newDiscoverySnapshot(view planner.ToolCatalogView) *discoverySnapshot {
	return &discoverySnapshot{view: view}
}

// get returns the view's discovery set, whether it is a complete deferred
// snapshot, and whether the view exposes discovery at all.
func (s *discoverySnapshot) get() ([]tools.Tool, bool, bool) {
	s.once.Do(func() {
		if s.view == nil {
			return
		}
		discovery, ok := s.view.(interface{ DiscoveryTools() ([]tools.Tool, bool) })
		if !ok {
			return
		}
		s.tools, s.complete = discovery.DiscoveryTools()
		s.ok = true
	})
	return s.tools, s.complete, s.ok
}

// renderDeferredToolIndex renders the index from a fresh snapshot with no
// granted scopes and the default budgets.
func renderDeferredToolIndex(rc planner.RunContext) string {
	return renderDeferredToolIndexFrom(rc, newDiscoverySnapshot(rc.Catalog), nil, 0, 0)
}

// renderDeferredToolIndexFrom renders the `<deferred_tools>` section: every
// deferred tool the run may discover, grouped by the owning provider source,
// within perSourceTokens per source and maxTokens overall. It returns ""
// when the index has nothing to say.
//
// # Source of truth
//
// The set and every rendered name come from [tools.NewDiscoveryNamespace]
// over the run view's complete discovery snapshot (owner scope,
// exclusions, allowlist, effective loading) and grantedScopes — the same
// constructor, predicate and scopes the discovery meta-tools use — so the
// index lists exactly the deferred tools `tool_search` may surface, under
// the disambiguated source labels and listed names that `tool_search` and
// `tool_get` resolve as `<source label>/<tool name>`. A listed name is the
// tool's readable provider-local name when prompt-safe, else its declared
// name. A view without a complete deferred snapshot, or a run that cannot
// call `tool_search`, renders nothing: the model could not load what the
// index would name.
//
// # Stability
//
// The section depends only on the run view, never on per-run discoveries
// (a discovered tool stays listed), and owners and names are sorted, so it
// is byte-stable across the turns of a run while the view is unchanged. A
// change to a deferred source (a list_changed refresh, an allowlist or
// loading-mode edit) changes it on the next turn.
func renderDeferredToolIndexFrom(rc planner.RunContext, snapshot *discoverySnapshot, grantedScopes []string, perSourceTokens, maxTokens int) string {
	if rc.Catalog == nil {
		return ""
	}
	if _, ok := rc.Catalog.Resolve(toolSearchToolName); !ok {
		return ""
	}
	candidates, complete, ok := snapshot.get()
	if !ok || !complete || len(candidates) == 0 {
		return ""
	}
	ns := tools.NewDiscoveryNamespace(candidates, grantedScopes)
	byOwner := map[tools.ToolSourceID]*deferredIndexOwner{}
	var owners []*deferredIndexOwner
	for _, tool := range ns.DeferredTools() {
		owner, ok := byOwner[tool.Source]
		if !ok {
			label, _ := ns.SourceLabel(tool.Source)
			owner = &deferredIndexOwner{key: string(tool.Source), label: label, tier: tools.PromptStabilityTierOf(tool)}
			byOwner[tool.Source] = owner
			owners = append(owners, owner)
		}
		listed, _ := ns.ListedName(tool.Name)
		owner.names = append(owner.names, listed)
	}
	if len(owners) == 0 {
		return ""
	}
	for _, owner := range owners {
		owner.finish()
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
	return layoutDeferredToolIndex(lead, owners, perSourceTokens, maxTokens)
}

// layoutDeferredToolIndex fits the owner lines into token budgets (the
// index's [estimateIndexTokens]): each owner line into perOwnerTokens, the
// whole section into budgetTokens. Each owner's cap is the most sorted
// names whose line fits perOwnerTokens. Every owner first gets a
// count-only line; the overall budget is then shared round-robin, one name
// at a time in sorted order up to each owner's cap, so a large owner cannot
// crowd out a small one. An owner whose next name does not fit stops
// growing; the others continue. When even the count-only lines exceed the
// overall budget, trailing owners collapse into one summary line — the
// only case an owner is not named.
func layoutDeferredToolIndex(lead string, owners []*deferredIndexOwner, perOwnerTokens, budgetTokens int) string {
	fixed := estimateIndexTokens(deferredToolsOpen) + estimateIndexTokens(lead) + 1 + estimateIndexTokens(deferredToolsClose)
	shown := make([]int, len(owners))
	total := fixed
	for _, owner := range owners {
		total += deferredOwnerLineTokens(owner, 0)
	}

	included := len(owners)
	if total > budgetTokens {
		// Count-only lines do not all fit: keep the longest owner prefix
		// that fits together with the summary line for the rest.
		total = fixed
		included = 0
		for included < len(owners) {
			next := total + deferredOwnerLineTokens(owners[included], 0)
			if next+estimateIndexTokens(deferredOverflowLine(owners[included+1:])) > budgetTokens {
				break
			}
			total = next
			included++
		}
	} else {
		limit := make([]int, len(owners))
		for i, owner := range owners {
			limit[i] = deferredOwnerCap(owner, perOwnerTokens)
		}
		stopped := make([]bool, len(owners))
		for grew := true; grew; {
			grew = false
			for i, owner := range owners {
				if stopped[i] || shown[i] == limit[i] {
					continue
				}
				delta := deferredOwnerLineTokens(owner, shown[i]+1) - deferredOwnerLineTokens(owner, shown[i])
				if total+delta > budgetTokens {
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
// perOwnerTokens (0 = count-only). The full list drops the "more" suffix,
// so it is checked on its own.
func deferredOwnerCap(owner *deferredIndexOwner, perOwnerTokens int) int {
	if deferredOwnerLineTokens(owner, len(owner.names)) <= perOwnerTokens {
		return len(owner.names)
	}
	k := 0
	for k+1 < len(owner.names) && deferredOwnerLineTokens(owner, k+1) <= perOwnerTokens {
		k++
	}
	return k
}

// deferredOwnerLineTokens estimates one owner line's cost as the sum of its
// parts: the head, the shown names, one token per ", " separator, the
// truncation tail and the newline.
func deferredOwnerLineTokens(owner *deferredIndexOwner, shown int) int {
	n := len(owner.names)
	cost := estimateIndexTokens(deferredOwnerHead(owner)) + 1
	if shown == 0 {
		return cost + estimateIndexTokens(deferredOmittedTail)
	}
	cost += owner.nameTokens[shown] + (shown - 1)
	if shown < n {
		cost += 1 + estimateIndexTokens(deferredMoreLead+strconv.Itoa(n-shown)+deferredMoreTail)
	}
	return cost
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

// estimateIndexTokens is a deliberately conservative token estimate for
// the index's identifier-heavy text, stricter than the request-wide
// chars/4 estimate (`llm.EstimateRequestTokens`), which undercounts
// identifiers: BPE tokenizers split hex digests, digit runs and short
// fragments into many small tokens. Over each maximal ASCII alphanumeric
// run:
//
//   - a hex digest (only [0-9a-f], at least one digit and one letter,
//     8+ characters) costs one token per 2.5 characters;
//   - otherwise each letter sub-run costs one token per 6 letters and each
//     digit sub-run one token per 3 digits (each at least one).
//
// A space or an underscore merges into the following word and costs
// nothing; every other character (punctuation, "…", newline) costs one.
func estimateIndexTokens(s string) int {
	tokens := 0
	for i := 0; i < len(s); {
		c := s[i]
		if !isASCIIAlnum(c) {
			r, size := utf8.DecodeRuneInString(s[i:])
			if r != ' ' && r != '_' {
				tokens++
			}
			i += size
			continue
		}
		j := i
		for j < len(s) && isASCIIAlnum(s[j]) {
			j++
		}
		tokens += alnumRunTokens(s[i:j])
		i = j
	}
	return tokens
}

func isASCIIAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func alnumRunTokens(run string) int {
	hex, digits, letters := true, 0, 0
	for i := range len(run) {
		c := run[i]
		switch {
		case c >= '0' && c <= '9':
			digits++
		case c >= 'a' && c <= 'f':
			letters++
		default:
			hex = false
			letters++
		}
	}
	if hex && digits > 0 && letters > 0 && len(run) >= 8 {
		return (len(run)*2 + 4) / 5 // ceil(len / 2.5)
	}
	tokens := 0
	for i := 0; i < len(run); {
		j := i
		isDigit := run[i] >= '0' && run[i] <= '9'
		for j < len(run) && (run[j] >= '0' && run[j] <= '9') == isDigit {
			j++
		}
		per := 6
		if isDigit {
			per = 3
		}
		tokens += (j - i + per - 1) / per
		i = j
	}
	return tokens
}
