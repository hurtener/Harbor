package skills

import (
	"fmt"
	"sort"
	"strings"
)

// agent_pack.go — the operator-managed per-agent skill pack surface (HA-55).
//
// A pack item is the full body of ONE operator-selected skill, pinned inside
// the agent's content-addressed config revision (the `agent_packs` section of
// `agentcfg.ConfigPayload`). It is addressed by `(tenant_id, agent_id, name)`
// at configuration-selection time, but `agent_id` never becomes an isolation
// principal: the run snapshot still starts from the caller's verified
// `(tenant, user, session)` and its signed reach to the effective agent, and
// the composed run view contains ONLY the selected operator pack for that
// effective agent + tenant plus that caller's permitted personal/session
// skills.
//
// A pack item NEVER widens capability. Its `RequiredTools` / `RequiredNS` /
// `RequiredTags` are provenance / filter metadata exactly like a stored
// `Skill`'s: the run-visible capability filter and the injection-time
// redactor sit in front of the composed view and all three `skill_*` tools,
// so a pack item that names a missing, paused, disabled, or scope-filtered
// MCP tool is filtered/redacted exactly as today — it never expands the run's
// visible tool set.
//
// The `Scope` is fixed by construction: `Skill()` stamps `ScopeTenant` (the
// pack is operator-managed and tenant+agent keyed). The item carries no
// scope field of its own, so a caller cannot widen visibility through the
// body. `Origin` is likewise forced to `OriginPack`.

// Pack bounds. Every bound is enforced at the protocol edge (upsert /
// commit / propose) so an oversized or malformed pack never reaches the
// revision store.
const (
	// MaxAgentPackItems bounds the number of pack items a single revision
	// may pin. The run-start resolver enumerates the composed view in
	// memory; this keeps that view bounded alongside
	// maxSessionSkillResolverBaseRows.
	MaxAgentPackItems = 256
	// MaxAgentPackSteps bounds the procedural steps of one pack item.
	MaxAgentPackSteps = 64
	// MaxAgentPackTags bounds the tags of one pack item.
	MaxAgentPackTags = 32
	// MaxAgentPackAnnotations bounds each of RequiredTools / RequiredNS /
	// RequiredTags on one pack item.
	MaxAgentPackAnnotations = 32
	// MaxAgentPackTextRunes bounds each single text field (title,
	// description, trigger, task_type, one step, one precondition, one
	// failure mode, one tag, one tool/ns/tag annotation). Bound in runes
	// because the planner renders these into the prompt.
	MaxAgentPackTextRunes = 2000
	// MaxAgentPackExtraKeys bounds the `Extra` metadata map of one pack
	// item (bounded metadata only — never secret material).
	MaxAgentPackExtraKeys = 16
)

// AgentPackItem is the canonical domain shape of ONE operator-managed
// per-agent skill body. It is a strict subset of `Skill` (the pack rung
// carries no scope/origin of its own); `Skill()`/`PackItemFromSkill`
// convert between the two shapes.
type AgentPackItem struct {
	// Name is the pack-local primary key, unique within the agent's pack.
	Name string `json:"name"`
	// Title is the human-readable title (may be empty).
	Title string `json:"title,omitempty"`
	// Description is the free-form body text (may be empty).
	Description string `json:"description,omitempty"`
	// Trigger is the planner-visible match cue. Mandatory (non-empty).
	Trigger string `json:"trigger"`
	// TaskType is the planner-facing task class (browser|api|code|domain|unknown).
	TaskType string `json:"task_type,omitempty"`
	// Tags are search/classification tags (sorted by the normalizer).
	Tags []string `json:"tags,omitempty"`
	// Steps are the ordered procedural steps. Mandatory (≥ 1).
	Steps []string `json:"steps"`
	// Preconditions are the optional ordered preconditions.
	Preconditions []string `json:"preconditions,omitempty"`
	// FailureModes are the optional ordered failure modes.
	FailureModes []string `json:"failure_modes,omitempty"`
	// RequiredTools are the capability-annotation tool names. Metadata
	// only — NEVER a grant; the injection-time filter + redactor enforce.
	RequiredTools []string `json:"required_tools,omitempty"`
	// RequiredNS are the capability-annotation namespaces. Metadata only.
	RequiredNS []string `json:"required_ns,omitempty"`
	// RequiredTags are the capability-annotation tags. Metadata only.
	RequiredTags []string `json:"required_tags,omitempty"`
	// OriginRef is the pack provenance stamp written by the mutation verb
	// (e.g. `pack.upserted.<agent>.<hash[:16]>`). Read-only at the wire
	// edge: the service re-stamps it deterministically and rejects a
	// caller-supplied value that differs.
	OriginRef string `json:"origin_ref,omitempty"`
	// Extra is bounded string-typed metadata. String-typed so the
	// canonical JSON encoding and the TS wire projection stay closed.
	Extra map[string]string `json:"extra,omitempty"`
}

// CanonicalPackName returns the canonical (lowercase, trimmed) form used for
// pack-internal name identity, dedup, and cross-agent collision checks.
func CanonicalPackName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// Validate checks the closed shape of a pack item. It is the pack analogue
// of `Skill.Validate`: a bad payload surfaces at the protocol edge (upsert /
// commit) rather than later via a corrupt revision. It does NOT check
// capability annotations against any live tool set — those are metadata, and
// the injection-time filter enforces them.
func (p AgentPackItem) Validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("%w: pack item Name empty", ErrInvalidSkill)
	}
	if CanonicalPackName(p.Name) == "" {
		return fmt.Errorf("%w: pack item Name has no canonical form", ErrInvalidSkill)
	}
	if strings.TrimSpace(p.Trigger) == "" {
		return fmt.Errorf("%w: pack item Trigger empty (planner match cue is mandatory)", ErrInvalidSkill)
	}
	if len(p.Steps) == 0 {
		return fmt.Errorf("%w: pack item Steps empty (skills must declare ≥ 1 step)", ErrInvalidSkill)
	}
	if err := p.bounded(); err != nil {
		return err
	}
	return nil
}

// bounded enforces the per-item size bounds. Runed so multi-byte text is
// counted in what the planner actually renders.
func (p AgentPackItem) bounded() error {
	for field, text := range map[string]string{
		"name": p.Name, "title": p.Title, "description": p.Description,
		"trigger": p.Trigger, "task_type": p.TaskType,
	} {
		if r := len([]rune(text)); r > MaxAgentPackTextRunes {
			return fmt.Errorf("%w: pack item %s exceeds %d runes (%d)", ErrInvalidSkill, field, MaxAgentPackTextRunes, r)
		}
	}
	if len(p.Steps) > MaxAgentPackSteps {
		return fmt.Errorf("%w: pack item steps exceed %d", ErrInvalidSkill, MaxAgentPackSteps)
	}
	for _, s := range p.Steps {
		if r := len([]rune(s)); r > MaxAgentPackTextRunes {
			return fmt.Errorf("%w: pack item step exceeds %d runes (%d)", ErrInvalidSkill, MaxAgentPackTextRunes, r)
		}
	}
	for field, list := range map[string][]string{
		"tags": p.Tags, "required_tools": p.RequiredTools,
		"required_ns": p.RequiredNS, "required_tags": p.RequiredTags,
	} {
		if len(list) > MaxAgentPackAnnotations {
			return fmt.Errorf("%w: pack item %s exceeds %d entries", ErrInvalidSkill, field, MaxAgentPackAnnotations)
		}
		for _, entry := range list {
			if strings.TrimSpace(entry) == "" {
				return fmt.Errorf("%w: pack item %s contains an empty entry", ErrInvalidSkill, field)
			}
			if r := len([]rune(entry)); r > MaxAgentPackTextRunes {
				return fmt.Errorf("%w: pack item %s entry exceeds %d runes (%d)", ErrInvalidSkill, field, MaxAgentPackTextRunes, r)
			}
		}
	}
	for _, s := range append(append([]string(nil), p.Preconditions...), p.FailureModes...) {
		if r := len([]rune(s)); r > MaxAgentPackTextRunes {
			return fmt.Errorf("%w: pack item text entry exceeds %d runes (%d)", ErrInvalidSkill, MaxAgentPackTextRunes, r)
		}
	}
	if len(p.Extra) > MaxAgentPackExtraKeys {
		return fmt.Errorf("%w: pack item extra exceeds %d keys", ErrInvalidSkill, MaxAgentPackExtraKeys)
	}
	for k, v := range p.Extra {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("%w: pack item extra has an empty key", ErrInvalidSkill)
		}
		if r := len([]rune(v)); r > MaxAgentPackTextRunes {
			return fmt.Errorf("%w: pack item extra[%q] exceeds %d runes (%d)", ErrInvalidSkill, k, MaxAgentPackTextRunes, r)
		}
	}
	return nil
}

// Skill converts the pack item into the canonical stored `Skill` shape for
// the composed run snapshot. The pack rung is fixed by construction:
//
//   - Origin is forced to `OriginPack` — a pack item is operator-managed and
//     can never be authored as generated provenance (provenance cannot be
//     smuggled through the body).
//   - Scope is forced to `ScopeTenant` — the pack is operator-managed and
//     tenant+agent keyed; the item carries no scope of its own, so a caller
//     cannot widen visibility (non-widening by construction).
//   - ScopeTenantID / ScopeProjectID are empty — the pack's tenant is the
//     revision's tenant, never a caller-supplied id.
//   - `OriginRef` is preserved verbatim (the mutation verb stamps it and
//     re-validates on read-back).
//
// The returned skill is validated (name / trigger / steps / origin / scope)
// so an invalid pack item fails loudly at conversion time.
func (p AgentPackItem) Skill() (Skill, error) {
	s := Skill{
		Name:          strings.TrimSpace(p.Name),
		Title:         p.Title,
		Description:   p.Description,
		Trigger:       strings.TrimSpace(p.Trigger),
		TaskType:      p.TaskType,
		Tags:          append([]string(nil), p.Tags...),
		Steps:         append([]string(nil), p.Steps...),
		Preconditions: append([]string(nil), p.Preconditions...),
		FailureModes:  append([]string(nil), p.FailureModes...),
		RequiredTools: append([]string(nil), p.RequiredTools...),
		RequiredNS:    append([]string(nil), p.RequiredNS...),
		RequiredTags:  append([]string(nil), p.RequiredTags...),
		Origin:        OriginPack,
		OriginRef:     strings.TrimSpace(p.OriginRef),
		Scope:         ScopeTenant,
		Extra:         stringMapToAny(p.Extra),
	}
	if err := s.Validate(); err != nil {
		return Skill{}, err
	}
	s.ContentHash = CanonicalContentHash(s)
	return s, nil
}

// PackItemFromSkill projects a stored `Skill` back onto the pack-item shape.
// It drops the scope/origin fields (the pack rung is fixed) and the lifecycle
// fields (the pack item never carries timestamps/counters; the revision
// stamps time).
func PackItemFromSkill(s Skill) AgentPackItem {
	return AgentPackItem{
		Name:          s.Name,
		Title:         s.Title,
		Description:   s.Description,
		Trigger:       s.Trigger,
		TaskType:      s.TaskType,
		Tags:          append([]string(nil), s.Tags...),
		Steps:         append([]string(nil), s.Steps...),
		Preconditions: append([]string(nil), s.Preconditions...),
		FailureModes:  append([]string(nil), s.FailureModes...),
		RequiredTools: append([]string(nil), s.RequiredTools...),
		RequiredNS:    append([]string(nil), s.RequiredNS...),
		RequiredTags:  append([]string(nil), s.RequiredTags...),
		OriginRef:     s.OriginRef,
		Extra:         anyMapToString(s.Extra),
	}
}

// PackItemsToSkills converts and validates a whole pack. A single invalid
// item fails the whole conversion loudly — the resolver must never compose a
// partially-validated pack (a malformed body would either be dropped
// silently or widen semantics).
func PackItemsToSkills(items []AgentPackItem) ([]Skill, error) {
	if len(items) == 0 {
		return nil, nil
	}
	out := make([]Skill, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		skill, err := item.Skill()
		if err != nil {
			return nil, fmt.Errorf("pack item %q: %w", item.Name, err)
		}
		canonical := CanonicalPackName(skill.Name)
		if _, dup := seen[canonical]; dup {
			return nil, fmt.Errorf("%w: duplicate pack item %q", ErrInvalidSkill, canonical)
		}
		seen[canonical] = struct{}{}
		out = append(out, skill)
	}
	return out, nil
}

// NormalizeAgentPackItems returns a defensive canonical copy: items sorted by
// canonical name with the LAST occurrence of a duplicate canonical name
// winning (a re-add replaces a prior item of the same name — the same
// last-wins convention normalizeConnections uses). Nil / empty input yields
// nil so an empty pack section drops out of the canonical revision form.
// Duplicate detection is the protocol edge's job (fail-loud there); this
// normalizer only guarantees a stable content hash.
func NormalizeAgentPackItems(items []AgentPackItem) []AgentPackItem {
	if len(items) == 0 {
		return nil
	}
	byName := make(map[string]AgentPackItem, len(items))
	for _, item := range items {
		canonical := CanonicalPackName(item.Name)
		if canonical == "" {
			continue
		}
		item.Name = canonical
		item.Tags = sortedCopy(item.Tags)
		item.RequiredTools = sortedCopy(item.RequiredTools)
		item.RequiredNS = sortedCopy(item.RequiredNS)
		item.RequiredTags = sortedCopy(item.RequiredTags)
		byName[canonical] = item
	}
	if len(byName) == 0 {
		return nil
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]AgentPackItem, 0, len(names))
	for _, name := range names {
		out = append(out, byName[name])
	}
	return out
}

func stringMapToAny(in map[string]string) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func anyMapToString(in map[string]any) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}
