// Package virtualagent owns Harbor's canonical virtual-agent profile
// representation: the bounded, non-recursive overlay a configured
// top-level agent applies to a planner-spawned child run, the immutable
// profile map frozen at the parent's run start, and the pinned binding
// persisted on the child task so a restart reproduces the exact profile.
//
// # What a virtual agent is
//
// A virtual agent is NOT a registered agent. It never appears in
// `agents.list`, is never a `control.start` target, and its key never
// joins the isolation tuple. `Task.AgentID` stays the owning top-level
// agent; the profile is a per-run CONFIGURATION projection only, exactly
// like the agent-config control plane's other sections.
//
// # The canonical representation (YAML and AgentConfig revisions)
//
// A profile is declared either in the boot YAML (`virtual_agents:`) or
// in an immutable AgentConfig revision's `virtual_agents` section. Both
// doors decode into the SAME canonical [Profile] / [Overlay] shapes in
// this package, normalized by the SAME functions, and hashed by the
// SAME [Profile.Hash] — a YAML-declared profile and a revision-pinned
// profile with identical content are byte-identical in canonical form.
//
// # The overlay is bounded and non-recursive
//
// An overlay MAY narrow the child's model parameters, skills
// (intersection), tool exposure (union into the exclusion set) and
// limits (max-steps / token-budget), and MAY add trusted specialist
// instructions. It CANNOT widen resources or guardrails, attach
// capabilities, own providers / hooks / memory, recurse into another
// profile, or target A2A — those fields do not exist in [Overlay], so
// no value can express them (structurally impossible, not merely
// validated). Omission is byte-compatible: a run that never selects a
// profile behaves exactly as before.
//
// # Freeze, bind, pin
//
// The parent's run start resolves the effective profile map (revision
// section over YAML), validates it, and FREEZES it against the parent's
// active config revision id + digest. A planner spawn selecting a
// profile validates key-unknown / overlay-invalid / stale-live-revision
// BEFORE anything persists; the accepted spawn carries a [Binding] (the
// owning agent, key, label, parent, config revision id + digest, profile
// hash) onto the task record. The child's run start re-resolves the
// current map and validates the pin: a moved config revision, an edited
// profile definition, or a tampered binding fails the child run LOUD —
// a restart reproduces the exact profile or does not run at all. A
// virtual-profile run cannot itself spawn a virtual profile (non-recursive).
package virtualagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrInvalidKey — a profile key is empty, over-long, or outside the
	// restricted identifier charset.
	ErrInvalidKey = errors.New("virtualagent: invalid profile key")
	// ErrInvalidOverlay — the overlay violates a bounded narrow-only
	// invariant (an unknown dimension is structurally impossible; this
	// covers bounds / charset / narrowing-shape violations).
	ErrInvalidOverlay = errors.New("virtualagent: invalid overlay")
	// ErrInvalidProfile — the profile envelope (key / label / parent /
	// overlay) failed validation.
	ErrInvalidProfile = errors.New("virtualagent: invalid profile")
	// ErrInvalidMap — the profile map failed validation (owner mismatch,
	// duplicate key, an invalid member).
	ErrInvalidMap = errors.New("virtualagent: invalid profile map")
	// ErrUnknown — a spawn selected a profile key absent from the frozen
	// map. Fails BEFORE persistence.
	ErrUnknown = errors.New("virtualagent: unknown profile key")
	// ErrInvalid — a spawn selected a profile whose overlay failed
	// re-validation at the dispatch boundary. Fails BEFORE persistence.
	ErrInvalid = errors.New("virtualagent: invalid selected profile")
	// ErrStale — the selected profile is stale: the live parent-config
	// revision no longer matches the frozen map's pinned revision (at
	// spawn), or the persisted binding no longer matches the current
	// revision / profile definition (at child run start). Fails before
	// persistence at spawn; fails the child run LOUD at run start.
	ErrStale = errors.New("virtualagent: stale profile pin")
	// ErrRecursion — a run that IS a virtual-profile run tried to spawn
	// another virtual-profile child. The overlay is non-recursive.
	ErrRecursion = errors.New("virtualagent: virtual profile recursion")
	// ErrNoMap — a spawn selected a profile in a run whose effective
	// agent is not the profile-owning top-level agent (no frozen map).
	ErrNoMap = errors.New("virtualagent: no virtual-agent profile map in this run")
	// ErrTampered — the persisted binding disagrees with the resolved
	// profile (label / parent / hash mismatch). Fails the child run LOUD.
	ErrTampered = errors.New("virtualagent: tampered profile binding")
	// ErrMissing — the persisted binding's key is absent from the current
	// profile map. Fails the child run LOUD.
	ErrMissing = errors.New("virtualagent: bound profile key is missing from the current map")
)

// Size bounds. Single-sourced here; the config validator and the
// agentcfg normalization call into this package rather than carrying
// literal copies.
const (
	// MaxKeyLength bounds a profile key in bytes.
	MaxKeyLength = 128
	// MaxLabelLength bounds a profile label in bytes.
	MaxLabelLength = 200
	// MaxModelLength bounds the overlay model name in bytes.
	MaxModelLength = 128
	// MaxSkillEntries bounds the overlay skill-narrowing set size.
	MaxSkillEntries = 100
	// MaxToolListEntries bounds each overlay tool-exclusion list size.
	MaxToolListEntries = 500
	// MaxInstructionsBytes bounds the additive specialist-instruction
	// text (trusted, operator-authored — bounded, never unbounded).
	MaxInstructionsBytes = 16 * 1024
	// MaxMaxTokens bounds the overlay max-tokens value.
	MaxMaxTokens = 1_000_000
	// MaxMaxSteps bounds the overlay max-steps value.
	MaxMaxSteps = 100_000
	// MaxMaxTokenBudget bounds the overlay token-budget value.
	MaxMaxTokenBudget = 10_000_000
	// DefaultMaxProfiles bounds the operator-owned virtual profile map.
	DefaultMaxProfiles = 32
)

// keyRe is the restricted identifier charset for a profile key:
// lowercase alphanumeric start, then lowercase alphanumeric / dot /
// underscore / hyphen. Keys stay legible as prompt labels and safe as
// map keys and block names.
var keyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Key identifies one virtual-agent profile within its owner.
type Key string

// ValidateKey checks the key charset + length bounds.
func ValidateKey(k Key) error {
	if k == "" {
		return fmt.Errorf("%w: empty key", ErrInvalidKey)
	}
	if len(k) > MaxKeyLength {
		return fmt.Errorf("%w: key %q exceeds %d bytes", ErrInvalidKey, k, MaxKeyLength)
	}
	if !keyRe.MatchString(string(k)) {
		return fmt.Errorf("%w: key %q must match %s", ErrInvalidKey, k, keyRe.String())
	}
	return nil
}

// Overlay is the explicit bounded non-recursive overlay a profile
// applies over its parent's frozen configuration. Every dimension is
// narrow-only or additive:
//
//   - Model / Temperature / MaxTokens / ReasoningEffort narrow the
//     child's sampling parameters (the child's max-tokens ceiling is
//     clamped to the parent's resolved ceiling — see
//     [OverlayClampMaxTokens]).
//   - Skills is an INTERSECTION: the child keeps only parent skills
//     named here (nil = no narrowing).
//   - DisabledTools / PausedServers are UNIONED into the exclusion set:
//     they can only hide tools the parent exposed, never re-expose one.
//   - MaxSteps / TokenBudget narrow the child's run limits (clamped to
//     the parent's own cap).
//   - Instructions adds trusted, operator-authored specialist guidance
//     (rendered verbatim in the trusted additive position, bounded).
//
// There is deliberately NO field for providers, hooks, memory,
// capabilities, a parent-profile reference, or an A2A target: those
// dimensions cannot be expressed, so they can never be widened,
// attached, or recursed into.
type Overlay struct {
	Model           *string  `json:"model,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	MaxTokens       *int     `json:"max_tokens,omitempty"`
	ReasoningEffort *string  `json:"reasoning_effort,omitempty"`
	// Skills is a pointer so an EXPLICIT empty set (narrow to no skills)
	// is distinguishable from omission (no narrowing). nil = no skill
	// narrowing.
	Skills        *[]string `json:"skills,omitempty"`
	DisabledTools []string  `json:"disabled_tools,omitempty"`
	PausedServers []string  `json:"paused_servers,omitempty"`
	MaxSteps      *int      `json:"max_steps,omitempty"`
	TokenBudget   *int      `json:"token_budget,omitempty"`
	Instructions  string    `json:"instructions,omitempty"`
}

// IsZero reports whether the overlay is entirely empty (no narrowing,
// no instructions) — the identity overlay that leaves a run
// byte-identical.
func (o Overlay) IsZero() bool {
	return o.Model == nil && o.Temperature == nil && o.MaxTokens == nil &&
		o.ReasoningEffort == nil && o.Skills == nil && len(o.DisabledTools) == 0 &&
		len(o.PausedServers) == 0 && o.MaxSteps == nil && o.TokenBudget == nil &&
		strings.TrimSpace(o.Instructions) == ""
}

// NormalizeOverlay returns the canonical form: sorted / de-duplicated
// list members, trimmed instructions, preserved pointer presence. A nil
// or empty-nil list normalises to nil so omission is byte-compatible.
func NormalizeOverlay(o Overlay) Overlay {
	out := Overlay{
		Model:           copyStringPtr(o.Model),
		Temperature:     copyFloat64Ptr(o.Temperature),
		MaxTokens:       copyIntPtr(o.MaxTokens),
		ReasoningEffort: copyStringPtr(o.ReasoningEffort),
		DisabledTools:   sortDedup(o.DisabledTools),
		PausedServers:   sortDedup(o.PausedServers),
		MaxSteps:        copyIntPtr(o.MaxSteps),
		TokenBudget:     copyIntPtr(o.TokenBudget),
		Instructions:    strings.TrimSpace(o.Instructions),
	}
	if o.Skills != nil {
		s := sortDedup(*o.Skills)
		if len(s) == 0 {
			// An explicit empty narrowing set is PRESENCE (narrow to
			// nothing), so it stays non-nil.
			out.Skills = &[]string{}
		} else {
			out.Skills = &s
		}
	}
	return out
}

// ValidateOverlay checks the bounded narrow-only invariants. Unknown
// dimensions are structurally unrepresentable; this validates bounds /
// charset / reasoning-effort values and the narrowing shape (list
// members bounded, never an enable set).
func ValidateOverlay(o Overlay) error {
	if o.Model != nil {
		m := strings.TrimSpace(*o.Model)
		if m == "" || len(m) > MaxModelLength {
			return fmt.Errorf("%w: model must be 1..%d bytes", ErrInvalidOverlay, MaxModelLength)
		}
	}
	if o.Temperature != nil {
		if *o.Temperature < 0 || *o.Temperature > 2 {
			return fmt.Errorf("%w: temperature must be in [0,2]", ErrInvalidOverlay)
		}
	}
	if o.MaxTokens != nil {
		if *o.MaxTokens <= 0 || *o.MaxTokens > MaxMaxTokens {
			return fmt.Errorf("%w: max_tokens must be in (0,%d]", ErrInvalidOverlay, MaxMaxTokens)
		}
	}
	if o.ReasoningEffort != nil {
		switch *o.ReasoningEffort {
		case "off", "low", "medium", "high":
		default:
			return fmt.Errorf("%w: reasoning_effort %q not in {off,low,medium,high}", ErrInvalidOverlay, *o.ReasoningEffort)
		}
	}
	if o.Skills != nil {
		if len(*o.Skills) > MaxSkillEntries {
			return fmt.Errorf("%w: skills narrowing exceeds %d entries", ErrInvalidOverlay, MaxSkillEntries)
		}
		for _, s := range *o.Skills {
			if strings.TrimSpace(s) == "" || len(s) > 200 {
				return fmt.Errorf("%w: skill name must be 1..200 bytes", ErrInvalidOverlay)
			}
		}
	}
	for _, list := range [][]string{o.DisabledTools, o.PausedServers} {
		if len(list) > MaxToolListEntries {
			return fmt.Errorf("%w: tool-exclusion list exceeds %d entries", ErrInvalidOverlay, MaxToolListEntries)
		}
		for _, s := range list {
			if strings.TrimSpace(s) == "" || len(s) > 200 {
				return fmt.Errorf("%w: tool / server name must be 1..200 bytes", ErrInvalidOverlay)
			}
		}
	}
	if o.MaxSteps != nil {
		if *o.MaxSteps <= 0 || *o.MaxSteps > MaxMaxSteps {
			return fmt.Errorf("%w: max_steps must be in (0,%d]", ErrInvalidOverlay, MaxMaxSteps)
		}
	}
	if o.TokenBudget != nil {
		if *o.TokenBudget <= 0 || *o.TokenBudget > MaxMaxTokenBudget {
			return fmt.Errorf("%w: token_budget must be in (0,%d]", ErrInvalidOverlay, MaxMaxTokenBudget)
		}
	}
	if len(o.Instructions) > MaxInstructionsBytes {
		return fmt.Errorf("%w: instructions exceed %d bytes", ErrInvalidOverlay, MaxInstructionsBytes)
	}
	return nil
}

// Profile is the canonical virtual-agent profile shared by the boot
// YAML declaration and immutable AgentConfig revisions.
type Profile struct {
	Key     Key     `json:"key"`
	Label   string  `json:"label,omitempty"`
	Parent  string  `json:"parent,omitempty"`
	Overlay Overlay `json:"overlay,omitempty"`
}

// NormalizeProfile returns the canonical profile form.
func NormalizeProfile(p Profile) Profile {
	return Profile{
		Key:     Key(strings.TrimSpace(string(p.Key))),
		Label:   strings.TrimSpace(p.Label),
		Parent:  strings.TrimSpace(p.Parent),
		Overlay: NormalizeOverlay(p.Overlay),
	}
}

// ValidateProfile checks the profile envelope (key / label / parent /
// overlay bounds).
func ValidateProfile(p Profile) error {
	if err := ValidateKey(p.Key); err != nil {
		return err
	}
	if len(p.Label) > MaxLabelLength {
		return fmt.Errorf("%w: label exceeds %d bytes", ErrInvalidProfile, MaxLabelLength)
	}
	if strings.TrimSpace(p.Parent) == "" {
		return fmt.Errorf("%w: parent agent must be set", ErrInvalidProfile)
	}
	if err := ValidateOverlay(p.Overlay); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidProfile, err)
	}
	return nil
}

// Hash returns the deterministic SHA-256 hex digest of the profile's
// canonical (sorted-key JSON) representation. Two profiles with
// identical content — declared in YAML or in a revision — hash equal;
// any content change bumps the hash. This is the digest a Binding pins.
func (p Profile) Hash() (string, error) {
	canon, err := canonicalJSON(NormalizeProfile(p))
	if err != nil {
		return "", fmt.Errorf("%w: hash: %v", ErrInvalidProfile, err)
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}

// Map is the canonical profile map owned by ONE configured top-level
// agent. The map's owner must equal the runtime's configured top-level
// agent id; every profile's Parent must equal the owner.
type Map struct {
	Owner       string    `json:"owner,omitempty"`
	MaxProfiles int       `json:"max_profiles,omitempty"`
	Profiles    []Profile `json:"profiles,omitempty"`
}

// NormalizeMap returns the canonical map form: profiles sorted by key
// (key-unique; the LAST duplicate wins, matching the agentcfg
// connection-section convention).
func NormalizeMap(m Map) Map {
	byKey := make(map[Key]Profile, len(m.Profiles))
	keys := make([]string, 0, len(m.Profiles))
	for _, p := range m.Profiles {
		np := NormalizeProfile(p)
		if np.Key == "" {
			continue
		}
		if _, seen := byKey[np.Key]; !seen {
			keys = append(keys, string(np.Key))
		}
		byKey[np.Key] = np
	}
	if len(keys) == 0 {
		return Map{Owner: strings.TrimSpace(m.Owner), MaxProfiles: m.MaxProfiles}
	}
	sort.Strings(keys)
	out := make([]Profile, 0, len(keys))
	for _, k := range keys {
		out = append(out, byKey[Key(k)])
	}
	return Map{Owner: strings.TrimSpace(m.Owner), MaxProfiles: m.MaxProfiles, Profiles: out}
}

// ValidateMap checks the owner + every member's parent-owner binding.
func ValidateMap(m Map) error {
	owner := strings.TrimSpace(m.Owner)
	if owner == "" {
		return fmt.Errorf("%w: owner agent must be set", ErrInvalidMap)
	}
	cap := m.MaxProfiles
	if cap <= 0 {
		cap = DefaultMaxProfiles
	}
	if len(m.Profiles) > cap {
		return fmt.Errorf("%w: %d profiles exceeds cap %d", ErrInvalidMap, len(m.Profiles), cap)
	}
	seen := make(map[Key]struct{}, len(m.Profiles))
	for _, p := range m.Profiles {
		if err := ValidateProfile(p); err != nil {
			return fmt.Errorf("%w: profile %q: %v", ErrInvalidMap, p.Key, err)
		}
		if p.Parent != owner {
			return fmt.Errorf("%w: profile %q parent %q != owner %q (profiles are owned by one top-level agent and never recurse)",
				ErrInvalidMap, p.Key, p.Parent, owner)
		}
		if _, dup := seen[p.Key]; dup {
			return fmt.Errorf("%w: duplicate profile key %q", ErrInvalidMap, p.Key)
		}
		seen[p.Key] = struct{}{}
	}
	return nil
}

// ByKey returns the keyed view of the map's profiles. Callers must not
// mutate the returned map or its profile values.
func (m Map) ByKey() map[Key]Profile {
	out := make(map[Key]Profile, len(m.Profiles))
	for _, p := range m.Profiles {
		out[p.Key] = p
	}
	return out
}

// LiveRevisionReader is the per-run injected seam the frozen map uses to
// re-read the parent agent's CURRENT active config revision at spawn
// time (the staleness probe). The run-loop driver builds it over its
// agentcfg registry + the run's identity triple; it carries no identity
// because the driver already captured it. A nil function is a no-op
// stale check (the frozen map's pinned revision is trusted as-is).
type LiveRevisionReader func(ctx context.Context) (revisionID, digest string, err error)

// FrozenMap is the profile map frozen at the parent's run start: the
// canonical [Map] bound to the parent agent's active config revision id
// + digest. A spawn validates against the frozen map (unknown / invalid
// / stale), and the persisted [Binding] pins the frozen revision +
// digest + per-profile hash so the child's run start reproduces the
// exact profile — or fails loud.
//
// A FrozenMap is per-run state carried on the run's ctx (the
// concurrent-reuse contract): never a field on a shared artifact.
type FrozenMap struct {
	Owner        string
	RevisionID   string
	ConfigDigest string
	profiles     map[Key]Profile
	hashes       map[Key]string
	live         LiveRevisionReader
}

// NewFrozenMap freezes a validated canonical map against a parent-config
// revision. A nil map or an empty owner returns ErrInvalidMap. live is
// the optional staleness probe (nil disables the live re-check).
func NewFrozenMap(m Map, revisionID, configDigest string, live LiveRevisionReader) (*FrozenMap, error) {
	if err := ValidateMap(m); err != nil {
		return nil, err
	}
	f := &FrozenMap{
		Owner:        m.Owner,
		RevisionID:   revisionID,
		ConfigDigest: configDigest,
		profiles:     make(map[Key]Profile, len(m.Profiles)),
		hashes:       make(map[Key]string, len(m.Profiles)),
		live:         live,
	}
	for _, p := range m.Profiles {
		h, err := p.Hash()
		if err != nil {
			return nil, fmt.Errorf("%w: hash profile %q: %v", ErrInvalidMap, p.Key, err)
		}
		f.profiles[p.Key] = p
		f.hashes[p.Key] = h
	}
	return f, nil
}

// Profile returns the frozen profile for key and whether it exists.
func (f *FrozenMap) Profile(k Key) (Profile, bool) {
	if f == nil {
		return Profile{}, false
	}
	p, ok := f.profiles[k]
	if !ok {
		return Profile{}, false
	}
	return cloneProfile(p), true
}

// HashOf returns the frozen profile hash for key and whether it exists.
func (f *FrozenMap) HashOf(k Key) (string, bool) {
	if f == nil {
		return "", false
	}
	h, ok := f.hashes[k]
	return h, ok
}

// VerifyCurrent is the spawn-time staleness probe: it re-reads the live
// parent-config revision and returns ErrStale when it no longer matches
// the frozen pin. A nil live reader (the default in pure unit tests)
// trusts the frozen pin and returns nil.
func (f *FrozenMap) VerifyCurrent(ctx context.Context) error {
	if f == nil || f.live == nil {
		return nil
	}
	rev, digest, err := f.live(ctx)
	if err != nil {
		return fmt.Errorf("%w: read live revision: %v", ErrStale, err)
	}
	if rev != f.RevisionID || digest != f.ConfigDigest {
		return fmt.Errorf("%w: live parent-config revision %q/%s no longer matches the frozen pin %q/%s",
			ErrStale, rev, shortDigest(digest), f.RevisionID, shortDigest(f.ConfigDigest))
	}
	return nil
}

// Binding is the immutable per-task metadata that pins a virtual-agent
// profile: persisted on the child task at spawn (before the child runs)
// and re-validated at the child's run start so a restart reproduces the
// exact profile. `AgentID` is the owning TOP-LEVEL agent — it stays
// `Task.AgentID`; the binding never re-keys the task to a different
// agent and never joins the isolation tuple.
type Binding struct {
	AgentID          string `json:"agent_id,omitempty"`
	Key              Key    `json:"key,omitempty"`
	Label            string `json:"label,omitempty"`
	Parent           string `json:"parent,omitempty"`
	ConfigRevisionID string `json:"config_revision_id,omitempty"`
	ConfigDigest     string `json:"config_digest,omitempty"`
	ProfileHash      string `json:"profile_hash,omitempty"`
	// Profile is the sealed canonical snapshot used to reconstruct the child
	// without consulting mutable configuration after admission.
	Profile Profile `json:"profile"`
}

// ValidateBinding checks the structural invariants of a persisted
// binding (all fields non-empty + bounded; the digest is a 64-hex
// SHA-256). Callers additionally verify the binding against the frozen /
// current map — this is the shape check only.
func ValidateBinding(b Binding) error {
	if strings.TrimSpace(b.AgentID) == "" {
		return fmt.Errorf("virtualagent: binding agent must be set")
	}
	if err := ValidateKey(b.Key); err != nil {
		return err
	}
	if len(b.Label) > MaxLabelLength {
		return fmt.Errorf("virtualagent: binding label exceeds %d bytes", MaxLabelLength)
	}
	if strings.TrimSpace(b.Parent) == "" {
		return fmt.Errorf("virtualagent: binding parent must be set")
	}
	if strings.TrimSpace(b.ConfigRevisionID) == "" {
		return fmt.Errorf("virtualagent: binding config revision must be set")
	}
	if len(b.ConfigDigest) != 64 {
		return fmt.Errorf("virtualagent: binding config digest must be a 64-hex SHA-256")
	}
	if _, err := hex.DecodeString(b.ConfigDigest); err != nil {
		return fmt.Errorf("virtualagent: binding config digest is not hex: %v", err)
	}
	if b.Parent != b.AgentID {
		return fmt.Errorf("virtualagent: binding parent %q != agent %q", b.Parent, b.AgentID)
	}
	if len(b.ProfileHash) != 64 {
		return fmt.Errorf("virtualagent: binding profile hash must be a 64-hex SHA-256")
	}
	if _, err := hex.DecodeString(b.ProfileHash); err != nil {
		return fmt.Errorf("virtualagent: binding profile hash is not hex: %v", err)
	}
	if b.Profile.Key != "" {
		if err := ValidateProfile(NormalizeProfile(b.Profile)); err != nil {
			return fmt.Errorf("virtualagent: binding profile snapshot: %w", err)
		}
		h, err := b.Profile.Hash()
		if err != nil || h != b.ProfileHash {
			return fmt.Errorf("virtualagent: binding profile snapshot does not match profile hash: %w", ErrTampered)
		}
	}
	return nil
}

// Bind constructs the binding for the frozen profile p under the map's
// owner: agent = parent = owner (the top-level agent), pinned to the
// frozen revision + digest + the profile's hash.
func (f *FrozenMap) Bind(p Profile) (Binding, error) {
	if f == nil {
		return Binding{}, ErrNoMap
	}
	h, ok := f.HashOf(p.Key)
	if !ok {
		return Binding{}, fmt.Errorf("%w: %q", ErrUnknown, p.Key)
	}
	return Binding{
		AgentID:          f.Owner,
		Key:              p.Key,
		Label:            p.Label,
		Parent:           p.Parent,
		ConfigRevisionID: f.RevisionID,
		ConfigDigest:     f.ConfigDigest,
		ProfileHash:      h,
		Profile:          cloneProfile(p),
	}, nil
}

// VerifyPin is the child-run-start pin check: it validates a persisted
// binding against the CURRENT frozen map so a restart reproduces the
// exact profile. It returns:
//
//   - ErrMissing when the bound key is absent from the current map,
//   - ErrStale when the current parent-config revision/digest differs
//     from the binding's pin,
//   - ErrTampered when the current profile's hash (or label / parent)
//     disagrees with the binding.
//
// On nil error, the returned profile is the exact profile the child
// must run under.
func (f *FrozenMap) VerifyPin(b Binding) (Profile, error) {
	if f == nil {
		return Profile{}, ErrNoMap
	}
	if err := ValidateBinding(b); err != nil {
		return Profile{}, fmt.Errorf("%w: %v", ErrTampered, err)
	}
	p, ok := f.Profile(b.Key)
	if !ok {
		return Profile{}, fmt.Errorf("%w: profile %q", ErrMissing, b.Key)
	}
	if f.RevisionID != b.ConfigRevisionID || f.ConfigDigest != b.ConfigDigest {
		return Profile{}, fmt.Errorf("%w: current parent-config revision %q/%s != bound %q/%s",
			ErrStale, f.RevisionID, shortDigest(f.ConfigDigest), b.ConfigRevisionID, shortDigest(b.ConfigDigest))
	}
	h, _ := f.HashOf(b.Key)
	if h != b.ProfileHash {
		return Profile{}, fmt.Errorf("%w: profile %q hash %s != bound %s", ErrTampered, b.Key, shortDigest(h), shortDigest(b.ProfileHash))
	}
	snapshotHash, _ := b.Profile.Hash()
	if snapshotHash != b.ProfileHash || !profilesEqual(p, b.Profile) {
		return Profile{}, fmt.Errorf("%w: profile snapshot disagrees with current profile", ErrTampered)
	}
	if p.Label != b.Label || p.Parent != b.Parent || p.Parent != f.Owner {
		return Profile{}, fmt.Errorf("%w: profile %q metadata disagrees with the binding", ErrTampered, b.Key)
	}
	return p, nil
}

func cloneProfile(p Profile) Profile {
	return NormalizeProfile(Profile{Key: p.Key, Label: p.Label, Parent: p.Parent, Overlay: p.Overlay})
}

// CloneBinding returns a defensive copy suitable for crossing a persistence
// or task boundary. The snapshot's pointer and slice fields never alias the
// caller's binding.
func CloneBinding(b Binding) Binding {
	b.Profile = cloneProfile(b.Profile)
	return b
}

func profilesEqual(a, b Profile) bool {
	aa, errA := canonicalJSON(NormalizeProfile(a))
	bb, errB := canonicalJSON(NormalizeProfile(b))
	return errA == nil && errB == nil && string(aa) == string(bb)
}

// IntersectStrings returns the sorted intersection of base and keep —
// the narrow-only skills operation. A nil/empty keep returns nil (no
// narrowing).
func IntersectStrings(base, keep []string) []string {
	if len(keep) == 0 || len(base) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(keep))
	for _, k := range keep {
		set[k] = struct{}{}
	}
	var out []string
	for _, b := range base {
		if _, ok := set[b]; ok {
			out = append(out, b)
		}
	}
	sort.Strings(out)
	return out
}

// UnionStrings returns the sorted union of two exclusion sets — the
// narrow-only tools operation (it can only hide more).
func UnionStrings(a, b []string) []string {
	if len(a) == 0 {
		return sortDedup(b)
	}
	if len(b) == 0 {
		return sortDedup(a)
	}
	set := make(map[string]struct{}, len(a)+len(b))
	for _, s := range a {
		set[s] = struct{}{}
	}
	for _, s := range b {
		set[s] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// ClampMax returns v clamped to at most parent. When parent is nil (the
// parent resolved no ceiling), v is returned unchanged. Used for the
// never-widen limits (max-tokens / max-steps / token-budget).
func ClampMax(v int, parent *int) int {
	if parent != nil && v > *parent {
		return *parent
	}
	return v
}

// OverlayClampMaxTokens clamps an overlay max-tokens against the
// parent's RESOLVED effective ceiling. When the parent resolved none
// (nil), the overlay value is used as-is; when the overlay is nil, the
// parent's value is preserved.
func OverlayClampMaxTokens(parentResolved *int, overlay *int) *int {
	if overlay == nil {
		return copyIntPtr(parentResolved)
	}
	if parentResolved != nil && *overlay > *parentResolved {
		v := *parentResolved
		return &v
	}
	return copyIntPtr(overlay)
}

// OverlayClampMaxSteps clamps an overlay max-steps against the parent
// driver's run-loop cap. The overlay may only ever tighten the cap.
func OverlayClampMaxSteps(parentCap int, overlay *int) int {
	if overlay == nil || *overlay <= 0 {
		return parentCap
	}
	if *overlay < parentCap {
		return *overlay
	}
	return parentCap
}

// OverlayClampTokenBudget clamps an overlay token budget against the
// parent's resolved budget. A zero / nil parent budget means "no
// budget" (compression off); an overlay budget then introduces a limit
// (a narrowing), and a parent budget caps the overlay at it.
func OverlayClampTokenBudget(parent int, overlay *int) int {
	if overlay == nil || *overlay <= 0 {
		return parent
	}
	if parent > 0 && *overlay > parent {
		return parent
	}
	return *overlay
}

// BlockName returns the ExtraSystemBlocks block name a profile's
// specialist instructions render under: `virtual_agent.<key>`. The name
// is stable and key-unique so a transcript reader can attribute the
// block, and a future profile edit that removes the block drops it.
func BlockName(k Key) string {
	return "virtual_agent." + string(k)
}

// ctxKey is the unexported context key namespace for this package.
type ctxKey int

const (
	frozenMapKey ctxKey = iota
	runBindingKey
)

// WithFrozenMap attaches the parent run's frozen profile map to ctx so
// the dispatch executor can validate planner spawn selectors against
// it. Per-run state in ctx — never on a shared artifact.
func WithFrozenMap(ctx context.Context, f *FrozenMap) context.Context {
	return context.WithValue(ctx, frozenMapKey, f)
}

// FrozenMapFrom returns the frozen map attached to ctx (nil when
// absent).
func FrozenMapFrom(ctx context.Context) *FrozenMap {
	f, _ := ctx.Value(frozenMapKey).(*FrozenMap)
	return f
}

// WithRunBinding attaches the CURRENT run's own profile binding to ctx
// so the dispatch executor can enforce the non-recursion rule (a
// virtual-profile run cannot spawn another virtual profile). Attached
// only when the run IS a virtual-profile run.
func WithRunBinding(ctx context.Context, b *Binding) context.Context {
	return context.WithValue(ctx, runBindingKey, b)
}

// RunBindingFrom returns the current run's own profile binding (nil
// when the run is not a virtual-profile run).
func RunBindingFrom(ctx context.Context) *Binding {
	b, _ := ctx.Value(runBindingKey).(*Binding)
	return b
}

func copyStringPtr(p *string) *string {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func copyFloat64Ptr(p *float64) *float64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func copyIntPtr(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// sortDedup returns a sorted, de-duplicated copy of in. A nil input
// returns nil; an empty non-nil input returns nil (the callers that
// need empty-set PRESENCE handle it explicitly).
func sortDedup(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// canonicalJSON marshals v, re-decodes into a generic value, and
// re-marshals so map keys are emitted in sorted order — the same
// canonicalization the agentcfg content hash uses, so the profile hash
// is stable across declarations.
func canonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	return json.Marshal(generic)
}

// shortDigest renders a digest for error messages: "" as "none", a
// full digest truncated to 12 hex chars.
func shortDigest(d string) string {
	if d == "" {
		return "none"
	}
	if len(d) > 12 {
		return d[:12]
	}
	return d
}
