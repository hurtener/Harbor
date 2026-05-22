// Package tools registers Harbor's planner-facing skill tools
// (`skill_search`, `skill_get`, `skill_list`) into the Phase 26
// `tools.ToolCatalog`. Each handler wraps the Phase 37
// `skills.SkillStore` with three injection-time concerns
// (RFC §6.7, brief 04 §4.5):
//
//  1. Capability filter — a skill is visible only when its
//     `RequiredTools / RequiredNS / RequiredTags` are subsets of the
//     planner-supplied `CapabilityContext.AllowedTools /
//     AllowedNamespaces / AllowedTags`.
//  2. Redaction — disallowed tool names are scrubbed from skill text
//     and replaced with `"a suitable tool (use tool_search)"` (when
//     `tool_search` itself is allowed) or `"a suitable tool"`
//     (otherwise). Optional PII redaction (`CapabilityContext.RedactPII`)
//     additionally rewrites emails / phone / bearer-tokens / URL
//     query strings across titles / triggers / descriptions / steps /
//     preconditions / failure modes.
//  3. Tiered budgeter — `skill_get` fits the returned skills inside
//     a planner-supplied `MaxTokens` envelope by stepping a ladder:
//     full → drop optional (preconditions, failure_modes) → cap
//     steps to 3 → `ErrSkillTooLarge`. Fail-loud per CLAUDE.md §5;
//     no silent degradation.
//
// Identity-mandatory: every handler validates the
// `(tenant, user, session)` triple at the boundary via
// `identity.From(ctx)`. Missing identity returns the wrapped
// `skills.ErrIdentityRequired` AND emits `skill.identity_rejected`
// on the bus (via `skills.EmitIdentityRejected`). Brief 04 §5
// removes the predecessor's `require_explicit_key` knob — there is
// no opt-out.
//
// Concurrent reuse (D-025): the catalog + the registered descriptors
// are stateless after `Register`; per-call state lives on
// `(ctx, args)`. One catalog + one store is safe to share across N
// concurrent goroutines.
//
// Registration is a runtime call: `Register(catalog, store, deps)`
// is invoked from the binary's bootstrap path (the catalog is built
// at boot, not at package-init). Phase 38 ships the helper; Phase 60+
// wires it from `harbor dev`. The package is blank-imported from
// `cmd/harbor/main.go` so its presence is visible in the import
// graph.
package tools

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/skills"
	tcat "github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// Tool names registered into the Phase 26 catalog. These are
// load-bearing strings — the planner prompt references them by name
// and a silent rename would break every downstream model. The
// smoke script pins them with a string-grep.
const (
	// ToolNameSkillSearch — `skill_search(query, limit, capability)
	// -> {skills, path}`. Returns ranked candidates from the
	// SkillStore's FTS5 → regex → exact ladder (Phase 37), filtered
	// by capability and redacted.
	ToolNameSkillSearch = "skill_search"
	// ToolNameSkillGet — `skill_get(names[], max_tokens, capability)
	// -> {skills, summarized, dropped_steps}`. Returns the full
	// content of the named skills, redacted and budget-fit through
	// the tiered ladder.
	ToolNameSkillGet = "skill_get"
	// ToolNameSkillList — `skill_list(scope, task_type, tags, limit,
	// offset, capability) -> {skills}`. Returns a paged enumeration
	// filtered by capability and redacted.
	ToolNameSkillList = "skill_list"
)

// Default budgeter envelope when the caller does not supply
// `MaxTokens`. Picked to align with the §6.5 LLM safety net's
// chars/4 estimator at the per-skill scale.
const defaultMaxTokens = 1024

// ErrSkillTooLarge is returned by `skill_get` when the tiered
// budgeter cannot fit the requested skills within `max_tokens`
// after exhausting the ladder (full → drop optional → cap steps).
// Fail-loud per CLAUDE.md §5 — callers must not silently degrade.
var ErrSkillTooLarge = errors.New("skills/tools: skill exceeds max_tokens after ladder")

// CapabilityContext is the planner-supplied envelope that gates and
// shapes a skill at injection time. Two purposes:
//
//   - Capability filter: a skill is visible only when its
//     `RequiredTools / RequiredNS / RequiredTags` are subsets of the
//     corresponding Allowed* set (see `filter.go`).
//   - Redaction: tool names not in `AllowedTools` are scrubbed from
//     skill text; `RedactPII=true` additionally redacts PII patterns
//     (see `redactor.go`).
//
// Empty `AllowedTools` / `AllowedNamespaces` / `AllowedTags` means
// "the planner did not declare a capability set"; skills with empty
// required lists pass; skills with non-empty required lists are
// excluded (a "default-deny" stance — brief 04 §4.5).
//
// Concurrent reuse: the struct is a value carried on the args; it
// is never mutated in-flight. Multiple goroutines may share a
// CapabilityContext value safely (slice headers are read-only).
type CapabilityContext struct {
	// AllowedTools — the set of tool names visible to the run.
	// Used by the filter (subset check) AND the redactor (skills
	// referencing names not in this set get the disallowed-name
	// replacement).
	AllowedTools []string `json:"allowed_tools,omitempty"`
	// AllowedNamespaces — namespaces (Skill.RequiredNS) the run may
	// reference.
	AllowedNamespaces []string `json:"allowed_namespaces,omitempty"`
	// AllowedTags — tags (Skill.RequiredTags) the run may reference.
	AllowedTags []string `json:"allowed_tags,omitempty"`
	// RedactPII opts in to PII redaction across skill text
	// (emails / phone / bearer tokens / URL query strings).
	RedactPII bool `json:"redact_pii,omitempty"`
}

// Deps carries the runtime dependencies `Register` needs. `Bus` is
// mandatory so the Phase 37 identity-rejection emit path
// (`skills.EmitIdentityRejected`) lands on the audit pipeline.
type Deps struct {
	Bus events.EventBus
}

// SearchArgs is the input shape for `skill_search`. Reflected into
// a JSON Schema by the inproc driver at registration time.
type SearchArgs struct {
	Query      string            `json:"query"`
	Capability CapabilityContext `json:"capability"`
	Limit      int               `json:"limit,omitempty"`
}

// SearchResult is the output shape for `skill_search`.
type SearchResult struct {
	Path   string               `json:"path"`
	Skills []skills.RankedSkill `json:"skills"`
}

// GetArgs is the input shape for `skill_get`.
type GetArgs struct {
	Names      []string          `json:"names"`
	Capability CapabilityContext `json:"capability"`
	MaxTokens  int               `json:"max_tokens,omitempty"`
}

// GetResult is the output shape for `skill_get`.
type GetResult struct {
	// Skills are the requested skills after capability filter +
	// redaction + budgeter. The slice MAY be empty (every requested
	// skill was filtered out or missing); it is never partially
	// over-budget.
	Skills []skills.Skill `json:"skills"`
	// Summarized reports whether the budgeter dropped optional
	// fields (preconditions / failure modes) — ladder step 1.
	Summarized bool `json:"summarized"`
	// DroppedSteps reports whether the budgeter capped procedural
	// steps to 3 — ladder step 2.
	DroppedSteps bool `json:"dropped_steps"`
}

// ListArgs is the input shape for `skill_list`.
type ListArgs struct {
	Scope      skills.Scope      `json:"scope,omitempty"`
	TaskType   string            `json:"task_type,omitempty"`
	Tags       []string          `json:"tags,omitempty"`
	Capability CapabilityContext `json:"capability"`
	Limit      int               `json:"limit,omitempty"`
	Offset     int               `json:"offset,omitempty"`
}

// ListResult is the output shape for `skill_list`.
type ListResult struct {
	// Skills are the paged rows after capability filter + redaction.
	// The list is NOT budget-trimmed — callers requesting a paged
	// enumeration accept that the planner-facing text is
	// summary-only (Title + Trigger + Tags), not full content.
	Skills []skills.Skill `json:"skills"`
}

// Register installs `skill_search`, `skill_get`, `skill_list` into
// `catalog`, wired against `store`. Returns wrapped errors on
// validation failure or catalog conflicts (e.g. a duplicate name —
// indicates a misconfigured boot path, not a runtime fault).
//
// Concurrent reuse: the registered descriptors hold only an
// immutable closure over `store` + `deps`; D-025 holds.
func Register(catalog tcat.ToolCatalog, store skills.SkillStore, deps Deps) error {
	if catalog == nil {
		return errors.New("skills/tools: catalog is nil")
	}
	if store == nil {
		return errors.New("skills/tools: store is nil")
	}
	if deps.Bus == nil {
		return errors.New("skills/tools: deps.Bus is required (events.EventBus)")
	}

	search := func(ctx context.Context, args SearchArgs) (SearchResult, error) {
		return searchHandler(ctx, store, deps.Bus, args)
	}
	if err := inproc.RegisterFunc[SearchArgs, SearchResult](
		catalog,
		ToolNameSkillSearch,
		search,
		tcat.WithDescription("Search Harbor's skills catalogue for entries matching a free-form query. Returns ranked candidates with name, title, trigger, and score. Capability filtering + tool-name redaction are applied at injection."),
		tcat.WithSideEffect(tcat.SideEffectRead),
		tcat.WithLoading(tcat.LoadingAlways),
		tcat.WithTags("skills", "search"),
		tcat.WithBus(deps.Bus),
		tcat.WithSource(tcat.ToolSourceID("skills/tools")),
	); err != nil {
		return fmt.Errorf("skills/tools: register %s: %w", ToolNameSkillSearch, err)
	}

	get := func(ctx context.Context, args GetArgs) (GetResult, error) {
		return getHandler(ctx, store, deps.Bus, args)
	}
	if err := inproc.RegisterFunc[GetArgs, GetResult](
		catalog,
		ToolNameSkillGet,
		get,
		tcat.WithDescription("Fetch the full content of one or more named skills. The returned content is capability-filtered, redacted, and budget-fit to max_tokens via the tiered ladder (full → drop optional → cap steps to 3). Over-budget skills surface ErrSkillTooLarge."),
		tcat.WithSideEffect(tcat.SideEffectRead),
		tcat.WithLoading(tcat.LoadingAlways),
		tcat.WithTags("skills", "fetch"),
		tcat.WithBus(deps.Bus),
		tcat.WithSource(tcat.ToolSourceID("skills/tools")),
	); err != nil {
		return fmt.Errorf("skills/tools: register %s: %w", ToolNameSkillGet, err)
	}

	list := func(ctx context.Context, args ListArgs) (ListResult, error) {
		return listHandler(ctx, store, deps.Bus, args)
	}
	if err := inproc.RegisterFunc[ListArgs, ListResult](
		catalog,
		ToolNameSkillList,
		list,
		tcat.WithDescription("List skills in the operator-declared catalogue, paged and filterable by scope / task_type / tags. Capability-filtered + redacted at injection. Summary-only (no full steps); call skill_get for full content."),
		tcat.WithSideEffect(tcat.SideEffectRead),
		tcat.WithLoading(tcat.LoadingAlways),
		tcat.WithTags("skills", "list"),
		tcat.WithBus(deps.Bus),
		tcat.WithSource(tcat.ToolSourceID("skills/tools")),
	); err != nil {
		return fmt.Errorf("skills/tools: register %s: %w", ToolNameSkillList, err)
	}
	return nil
}

// searchHandler is the `skill_search` planner-tool body. Identity
// from ctx → SkillStore.Search → Filter → Redact (per-row). Path is
// surfaced for observability.
func searchHandler(ctx context.Context, store skills.SkillStore, bus events.EventBus, args SearchArgs) (SearchResult, error) {
	q, err := skills.IdentityFromCtx(ctx)
	if err != nil {
		return SearchResult{}, skills.EmitIdentityRejected(ctx, bus, q, "tools.skill_search")
	}
	ranked, err := store.Search(ctx, q, args.Query, args.Limit)
	if err != nil {
		return SearchResult{}, fmt.Errorf("skills/tools: search: %w", err)
	}

	// Extract the SkillStore-reported path from the first row (the
	// ladder is single-branch — every row in a Search response shares
	// the same Path). Falls through to an empty string when no rows.
	var path string
	if len(ranked) > 0 {
		path = ranked[0].Path
	}

	// Apply capability filter + redaction. Filter operates on
	// `Skill` values, so unpack→repack the ranked slice.
	flat := make([]skills.Skill, len(ranked))
	for i, r := range ranked {
		flat[i] = r.Skill
	}
	filtered := Filter(flat, args.Capability)

	// Re-pack: keep score + path alongside the filtered skills.
	// Index-walk is O(N*M); for the typical M ≤ 20 SkillStore limit
	// it is dominated by the redactor's regex passes.
	out := make([]skills.RankedSkill, 0, len(filtered))
	for _, s := range filtered {
		redacted := normalizeSkill(Redact(s, args.Capability))
		// Find the matching RankedSkill to preserve Score + Path.
		for _, r := range ranked {
			if r.Skill.Name == s.Name {
				out = append(out, skills.RankedSkill{
					Skill: redacted,
					Score: r.Score,
					Path:  r.Path,
				})
				break
			}
		}
	}
	return SearchResult{Skills: out, Path: path}, nil
}

// getHandler is the `skill_get` planner-tool body. Identity → fetch
// each named skill → Filter → Redact → Budgeter ladder. Missing
// names are silently skipped (a partial response is more useful than
// a hard error for stale planner caches); the budgeter is the only
// hard error path.
func getHandler(ctx context.Context, store skills.SkillStore, bus events.EventBus, args GetArgs) (GetResult, error) {
	q, err := skills.IdentityFromCtx(ctx)
	if err != nil {
		return GetResult{}, skills.EmitIdentityRejected(ctx, bus, q, "tools.skill_get")
	}

	gathered := make([]skills.Skill, 0, len(args.Names))
	for _, name := range args.Names {
		s, err := store.Get(ctx, q, name) //nolint:govet // loop-local; err shadow is benign
		if err != nil {
			if errors.Is(err, skills.ErrSkillNotFound) {
				// Partial response is acceptable.
				continue
			}
			return GetResult{}, fmt.Errorf("skills/tools: get %q: %w", name, err)
		}
		gathered = append(gathered, s)
	}

	filtered := Filter(gathered, args.Capability)
	redacted := make([]skills.Skill, len(filtered))
	for i, s := range filtered {
		redacted[i] = Redact(s, args.Capability)
	}

	maxTokens := args.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	fit, summarized, droppedSteps, err := Fit(redacted, maxTokens)
	if err != nil {
		return GetResult{}, err
	}
	// Normalise so the inproc driver's reflection-derived JSON
	// Schema (every slice / map required) accepts a nil-valued
	// post-filter skill.
	for i := range fit {
		fit[i] = normalizeSkill(fit[i])
	}
	return GetResult{Skills: fit, Summarized: summarized, DroppedSteps: droppedSteps}, nil
}

// listHandler is the `skill_list` planner-tool body. Identity →
// SkillStore.List → Filter → Redact (summary fields only — full
// content is reserved for `skill_get`).
func listHandler(ctx context.Context, store skills.SkillStore, bus events.EventBus, args ListArgs) (ListResult, error) {
	q, err := skills.IdentityFromCtx(ctx)
	if err != nil {
		return ListResult{}, skills.EmitIdentityRejected(ctx, bus, q, "tools.skill_list")
	}
	listed, err := store.List(ctx, q, skills.ListFilter{
		Scope:    args.Scope,
		TaskType: args.TaskType,
		Tags:     args.Tags,
		Limit:    args.Limit,
		Offset:   args.Offset,
	})
	if err != nil {
		return ListResult{}, fmt.Errorf("skills/tools: list: %w", err)
	}
	filtered := Filter(listed, args.Capability)
	out := make([]skills.Skill, len(filtered))
	for i, s := range filtered {
		// Listing returns summary-only views; the budgeter is
		// reserved for `skill_get`. Apply redaction so disallowed
		// tool names still don't leak via titles / triggers.
		out[i] = normalizeSkill(Redact(s, args.Capability))
	}
	return ListResult{Skills: out}, nil
}
