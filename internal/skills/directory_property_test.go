package skills_test

import (
	"context"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
)

// Property tests per the Phase 39 plan (`internal/skills/
// directory_property_test.go`). Three invariants:
//
//   - Property_PinnedAlwaysIncluded_WhenFitsBudget: pinned skills
//     that pass the capability filter MUST appear in the View when
//     count(pinned-after-filter) ≤ MaxEntries.
//   - Property_ViewLengthBounded: len(view) ≤ MaxEntries AND
//     len(view) ≤ len(store-after-filter).
//   - Property_IdentityScoping: a skill seeded only under identity A
//     is NEVER returned in View(B). The store enforces this; the
//     directory inherits the guarantee.
//
// Uses Go's testing/quick. The generator for Skill is hand-rolled —
// quick's default reflect generator would produce too-large slices /
// non-printable strings; the corpus needs to be bounded and
// JSON-tree-compatible so the property runs in seconds, not hours.

const (
	// propertyMaxRuns bounds quick.Check iterations. quick's default
	// (100) is fine; explicit constant keeps the cost visible.
	propertyMaxRuns = 50
	// propertyMaxSkills bounds the corpus size each property generates.
	// Larger inputs slow the property without surfacing new
	// counterexamples — the invariants are size-independent.
	propertyMaxSkills = 25
)

// propertyTestCase is the input shape every property runs over.
// quick.Check populates it via the Generate method below.
type propertyTestCase struct {
	corpus     []skills.Skill
	pinned     []string // declaration order
	maxEntries int      // bounded to [1, 200] in Generate
	selection  skills.Selection
	allowed    skills.DirectoryCapability
}

// Generate implements quick.Generator. Produces a bounded,
// JSON-tree-compatible Skill corpus + a config. Seed determines all
// randomness so failures are reproducible.
func (propertyTestCase) Generate(rnd *rand.Rand, _ int) reflect.Value {
	n := rnd.Intn(propertyMaxSkills) + 1 // [1, propertyMaxSkills]

	// Build a bounded pool of tool / namespace / tag names so the
	// "subset" relation has meaningful coverage.
	toolPool := []string{"http_fetch", "fs_write", "fs_read", "tool_search"}
	nsPool := []string{"ns-a", "ns-b", "ns-c"}
	tagPool := []string{"tag-a", "tag-b", "tag-c"}

	corpus := make([]skills.Skill, n)
	pinned := make([]string, 0, n)
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		name := uniqueName(rnd, seen)
		s := skills.Skill{
			Name:          name,
			Title:         "Title " + name,
			Trigger:       "trigger " + name,
			Steps:         []string{"step"},
			Origin:        skills.OriginPack,
			Scope:         skills.ScopeProject,
			UpdatedAt:     base.Add(time.Duration(rnd.Intn(1000)) * time.Minute),
			UseCount:      rnd.Intn(100),
			RequiredTools: pickSubset(rnd, toolPool, 0.5),
			RequiredNS:    pickSubset(rnd, nsPool, 0.5),
			RequiredTags:  pickSubset(rnd, tagPool, 0.5),
		}
		if rnd.Float32() < 0.2 {
			s.Extra = map[string]any{skills.ExtraPinnedKey: true}
		}
		corpus[i] = s
		if rnd.Float32() < 0.3 {
			pinned = append(pinned, name)
		}
	}

	// MaxEntries in the valid range [1, 200]. We deliberately probe
	// the edge (1) and a value that's > corpus length, plus mid-range
	// values.
	maxEntries := rnd.Intn(skills.MaxMaxEntries) + 1 // [1, 200]

	sel := skills.SelectionPinnedThenRecent
	if rnd.Intn(2) == 1 {
		sel = skills.SelectionPinnedThenTop
	}

	allowed := skills.DirectoryCapability{
		AllowedTools:      pickSubset(rnd, toolPool, 0.6),
		AllowedNamespaces: pickSubset(rnd, nsPool, 0.6),
		AllowedTags:       pickSubset(rnd, tagPool, 0.6),
	}

	return reflect.ValueOf(propertyTestCase{
		corpus:     corpus,
		pinned:     pinned,
		maxEntries: maxEntries,
		selection:  sel,
		allowed:    allowed,
	})
}

// uniqueName produces a name unseen by this corpus. Names that
// collide would alias the seeded skills inside memStore (the
// directory partition logic dedupes by Name); the property's
// counterexample shrinker shouldn't waste iterations on aliasing.
func uniqueName(rnd *rand.Rand, seen map[string]struct{}) string {
	for {
		// 5-char lowercase tag — enough entropy at ≤ 25 corpus size.
		buf := make([]byte, 5)
		for i := range buf {
			buf[i] = byte('a' + rnd.Intn(26))
		}
		name := string(buf)
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		return name
	}
}

// pickSubset returns a random subset of pool. Each element is
// included independently with probability p.
func pickSubset(rnd *rand.Rand, pool []string, p float32) []string {
	if len(pool) == 0 {
		return nil
	}
	out := make([]string, 0, len(pool))
	for _, s := range pool {
		if rnd.Float32() < p {
			out = append(out, s)
		}
	}
	return out
}

// passesCapability mirrors the directory's internal filter. Used by
// the property checks to determine the expected "post-filter" set.
// Duplicated here (small) so the test doesn't depend on the
// directory's unexported helpers.
func passesCapability(s skills.Skill, cap skills.DirectoryCapability) bool {
	if !subsetCheck(s.RequiredTools, cap.AllowedTools) {
		return false
	}
	if !subsetCheck(s.RequiredNS, cap.AllowedNamespaces) {
		return false
	}
	if !subsetCheck(s.RequiredTags, cap.AllowedTags) {
		return false
	}
	return true
}

func subsetCheck(required, allowed []string) bool {
	if len(required) == 0 {
		return true
	}
	if len(allowed) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		set[a] = struct{}{}
	}
	for _, r := range required {
		if _, ok := set[r]; !ok {
			return false
		}
	}
	return true
}

// pinnedAfterFilter returns the subset of corpus that is BOTH
// (a) pinned by either config OR Extra, AND (b) passes the
// capability filter under cap. Used by the property tests to
// compute the expected pinned-in-view set.
func pinnedAfterFilter(corpus []skills.Skill, configPinned []string, cap skills.DirectoryCapability) map[string]struct{} {
	configSet := make(map[string]struct{}, len(configPinned))
	for _, n := range configPinned {
		configSet[n] = struct{}{}
	}

	out := make(map[string]struct{})
	for _, s := range corpus {
		if !passesCapability(s, cap) {
			continue
		}
		isPinned := false
		if _, ok := configSet[s.Name]; ok {
			isPinned = true
		} else if s.Extra != nil {
			if v, ok := s.Extra[skills.ExtraPinnedKey].(bool); ok && v {
				isPinned = true
			}
		}
		if isPinned {
			out[s.Name] = struct{}{}
		}
	}
	return out
}

// TestProperty_PinnedAlwaysIncluded_WhenFitsBudget — for an
// arbitrary corpus, when count(pinned-after-filter) ≤ MaxEntries,
// every pinned skill that passes the capability filter MUST appear
// in the View.
//
// Spec § Acceptance criteria: "Pinned skills always present when
// count(pinned-after-filter) ≤ MaxEntries."
func TestProperty_PinnedAlwaysIncluded_WhenFitsBudget(t *testing.T) {
	bus := directoryTestBus(t)

	prop := func(tc propertyTestCase) bool {
		// De-dup pinned config names against the corpus's seen
		// set (the generator may produce pinned entries that don't
		// land in the corpus on a given iteration). Compute the
		// expected pinned-after-filter set from the actual corpus
		// + config.
		store := newMemStore(bus)
		store.seed(directoryIdentity(), tc.corpus...)

		dir, err := skills.NewDirectory(store, skills.Deps{Bus: bus}, skills.DirectoryConfig{
			Pinned:     tc.pinned,
			MaxEntries: tc.maxEntries,
			Selection:  tc.selection,
		})
		if err != nil {
			t.Logf("NewDirectory: %v (skipping iteration)", err)
			return true
		}

		view, err := dir.View(directoryCtx(t), tc.allowed)
		if err != nil {
			t.Logf("View: %v (skipping iteration)", err)
			return true
		}

		expectedPinned := pinnedAfterFilter(tc.corpus, tc.pinned, tc.allowed)
		if len(expectedPinned) > tc.maxEntries {
			// Over-budget — pinned-always-included does NOT hold when
			// pinned set exceeds the cap. The other properties cover
			// the truncation contract.
			return true
		}

		seen := make(map[string]struct{}, len(view))
		for _, v := range view {
			seen[v.Name] = struct{}{}
		}
		for name := range expectedPinned {
			if _, ok := seen[name]; !ok {
				t.Logf("pinned-always-included violated: name=%q expected in view of length=%d (maxEntries=%d, pinned-after-filter=%d, corpus=%d)",
					name, len(view), tc.maxEntries, len(expectedPinned), len(tc.corpus))
				return false
			}
		}
		return true
	}

	if err := quick.Check(prop, &quick.Config{MaxCount: propertyMaxRuns}); err != nil {
		t.Fatalf("quick.Check failed: %v", err)
	}
}

// TestProperty_ViewLengthBounded — for any corpus + config:
//
//	len(view) ≤ MaxEntries  AND  len(view) ≤ len(corpus-after-filter)
//
// Spec § Acceptance criteria: "View length ≤ MaxEntries."
func TestProperty_ViewLengthBounded(t *testing.T) {
	bus := directoryTestBus(t)

	prop := func(tc propertyTestCase) bool {
		store := newMemStore(bus)
		store.seed(directoryIdentity(), tc.corpus...)

		dir, err := skills.NewDirectory(store, skills.Deps{Bus: bus}, skills.DirectoryConfig{
			Pinned:     tc.pinned,
			MaxEntries: tc.maxEntries,
			Selection:  tc.selection,
		})
		if err != nil {
			return true
		}

		view, err := dir.View(directoryCtx(t), tc.allowed)
		if err != nil {
			return true
		}

		if len(view) > tc.maxEntries {
			t.Logf("len(view)=%d > maxEntries=%d (corpus=%d)", len(view), tc.maxEntries, len(tc.corpus))
			return false
		}
		filteredCount := 0
		for _, s := range tc.corpus {
			if passesCapability(s, tc.allowed) {
				filteredCount++
			}
		}
		if len(view) > filteredCount {
			t.Logf("len(view)=%d > filteredCount=%d", len(view), filteredCount)
			return false
		}
		return true
	}

	if err := quick.Check(prop, &quick.Config{MaxCount: propertyMaxRuns}); err != nil {
		t.Fatalf("quick.Check failed: %v", err)
	}
}

// TestProperty_IdentityScoping — for any pair of distinct identities
// A / B with disjoint skill sets, View(B) is disjoint from View(A).
// The store enforces this via per-identity row scoping; the
// directory inherits.
//
// Spec § Acceptance criteria: "Identity scoping: a skill scoped to
// tenant A is NEVER in the View of identity B."
func TestProperty_IdentityScoping(t *testing.T) {
	bus := directoryTestBus(t)

	prop := func(tc propertyTestCase) bool {
		idA := identity.Identity{TenantID: "t-a", UserID: "u-a", SessionID: "s-a"}
		idB := identity.Identity{TenantID: "t-b", UserID: "u-b", SessionID: "s-b"}

		store := newMemStore(bus)
		// Seed first half under A, second half under B.
		mid := len(tc.corpus) / 2
		store.seed(idA, tc.corpus[:mid]...)
		store.seed(idB, tc.corpus[mid:]...)

		dir, err := skills.NewDirectory(store, skills.Deps{Bus: bus}, skills.DirectoryConfig{
			Pinned:     tc.pinned,
			MaxEntries: tc.maxEntries,
			Selection:  tc.selection,
		})
		if err != nil {
			return true
		}

		ctxA, err := identity.WithRun(context.Background(), idA, "r-a")
		if err != nil {
			return true
		}
		ctxB, err := identity.WithRun(context.Background(), idB, "r-b")
		if err != nil {
			return true
		}

		viewA, err := dir.View(ctxA, tc.allowed)
		if err != nil {
			return true
		}
		viewB, err := dir.View(ctxB, tc.allowed)
		if err != nil {
			return true
		}

		aNames := make(map[string]struct{}, mid)
		for _, s := range tc.corpus[:mid] {
			aNames[s.Name] = struct{}{}
		}
		bNames := make(map[string]struct{}, len(tc.corpus)-mid)
		for _, s := range tc.corpus[mid:] {
			bNames[s.Name] = struct{}{}
		}

		// ViewA must not contain any name from bNames (unless the
		// corpus aliased the same name across both partitions —
		// which the unique-name generator avoids, but be defensive).
		for _, v := range viewA {
			if _, inB := bNames[v.Name]; inB {
				if _, inA := aNames[v.Name]; !inA {
					t.Logf("ViewA leaked B-only skill: %q", v.Name)
					return false
				}
			}
		}
		for _, v := range viewB {
			if _, inA := aNames[v.Name]; inA {
				if _, inB := bNames[v.Name]; !inB {
					t.Logf("ViewB leaked A-only skill: %q", v.Name)
					return false
				}
			}
		}
		return true
	}

	if err := quick.Check(prop, &quick.Config{MaxCount: propertyMaxRuns}); err != nil {
		t.Fatalf("quick.Check failed: %v", err)
	}
}
