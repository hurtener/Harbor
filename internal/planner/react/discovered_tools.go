package react

import (
	"encoding/json"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
)

// toolSearchToolName is the canonical builtin meta-tool whose results
// the React planner harvests for per-run discovered-tool surfacing
// (Phase 107c — AC-18 / D-167). The builtin lives at
// `internal/tools/builtin/tool_search.go`; its `Invoke` returns a JSON
// payload whose top-level `tools` array carries `{name, description}`
// entries. The planner reads `tools[].name` and adds each name to the
// next turn's `req.Tools` declaration so the LLM can call discovered
// surfaces natively without re-discovery.
//
// A second producer (`skill_search`) lives alongside it but does NOT
// contribute to the discovered-TOOLS set — skills are pre-prompt
// retrieval surfaces, not invokable tools.
const toolSearchToolName = "tool_search"

// deriveDiscoveredFromTrajectory walks the trajectory's executed steps
// and returns the union of tool names surfaced by every prior
// `tool_search` invocation's observation (AC-18). The function reads
// only — it never mutates the trajectory. Returns a nil slice when no
// `tool_search` step landed yet.
//
// Per-step observation shape (the `tool_search` builtin's contract):
//
//	{
//	  "tools": [
//	    {"name": "<tool-1-name>", ...},
//	    {"name": "<tool-2-name>", ...}
//	  ],
//	  ...
//	}
//
// The walker tolerates either a `map[string]any` (the typical
// dispatcher observation) or a `json.RawMessage` / `[]byte` shape
// (when the dispatcher serialised the result to bytes before storing).
// A `LLMObservation` projection (Phase 44 D-026) is preferred over the
// raw `Observation` when both are present, matching the prompt
// renderer's heavy-content discipline.
//
// Malformed observations are ignored silently — discovery is best-
// effort, and the LLM still observed the step's content in the prior
// turn's prompt; failing the next turn over an unparseable observation
// would burn the run for no benefit.
func deriveDiscoveredFromTrajectory(t *planner.Trajectory) []string {
	if t == nil || len(t.Steps) == 0 {
		return nil
	}
	var out []string
	seen := make(map[string]struct{})
	for _, step := range t.Steps {
		call, ok := step.Action.(planner.CallTool)
		if !ok || call.Tool != toolSearchToolName {
			continue
		}
		obs := step.LLMObservation
		if obs == nil {
			obs = step.Observation
		}
		names := extractDiscoveredNames(obs)
		for _, n := range names {
			if n == "" {
				continue
			}
			if _, dup := seen[n]; dup {
				continue
			}
			seen[n] = struct{}{}
			out = append(out, n)
		}
	}
	return out
}

// extractDiscoveredNames returns the `tools[].name` slice carried by a
// `tool_search` observation. Tolerates the three common observation
// shapes the runtime produces (typed map, JSON bytes, JSON
// RawMessage). Unknown shapes return nil.
func extractDiscoveredNames(obs any) []string {
	switch v := obs.(type) {
	case nil:
		return nil
	case map[string]any:
		return extractNamesFromMap(v)
	case json.RawMessage:
		return extractNamesFromBytes(v)
	case []byte:
		return extractNamesFromBytes(v)
	case string:
		return extractNamesFromBytes([]byte(v))
	default:
		// Best-effort: re-encode any other shape (struct, typed
		// map) and parse the bytes. This catches the
		// `inproc.publishToolOutcome` path that hands the planner a
		// struct projection rather than a generic map.
		raw, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		return extractNamesFromBytes(raw)
	}
}

// extractNamesFromBytes parses a `tool_search` observation's bytes
// form. Returns nil on any parse failure (silent — see godoc).
func extractNamesFromBytes(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var shaped struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &shaped); err != nil {
		return nil
	}
	if len(shaped.Tools) == 0 {
		return nil
	}
	out := make([]string, 0, len(shaped.Tools))
	for _, t := range shaped.Tools {
		out = append(out, t.Name)
	}
	return out
}

// extractNamesFromMap pulls names from a `tool_search` observation
// already decoded as `map[string]any`. The `tools` key may be a
// `[]any` (each entry a map[string]any) or a `[]map[string]any`
// depending on the runtime's decoding choice.
func extractNamesFromMap(m map[string]any) []string {
	raw, ok := m["tools"]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, entry := range v {
			if em, ok := entry.(map[string]any); ok {
				if name, _ := em["name"].(string); name != "" {
					out = append(out, name)
				}
			}
		}
		return out
	case []map[string]any:
		out := make([]string, 0, len(v))
		for _, entry := range v {
			if name, _ := entry["name"].(string); name != "" {
				out = append(out, name)
			}
		}
		return out
	default:
		return nil
	}
}

// mergeDiscovered returns the deduplicated union of two name slices.
// The result preserves the input order: every entry of `existing` is
// kept in place, then every entry of `derived` not already present is
// appended. A nil result for both-nil inputs.
func mergeDiscovered(existing, derived []string) []string {
	if len(existing) == 0 && len(derived) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(existing)+len(derived))
	out := make([]string, 0, len(existing)+len(derived))
	for _, n := range existing {
		if n == "" {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	for _, n := range derived {
		if n == "" {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}

// buildToolDeclarations assembles the per-turn `req.Tools` slice from
// the always-loaded catalog subset (returned by `rc.Catalog.List()`,
// already identity- and LoadingMode-filtered by the runtime's catalog
// view) plus the per-run discovered tools (AC-17 / AC-18). The
// declarations carry name + description + the args schema; the LLM
// uses them to emit native ToolCalls.
//
// Ordering is insertion-order: always-loaded tools first (in catalog
// registration order), then discovered tools (in discovery order). A
// discovered name that already exists in the always-loaded set is
// skipped — duplicates would confuse provider-side dispatch.
//
// A nil `rc.Catalog` returns an empty slice; the LLM still receives
// the prompt and can emit a tool-free response (the projector then
// produces Finish{Goal} or Finish{NoPath}).
func buildToolDeclarations(rc planner.RunContext, discovered []string) []llm.ToolDeclaration {
	if rc.Catalog == nil {
		return nil
	}
	always := rc.Catalog.List()
	if len(always) == 0 && len(discovered) == 0 {
		return nil
	}
	decls := make([]llm.ToolDeclaration, 0, len(always)+len(discovered))
	seen := make(map[string]struct{}, len(always)+len(discovered))
	for _, t := range always {
		if t.Name == "" {
			continue
		}
		if _, dup := seen[t.Name]; dup {
			continue
		}
		seen[t.Name] = struct{}{}
		decls = append(decls, toolToDeclaration(t))
	}
	for _, name := range discovered {
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		t, ok := rc.Catalog.Resolve(name)
		if !ok {
			continue
		}
		seen[name] = struct{}{}
		decls = append(decls, toolToDeclaration(t))
	}
	return decls
}

// toolToDeclaration projects a `tools.Tool` view onto the wire-facing
// `llm.ToolDeclaration`. Carries name + description + the args JSON
// Schema verbatim — the bifrost translator (and downstream provider
// adapters) consume this shape directly.
func toolToDeclaration(t tools.Tool) llm.ToolDeclaration {
	return llm.ToolDeclaration{
		Name:        t.Name,
		Description: t.Description,
		Schema:      t.ArgsSchema,
	}
}
