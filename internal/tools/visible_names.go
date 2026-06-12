package tools

import "sort"

// VisibleNames returns the sorted names of every tool visible under
// `filter` — both `LoadingAlways` and `LoadingDeferred` modes, so the
// result is the run's FULL reachable tool set (the prompt-time set
// plus everything `tool_search` can surface).
//
// this is the ONE producer of the "allowed tools"
// capability input the skills surface consumes — the builtin
// `skill_search` / `skill_get` / `skill_list` delegations and the run
// loop's `skills.Directory.View` call both derive their
// `CapabilityContext.AllowedTools` / `DirectoryCapability.AllowedTools`
// from it. Centralised here so the three call sites cannot drift (the
// SDK friction audit's triple-duplication finding, read forward).
//
// The supplied filter's LoadingModes is OVERRIDDEN to the full
// two-mode set; every other predicate (identity triple, GrantedScopes,
// NameRegex) applies as given. A nil catalog returns nil — callers on
// catalog-less stacks get the empty (default-deny) capability set.
//
// Concurrent reuse: pure function over the catalog's
// concurrent-safe List; no shared state.
func VisibleNames(cat ToolCatalog, filter CatalogFilter) []string {
	if cat == nil {
		return nil
	}
	filter.LoadingModes = []LoadingMode{LoadingAlways, LoadingDeferred}
	visible := cat.List(filter)
	if len(visible) == 0 {
		return nil
	}
	names := make([]string, 0, len(visible))
	for _, t := range visible {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return names
}
