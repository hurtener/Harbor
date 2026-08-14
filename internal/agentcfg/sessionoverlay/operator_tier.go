// operator_tier.go — the ONE shared strict operator-tier composer (HA-66).
//
// The effective operator tier is the strict merge of the boot-declared
// operator skill baseline (internal/skills/bootpacks — config-file-declared,
// resource-free, loaded eagerly before readiness) and the selected agent's
// active durable operator-pack revision (the `agent_packs` section of the
// active agent-config revision). The tier is composed FIRST — before the
// caller's base/user/session rungs — and applied LAST, so operator-authored
// content deterministically wins a name collision against caller content,
// while a boot/revision collision is a typed conflict, never last-write-wins.
//
// The merge is STRICT: identity is the canonical (lowercase, trimmed) name
// paired with the canonical attachment-free semantic content hash
// (skills.CanonicalContentHash — Origin/Scope/provenance and lifecycle fields
// excluded). The same name with the same hash dedupes to ONE item marked
// `both` (migration compatibility: moving an unchanged body between the
// durable revision and the boot config must not split or overwrite the
// composed view). The same name with a differing hash fails loud — a silent
// map overwrite is impossible. The unique combined tier holds at most
// MaxOperatorTierItems items.
//
// The composer is PURE: it performs no lifecycle, config, store, admin-verb,
// or filesystem access. Boot entries arrive already frozen by the eager boot
// loader; revision items arrive already converted by the projection. The
// composer re-validates and re-hashes every input so a tampered membership
// input cannot smuggle a malformed body or a mismatched semantic hash into
// the composed tier.
//
// The result is IMMUTABLE: [OperatorTier] never mutates after construction,
// and every accessor returns deep copies, so boot preflight, run start, and
// preview share one frozen view under -race, and concurrent changes affect
// only later snapshots.

package sessionoverlay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"

	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/bootpacks"
)

// MaxOperatorTierItems caps the unique combined operator-tier items after
// strict dedup. It mirrors skills.SnapshotSemanticCandidateCap so the
// composed view stays inside the frozen-candidate budget the resolver feeds
// the semantic searcher.
const MaxOperatorTierItems = skills.SnapshotSemanticCandidateCap // 256

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrOperatorTierInvalid — an input item failed validation: a missing
	// canonical name, an invalid skill body, or a boot entry whose
	// semantic hash does not match its own body.
	ErrOperatorTierInvalid = errors.New("agentcfg/sessionoverlay: invalid operator tier input")
	// ErrOperatorTierConflict — the same canonical name appears with
	// differing canonical semantic hashes across (or within) the boot and
	// revision inputs. Never a silent map overwrite.
	ErrOperatorTierConflict = errors.New("agentcfg/sessionoverlay: operator tier name conflict (same canonical name, differing semantic hash)")
	// ErrOperatorTierBound — the unique combined operator-tier item count
	// exceeds MaxOperatorTierItems.
	ErrOperatorTierBound = errors.New("agentcfg/sessionoverlay: operator tier exceeds 256 unique items")
)

// Set-hash envelopes. The boot envelope MUST byte-match the bootpacks index's
// framing (internal/skills/bootpacks/index.go, "boot-pack-set-v1\x00") so the
// composer's boot_pack_set_hash is identical to the eager index's value for
// the same entries across restarts. The combined and revision envelopes are
// distinct so the three hashes can never collide.
const (
	bootPackSetHashEnvelope      = "boot-pack-set-v1\x00"
	operatorTierHashEnvelope     = "operator-tier-v1\x00"
	operatorRevisionHashEnvelope = "operator-revision-v1\x00"
)

// OperatorTierItem is ONE composed effective-operator-tier item: the deep-
// copied skill body, its canonical attachment-free semantic content hash, and
// its strict-merge provenance marker.
type OperatorTierItem struct {
	// Skill is the composed skill body (boot body retained when the item
	// is `both` — the boot baseline is the higher-authority source and the
	// bodies are semantically identical by canonical hash).
	Skill skills.Skill
	// SemanticHash is the canonical attachment-free content hash of Skill
	// (skills.CanonicalContentHash) — the semantic identity the merge and
	// every set hash use.
	SemanticHash string
	// Source reports whether the item came from the boot baseline, the
	// active durable revision, or both (identical content).
	Source skills.OperatorTierSource
}

// OperatorTier is the immutable effective operator tier: the strict merge of
// the boot baseline and the active durable operator-pack revision. It is
// safe to copy and safe for N concurrent readers after construction; every
// accessor returns deep copies and no field is mutated after construction.
type OperatorTier struct {
	items  []OperatorTierItem
	byName map[string]OperatorTierItem
	// bootPackSetHash is the deterministic set hash over the boot baseline
	// entries only ("" when no boot baseline is bound) — identical to the
	// bootpacks index's BootPackSetHash for the same entries.
	bootPackSetHash string
	// combinedHash is the deterministic set hash over the unique combined
	// operator-tier items ("" when the tier is empty).
	combinedHash string
	// revisionHash is the deterministic set hash over the revision items
	// only ("" when no revision pack is bound).
	revisionHash string
}

// operatorTierCandidate is one normalized input item mid-merge: the
// canonical name key, the validated skill body, its canonical semantic hash,
// and the running provenance marker.
type operatorTierCandidate struct {
	skill  skills.Skill
	hash   string
	source skills.OperatorTierSource
}

// ComposeOperatorTier performs the strict merge of the boot baseline entries
// and the active-revision pack skills into ONE immutable effective operator
// tier. Either input may be nil/empty (an agent with no boot baseline, no
// revision pack, or neither). The returned tier is deterministic: identical
// inputs — in any order — produce byte-identical Items() and identical set
// hashes.
func ComposeOperatorTier(boot []bootpacks.Entry, revision []skills.Skill) (OperatorTier, error) {
	bootByName, err := normalizeBootCandidates(boot)
	if err != nil {
		return OperatorTier{}, err
	}
	revisionByName, err := normalizeRevisionCandidates(revision)
	if err != nil {
		return OperatorTier{}, err
	}
	merged, err := mergeOperatorTierCandidates(bootByName, revisionByName)
	if err != nil {
		return OperatorTier{}, err
	}
	if len(merged) > MaxOperatorTierItems {
		return OperatorTier{}, fmt.Errorf("%w: %d unique combined items", ErrOperatorTierBound, len(merged))
	}
	tier := buildOperatorTier(merged)
	if len(bootByName) > 0 {
		tier.bootPackSetHash = candidatesSetHash(bootPackSetHashEnvelope, bootByName)
	}
	if len(revisionByName) > 0 {
		tier.revisionHash = candidatesSetHash(operatorRevisionHashEnvelope, revisionByName)
	}
	if len(merged) > 0 {
		tier.combinedHash = candidatesSetHash(operatorTierHashEnvelope, merged)
	}
	return tier, nil
}

// normalizeBootCandidates validates, canonicalizes, and dedupes the boot
// baseline entries. A boot entry whose frozen SemanticHash does not equal the
// canonical hash of its own body is rejected: the boot set hash must stay
// byte-identical to the eager index's value, and a tampered entry must never
// smuggle a mismatched identity into the merge.
func normalizeBootCandidates(entries []bootpacks.Entry) (map[string]operatorTierCandidate, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := make(map[string]operatorTierCandidate, len(entries))
	for i := range entries {
		normalized, err := normalizeOperatorTierSkill(entries[i].Skill)
		if err != nil {
			return nil, err
		}
		name := canonicalNameFor(normalized.Name)
		if name == "" {
			return nil, fmt.Errorf("%w: boot entry has an empty canonical name", ErrOperatorTierInvalid)
		}
		hash := skills.CanonicalContentHash(normalized)
		if entries[i].SemanticHash != hash {
			return nil, fmt.Errorf("%w: boot entry %q semantic hash does not match its body", ErrOperatorTierInvalid, name)
		}
		if prior, exists := out[name]; exists {
			if prior.hash != hash {
				return nil, fmt.Errorf("%w: %q (boot only)", ErrOperatorTierConflict, name)
			}
			continue
		}
		out[name] = operatorTierCandidate{skill: normalized, hash: hash}
	}
	return out, nil
}

// normalizeRevisionCandidates validates, canonicalizes, and dedupes the
// active-revision pack skills.
func normalizeRevisionCandidates(items []skills.Skill) (map[string]operatorTierCandidate, error) {
	if len(items) == 0 {
		return nil, nil
	}
	out := make(map[string]operatorTierCandidate, len(items))
	for i := range items {
		normalized, err := normalizeOperatorTierSkill(items[i])
		if err != nil {
			return nil, err
		}
		name := canonicalNameFor(normalized.Name)
		if name == "" {
			return nil, fmt.Errorf("%w: revision pack item has an empty canonical name", ErrOperatorTierInvalid)
		}
		hash := skills.CanonicalContentHash(normalized)
		if prior, exists := out[name]; exists {
			if prior.hash != hash {
				return nil, fmt.Errorf("%w: %q (revision only)", ErrOperatorTierConflict, name)
			}
			continue
		}
		out[name] = operatorTierCandidate{skill: normalized, hash: hash}
	}
	return out, nil
}

// mergeOperatorTierCandidates cross-merges the two normalized input sets. A
// name in both sets with an identical hash dedupes to ONE item marked both
// (the boot body is retained); a name in both with a differing hash fails
// loud — never a silent overwrite.
func mergeOperatorTierCandidates(boot, revision map[string]operatorTierCandidate) (map[string]operatorTierCandidate, error) {
	if len(boot) == 0 {
		if len(revision) == 0 {
			return nil, nil
		}
		out := make(map[string]operatorTierCandidate, len(revision))
		for name, candidate := range revision {
			candidate.source = skills.OperatorTierSourceRevision
			out[name] = candidate
		}
		return out, nil
	}
	out := make(map[string]operatorTierCandidate, len(boot)+len(revision))
	for name, candidate := range boot {
		candidate.source = skills.OperatorTierSourceBoot
		out[name] = candidate
	}
	for name, candidate := range revision {
		prior, exists := out[name]
		if !exists {
			candidate.source = skills.OperatorTierSourceRevision
			out[name] = candidate
			continue
		}
		if prior.hash != candidate.hash {
			return nil, fmt.Errorf("%w: %q (boot %s…, revision %s…)", ErrOperatorTierConflict, name, shortHash(prior.hash), shortHash(candidate.hash))
		}
		prior.source = skills.OperatorTierSourceBoth
		out[name] = prior
	}
	return out, nil
}

// buildOperatorTier freezes the merged candidates: deterministic canonical-
// name order, the canonical-name index, and deep-copied immutable items.
func buildOperatorTier(merged map[string]operatorTierCandidate) OperatorTier {
	tier := OperatorTier{byName: make(map[string]OperatorTierItem, len(merged))}
	if len(merged) == 0 {
		return tier
	}
	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	sort.Strings(names)
	tier.items = make([]OperatorTierItem, 0, len(names))
	for _, name := range names {
		candidate := merged[name]
		item := OperatorTierItem{
			Skill:        cloneSkill(candidate.skill),
			SemanticHash: candidate.hash,
			Source:       candidate.source,
		}
		tier.items = append(tier.items, item)
		tier.byName[name] = item
	}
	return tier
}

// normalizeOperatorTierSkill validates a skill body and freezes Extra into
// its JSON canonical form, so the canonical content hash and every deep copy
// are stable regardless of the input representation.
func normalizeOperatorTierSkill(skill skills.Skill) (skills.Skill, error) {
	if err := skill.Validate(); err != nil {
		return skills.Skill{}, fmt.Errorf("%w: %q: %w", ErrOperatorTierInvalid, skill.Name, err)
	}
	if canonicalNameFor(skill.Name) == "" {
		return skills.Skill{}, fmt.Errorf("%w: skill name has no canonical form", ErrOperatorTierInvalid)
	}
	if skill.Extra == nil {
		return skill, nil
	}
	bytes, err := json.Marshal(skill.Extra)
	if err != nil {
		return skills.Skill{}, fmt.Errorf("%w: skill %q Extra must be JSON-compatible and acyclic", ErrOperatorTierInvalid, skill.Name)
	}
	var normalized map[string]any
	if err := json.Unmarshal(bytes, &normalized); err != nil {
		return skills.Skill{}, fmt.Errorf("%w: skill %q Extra normalization failed", ErrOperatorTierInvalid, skill.Name)
	}
	skill.Extra = normalized
	return skill, nil
}

// Items returns the combined operator-tier items in deterministic canonical-
// name order as deep copies. Caller mutation of a returned item cannot affect
// the tier or another caller's copy.
func (t OperatorTier) Items() []OperatorTierItem {
	out := make([]OperatorTierItem, len(t.items))
	for i, item := range t.items {
		out[i] = item
		out[i].Skill = cloneSkill(item.Skill)
	}
	return out
}

// Len returns the unique combined operator-tier item count.
func (t OperatorTier) Len() int { return len(t.items) }

// Get returns the deep-copied item for canonical name name. The bool is false
// when the name is not an operator-tier item.
func (t OperatorTier) Get(name string) (OperatorTierItem, bool) {
	item, ok := t.byName[canonicalNameFor(name)]
	if !ok {
		return OperatorTierItem{}, false
	}
	item.Skill = cloneSkill(item.Skill)
	return item, true
}

// Source returns the strict-merge provenance marker for canonical name name.
// The bool is false when the name is not an operator-tier item.
func (t OperatorTier) Source(name string) (skills.OperatorTierSource, bool) {
	item, ok := t.byName[canonicalNameFor(name)]
	if !ok {
		return "", false
	}
	return item.Source, true
}

// BootPackSetHash returns the deterministic set hash over the boot baseline
// entries only ("" when no boot baseline is bound). Identical to the
// bootpacks index's BootPackSetHash for the same entries.
func (t OperatorTier) BootPackSetHash() string { return t.bootPackSetHash }

// CombinedHash returns the deterministic set hash over the unique combined
// operator-tier items ("" when the tier is empty).
func (t OperatorTier) CombinedHash() string { return t.combinedHash }

// RevisionHash returns the deterministic set hash over the active-revision
// pack items only ("" when no revision pack is bound).
func (t OperatorTier) RevisionHash() string { return t.revisionHash }

// candidatesSetHash computes the deterministic set hash over canonical
// ordered name+semantic-hash pairs of one candidate set, using the same
// length-framed construction as the bootpacks index (each pair is framed so
// no name or hash content can perturb the framing). The input order never
// matters: the pairs are sorted by canonical name before hashing.
func candidatesSetHash(envelope string, candidates map[string]operatorTierCandidate) string {
	h := sha256.New()
	_, _ = io.WriteString(h, envelope)
	names := make([]string, 0, len(candidates))
	for name := range candidates {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		writeOperatorTierFramed(h, name)
		writeOperatorTierFramed(h, candidates[name].hash)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeOperatorTierFramed appends the length-prefixed framing of one field:
// "<byte-len>:<bytes>;". Byte-identical to the bootpacks index's framing.
func writeOperatorTierFramed(w io.Writer, s string) {
	_, _ = io.WriteString(w, strconv.Itoa(len(s)))
	_, _ = io.WriteString(w, ":")
	_, _ = io.WriteString(w, s)
	_, _ = io.WriteString(w, ";")
}

// shortHash renders a hash prefix for a deterministic, readable conflict
// message.
func shortHash(hash string) string {
	if len(hash) > 8 {
		return hash[:8]
	}
	return hash
}
