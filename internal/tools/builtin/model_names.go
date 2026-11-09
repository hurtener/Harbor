package builtin

import (
	"context"
	"strings"

	"github.com/hurtener/Harbor/internal/tools"
)

// ErrRunToolViewRequired is returned by the run-scoped meta-tools
// (`tool_search`, `tool_get`, `declarative_action`) when ctx carries no run
// tool view. The process catalog holds every principal's tools, so these
// tools never fall back to it: without the calling run's sealed view there is
// nothing they may describe or dispatch.
//
// It is a configuration fault, not a transient one, so the policy shell never
// retries it.
var ErrRunToolViewRequired error = permanentError("builtin: the calling run's tool view is required")

// permanentError is a comparable sentinel the policy shell classifies as
// non-retryable (it implements Permanent).
type permanentError string

func (e permanentError) Error() string   { return string(e) }
func (e permanentError) Permanent() bool { return true }

// runDiscovery is the calling run's discovery namespace: exactly the tools
// the run's sealed view admits for invocation (owner scope, exclusions,
// allowlist, effective loading — both loading modes, since tool_search is the
// path by which deferred tools become model-visible), projected onto the
// model-visible declared names the planner uses.
type runDiscovery struct {
	view       tools.PlannerCatalogView
	projection tools.ModelToolNameProjection
	// visible maps a physical catalog name to the run view's Tool for every
	// tool the run may discover: kept by the declared-name projection and
	// covered by the registration's granted scopes.
	visible map[string]tools.Tool
	// order lists visible's keys in namespace-precedence order.
	order []string
}

// discoveryForRun builds the caller's discovery namespace from the run tool
// view seated on ctx. Identity is checked first (fail loud), then the view
// (fail closed). grantedScopes is the registration-time operator scope list,
// applied as an additional narrowing on top of the view — never a widening.
//
// The projection is built over the view's complete discovery set in the
// view's own order, which is the same construction the ReAct planner uses to
// resolve a discovered declared name, so a name taught here resolves to the
// same physical tool there.
func discoveryForRun(ctx context.Context, grantedScopes []string) (runDiscovery, error) {
	if _, err := requireIdentity(ctx); err != nil {
		return runDiscovery{}, err
	}
	view, ok := tools.RunToolView(ctx)
	if !ok {
		return runDiscovery{}, ErrRunToolViewRequired
	}
	// Memoized per seat (one dispatch decision), so a decision's meta-tool
	// calls share one scan of the view.
	candidates, _, _ := tools.RunToolDiscovery(ctx)
	names := make([]string, 0, len(candidates))
	for _, tool := range candidates {
		names = append(names, tool.Name)
	}
	projection := tools.NewModelToolNameProjectionWithReservedControls(names)
	granted := make(map[string]struct{}, len(grantedScopes))
	for _, s := range grantedScopes {
		granted[s] = struct{}{}
	}
	d := runDiscovery{
		view:       view,
		projection: projection,
		visible:    make(map[string]tools.Tool, len(candidates)),
		order:      make([]string, 0, len(candidates)),
	}
	for _, tool := range candidates {
		if _, dup := d.visible[tool.Name]; dup {
			continue
		}
		// Search indexes can lag catalog changes and a residual-collision
		// loser is not callable under the declared namespace; neither is
		// taught to the model.
		if _, kept := projection.DeclaredName(tool.Name); !kept {
			continue
		}
		if !scopesCovered(tool.AuthScopes, granted) {
			continue
		}
		d.visible[tool.Name] = tool
		d.order = append(d.order, tool.Name)
	}
	return d, nil
}

func scopesCovered(required []string, granted map[string]struct{}) bool {
	for _, s := range required {
		if _, ok := granted[s]; !ok {
			return false
		}
	}
	return true
}

// resolveDeclared maps a model-authored declared name to the run view's
// Tool. Only a name the run may discover resolves; the view's own Resolve is
// re-checked so the result is exactly what the dispatcher would admit.
func (d runDiscovery) resolveDeclared(declared string) (tools.Tool, bool) {
	catalogName, ok := d.projection.ResolveDeclared(declared)
	if !ok {
		return tools.Tool{}, false
	}
	if _, ok := d.visible[catalogName]; !ok {
		return tools.Tool{}, false
	}
	resolved, ok := d.view.Resolve(catalogName)
	if !ok || resolved.Name != catalogName {
		return tools.Tool{}, false
	}
	return resolved, true
}

// exactMatches resolves query as an exact tool reference within the run's
// discovery namespace, in precedence order: a declared model name, a
// physical catalog name, then the readable provider-local name — bare, or
// qualified by its source label as `<source label>/<tool name>` (the form
// the planner's deferred-tool index lists). A bare readable name may match
// tools of several sources; all of them are returned in namespace order.
// Only tools in d.visible can match, so scoping is exactly the search's.
func (d runDiscovery) exactMatches(query string) []string {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}
	if catalogName, ok := d.projection.ResolveDeclared(query); ok {
		if _, visible := d.visible[catalogName]; visible {
			return []string{catalogName}
		}
	}
	if _, visible := d.visible[query]; visible {
		return []string{query}
	}
	var out []string
	for _, name := range d.order {
		t := d.visible[name]
		if tools.ReadableToolName(t) == query || tools.MatchesQualifiedToolName(t, query) {
			out = append(out, name)
		}
	}
	return out
}

// resolveExact maps an exact, unambiguous reference (see exactMatches) to
// the run view's Tool, re-checked against the view like resolveDeclared.
func (d runDiscovery) resolveExact(query string) (tools.Tool, bool) {
	if t, ok := d.resolveDeclared(query); ok {
		return t, true
	}
	matches := d.exactMatches(query)
	if len(matches) != 1 {
		return tools.Tool{}, false
	}
	resolved, ok := d.view.Resolve(matches[0])
	if !ok || resolved.Name != matches[0] {
		return tools.Tool{}, false
	}
	return resolved, true
}
