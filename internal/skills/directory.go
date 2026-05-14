package skills

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/skills/capfilter"
)

// Phase 39 — Virtual directory subsystem (RFC §6.7).
//
// The virtual directory is a bounded, identity-scoped, capability-
// filtered, redacted snapshot of the skills catalogue. Built once
// (per operator-config reload); shared across N concurrent
// goroutines (D-025); read-only on a flat View(ctx, cap) API. See
// brief 04 §4.6 for the design intent.
//
// Selection semantics:
//
//   - SelectionPinnedThenRecent — pinned skills first, then unpinned
//     remainder ordered by UpdatedAt DESC, Name ASC.
//   - SelectionPinnedThenTop    — pinned skills first, then unpinned
//     remainder ordered by UseCount  DESC, Name ASC.
//
// Pinning model: a skill is pinned when ANY of these is true:
//
//  1. Its Name appears in DirectoryConfig.Pinned (operator-declared,
//     config-time anchor; preserves declaration order at the head of
//     the pinned partition).
//  2. Its Extra map carries ExtraPinnedKey ("pinned") bound to the
//     bool true (runtime-stamped anchor; orders inside its sub-
//     partition by the per-Selection secondary key).
//
// Pinning is an ORDERING preference; pinned skills are NOT exempt
// from the capability filter. A pinned skill whose required tools/
// namespaces/tags are not a subset of the planner-supplied allowed
// sets is still excluded from the View — fail-loud per CLAUDE.md §6
// rule 9 + D-052.
//
// Identity-mandatory: View reads the identity Quadruple/Identity
// from ctx; a missing triple returns wrapped ErrIdentityRequired
// AND emits skill.identity_rejected via EmitIdentityRejected. No
// silent-fallback path (CLAUDE.md §5 + §13 + D-052).

// Selection picks the unpinned-partition ordering. Pinned skills
// always come first; Selection orders only the unpinned remainder
// (and the secondary ordering inside the Extra-pinned subset).
type Selection string

// Selection values.
const (
	// SelectionPinnedThenRecent — pinned first, then most-recently-
	// updated skills (UpdatedAt DESC, Name ASC).
	SelectionPinnedThenRecent Selection = "pinned_then_recent"
	// SelectionPinnedThenTop — pinned first, then most-used skills
	// (UseCount DESC, Name ASC).
	SelectionPinnedThenTop Selection = "pinned_then_top"
)

// MaxEntries bounds — brief 04 §3 contract.
const (
	// DefaultMaxEntries is the per-View row cap when DirectoryConfig
	// leaves MaxEntries at zero.
	DefaultMaxEntries = 30
	// MinMaxEntries is the inclusive lower bound for MaxEntries.
	MinMaxEntries = 1
	// MaxMaxEntries is the inclusive upper bound for MaxEntries.
	MaxMaxEntries = 200
)

// ExtraPinnedKey is the Skill.Extra map key the directory treats as a
// runtime-stamped pin marker. Value MUST be the bool true; any other
// shape is ignored (no string "true" / int 1 / etc. — fail-loud on
// shape drift rather than silently accept).
const ExtraPinnedKey = "pinned"

// DirectoryCapability mirrors the planner-supplied capability envelope
// the directory consults for filtering + redaction at View time. The
// shape is identical to internal/skills/tools.CapabilityContext;
// duplicating the type here avoids the import cycle (`internal/skills/
// tools` imports `internal/skills` already). When a caller already
// holds a `tools.CapabilityContext`, the fields map one-to-one and
// the conversion is mechanical at the planner boundary.
//
// Concurrent reuse: the struct is a value carried on the args; never
// mutated in-flight. Multiple goroutines may share a
// DirectoryCapability value safely.
type DirectoryCapability struct {
	// AllowedTools — the set of tool names visible to the run.
	// Used by the filter (subset check) AND the redactor (skills
	// referencing names not in this set get the disallowed-name
	// replacement in Title / Trigger).
	AllowedTools []string `json:"allowed_tools,omitempty"`
	// AllowedNamespaces — namespaces (Skill.RequiredNS) the run may
	// reference.
	AllowedNamespaces []string `json:"allowed_namespaces,omitempty"`
	// AllowedTags — tags (Skill.RequiredTags) the run may reference.
	AllowedTags []string `json:"allowed_tags,omitempty"`
}

// SkillView is the planner-visible projection of a Skill returned by
// Directory.View. Four content fields plus a Pinned marker; the rest
// of the Skill is dropped at the boundary so the View is cheap to
// inject and Console-renderable without leaking storage-layer detail.
type SkillView struct {
	Name     string `json:"name"`
	Title    string `json:"title,omitempty"`
	Trigger  string `json:"trigger,omitempty"`
	TaskType string `json:"task_type,omitempty"`
	// Pinned is true when the skill was anchored by either
	// DirectoryConfig.Pinned (config-time) or Skill.Extra["pinned"]
	// (runtime-time). Surfaced for Console rendering; inert to the
	// planner's reasoning.
	Pinned bool `json:"pinned"`
}

// DirectoryConfig configures one Directory instance.
type DirectoryConfig struct {
	// Pinned is the operator-declared list of skill names to anchor
	// at the top of every View. Order is preserved across calls.
	// Names that don't exist under the calling identity are dropped
	// silently (config + storage may legitimately disagree; storage
	// wins).
	Pinned []string
	// MaxEntries caps the View length. 0 → DefaultMaxEntries (30).
	// Outside [1, 200] → NewDirectory returns wrapped
	// ErrInvalidConfig.
	MaxEntries int
	// Selection picks the unpinned-partition ordering. Empty →
	// SelectionPinnedThenRecent.
	Selection Selection
}

// Directory exposes the virtual-directory snapshot. Built once at
// boot (or per operator-config reload); safe to share across N
// concurrent goroutines (D-025). Per-call state lives on ctx + the
// args; no mutable field on Directory itself changes after
// construction.
type Directory struct {
	store     SkillStore
	bus       events.EventBus
	maxEntry  int
	selection Selection
	// pinSet is the operator-declared set of pinned names; presence-
	// only for O(1) membership checks. The ordered slice
	// (pinnedOrder) preserves the declaration order at View time.
	pinSet      map[string]struct{}
	pinnedOrder []string
}

// ErrInvalidConfig — NewDirectory rejected the DirectoryConfig.
// Returned wrapped via fmt.Errorf("%w: ...") so callers extract the
// cause via errors.Is. Fail-loud per CLAUDE.md §5 + §13.
var ErrInvalidConfig = errors.New("skills: invalid directory config")

// NewDirectory validates cfg and returns a usable Directory.
//
// Validation rules:
//
//   - store == nil               → wrapped ErrInvalidConfig
//   - deps.Bus == nil            → wrapped ErrInvalidConfig
//   - cfg.MaxEntries == 0        → DefaultMaxEntries (30)
//   - cfg.MaxEntries < 1 or > 200 → wrapped ErrInvalidConfig
//   - cfg.Selection == ""        → SelectionPinnedThenRecent
//   - cfg.Selection not in the two canonical values → wrapped ErrInvalidConfig
//
// Concurrent reuse: the returned *Directory holds no mutable state;
// safe to share across N goroutines (D-025).
func NewDirectory(store SkillStore, deps Deps, cfg DirectoryConfig) (*Directory, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: store is nil", ErrInvalidConfig)
	}
	if deps.Bus == nil {
		return nil, fmt.Errorf("%w: deps.Bus is required (events.EventBus)", ErrInvalidConfig)
	}

	maxEntry := cfg.MaxEntries
	if maxEntry == 0 {
		maxEntry = DefaultMaxEntries
	}
	if maxEntry < MinMaxEntries || maxEntry > MaxMaxEntries {
		return nil, fmt.Errorf("%w: MaxEntries=%d (must be in [%d,%d] or 0 for default)",
			ErrInvalidConfig, cfg.MaxEntries, MinMaxEntries, MaxMaxEntries)
	}

	sel := cfg.Selection
	if sel == "" {
		sel = SelectionPinnedThenRecent
	}
	switch sel {
	case SelectionPinnedThenRecent, SelectionPinnedThenTop:
	default:
		return nil, fmt.Errorf("%w: Selection=%q (expected %q or %q)",
			ErrInvalidConfig, sel, SelectionPinnedThenRecent, SelectionPinnedThenTop)
	}

	// Build the pinned set + ordered slice. Deduplicate while
	// preserving the first-seen declaration order.
	pinSet := make(map[string]struct{}, len(cfg.Pinned))
	pinnedOrder := make([]string, 0, len(cfg.Pinned))
	for _, name := range cfg.Pinned {
		if name == "" {
			// Skip empty entries; a config typo shouldn't surface as
			// a runtime panic, but the directory also won't anchor a
			// nameless skill.
			continue
		}
		if _, exists := pinSet[name]; exists {
			continue
		}
		pinSet[name] = struct{}{}
		pinnedOrder = append(pinnedOrder, name)
	}

	return &Directory{
		store:       store,
		bus:         deps.Bus,
		maxEntry:    maxEntry,
		selection:   sel,
		pinSet:      pinSet,
		pinnedOrder: pinnedOrder,
	}, nil
}

// View returns the identity-scoped, capability-filtered, redacted,
// bounded snapshot of the skill catalogue.
//
// Pipeline:
//
//  1. Read identity from ctx (Quadruple or Identity); missing →
//     wrapped ErrIdentityRequired + skill.identity_rejected emit.
//  2. SkillStore.List for the identity (Limit=0 — driver default;
//     the directory bounds locally to maxEntry afterwards).
//  3. Capability filter: exclude skills whose RequiredTools /
//     RequiredNS / RequiredTags are NOT subsets of cap.Allowed*.
//  4. Partition by pinning (config-declared first, then Extra-pinned,
//     then unpinned).
//  5. Sort each partition by the Selection's secondary key
//     (UpdatedAt DESC, Name ASC for pinned_then_recent; UseCount
//     DESC, Name ASC for pinned_then_top). Config-declared pins
//     preserve declaration order.
//  6. Concatenate pinned ++ unpinned; truncate to maxEntry.
//  7. Project to SkillView, redacting Title + Trigger.
//
// Concurrent-safe: pure read pipeline over the immutable *Directory.
// Per-call state lives on ctx + cap; no shared mutable state.
func (d *Directory) View(ctx context.Context, cap DirectoryCapability) ([]SkillView, error) {
	q, err := IdentityFromCtx(ctx)
	if err != nil {
		return nil, EmitIdentityRejected(ctx, d.bus, q, "directory.View")
	}

	// Fetch every skill under the identity. We pass Limit=0 so the
	// store applies its driver default (Phase 37 localdb: 100); the
	// directory itself bounds to maxEntry after filter + partition,
	// so a larger driver default just gives us more candidates.
	all, err := d.store.List(ctx, q, ListFilter{Limit: 0})
	if err != nil {
		return nil, fmt.Errorf("skills/directory: list: %w", err)
	}

	// Capability filter — subset gate on RequiredTools / RequiredNS /
	// RequiredTags. Skills with empty Required* slices pass; skills
	// whose Required* lists are not subsets of cap.Allowed* are
	// excluded. Default-deny when Allowed* is empty AND Required* is
	// non-empty — matches Phase 38's filter stance (D-048).
	filtered := filterByCapability(all, cap)

	// Partition by pinning state. Order rules:
	//   - configPinned[]   in DirectoryConfig.Pinned declaration order
	//   - extraPinned[]    sorted by the Selection's secondary key
	//   - unpinned[]       sorted by the Selection's secondary key
	configPinned, extraPinned, unpinned := d.partitionByPinning(filtered)

	sortBySelection(extraPinned, d.selection)
	sortBySelection(unpinned, d.selection)

	// Assemble pinned partition: config-declared first (declaration
	// order), then Extra-pinned (secondary-key order).
	pinned := make([]Skill, 0, len(configPinned)+len(extraPinned))
	pinned = append(pinned, configPinned...)
	pinned = append(pinned, extraPinned...)

	// Final assembly: pinned ++ unpinned, truncated to maxEntry.
	combined := make([]Skill, 0, len(pinned)+len(unpinned))
	combined = append(combined, pinned...)
	combined = append(combined, unpinned...)
	if len(combined) > d.maxEntry {
		combined = combined[:d.maxEntry]
	}

	// Project to SkillView with Title + Trigger redacted. The pinned
	// flag is computed from the final partition (a skill that fell
	// off the truncation boundary doesn't surface, so its flag is
	// moot).
	out := make([]SkillView, 0, len(combined))
	pinnedCount := len(pinned)
	if pinnedCount > d.maxEntry {
		pinnedCount = d.maxEntry
	}
	for i, s := range combined {
		out = append(out, projectToSkillView(s, cap, i < pinnedCount))
	}
	return out, nil
}

// partitionByPinning splits filtered skills into the three subsets
// the View pipeline needs:
//
//   - configPinned: skills whose Name is in DirectoryConfig.Pinned,
//     emitted in declaration order.
//   - extraPinned: skills not in DirectoryConfig.Pinned whose Extra
//     map carries ExtraPinnedKey bound to the bool true.
//   - unpinned: everything else.
//
// Pinned-by-config skills missing from the filtered set are SKIPPED
// (config + storage may disagree — storage wins per brief 04 §4.6).
// Order preservation: configPinned mirrors d.pinnedOrder; the other
// two partitions preserve the input slice's order, which the
// Selection-aware sort then re-orders.
func (d *Directory) partitionByPinning(in []Skill) (configPinned, extraPinned, unpinned []Skill) {
	byName := make(map[string]Skill, len(in))
	for _, s := range in {
		byName[s.Name] = s
	}

	// configPinned: walk d.pinnedOrder, emit each name's skill IF
	// present in byName (i.e. survived the capability filter under
	// this identity).
	configPinned = make([]Skill, 0, len(d.pinnedOrder))
	configPinnedSet := make(map[string]struct{}, len(d.pinnedOrder))
	for _, name := range d.pinnedOrder {
		s, ok := byName[name]
		if !ok {
			continue
		}
		configPinned = append(configPinned, s)
		configPinnedSet[name] = struct{}{}
	}

	// extraPinned + unpinned: walk the input order so the secondary
	// sort sees a deterministic seed slice. Skip names already
	// emitted in configPinned.
	extraPinned = make([]Skill, 0, len(in))
	unpinned = make([]Skill, 0, len(in))
	for _, s := range in {
		if _, ok := configPinnedSet[s.Name]; ok {
			continue
		}
		if isExtraPinned(s) {
			extraPinned = append(extraPinned, s)
			continue
		}
		unpinned = append(unpinned, s)
	}
	return configPinned, extraPinned, unpinned
}

// isExtraPinned reports whether the skill's Extra map marks it as
// runtime-pinned. The single canonical shape is ExtraPinnedKey →
// bool(true); other shapes (string "true", int 1, etc.) are ignored.
// Fail-loud on shape drift rather than silently accept — D-052.
func isExtraPinned(s Skill) bool {
	if s.Extra == nil {
		return false
	}
	v, ok := s.Extra[ExtraPinnedKey]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

// sortBySelection orders the slice in-place per the Selection's
// secondary key:
//
//   - SelectionPinnedThenRecent → UpdatedAt DESC, Name ASC
//   - SelectionPinnedThenTop    → UseCount  DESC, Name ASC
//
// Name-ASC is the tie-breaker on both branches so concurrent reads
// of the same input produce byte-identical orderings (property
// tests under -race depend on this).
func sortBySelection(in []Skill, sel Selection) {
	switch sel {
	case SelectionPinnedThenTop:
		sort.SliceStable(in, func(i, j int) bool {
			if in[i].UseCount != in[j].UseCount {
				return in[i].UseCount > in[j].UseCount
			}
			return in[i].Name < in[j].Name
		})
	default: // SelectionPinnedThenRecent (also the default fallback)
		sort.SliceStable(in, func(i, j int) bool {
			if !in[i].UpdatedAt.Equal(in[j].UpdatedAt) {
				return in[i].UpdatedAt.After(in[j].UpdatedAt)
			}
			return in[i].Name < in[j].Name
		})
	}
}

// filterByCapability is the directory's capability subset gate. The
// subset logic lives in capfilter — the single source shared with
// Phase 38's tools.Filter (D-052). capfilter is a leaf package, so
// internal/skills can import it without the cycle that blocked a
// direct internal/skills/tools import.
//
// A skill passes when, for each of RequiredTools / RequiredNS /
// RequiredTags, the required entries are a subset of the
// corresponding cap.Allowed* set. Empty Required* is unconstrained
// on its axis; empty Allowed* default-denies any non-empty Required*
// on that axis.
func filterByCapability(in []Skill, cap DirectoryCapability) []Skill {
	if len(in) == 0 {
		return nil
	}
	allowTools := capfilter.BuildSet(cap.AllowedTools)
	allowNS := capfilter.BuildSet(cap.AllowedNamespaces)
	allowTags := capfilter.BuildSet(cap.AllowedTags)

	out := make([]Skill, 0, len(in))
	for _, s := range in {
		if !capfilter.Subset(s.RequiredTools, allowTools) {
			continue
		}
		if !capfilter.Subset(s.RequiredNS, allowNS) {
			continue
		}
		if !capfilter.Subset(s.RequiredTags, allowTags) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// projectToSkillView returns the planner-visible projection of s.
// Title + Trigger are passed through the directory-local tool-name
// scrubber (Skill.Description / Steps / Preconditions / FailureModes
// are NOT projected, so they don't need redaction at this boundary —
// the SkillView is intentionally compact for cheap browsing per
// brief 04 §4.6).
//
// The `pinned` flag is supplied by the caller (the View pipeline
// computes it from the final partition assembly).
func projectToSkillView(s Skill, cap DirectoryCapability, pinned bool) SkillView {
	allowedTools := capfilter.BuildSet(cap.AllowedTools)
	disallowed := capfilter.DisallowedNames(s.RequiredTools, allowedTools)
	replacement := capfilter.Replacement(allowedTools)

	return SkillView{
		Name:     s.Name,
		Title:    capfilter.Scrub(s.Title, disallowed, replacement),
		Trigger:  capfilter.Scrub(s.Trigger, disallowed, replacement),
		TaskType: s.TaskType,
		Pinned:   pinned,
	}
}
